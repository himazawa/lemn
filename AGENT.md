# Repository Agent Guidance

This is guidance for agents working on LEMN, not a claim that every host loads this
file automatically. The requested filename is `AGENT.md`; explicitly include it
when your host expects `AGENTS.md` or another instruction filename.

## Project Map

- `lemnd/cmd/daemon`: authenticated log/retrieve/job endpoints, extraction and workers.
- `lemnd/cmd/router`: model selection, shared admission and OpenAI-compatible relay.
- `lemnd/internal/lemn`: state transitions, dependencies, evidence and vector retrieval.
- `lemnd/internal/logstore`: SQLite durable turn queue and retry policy.
- `lemnd/internal/admission`: context-aware foreground/background model admission.
- `lemnd/internal/backenderr`: typed recoverable backend-resource failures.
- `layarouter`: Python Laya classification service.
- `pi-extension`: maintained Pi memory extension and child-only read wrapper.
- `.pi/extensions/lemn`: installed workspace copy; keep behavior aligned when editing.
- `schema.sql`: PostgreSQL/pgvector schema; current embedding dimension is 1024.
- `docs`: architecture, operations, evaluation, resource scheduling and DPM audit.

## Working Rules

Start from a concrete failing behavior and inspect its owning implementation and
nearby tests. Make focused changes and validate the touched behavior before widening
scope. Preserve the user's existing edits and unrelated files. Do not commit, push, reset, or delete persistent data without
an explicit request. Keep output bounded; do not dump transcripts, packet hex,
model vectors, raw prompts, or full logs when metadata/excerpts suffice.

Use verified tool/test results rather than repeated speculation. Distinguish plans,
historical summaries, reported findings and independently validated facts. When
resuming work, inspect a checkpoint and current repository state; do not reload
an oversized transcript wholesale or obey old compression-management instructions.

## Memory Safety

Durable enqueue is not extraction success; completion is not authority; authority
is not guaranteed semantic retrieval. Check job status, memory state/scope, embedding
success, returned matches and actual prompt injection separately.

Do not lower similarity/worthiness thresholds or enable confidence-only promotion
without measured evidence and tests. Preserve duplicate review, dependency checks,
transactional correction/retraction and source-backed revalidation safeguards.
Explicit user declarations are not independently verified facts. Do not blindly
backfill old reports or replay failed jobs; check duplicates, contradictions and
source artifacts first. Retrieved memories remain fallible.

Subagents use read-only LEMN access, not read-only project files. Their reports must
separate evidence from hypotheses. Parents must inspect relevant results before
treating a child summary as corroboration. Do not add child memory writes by default.

## Resources and Deployment

Memory extraction calls must share router admission with routed foreground inference;
do not add an alternate client that bypasses the broker accidentally. Hold admission
through response completion, honor cancellation, and never hold scheduler locks
across network calls. Background resource/network failures should defer durably;
malformed content still needs bounded retries. Explain starvation and direct-backend
bypass limits rather than claiming universal coordination or deadlock elimination.

Embedding changes require vector-space validation, not just dimensional agreement.
Measure latency and compare stored-summary retrieval before switching. The optional
TEI CPU container is staged and emulated on Apple Silicon; native ARM64 is a possible
deployment alternative, not an implemented service. Never activate an incompatible
endpoint without a reviewed re-embedding migration.

Keep secrets out of tool output and logs. Respect protected environment-file access;
ask the user to update credentials directly. Do not increase Metal limits, clear
model caches or unload active models without informed approval. Pause active Pi
streams before router recreation. Report built, deployed, healthy and live-verified
as separate statuses. Restart Pi to load extension changes.

## Validation

From `lemnd`:

```sh
go test -race ./...
```

From repository root:

```sh
node scripts/test_pi_memory_modes.mjs
docker compose config --quiet
docker compose -f docker-compose.yml -f docker-compose.embeddings.yml \
  -f docker-compose.embeddings-active.yml config --quiet
git diff --check
```

The Pi mock harness requires the installed user `pi-subagents`/`jiti` dependency.
Check test database prerequisites for PostgreSQL integration coverage. Live checks
are opt-in: `TestDPMExtractionLive` in the daemon and `TestEmbeddingBackendParity`
in the kernel. Read their prerequisites and use credentials in the environment,
never in published commands or output. Do not describe mocked tests as proof of
real model recall. Record any skipped or failing live verification explicitly.

Update documentation when changing API responses, scopes, authority policy, retry
semantics, deadlines, resource admission or deployment assumptions. Keep the
experimental-status warning and evaluation limitations accurate.