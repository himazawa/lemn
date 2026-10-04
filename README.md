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

The core services are Go; the Laya classifier is Python; the Pi integration is
TypeScript. See [Architecture](docs/architecture.md) for the flow and lifecycle.

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

## Guides

- [Architecture](docs/architecture.md): routing, memory storage and retrieval, lifecycle, scope, and network use.
- [Operations](docs/operations.md): setup, Pi integration, administration, configuration, and troubleshooting.
- [Evaluation](docs/evaluation.md): threshold tuning, benchmark steps, results, and limitations.

## License

LEMN's original source code and documentation are licensed under
[AGPL-3.0-only](LICENSE). AGPL permits commercial use and does not require
payment merely because a project earns money. Third-party dependencies, model
checkpoints, container images, and external services have their own terms.
