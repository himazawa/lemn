# LIMN — Local Epistemic Memory Kernel + Laya Model Router

## Components

| Component | Language | Purpose |
|---|---|---|
| `limnd/cmd/daemon` | Go | Ingests Pi turns (durable queue), asks Laya if they are memory-worthy, then extracts and routes approved memories into the epistemic kernel |
| `limnd/cmd/labeler` | Go | CLI to hand-label unreviewed turns for SLM fine-tuning |
| `limnd/cmd/export` | Go | Exports human-reviewed turns to train/valid JSONL for MLX fine-tuning |
| `limnd/cmd/limn` | Go | Admin CLI — list/confirm/reject PENDING memories |
| `limnd/cmd/router` | Go | OpenAI-compatible proxy: classifies each request with Laya, forwards to fast or heavy Qwen backend |
| `layarouter/server.py` | Python | Wraps the Hugging Face `convaiinnovations/laya-typed-decisions` checkpoint behind `/classify` and `/memory-worthiness` HTTP endpoints |
| `pi-extension/` | TypeScript | Pi hooks: logs turns (write path), injects retrieved memories (read path) |
| `schema.sql` | SQL | PostgreSQL + pgvector schema for the memory kernel |

## Security model

Every internal HTTP endpoint (`/log`, `/retrieve`, `/v1/chat/completions`, `/classify`)
requires `Authorization: Bearer <LIMN_SHARED_SECRET>`, checked in constant time.
**Each service refuses to start if `LIMN_SHARED_SECRET` is unset** — there is no
"runs open by default" fallback. Generate one long random value and use it
everywhere:

```bash
export LIMN_SHARED_SECRET=$(openssl rand -hex 32)
```

All services bind to `127.0.0.1` by default (override with `LIMN_DAEMON_BIND` /
`LIMN_ROUTER_BIND` if you genuinely need non-loopback access — e.g. a
container network — and put your own firewalling in front of it).

`cmd/limn` and the daemon both require `LIMN_POSTGRES_DSN` explicitly; there is
no hardcoded fallback credential. Use a real secrets manager or a gitignored
env file, not a value committed to source.

Since OpenAI-compatible clients send `Authorization: Bearer <api_key>` by
convention, setting Pi's model `api_key` config to `LIMN_SHARED_SECRET`'s
value is enough to authenticate against the router — no Pi-side code needed
beyond that config field.

## Durability model

`/log` writes the raw turn payload into `memory_jobs` (SQLite) **synchronously,
before responding** — this replaced the old fire-and-forget goroutine. A pool
of worker goroutines (`LIMN_WORKER_COUNT`, default 2) claims jobs atomically,
runs extraction + kernel routing, and retries failures with exponential
backoff up to `max_attempts` (default 5) before marking a job permanently
`FAILED` for inspection. On daemon startup, any job stuck in `PROCESSING`
from a prior crash is reset to `RETRY` — turns can no longer be silently lost
to a crash between acceptance and processing.

## Port map

| Port | Service |
|---|---|
| 8000 | Extraction model (Qwen 2.5 Coder, used by `daemon` for summary/category extraction after Laya approves storage) |
| 8001 | Embedding model (bge-large-en-v1.5, used by `daemon`/`kernel` for vector similarity) |
| 8002 | Laya classifier service (`layarouter/server.py`) |
| 8010 | Fast Qwen backend (7B/14B) — point your model server here |
| 8011 | Heavy Qwen backend (27B / Qwen 3.6 MoE) — point your model server here |
| 8080 | `limnd` daemon (`/log`, `/retrieve`) |
| 8090 | Model router (`/v1/chat/completions`) — point Pi's model config here instead of a model server directly |

## Setup order

```bash
# 0. Generate the shared secret once, export it in every shell/service
#    that needs it (daemon, router, Laya service, and Pi's config)
export LIMN_SHARED_SECRET=$(openssl rand -hex 32)

# 1. Build the Go binaries
cd limnd
go mod tidy
go build -o bin/daemon  ./cmd/daemon
go build -o bin/labeler ./cmd/labeler
go build -o bin/export  ./cmd/export
go build -o bin/limn    ./cmd/limn
go build -o bin/router  ./cmd/router

# 2. Stand up your model backends
#    - extraction model on :8000  (e.g. Qwen 2.5 Coder via oMLX/Ollama)
#    - embedding model on :8001   (e.g. bge-large-en-v1.5)
#    - fast Qwen (7B/14B) on :8010
#    - heavy Qwen (27B / 3.6 MoE) on :8011

# 3. Start the Laya classifier service (binds to all interfaces by
#    default under uvicorn — pass --host 127.0.0.1 unless you have a
#    specific reason not to)
cd ../layarouter
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
USE_TF=0 uvicorn server:app --host 127.0.0.1 --port 8002

# 4. Start the model router
cd ../limnd
./bin/router
# override backend URLs if needed:
# LIMN_FAST_MODEL_URL=http://localhost:8010/v1/chat/completions \
# LIMN_HEAVY_MODEL_URL=http://localhost:8011/v1/chat/completions \
# LIMN_LAYA_URL=http://localhost:8002/classify \
# ./bin/router

# 5. Start the LIMN daemon
#    Phase 1-2 (bootstrap logging only, SQLite):
./bin/daemon
#    Phase 3-4 (full epistemic kernel, SQLite + PostgreSQL):
export LIMN_POSTGRES_DSN="postgres://limn:CHANGE_ME@localhost:5432/limn_kernel?sslmode=disable"
#    Optional overrides:
# LIMN_LAYA_MEMORY_URL=http://localhost:8002/memory-worthiness \
# ./bin/daemon
./bin/daemon

# 6. Point Pi at the router instead of a model directly
#    In Pi's model config: base_url = http://localhost:8090/v1
#                          api_key  = value of $LIMN_SHARED_SECRET
#    And set LIMN_SHARED_SECRET in Pi's own environment so the
#    extension can authenticate to the daemon's /log and /retrieve.
s
# 7. Hand-label and fine-tune the Level 1 extractor as needed
./bin/labeler
./bin/export
pip install "mlx-lm[train]"
mlx_lm.lora --model Qwen/Qwen2.5-1.5B-Instruct --train --data ./data --iters 250 --batch-size 4 --val-batches 10 --learning-rate 1e-5
mlx_lm.fuse --model Qwen/Qwen2.5-1.5B-Instruct --adapter-path ./adapters --save-path ./laya-1.5b-custom

# 8. Review pending memories as they accumulate
LIMN_POSTGRES_DSN=$LIMN_POSTGRES_DSN ./bin/limn pending
LIMN_POSTGRES_DSN=$LIMN_POSTGRES_DSN ./bin/limn confirm <id>
LIMN_POSTGRES_DSN=$LIMN_POSTGRES_DSN ./bin/limn reject <id>
```

## Env var reference (new in this revision)

| Variable | Default | Used by |
|---|---|---|
| `LIMN_SHARED_SECRET` | *(required, no default)* | daemon, router, Laya service |
| `LIMN_DAEMON_BIND` | `127.0.0.1:8080` | daemon |
| `LIMN_ROUTER_BIND` | `127.0.0.1:8090` | router |
| `LIMN_LAYA_MEMORY_URL` | `http://localhost:8002/memory-worthiness` | daemon |
| `LAYA_MEMORY_THRESHOLD` | `0.5` | Laya service (`/memory-worthiness`) |
| `LIMN_WORKER_COUNT` | `2` | daemon (job queue workers) |
| `LIMN_POSTGRES_DSN` | *(required, no default)* | daemon, `cmd/limn` |

## Still open (not covered by this pass)

This pass covered auth + durable ingestion. Still unbuilt from the original
design and worth prioritizing next: Level 2 (Qwen 32B) escalation for
low-confidence/ambiguous extractions, a consumer for the `NEEDS_REVALIDATION`
state, structured metrics/observability, and a CI test suite for
`internal/limn`'s evidence/promotion logic.

## One caveat worth keeping in view

The router's `LAYA_ROUTING_THRESHOLD` (default 0.5) and the daemon's
`LAYA_MEMORY_THRESHOLD` (default 0.5) are starting points, not validated
cutoffs — Laya's zero-shot accuracy on a schema it wasn't fine-tuned for is
well below its benchmarked numbers, and both `requires_reasoning` and
`memory_worthy` are questions you're writing fresh, not ones it was trained
on. Log the probabilities and treat the first weeks of real traffic as data
to tune those thresholds against.
