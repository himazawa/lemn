#!/usr/bin/env python3
"""Evaluate LEMN's real memory-worthiness and extraction pipeline in fresh scopes."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
REPO_DIR = SCRIPT_DIR.parent
VECTOR_DIMS = 1024


def read_cases(path: Path) -> list[dict[str, Any]]:
    cases = []
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        try:
            case = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"{path}:{line_number}: invalid JSON: {exc}") from exc
        cases.append(case)
    if not cases:
        raise ValueError("case file has no cases")
    seen: set[str] = set()
    for case in cases:
        case_id = str(case.get("id", "")).strip()
        if not case_id or case_id in seen:
            raise ValueError("case IDs must be present and unique")
        seen.add(case_id)
        if not case.get("project_key") or not case.get("user_message"):
            raise ValueError(f"{case_id}: project_key and user_message are required")
        if not isinstance(case.get("assistant_response"), str):
            raise ValueError(f"{case_id}: assistant_response must be a string")
        if not isinstance(case.get("expected_memory_worthy"), bool):
            raise ValueError(f"{case_id}: expected_memory_worthy must be boolean")
        if case["expected_memory_worthy"] and not case.get("summary_terms"):
            raise ValueError(f"{case_id}: positive cases need summary_terms")
        for memory in case.get("initial_memories", []):
            if not isinstance(memory, str) or not memory.strip():
                raise ValueError(f"{case_id}: initial_memories must be non-empty strings")
    return cases


def request_json(url: str, payload: dict[str, Any] | None, headers: dict[str, str], timeout: int) -> Any:
    body = json.dumps(payload).encode("utf-8") if payload is not None else None
    request = urllib.request.Request(url, data=body, headers=headers, method="POST" if payload is not None else "GET")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {exc.code} from {url}: {detail[:1000]}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"request to {url} failed: {exc.reason}") from exc


def sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def run_psql(sql: str) -> list[str]:
    command = [
        "docker", "compose", "exec", "-T", "postgres", "sh", "-c",
        'psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
    ]
    result = subprocess.run(command, cwd=REPO_DIR, input=sql, text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f"Postgres fixture operation failed: {result.stderr.strip()}")
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def embed(text: str, args: argparse.Namespace, headers: dict[str, str]) -> list[float]:
    result = request_json(
        args.embeddings_url,
        {"model": args.embedding_model, "input": text},
        headers,
        args.request_timeout,
    )
    try:
        vector = result["data"][0]["embedding"]
    except (KeyError, IndexError, TypeError) as exc:
        raise RuntimeError("embedding response missing data[0].embedding") from exc
    if len(vector) != VECTOR_DIMS:
        raise RuntimeError(f"embedding has {len(vector)} dimensions; schema expects {VECTOR_DIMS}")
    return [float(value) for value in vector]


def vector_sql(vector: list[float]) -> str:
    return "[" + ",".join(f"{value:.9g}" for value in vector) + "]"


def scope_key(case_id: str, repeat: int) -> str:
    return f"{case_id}#repeat-{repeat}"


def seed_existing_memories(case_runs: list[tuple[dict[str, Any], int]], scopes: dict[str, str], run_id: str, args: argparse.Namespace, headers: dict[str, str]) -> dict[str, list[int]]:
    statements = ["BEGIN;"]
    ids_by_case: dict[str, list[int]] = {}
    expected_count = 0
    for case, repeat in case_runs:
        key = scope_key(case["id"], repeat)
        case_ids = []
        for summary in case.get("initial_memories", []):
            vector = vector_sql(embed(summary, args, headers))
            provenance = json.dumps({"write_bench_run": run_id, "case_id": case["id"], "repeat": repeat, "fixture": True}, separators=(",", ":"))
            statements.append(
                "INSERT INTO lemn_memories "
                "(state, project_id, confidence, category, summary, rationale, embedding, provenance) VALUES ("
                f"'AUTHORITATIVE', {sql_literal(scopes[key])}, 1.0, 'architecture', "
                f"{sql_literal(summary)}, 'write benchmark starting fact', {sql_literal(vector)}::vector, "
                f"{sql_literal(provenance)}::jsonb) RETURNING id;"
            )
            expected_count += 1
        ids_by_case[key] = case_ids
    statements.append("COMMIT;")
    returned = run_psql("\n".join(statements))
    if len(returned) != expected_count or any(not value.isdigit() for value in returned):
        cleanup_memories(scopes)
        raise RuntimeError(f"expected {expected_count} initial memory IDs, received {returned}")
    index = 0
    for case, repeat in case_runs:
        key = scope_key(case["id"], repeat)
        count = len(case.get("initial_memories", []))
        ids_by_case[key] = [int(value) for value in returned[index:index + count]]
        index += count
    return ids_by_case


def cleanup_memories(scopes: dict[str, str]) -> None:
    scope_list = ",".join(sql_literal(scope) for scope in scopes.values())
    run_psql(
        "BEGIN;\n"
        f"DELETE FROM lemn_edges WHERE source_id IN (SELECT id FROM lemn_memories WHERE project_id IN ({scope_list})) "
        f"OR target_id IN (SELECT id FROM lemn_memories WHERE project_id IN ({scope_list}));\n"
        f"DELETE FROM lemn_memories WHERE project_id IN ({scope_list});\n"
        "COMMIT;"
    )
    remaining = run_psql(f"SELECT count(*) FROM lemn_memories WHERE project_id IN ({scope_list});")
    if remaining != ["0"]:
        raise RuntimeError(f"write-benchmark cleanup left rows in scopes {list(scopes.values())}: {remaining}")


def read_created_memories(scope: str, turn_id: str) -> list[dict[str, Any]]:
    query = (
        "SELECT COALESCE(json_agg(json_build_object('id', id, 'state', state, 'summary', summary, "
        "'confidence', confidence, 'provenance', provenance)), '[]'::json)::text "
        f"FROM lemn_memories WHERE project_id = {sql_literal(scope)} "
        f"AND provenance->>'source_turn_id' = {sql_literal(turn_id)};"
    )
    result = run_psql(query)
    return json.loads(result[0]) if result else []


def start_isolated_daemon(run_id: str, startup_timeout: int) -> tuple[str, str]:
    sqlite_path = f"/tmp/lemn-writebench-{run_id}.db"
    command = [
        "docker", "compose", "run", "--detach", "--rm", "--build", "--no-deps",
        "--publish", "127.0.0.1::8080", "--env", f"LEMN_SQLITE_PATH={sqlite_path}", "daemon",
    ]
    result = subprocess.run(command, cwd=REPO_DIR, text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f"could not start isolated daemon: {result.stderr.strip()}")
    container_id = result.stdout.strip().splitlines()[-1]
    if not re.fullmatch(r"[0-9a-f]{12,64}", container_id):
        raise RuntimeError(f"unexpected docker compose run output: {result.stdout.strip()}")
    port_result = subprocess.run(["docker", "port", container_id, "8080/tcp"], text=True, capture_output=True, check=False)
    if port_result.returncode:
        subprocess.run(["docker", "stop", container_id], capture_output=True, check=False)
        raise RuntimeError(f"could not discover isolated daemon port: {port_result.stderr.strip()}")
    mapping = port_result.stdout.strip().splitlines()[0]
    port = mapping.rsplit(":", 1)[-1]
    base_url = f"http://127.0.0.1:{port}"
    deadline = time.monotonic() + startup_timeout
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(base_url + "/healthz", timeout=2):
                print(f"Started isolated daemon on {base_url} with temporary SQLite store.")
                return container_id, base_url
        except (OSError, urllib.error.URLError):
            time.sleep(0.5)
    subprocess.run(["docker", "stop", container_id], capture_output=True, check=False)
    raise TimeoutError("isolated daemon did not become healthy before timeout")


def stop_isolated_daemon(container_id: str) -> None:
    result = subprocess.run(["docker", "stop", container_id], text=True, capture_output=True, check=False)
    if result.returncode and "No such container" not in result.stderr:
        raise RuntimeError(f"could not stop isolated daemon: {result.stderr.strip()}")


def wait_for_job(base_url: str, job_id: int, headers: dict[str, str], timeout: int) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = request_json(f"{base_url}/jobs/{job_id}", None, headers, 10)
        if result.get("status") in {"COMPLETED", "FAILED"}:
            return result
        time.sleep(0.5)
    raise TimeoutError(f"job {job_id} did not finish within {timeout}s")


def summary_terms_match(summary: str, terms: list[str]) -> bool:
    lowered = summary.casefold()
    return all(term.casefold() in lowered for term in terms)


def evaluate(results: list[dict[str, Any]], cases_by_id: dict[str, dict[str, Any]]) -> dict[str, Any]:
    gate_tp = gate_fp = gate_tn = gate_fn = 0
    final_tp = final_fp = final_tn = final_fn = 0
    extractor_vetoes = 0
    summary_checked = summary_passed = 0
    relation_checked = relation_passed = 0
    global_checked = global_passed = 0
    evidence_scores: list[float] = []
    target_scores: list[float] = []
    for row in results:
        case = cases_by_id[row["case_id"]]
        extraction = row.get("extraction") or {}
        expected_final = case["expected_memory_worthy"]
        actual_final = bool(extraction.get("memory_worthy"))
        expected_gate = case.get("expected_gate_passed", expected_final)
        actual_gate = bool(extraction.get("gate_passed"))
        if expected_gate and actual_gate:
            gate_tp += 1
        elif actual_gate:
            gate_fp += 1
        elif expected_gate:
            gate_fn += 1
        else:
            gate_tn += 1
        if expected_final and actual_final:
            final_tp += 1
        elif actual_final:
            final_fp += 1
        elif expected_final:
            final_fn += 1
        else:
            final_tn += 1
        if actual_gate and not actual_final:
            extractor_vetoes += 1
        if expected_final:
            summary_checked += 1
            summary = str(extraction.get("summary", ""))
            summary_passed += int(summary_terms_match(summary, case.get("summary_terms", [])))
        if case.get("expected_relation"):
            relation_checked += 1
            actual_relation = str(extraction.get("relation", "independent"))
            relation_passed += int(actual_relation == case["expected_relation"])
        if "expected_global_scoped" in case:
            global_checked += 1
            global_passed += int(bool(extraction.get("global_scoped")) == case["expected_global_scoped"])
        for memory in row.get("created_memories", []):
            signals = memory.get("provenance", {}).get("signals", {})
            if isinstance(signals.get("evidence_similarity"), (int, float)):
                evidence_scores.append(float(signals["evidence_similarity"]))
            if isinstance(signals.get("target_similarity"), (int, float)):
                target_scores.append(float(signals["target_similarity"]))
    gate_precision = gate_tp / (gate_tp + gate_fp) if gate_tp + gate_fp else 0.0
    gate_recall = gate_tp / (gate_tp + gate_fn) if gate_tp + gate_fn else 0.0
    gate_f1 = 2 * gate_precision * gate_recall / (gate_precision + gate_recall) if gate_precision + gate_recall else 0.0
    final_precision = final_tp / (final_tp + final_fp) if final_tp + final_fp else 0.0
    final_recall = final_tp / (final_tp + final_fn) if final_tp + final_fn else 0.0
    final_f1 = 2 * final_precision * final_recall / (final_precision + final_recall) if final_precision + final_recall else 0.0

    def score_distribution(values: list[float]) -> dict[str, float | int | None]:
        if not values:
            return {"count": 0, "mean": None, "min": None, "max": None}
        return {"count": len(values), "mean": sum(values) / len(values), "min": min(values), "max": max(values)}

    return {
        "cases": len(results),
        "gate_true_positive": gate_tp, "gate_false_positive": gate_fp,
        "gate_true_negative": gate_tn, "gate_false_negative": gate_fn,
        "gate_precision": gate_precision, "gate_recall": gate_recall, "gate_f1": gate_f1,
        "final_true_positive": final_tp, "final_false_positive": final_fp,
        "final_true_negative": final_tn, "final_false_negative": final_fn,
        "final_precision": final_precision, "final_recall": final_recall, "final_f1": final_f1,
        "extractor_vetoes": extractor_vetoes,
        "summary_terms_passed": summary_passed, "summary_terms_checked": summary_checked,
        "relation_passed": relation_passed, "relation_checked": relation_checked,
        "global_scope_passed": global_passed, "global_scope_checked": global_checked,
        "evidence_cosine": score_distribution(evidence_scores),
        "target_cosine": score_distribution(target_scores),
    }


def run_benchmark(args: argparse.Namespace) -> None:
    cases = read_cases(Path(args.cases))
    if args.case_ids:
        requested = {item.strip() for item in args.case_ids.split(",") if item.strip()}
        known = {case["id"] for case in cases}
        unknown = requested - known
        if unknown:
            raise ValueError(f"unknown case IDs: {', '.join(sorted(unknown))}")
        cases = [case for case in cases if case["id"] in requested]
    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    if args.validate_only:
        seeded_count = sum(len(case.get("initial_memories", [])) for case in cases) * args.repeats
        print(f"Validated {len(cases)} labeled turns across {args.repeats} repeat(s); {seeded_count} optional starting memories; no services will be started.")
        return

    secret = os.environ.get(args.shared_secret_env, "")
    if not secret:
        raise ValueError(f"environment variable {args.shared_secret_env} is required")
    backend_key = os.environ.get(args.backend_key_env, "") if args.backend_key_env else ""
    embedding_headers = {"Content-Type": "application/json"}
    if backend_key:
        embedding_headers["Authorization"] = f"Bearer {backend_key}"
    auth_headers = {"Content-Type": "application/json", "Authorization": f"Bearer {secret}"}
    run_id = uuid.uuid4().hex[:12]
    case_runs = [(case, repeat) for repeat in range(1, args.repeats + 1) for case in cases]
    scopes = {
        scope_key(case["id"], repeat): f"lemn-writebench-{run_id}-{case['id']}-r{repeat}"
        for case, repeat in case_runs
    }
    container_id = ""
    memory_rows_seeded = False
    results = []
    try:
        container_id, base_url = start_isolated_daemon(run_id, args.startup_timeout)
        seeded = seed_existing_memories(case_runs, scopes, run_id, args, embedding_headers)
        memory_rows_seeded = True
        for case, repeat in case_runs:
            case_scope = scope_key(case["id"], repeat)
            turn_id = f"writebench_{run_id}_{case['id']}_r{repeat}"
            payload = {
                "id": turn_id,
                "project_id": scopes[case_scope],
                "user_message": case["user_message"],
                "assistant_response": case["assistant_response"],
                "tool_calls": case.get("tool_calls", []),
            }
            queued = request_json(base_url + "/log", payload, auth_headers, args.request_timeout)
            job = wait_for_job(base_url, int(queued["job_id"]), auth_headers, args.job_timeout)
            if job["status"] != "COMPLETED":
                results.append({
                    "case_id": case["id"],
                    "repeat": repeat,
                    "turn_id": turn_id,
                    "project_id": scopes[case_scope],
                    "job_id": queued["job_id"],
                    "job_error": job.get("error", f"job status {job['status']}"),
                    "extraction": {},
                    "created_memories": [],
                })
                print(f"{case['id']}: JOB FAILED: {job.get('error', job['status'])}")
                continue
            extraction = job.get("extraction") or {}
            memories = read_created_memories(scopes[case_scope], turn_id)
            results.append({
                "case_id": case["id"],
                "repeat": repeat,
                "turn_id": turn_id,
                "project_id": scopes[case_scope],
                "job_id": queued["job_id"],
                "extraction": extraction,
                "created_memories": memories,
            })
            print(f"{case['id']}: memory_worthy={extraction.get('memory_worthy')} summary={extraction.get('summary', '')!r}")
    finally:
        cleanup_error = None
        if memory_rows_seeded:
            try:
                cleanup_memories(scopes)
            except Exception as exc:
                cleanup_error = exc
        if container_id:
            try:
                stop_isolated_daemon(container_id)
            except Exception as exc:
                cleanup_error = cleanup_error or exc
        if cleanup_error:
            raise cleanup_error

    cases_by_id = {case["id"]: case for case in cases}
    report = evaluate(results, cases_by_id)
    report["run_id"] = run_id
    report["details"] = results
    (out_dir / "write-results.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({key: value for key, value in report.items() if key != "details"}, indent=2))
    print(f"Detailed output: {out_dir / 'write-results.json'}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", default=str(SCRIPT_DIR / "memory_write_bench_cases.jsonl"))
    parser.add_argument("--out", default="/tmp/lemn-memory-write-bench")
    parser.add_argument("--case-ids", help="comma-separated labeled case IDs to run")
    parser.add_argument("--embeddings-url", default=os.environ.get("LEMN_EMBEDDING_URL", "http://localhost:9001/v1/embeddings"))
    parser.add_argument("--embedding-model", default=os.environ.get("LEMN_EMBEDDING_MODEL", "bge-m3-mlx-fp16"))
    parser.add_argument("--shared-secret-env", default="LEMN_SHARED_SECRET")
    parser.add_argument("--backend-key-env", default="LEMN_BACKEND_API_KEY")
    parser.add_argument("--request-timeout", type=int, default=120)
    parser.add_argument("--job-timeout", type=int, default=300)
    parser.add_argument("--startup-timeout", type=int, default=60)
    parser.add_argument("--repeats", type=int, default=1, help="independent fresh-scope runs per labeled turn")
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()
    if min(args.request_timeout, args.job_timeout, args.startup_timeout, args.repeats) < 1:
        parser.error("timeouts and repeats must be positive")
    return args


if __name__ == "__main__":
    try:
        arguments = parse_args()
        run_benchmark(arguments)
    except (OSError, ValueError, RuntimeError, TimeoutError) as exc:
        print(f"memory_write_bench: {exc}", file=sys.stderr)
        sys.exit(2)
