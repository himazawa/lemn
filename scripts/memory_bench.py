#!/usr/bin/env python3
"""Compare no-memory, AGENTS.md, and LEMN retrieval on identical questions."""

from __future__ import annotations

import argparse
import csv
import json
import os
import random
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from collections import defaultdict
from pathlib import Path
from typing import Any

ARMS = ("none", "agents", "lemn")
SCRIPT_DIR = Path(__file__).resolve().parent
REPO_DIR = SCRIPT_DIR.parent
SYSTEM_PROMPT = (
    "Answer the user's project-specific question accurately and concisely. "
    "Use supplied context when relevant. If the context does not contain the "
    "answer, say that you do not know instead of guessing. Do not claim that "
    "you accessed files or tools."
)


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows = []
    for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"{path}:{line_number}: invalid JSON: {exc}") from exc
        if not isinstance(row, dict):
            raise ValueError(f"{path}:{line_number}: expected a JSON object")
        rows.append(row)
    return rows


def validate_tasks(tasks: list[dict[str, Any]]) -> None:
    if not tasks:
        raise ValueError("task file contains no tasks")
    seen: set[str] = set()
    for index, task in enumerate(tasks, 1):
        task_id = str(task.get("id", "")).strip()
        if not task_id or task_id in seen:
            raise ValueError(f"task {index}: id must be present and unique")
        seen.add(task_id)
        if not str(task.get("query", "")).strip():
            raise ValueError(f"task {task_id}: query is required")
        if not str(task.get("category", "")).strip():
            raise ValueError(f"task {task_id}: category is required")
        if "expected" not in task or not str(task.get("rubric", "")).strip():
            raise ValueError(f"task {task_id}: expected and rubric are required for scoring")
        if not str(task.get("project_key", "")).strip():
            raise ValueError(f"task {task_id}: project_key is required for isolated fixtures")
        if task.get("phase") not in {"before_learning", "after_learning"}:
            raise ValueError(f"task {task_id}: phase must be before_learning or after_learning")


def request_json(url: str, payload: dict[str, Any], headers: dict[str, str], timeout: int) -> Any:
    body = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"POST {url} returned HTTP {exc.code}: {detail[:1000]}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"POST {url} failed: {exc.reason}") from exc


def fetch_memories(args: argparse.Namespace, query: str, project_id: str, headers: dict[str, str]) -> list[dict[str, Any]]:
    payload = {"query": query, "project_id": project_id, "limit": args.retrieval_limit}
    result = request_json(args.daemon_url.rstrip("/") + "/retrieve", payload, headers, args.timeout)
    if not isinstance(result, list):
        raise RuntimeError("LEMN /retrieve response was not a JSON array")
    return result


def sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def run_psql(sql: str) -> list[str]:
    command = [
        "docker", "compose", "exec", "-T", "postgres", "sh", "-c",
        'psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
    ]
    try:
        result = subprocess.run(command, cwd=REPO_DIR, input=sql, text=True, capture_output=True, check=False)
    except OSError as exc:
        raise RuntimeError(f"could not run Docker Compose psql: {exc}") from exc
    if result.returncode:
        raise RuntimeError(f"temporary benchmark database operation failed: {result.stderr.strip()}")
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def create_isolated_fixtures(args: argparse.Namespace, run_id: str, embedding_headers: dict[str, str]) -> tuple[dict[str, str], dict[str, str]]:
    corpus = json.loads(Path(args.corpus).read_text(encoding="utf-8"))
    projects = corpus.get("projects", {})
    if not projects:
        raise ValueError("fixture corpus must define at least one project")
    scopes = {key: f"lemn-bench-{run_id}-{key}" for key in projects}
    contexts = {}
    for key, project in projects.items():
        agents_path = SCRIPT_DIR / str(project.get("agents_file", ""))
        contexts[key] = agents_path.read_text(encoding="utf-8").strip() if agents_path.is_file() else ""
    if any(not context for context in contexts.values()):
        raise ValueError("every fixture project needs a non-empty AGENTS.md fixture")

    statements = ["BEGIN;"]
    expected_inserts = 0
    for key, project in projects.items():
        facts = project.get("memories", [])
        if not facts:
            raise ValueError(f"fixture project {key} has no memories")
        for fact in facts:
            summary = str(fact.get("summary", "")).strip()
            state = str(fact.get("state", "AUTHORITATIVE"))
            if not summary or state not in {"AUTHORITATIVE", "SUPERSEDED", "CONTRADICTED"}:
                raise ValueError(f"fixture project {key} has invalid memory fact")
            embedding_result = request_json(
                args.embeddings_url,
                {"model": args.embedding_model, "input": summary},
                embedding_headers,
                args.timeout,
            )
            try:
                vector = embedding_result["data"][0]["embedding"]
            except (KeyError, IndexError, TypeError) as exc:
                raise RuntimeError("embedding response missing data[0].embedding") from exc
            if len(vector) != 1024:
                raise RuntimeError(f"fixture embedding has {len(vector)} dimensions; schema expects 1024")
            vector_literal = "[" + ",".join(f"{float(value):.9g}" for value in vector) + "]"
            provenance = json.dumps({"benchmark_run": run_id, "fixture_project": key}, separators=(",", ":"))
            statements.append(
                "INSERT INTO lemn_memories "
                "(state, project_id, confidence, category, summary, rationale, embedding, provenance) VALUES ("
                f"{sql_literal(state)}, {sql_literal(scopes[key])}, 1.0, 'architecture', "
                f"{sql_literal(summary)}, 'isolated memory benchmark fixture', "
                f"{sql_literal(vector_literal)}::vector, {sql_literal(provenance)}::jsonb) RETURNING id;"
            )
            expected_inserts += 1
    statements.append("COMMIT;")
    ids = run_psql("\n".join(statements))
    if len(ids) != expected_inserts or any(not value.isdigit() for value in ids):
        cleanup_isolated_fixtures(run_id)
        raise RuntimeError(f"expected {expected_inserts} seeded memory IDs, got {ids}")
    print(f"Created disposable scopes {', '.join(scopes.values())}; seeded {expected_inserts} synthetic memory facts.")
    return scopes, contexts


def apply_learning_updates(args: argparse.Namespace, run_id: str, scopes: dict[str, str], embedding_headers: dict[str, str]) -> None:
    corpus = json.loads(Path(args.corpus).read_text(encoding="utf-8"))
    statements = ["BEGIN;"]
    update_count = 0
    for key, project in corpus["projects"].items():
        for update in project.get("updates", []):
            target_summary = str(update.get("target_summary", "")).strip()
            new_summary = str(update.get("summary", "")).strip()
            if not target_summary or not new_summary:
                raise ValueError(f"project {key} has an update missing target_summary or summary")
            embedding_result = request_json(
                args.embeddings_url,
                {"model": args.embedding_model, "input": new_summary},
                embedding_headers,
                args.timeout,
            )
            try:
                vector = embedding_result["data"][0]["embedding"]
            except (KeyError, IndexError, TypeError) as exc:
                raise RuntimeError("embedding response missing data[0].embedding during learning update") from exc
            if len(vector) != 1024:
                raise RuntimeError(f"learning update embedding has {len(vector)} dimensions; schema expects 1024")
            vector_literal = "[" + ",".join(f"{float(value):.9g}" for value in vector) + "]"
            scope = sql_literal(scopes[key])
            marker = sql_literal(run_id)
            target = sql_literal(target_summary)
            provenance = sql_literal(json.dumps({"benchmark_run": run_id, "fixture_project": key, "learned_update": True}, separators=(",", ":")))
            statements.append(
                "WITH target AS ("
                f"UPDATE lemn_memories SET state = 'SUPERSEDED' WHERE project_id = {scope} "
                f"AND provenance->>'benchmark_run' = {marker} AND state = 'AUTHORITATIVE' "
                f"AND summary = {target} RETURNING id"
                "), learned AS ("
                "INSERT INTO lemn_memories "
                "(state, project_id, confidence, category, summary, rationale, embedding, provenance) VALUES ("
                f"'AUTHORITATIVE', {scope}, 1.0, 'decision', {sql_literal(new_summary)}, "
                f"'isolated benchmark learning update', {sql_literal(vector_literal)}::vector, {provenance}::jsonb) RETURNING id"
                "), edge AS ("
                "INSERT INTO lemn_edges (source_id, target_id, relationship) "
                "SELECT learned.id, target.id, 'supersedes' FROM learned CROSS JOIN target RETURNING source_id"
                ") SELECT target.id || ':' || learned.id FROM target CROSS JOIN learned;"
            )
            update_count += 1
    statements.append("COMMIT;")
    if not update_count:
        return
    updated = run_psql("\n".join(statements))
    if len(updated) != update_count or any(":" not in value for value in updated):
        raise RuntimeError(f"expected {update_count} applied learning updates, got {updated}")
    print(f"Applied {update_count} simulated learning update(s); AGENTS.md fixtures were not modified.")


def cleanup_isolated_fixtures(run_id: str) -> None:
    marker = sql_literal(run_id)
    run_psql(
        "BEGIN;\n"
        "DELETE FROM lemn_edges WHERE source_id IN (SELECT id FROM lemn_memories WHERE provenance->>'benchmark_run' = " + marker + ") "
        "OR target_id IN (SELECT id FROM lemn_memories WHERE provenance->>'benchmark_run' = " + marker + ");\n"
        "DELETE FROM lemn_memories WHERE provenance->>'benchmark_run' = " + marker + ";\n"
        "COMMIT;"
    )
    remaining = run_psql("SELECT count(*) FROM lemn_memories WHERE provenance->>'benchmark_run' = " + marker + ";")
    if remaining != ["0"]:
        raise RuntimeError(f"benchmark cleanup left {remaining} memory rows for run {run_id}")


def format_lemn_context(memories: list[dict[str, Any]]) -> str:
    if not memories:
        return ""
    lines = [f"- [{str(m.get('category', '')).upper()} #{m.get('id')}] {m.get('summary', '')}" for m in memories]
    if any(m.get("below_threshold") for m in memories):
        intro = (
            "No memory strongly matched this question. These are the closest "
            "confirmed memories; treat them as background, not a direct answer."
        )
    else:
        intro = "These are previously confirmed facts. Treat them as authoritative unless contradicted."
    return "<retrieved_memory>\n" + intro + "\n" + "\n".join(lines) + "\n</retrieved_memory>"


def completion(args: argparse.Namespace, task: dict[str, Any], context: str, headers: dict[str, str]) -> tuple[str, dict[str, Any], float]:
    messages = [{"role": "system", "content": SYSTEM_PROMPT}]
    if context:
        messages.append({"role": "system", "content": context})
    messages.append({"role": "user", "content": str(task["query"])})
    payload = {
        "model": args.model,
        "messages": messages,
        "temperature": 0,
        "max_tokens": args.max_tokens,
        "stream": False,
    }
    start = time.monotonic()
    result = request_json(args.completions_url, payload, headers, args.timeout)
    elapsed = time.monotonic() - start
    try:
        answer = result["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise RuntimeError("completion response did not contain choices[0].message.content") from exc
    if not isinstance(answer, str):
        answer = json.dumps(answer, ensure_ascii=False)
    usage = result.get("usage") or {}
    metrics = {
        "prompt_tokens": usage.get("prompt_tokens"),
        "completion_tokens": usage.get("completion_tokens"),
        "total_tokens": usage.get("total_tokens"),
        "elapsed_seconds": round(elapsed, 3),
    }
    return answer, metrics, elapsed


def run_benchmark(args: argparse.Namespace) -> None:
    task_path = Path(args.tasks)
    all_tasks = read_jsonl(task_path)
    validate_tasks(all_tasks)
    if args.task_ids:
        selected_ids = {value.strip() for value in args.task_ids.split(",") if value.strip()}
        known_ids = {str(task["id"]) for task in all_tasks}
        unknown_ids = selected_ids - known_ids
        if unknown_ids:
            raise ValueError(f"unknown task IDs: {', '.join(sorted(unknown_ids))}")
        tasks = [task for task in all_tasks if str(task["id"]) in selected_ids]
    else:
        tasks = all_tasks
    corpus = json.loads(Path(args.corpus).read_text(encoding="utf-8"))
    fixture_projects = corpus.get("projects", {})
    if not fixture_projects:
        raise ValueError("fixture corpus must define projects")
    if any(task["project_key"] not in fixture_projects for task in tasks):
        raise ValueError("every task project_key must be defined in the fixture corpus")
    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    results_path = out_dir / "blind-results.jsonl"
    key_path = out_dir / "condition-key.jsonl"
    score_path = out_dir / "blind-scores.csv"

    if args.validate_only:
        fixture_count = sum(len(project.get("memories", [])) for project in fixture_projects.values())
        update_count = sum(len(project.get("updates", [])) for project in fixture_projects.values())
        print(f"Validated {len(tasks)} tasks; {fixture_count} initial facts and {update_count} learning updates would be applied in disposable scopes; {len(tasks) * len(ARMS) * args.repeats} responses would be collected.")
        return

    shared_secret = os.environ.get(args.lemn_secret_env, "")
    if not shared_secret:
        raise ValueError(f"environment variable {args.lemn_secret_env} is required for LEMN retrieval")
    retrieval_headers = {"Content-Type": "application/json", "Authorization": f"Bearer {shared_secret}"}
    completion_headers = {"Content-Type": "application/json"}
    api_key = os.environ.get(args.api_key_env, "") if args.api_key_env else ""
    if api_key:
        completion_headers["Authorization"] = f"Bearer {api_key}"
        embedding_headers = {"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"}
    else:
        embedding_headers = {"Content-Type": "application/json"}

    fixture_run_id = uuid.uuid4().hex[:12]
    scopes, project_contexts = create_isolated_fixtures(args, fixture_run_id, embedding_headers)

    arms = list(ARMS)
    rng = random.Random(args.seed)
    try:
        learning_applied = False
        for phase in ("before_learning", "after_learning"):
            phase_tasks = [task for task in tasks if task["phase"] == phase]
            if phase == "after_learning" and phase_tasks and not learning_applied:
                apply_learning_updates(args, fixture_run_id, scopes, embedding_headers)
                learning_applied = True
            task_runs = [(task, repeat) for repeat in range(args.repeats) for task in phase_tasks]
            rng.shuffle(task_runs)
            file_mode = "w" if phase == "before_learning" else "a"
            with results_path.open(file_mode, encoding="utf-8") as results_file, key_path.open(file_mode, encoding="utf-8") as key_file:
                for task, repeat in task_runs:
                    task_arms = list(arms)
                    rng.shuffle(task_arms)
                    project_key = str(task["project_key"])
                    project_id = scopes[project_key]
                    for arm in task_arms:
                        response_id = uuid.uuid4().hex[:12]
                        memories: list[dict[str, Any]] = []
                        if arm == "agents":
                            context = "<project_guidance>\n" + project_contexts[project_key] + "\n</project_guidance>"
                        elif arm == "lemn":
                            memories = fetch_memories(args, str(task["query"]), project_id, retrieval_headers)
                            context = format_lemn_context(memories)
                        else:
                            context = ""

                        error = ""
                        metrics: dict[str, Any] = {}
                        answer = ""
                        try:
                            answer, metrics, _ = completion(args, task, context, completion_headers)
                        except Exception as exc:  # record per-run failures and continue paired cases
                            error = str(exc)
                        result_row = {
                            "run_id": response_id,
                            "task_id": task["id"],
                            "category": task["category"],
                            "phase": phase,
                            "repeat": repeat + 1,
                            "query": task["query"],
                            "answer": answer,
                            "error": error,
                            **metrics,
                        }
                        key_row = {
                            "run_id": response_id,
                            "task_id": task["id"],
                            "category": task["category"],
                            "phase": phase,
                            "project_id": project_id,
                            "fixture_run": fixture_run_id,
                            "repeat": repeat + 1,
                            "condition": arm,
                            "retrieved": [
                                {"id": m.get("id"), "similarity": m.get("similarity"), "below_threshold": m.get("below_threshold", False)}
                                for m in memories
                            ],
                        }
                        results_file.write(json.dumps(result_row, ensure_ascii=False) + "\n")
                        key_file.write(json.dumps(key_row, ensure_ascii=False) + "\n")
                        print(f"{response_id}: completed {len(result_row['answer'])} chars" if not error else f"{response_id}: ERROR {error}")
    finally:
        cleanup_isolated_fixtures(fixture_run_id)
        print(f"Removed disposable benchmark scopes for run {fixture_run_id}.")

    with score_path.open("w", newline="", encoding="utf-8") as score_file:
        writer = csv.writer(score_file)
        writer.writerow(["run_id", "task_id", "accuracy_0_or_1", "factuality_0_to_2", "usefulness_0_to_2", "notes"])
        for row in read_jsonl(results_path):
            writer.writerow([row["run_id"], row["task_id"], "", "", "", ""])
    print(f"Blind responses: {results_path}")
    print(f"Condition key:   {key_path}")
    print(f"Score sheet:     {score_path}")
    print("Do not open the condition key until scoring is complete.")


def report(args: argparse.Namespace) -> None:
    out_dir = Path(args.report)
    key_by_id = {row["run_id"]: row for row in read_jsonl(out_dir / "condition-key.jsonl")}
    scores: dict[str, list[float]] = defaultdict(list)
    metrics: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    category_scores: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    retrieval_nonempty: dict[str, list[bool]] = defaultdict(list)
    for run_id, key in key_by_id.items():
        if key["condition"] == "lemn":
            retrieval_nonempty[key.get("category", "general")].append(bool(key.get("retrieved")))
    for row in read_jsonl(out_dir / "blind-results.jsonl"):
        condition = key_by_id[row["run_id"]]["condition"]
        for field in ("elapsed_seconds", "prompt_tokens", "completion_tokens", "total_tokens"):
            if isinstance(row.get(field), (int, float)):
                metrics[condition][field].append(float(row[field]))
    with (out_dir / "blind-scores.csv").open(newline="", encoding="utf-8") as score_file:
        for row in csv.DictReader(score_file):
            if not row.get("accuracy_0_or_1", "").strip():
                continue
            condition = key_by_id[row["run_id"]]["condition"]
            accuracy = float(row["accuracy_0_or_1"])
            if accuracy not in (0.0, 1.0):
                raise ValueError(f"{row['run_id']}: accuracy_0_or_1 must be 0 or 1")
            scores[condition].append(accuracy)
            category = key_by_id[row["run_id"]].get("category", "general")
            category_scores[condition][category].append(accuracy)
            for field in ("factuality_0_to_2", "usefulness_0_to_2"):
                value = row.get(field, "").strip()
                if value:
                    score = float(value)
                    if score < 0 or score > 2:
                        raise ValueError(f"{row['run_id']}: {field} must be between 0 and 2")
                    metrics[condition][field].append(score)
    if not scores:
        raise ValueError("no scored rows; fill accuracy_0_or_1 in blind-scores.csv")
    for condition in ARMS:
        if condition not in scores:
            print(f"{condition:>6}: no scores")
            continue
        accuracy = statistics.mean(scores[condition])
        factuality = statistics.mean(metrics[condition]["factuality_0_to_2"]) if metrics[condition]["factuality_0_to_2"] else float("nan")
        usefulness = statistics.mean(metrics[condition]["usefulness_0_to_2"]) if metrics[condition]["usefulness_0_to_2"] else float("nan")
        elapsed = statistics.mean(metrics[condition]["elapsed_seconds"]) if metrics[condition]["elapsed_seconds"] else float("nan")
        tokens = statistics.mean(metrics[condition]["total_tokens"]) if metrics[condition]["total_tokens"] else float("nan")
        line = f"{condition:>6}: n={len(scores[condition]):2} accuracy={accuracy:.3f} factuality={factuality:.2f} usefulness={usefulness:.2f} avg_s={elapsed:.1f} avg_tokens={tokens:.0f}"
        if condition == "lemn":
            samples = [value for category_values in retrieval_nonempty.values() for value in category_values]
            if samples:
                line += f" retrieval_nonempty_rate={statistics.mean(samples):.3f}"
        print(line)
    print("Accuracy by scenario:")
    categories = sorted({category for grouped in category_scores.values() for category in grouped})
    for category in categories:
        values = []
        for condition in ARMS:
            category_values = category_scores[condition].get(category, [])
            if category_values:
                values.append(f"{condition}={statistics.mean(category_values):.2f} (n={len(category_values)})")
        print(f"  {category}: " + ", ".join(values))


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tasks", default=str(SCRIPT_DIR / "memory_bench_tasks.jsonl"), help="JSONL task set with hidden expected answers/rubrics")
    parser.add_argument("--corpus", default=str(SCRIPT_DIR / "memory_bench_corpus.json"), help="synthetic memories and matching per-project AGENTS contexts")
    parser.add_argument("--out", default="/tmp/lemn-memory-bench", help="directory for blind results, condition key, and score sheet")
    parser.add_argument("--completions-url", default="http://localhost:9001/v1/chat/completions", help="direct fixed-model OpenAI-compatible endpoint; avoid the LEMN router")
    parser.add_argument("--model", required=False, help="model ID accepted by completions endpoint")
    parser.add_argument("--daemon-url", default="http://localhost:8080", help="LEMN daemon base URL")
    parser.add_argument("--embeddings-url", default=os.environ.get("LEMN_EMBEDDING_URL", "http://localhost:9001/v1/embeddings"))
    parser.add_argument("--embedding-model", default=os.environ.get("LEMN_EMBEDDING_MODEL", "bge-m3-mlx-fp16"))
    parser.add_argument("--project-id", default=os.environ.get("LEMN_PROJECT_ID", REPO_DIR.name), help="default project scope; individual tasks may override this")
    parser.add_argument("--retrieval-limit", type=int, default=5)
    parser.add_argument("--max-tokens", type=int, default=512)
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--task-ids", help="comma-separated task IDs to run instead of the full suite")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--api-key-env", default="LEMN_BACKEND_API_KEY", help="environment variable containing model API key; empty disables auth")
    parser.add_argument("--lemn-secret-env", default="LEMN_SHARED_SECRET")
    parser.add_argument("--validate-only", action="store_true", help="validate task/config files without making network calls")
    parser.add_argument("--report", help="summarize completed scores in an output directory")
    args = parser.parse_args()
    if args.repeats < 1 or args.retrieval_limit < 1 or args.max_tokens < 1:
        parser.error("repeats, retrieval-limit, and max-tokens must be positive")
    if not args.report and not args.validate_only and not args.model:
        parser.error("--model is required when collecting responses")
    return args


if __name__ == "__main__":
    try:
        parsed = parse_args()
        if parsed.report:
            report(parsed)
        else:
            run_benchmark(parsed)
    except (OSError, ValueError, RuntimeError) as exc:
        print(f"memory_bench: {exc}", file=sys.stderr)
        sys.exit(2)