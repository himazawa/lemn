# LEMN: Local Epistemic Memory Kernel + Laya Model Router

<img width="1024" height="512" alt="lemn_logo" src="https://github.com/user-attachments/assets/cf0358c6-ebdc-4ded-ade7-5e1660b8b214" />

**Experimental prototype; not production-ready.** Current evaluations are small,
synthetic and hand-labeled. They do not establish reliable long-term memory
quality or improved coding outcomes. Treat retrieved memories as fallible and
keep critical project knowledge in version-controlled documentation.

LEMN explores persistent project memory for coding agents. It proposes facts,
tracks whether they apply to one project or across projects, and lets people
review, correct, replace, or revalidate them. An optional router sends requests
to fast or heavy models.

## What it does

- Logs completed Pi turns to a durable background queue.
- Uses typed model decisions and extraction to propose memories.
- Stores memory state, provenance, scope, and dependencies in PostgreSQL with pgvector.
- Retrieves authoritative memories before a turn and injects them separately.
- Optionally routes chat requests through a fast/heavy model router.
- Coordinates routed foreground generation and background extraction through
	a shared admission gate, with foreground priority.
- Defers resource-related background failures durably instead of exhausting
	the content-error retry budget during an outage.

The core services are Go; the Laya classifier is Python; the Pi integration is
TypeScript. See [Architecture](docs/architecture.md) for the flow and lifecycle.

## Memory guarantees and diagnostics

LEMN augments Pi's context; it does **not** replace Pi's conversation history,
compact the transcript, or guarantee that every completed task becomes a memory.
Billion Context (Bili), when used, manages its own compression independently.
Its model-driven compression tools and Pi's native compactor are not LEMN memory
operations. Local Pi/Bili tuning is not installed by this repository.

The Pi extension waits up to five seconds for `/log` to acknowledge durable
enqueue. It reports submission failures and uses the current project scope for
both reads and writes. Acceptance means **queued**, not saved or authoritative.
In Pi, `/lemn-job` checks the last acknowledged job in the current process and
reports waiting, processing, failed, rejected, or processed outcomes. Restart Pi
after updating its extension. `/retrieve` is an authenticated daemon HTTP endpoint,
not a Pi slash command.

Extraction receives bounded, turn-local tool excerpts alongside user and assistant
text. Explicit remember requests and reports with non-control tool evidence can
receive extractor review even if the worthiness classifier rejects them. The
extractor can still return `none`; classifier scores and fallback provenance are
retained. Extraction preserves user declarations, validated findings and uncertainty
without converting plans into completed work. These rules improve coverage, not
prove factual accuracy. One turn currently produces at most one extracted summary.

Retrieved memories must be `AUTHORITATIVE`, scoped to the current project or
`global`, and relevant to the query. Merely saying "remember y is 2" or receiving
an acknowledgment does not guarantee fresh-session recall. Unbacked claims remain
reviewable by default; confidence-only promotion stays disabled. Duplicate suspects,
dependencies, corrections and revalidation still follow the kernel's review rules.
Use `lemn pending`, `lemn confirm`, `lemn reject --reason`, and `lemn revalidate`
as documented in [Operations](docs/operations.md).

Retrieval backend/database failures return HTTP **503**, distinct from a successful
empty match list. Pi shows that memory retrieval is unavailable and continues
generation without injected memories. The service currently allows two seconds for
retrieval and Pi five seconds for the read. Backend response status and empty or
non-finite embedding vectors are validated; a healthy HTTP listener alone does not
prove usable embedding inference.

## Resource scheduling

Compose sends daemon extraction, dependency resolution and relation classification
through the router's authenticated `/internal/extraction` endpoint. The router
admits one model call at a time across that endpoint and foreground chat; the slot
is held through the entire response stream. Foreground calls wait with cancellation
support. Background calls defer with HTTP 503 when busy or foreground work is waiting.
Running work is not preempted, so foreground requests can still wait for an already
running extraction.

Multiple queue workers may remain enabled: they no longer imply simultaneous routed
model inference. HTTP 429/502/503/504/507 and network/deadline failures enter durable
`RETRY` with 30-60 seconds of jitter without consuming content-error attempts.
Other processing errors retain bounded retries. Persistent foreground traffic can
starve background work, and old `FAILED` jobs are not automatically replayed.

This admission control is **single-router and single-slot**, not distributed or
adaptive memory accounting. Direct oMLX calls, other router instances, and providers
that bypass LEMN remain outside it. No deadlock was established in the observed
incident: logs showed memory-pressure eviction, resource rejection and long waits.
Do not raise memory ceilings or extend timeouts blindly to hide those failures.

See [Resource Scheduling](docs/resource-scheduling.md) for configuration, limits,
staged deployment and recovery procedures.

## Embedding isolation

Embeddings can run separately from generation to avoid sharing its scheduler and
Metal caches. **A container is not required**: a dedicated native ARM64 CPU process
may be a better fit on Apple Silicon. Both approaches still consume shared system
RAM and need measured concurrency and latency limits.

The optional [embedding Compose override](docker-compose.embeddings.yml) stages
TEI with CPU BGE-M3. It uses an **amd64 image emulated on Apple Silicon**, with VM
overhead; it is not a verified performance recommendation. The separate
[activation override](docker-compose.embeddings-active.yml) changes the daemon's
endpoint only when explicitly included. Base Compose does not activate it.

Before switching, verify service readiness, matching vector dimensions, vector
parity against the existing embedder, retrieval quality against stored summaries,
and latency within the retrieval deadline. Matching 1024 dimensions alone is not
enough. The opt-in `TestEmbeddingBackendParity` provides a four-text cosine smoke
test, not complete compatibility certification. An incompatible model needs an
explicit re-embedding migration; never silently mix vector spaces.

TEI's INFO startup log can contain its API-key argument; the staged configuration
uses warning-level logging. Rotate any previously exposed credential.

## Subagents and memory

With the [child-extension setup](docs/operations.md#subagent-memory), Pi
subagents automatically retrieve relevant LEMN project and global memories,
but do not submit their runs as new memory candidates. **Read-only refers to
LEMN memory access, not project files:** children can still discover facts,
edit code, and run tests according to their assigned tools and permissions.

Children should return established facts separately from hypotheses, along
with changed files, validation commands/results, and evidence references. The
parent reviews and integrates those findings; its normal LEMN extension logs
the completed parent turn. LEMN then decides what to retain and whether review
is required. This does not automatically approve a child's findings.

Child tool evidence is not automatically copied into the parent's memory log.
The parent should inspect relevant diffs or test results itself; a summarized
child report alone may produce an unbacked candidate that needs human review.
This avoids overlapping or speculative child reports becoming durable memory.
Child-only defaults can be overridden by agent-specific extension settings;
see the setup guide and reload Pi after configuration changes.

## Prototype limits and network use

“Local-first” does not mean no network traffic. On first startup, LEMN downloads
the Laya checkpoint. Model and embedding requests go to the configured
endpoints, which may be remote. Source-backed revalidation fetches allowlisted
HTTPS URLs. Docker Compose publishes service ports on host interfaces by
default. Operational endpoints require the shared secret, but health checks do
not. See [Operations](docs/operations.md#ports-and-exposure) to restrict access.

The current benchmarks do not establish superiority over a maintained
`AGENTS.md` or a no-memory workflow. See [Evaluation](docs/evaluation.md) for
methods, results, and limitations.

## Quick start

You need Docker Compose and an OpenAI-compatible model server that the
containers can reach. Pi is needed only for the agent integration. The
embedding model must match the vector size in `schema.sql` (currently 1024).
Check that your fast model emits tool calls reliably. If it only describes a
tool call, the agent may appear to hang.

```bash
cp .env.example .env
```

Set `LEMN_SHARED_SECRET`, `POSTGRES_PASSWORD`, and your model IDs and URLs in
`.env`, then start the services:

```bash
docker compose up -d --build
docker compose ps
```

Wait for all services to become healthy. Laya downloads its checkpoint the
first time it starts. Follow [the setup guide](docs/operations.md#docker-setup)
to configure Pi, verify requests, and review memories. Pi runs on the host and
does not read `.env` automatically. Set its shared secret in Pi's environment.

To stop while retaining data, run `docker compose stop`. To remove containers
and volumes, run `docker compose down -v`; this deletes stored memories and the
queue.

For updates that replace the router, finish or pause active Pi requests first:

```sh
docker compose up -d --build --no-deps router daemon
```

Recreating the router interrupts its active streams. A built image is not evidence
that the running container has been updated. Verify deployment and live inference
separately from unit tests. Protected local environment files require user edits;
never print their credentials in diagnostic output.

## Development and agent guidance

Repository-specific agent guidance is in [AGENT.md](AGENT.md). This exact filename
was chosen intentionally; tools commonly discover `AGENTS.md` instead, so load
`AGENT.md` explicitly where needed.

Run the Go suite from the module directory and the Pi integration tests from root:

```sh
cd lemnd
go test -race ./...
cd ..
node scripts/test_pi_memory_modes.mjs
docker compose config --quiet
git diff --check
```

The Pi test harness uses `jiti` from the user's installed `pi-subagents` package;
it is not a dependency-free clean-install test. PostgreSQL integration tests need
their configured test database. Live extraction and embedding parity tests are
opt-in and need authenticated backends; ordinary test success does not establish
backend availability or cross-session recall.

## Guides

- [Architecture](docs/architecture.md): routing, memory storage and retrieval, lifecycle, scope, and network use.
- [Operations](docs/operations.md): setup, Pi integration, administration, configuration, and troubleshooting.
- [Evaluation](docs/evaluation.md): threshold tuning, benchmark steps, results, and limitations.
- [Resource Scheduling](docs/resource-scheduling.md): priority admission, retries, and embedding isolation.

## AI Full Disclosure

LEMN was built with strong assistance from AI coding agents. Humans led the
ideas, testing, debugging, and final decisions behind the router, daemon, Pi
integration, and Laya-based decision flow.

If you are not comfortable using software developed with significant AI
assistance, this project may not be for you.

## License

LEMN's original source code and documentation are licensed under
[AGPL-3.0-only](LICENSE). AGPL permits commercial use and does not require
payment merely because a project earns money. Third-party dependencies, model
checkpoints, container images, and external services have their own terms.
