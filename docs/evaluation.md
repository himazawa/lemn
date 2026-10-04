# Evaluation and Calibration

LEMN is an experimental prototype. The evaluations here are small, synthetic,
or hand-labeled. They can catch regressions, but do not prove production
reliability or long-term improvements to coding tasks.

## Calibrate Laya for your workload

Laya answers typed yes/no questions about routing, memory-worthiness, global
scope, and corrections. The model was not trained on these project-specific
questions. Scores can change when wording changes, and the score range is
compressed. Do not assume `0.5` is a neutral cutoff. Defaults are starting
points, not universal thresholds.

| Question | Setting | Effect |
|---|---|---|
| Does the task span many codebase areas? | `LAYA_ROUTING_THRESHOLD` | Route to fast or heavy model |
| Is this turn worth remembering? | `LAYA_MEMORY_THRESHOLD` | Continue or drop the memory write |
| Is the memory cross-project? | `LAYA_GLOBAL_THRESHOLD` | Choose global or project scope |
| Is this a correction? | `LAYA_CORRECTION_THRESHOLD` | Correction handling/review |

Create a labeled set from your own short and long requests. Keep the labels
fixed when comparing question wording. Use `layarouter/eval_global_scope.py` to
measure cross-project scope classification. Run
`layarouter/eval_memory_gate.py --assert` to check the deployed
memory-worthiness question and threshold against its labeled examples.

For routing, compare the highest score among examples labeled “fast” with the
lowest score among examples labeled “heavy.” If the scores overlap, changing
the threshold will not separate the groups. Revise the question and measure
again. For memory-worthiness, use the repository's regression check instead of
changing the question or threshold by guesswork:

```bash
docker compose exec laya python eval_memory_gate.py --assert
```

The current routing question uses concrete codebase breadth because it
separated the local labeled examples better than “deep multi-step reasoning.”
For global scope, asking whether a memory would still apply in an unrelated
project worked better than asking whether it is “about the user.” Even so, the
groups remain hard to separate, so the high cutoff is intentionally
conservative. A separate explicit-phrase override catches a few known forms; it
does not handle every way someone might state a preference.

Observe real decisions and correct labels you disagree with:

```bash
docker compose logs daemon | grep '\[Gate\]'
docker compose logs router | grep requires_reasoning
```

Threshold changes affect live behavior. Record a baseline and test against your
own labeled traffic before adopting them. You can also fine-tune a checkpoint:
use `docker compose exec -it daemon labeler` to review turns and
`docker compose exec daemon export` to create JSONL training data. Train a
compatible checkpoint and set `LAYA_MODEL_REPO` to use it.

## End-to-end retrieval benchmark

`scripts/memory_bench.py` compares three setups: no memory, a fixed project
`AGENTS.md`, and results from the real `/retrieve` endpoint. It starts an
isolated daemon and a temporary Postgres database. Each synthetic project gets
its own scope, and scheduled updates are applied between sessions. The script
removes test rows and the database on normal completion or handled errors. A
forced stop can skip cleanup; scratch databases are named
`lemn_bench_<run-id>`.

The benchmark inserts fixture memories directly. It tests retrieval and stored
updates, not whether conversational extraction notices changed facts or
whether tool evidence supports them. It does not measure tool-using coding
tasks.

Review the fixed tasks, corpus, and project fixture files before running. Keep rubrics fixed before seeing answers. Ensure Compose Postgres, daemon, embedding endpoint, and a direct model endpoint are available; use the direct model endpoint, not the LEMN router, to hold the answering model constant.

```bash
set -a; source .env; set +a
python3 scripts/memory_bench.py --validate-only
python3 scripts/memory_bench.py --model 4-bit \
  --completions-url http://localhost:8000/v1/chat/completions \
  --embeddings-url http://localhost:8000/v1/embeddings \
  --out /tmp/lemn-memory-bench --repeats 2
```

Score `blind-results.jsonl` before opening `condition-key.jsonl`, then report:

```bash
python3 scripts/memory_bench.py --report /tmp/lemn-memory-bench
```

A task subset can be selected with `--task-ids id,id`. The script prints run-specific project IDs; only remove its named scratch database if forced termination prevents cleanup.

### Latest retrieval result

On 2026-10-03, a five-session synthetic replay ran 15 tasks under two independent within-session shuffle seeds (90 responses), using the same Qwen 3.8 model in all arms. All arms scored 15/15 on their arm-specific rubrics in both seeds. LEMN scored mean factuality 2.00/2 against the latest replay state, versus 1.07 for static `AGENTS.md` (which retained intentionally stale guidance) and 1.20 for no-memory (which mostly abstained). Mean prompt use was 291 tokens for LEMN, 272 for AGENTS, and 231 for no-memory; mean response times were 6.3s, 6.7s, and 7.5s. LEMN returned context on all 15 tasks.

The 15/15 scores use a separate rubric for each arm. The AGENTS arm is scored
against its fixed instructions, while the no-memory arm can earn credit for
abstaining instead of inventing facts. Those scores alone do not show which arm
best matches the latest project state. The factuality score measures agreement
with that state; it is the more relevant comparison for updated facts.

This is one small synthetic scenario repeated under two task-order seeds, not evidence of statistical superiority. All updates were applied by the fixture; the benchmark does not test conversational extraction, real tool evidence, or whether the system should infer interrupted work.

## Write-path benchmark

`scripts/memory_write_bench.py` sends labeled turns through `/log` and the real worker path: Laya gating, extraction, evidence similarity, relation handling, and Postgres routing. Each case/repeat gets a fresh temporary database and one-off daemon, isolating even global-scope writes. It removes these resources on normal completion or handled errors. Forced termination can bypass cleanup; scratch databases use the `lemn_writebench_*` naming pattern.

Cases in `scripts/memory_write_bench_cases.jsonl` include durable decisions/preferences, corrections, tool-backed changes, lookups, acknowledgements, recaps, tentative ideas, and one-answer instructions. Repeats run in fresh scopes so one extraction cannot affect another. The output includes gate and final-memory precision/recall, extractor vetoes, summary checks, scope/relation results, and cosine distributions. These are hand-labeled diagnostics, not calibrated quality estimates.

```bash
set -a; source .env; set +a
python3 scripts/memory_write_bench.py --validate-only
python3 scripts/memory_write_bench.py --repeats 3 \
  --out /tmp/lemn-memory-write-bench
```

Use `--case-ids id,id --repeats N` to restrict a run. Validate your environment before starting services or creating scratch databases.

### Latest write-path result

On 2026-10-03, 20 cases ran three times each (60 turns), with a separate new database and daemon per trial. The gate passed all 33 expected positives (recall 1.00) and 15 of 27 negatives (precision 0.688). Extraction saved all 33 expected positives and rejected all 27 negatives on this label set (final precision/recall 1.00); it vetoed 9 gate false positives as `none` and 6 temporary instructions as `transient_instruction`. All 33 expected scope decisions and all 27 relation labels matched; repeated gate/save/scope decisions were identical. Exact summary-text consistency was 0.917.

The raw global-scope classifier scored tested durable cross-project preferences below its 0.90 cutoff. Candidate prompts did not separate the 16-turn labeled set at that cutoff; an explicit cue override recovered several specifically worded preferences. A follow-up with four such paraphrases, one project preference, two durable technical decisions, and a transient instruction passed all 24 labeled decisions across three repeats. This reflects the explicit cue override on a small synthetic set, not general classifier accuracy.

Evidence cosine was 0.784–0.861 across 9 observations; target similarity was 0.880–0.908 across 6. Those samples are too small and synthetic to calibrate the cutoffs. These results are not representative traffic samples, statistical quality guarantees, or evidence of production readiness. The raw classifier admitted false positives; extraction vetoes are part of the measured pipeline behavior, not proof that future false positives will be caught.

## What is not established

The available benchmarks do not show whether LEMN improves real project outcomes over time, whether automatic extraction catches natural corrections, whether tool-diff similarity reliably supports claims, or whether the system is better than maintaining project guidance manually. A useful next evaluation would use diverse real projects, blinded task scoring, a no-memory and maintained-instructions baseline, and explicit tracking of stale-memory harms and correction quality.
