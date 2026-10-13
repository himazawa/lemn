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
- Coordinates routed chat, extraction, and embedding inference through a shared
  admission gate, with foreground priority.
- Defers resource-related background failures durably instead of exhausting
	the content-error retry budget during an outage.

The core services are Go; the Laya classifier is Python; the Pi integration is
TypeScript. See [Architecture](docs/architecture.md) for the flow and lifecycle.

## Quick start

You need Docker Compose and an OpenAI-compatible model server that the
containers can reach. Pi is needed only for the agent integration. The
embedding model must match the vector size in `schema.sql` (currently 1024).
Your fast model must emit tool calls reliably; if it only describes them, the
agent may appear to hang (see the [tool-call check](docs/operations.md#model-tool-call-check)).

```bash
cp .env.example .env
```

Set `LEMN_SHARED_SECRET`, `POSTGRES_PASSWORD`, and your model IDs and URLs in
`.env`, then start the services:

```bash
docker compose up -d --build
docker compose ps
```

Wait for all services to become healthy; Laya downloads its checkpoint on first
start. Pi runs on the host and does not read `.env`, so set its shared secret in
Pi's environment. Then follow [the setup guide](docs/operations.md#docker-setup)
to configure Pi, verify requests, and review memories.

`docker compose stop` keeps data; `docker compose down -v` deletes stored
memories and the queue. Recreating the router interrupts its active streams, so
finish or pause Pi requests before updating:

```sh
docker compose up -d --build --no-deps router daemon
```

## How it works

### Memory

LEMN adds retrieved memories to Pi's context as a separate message. It does not
replace or compact Pi's conversation history.

- **Writes are queued.** Pi logs each completed turn; acknowledgment means
  queued, not saved. `/lemn-job` in Pi reports the last job's outcome.
- **Not every turn becomes a memory.** Laya gates and an extraction model propose
  at most one summary per turn. A message starting with `remember` is stored
  directly as a user-declared fact.
- **Only authoritative memories are retrieved**, scoped to the current project or
  `global`. Unbacked claims, duplicates, and corrections go to review with
  `lemn pending`, `lemn confirm`, `lemn reject --reason`, and `lemn revalidate`.
- **Outages are visible.** Retrieval failures return HTTP 503; Pi reports them
  and continues without memories.

Details: [write path](docs/architecture.md#memory-write-path),
[read path](docs/architecture.md#memory-read-path),
[review states](docs/architecture.md#review-states), and
[admin commands](docs/operations.md#admin-commands).

### Model scheduling

The router admits one model call at a time across chat, extraction, and
embeddings, so background memory work doesn't stack model loads on a shared
local backend. Foreground requests take priority and cancel running background
calls. Those jobs return to the durable queue with exponential backoff, without
using their retry budget. Only traffic through this router is coordinated. See
[Resource Scheduling](docs/resource-scheduling.md).

### Embeddings

The embedding model (default `bge-m3`) must produce 1024-dimensional vectors.
Changing it after memories are stored requires a retrieval parity check and
possibly re-embedding; see
[Native Embeddings](docs/resource-scheduling.md#native-embeddings).

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
- [Operations](docs/operations.md): setup, Pi integration, optional subagent memory, administration, configuration, and troubleshooting.
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
