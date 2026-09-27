# LIMN — Local Epistemic Memory Kernel + Laya Model Router

LIMN is a small local system for two related jobs:

1. route chat requests to a fast or heavy model
2. decide which turns are worth storing as long-term memory

It uses:

- a Go router for model selection
- a Go daemon for logging, extraction, and memory routing
- a Python Laya service for typed decisions
- PostgreSQL + pgvector for the memory kernel
- a Pi extension that logs turns and injects retrieved memories

## Components

| Component | Language | Purpose |
|---|---|---|
| `limnd/cmd/router` | Go | OpenAI-compatible proxy that routes each request to fast or heavy chat models |
| `limnd/cmd/daemon` | Go | Durably ingests turns, decides whether they are memory-worthy, and routes approved memories into the kernel |
| `limnd/cmd/limn` | Go | Admin CLI for reviewing pending memories |
| `limnd/cmd/labeler` | Go | Interactive tool for reviewing unlabelled turns |
| `limnd/cmd/export` | Go | Exports reviewed turns to JSONL for training |
| `layarouter/server.py` | Python | Laya typed-decision service for `/classify` and `/memory-worthiness` |
| `pi-extension/` | TypeScript | Pi hooks for memory read/write integration |
| `schema.sql` | SQL | PostgreSQL + pgvector schema for the kernel |

## How it works

1. Pi sends each completed turn to the daemon.
2. The daemon stores the raw turn in SQLite first, so the turn is not lost if the process crashes.
3. Laya decides whether the turn is worth storing.
4. If yes, the daemon extracts a summary and category, embeds it, and compares it with the current authoritative memories in Postgres.
5. If the new memory is too close to an existing one, it is either marked as a superseding candidate or sent to human review instead of being blindly added.
6. When a newer memory is confirmed, the older authoritative memory is marked `SUPERSEDED` and the new one becomes the active version.
7. Pi asks the daemon for relevant memories before the next response and appends the retrieved authoritative memories to the prompt.

## Why This Helps Long-Horizon Use

LIMN is useful when the conversation has to stay coherent across many turns, sessions, or even days.

It helps because it does not treat memory as a single flat log. It separates raw turn capture, memory-worthiness gating, extraction, retrieval, and supersession, so the system can keep useful information while still replacing stale or duplicated memories over time.

That matters for long-horizon work because the prompt only needs the current authoritative view of the user, the project, or the task. Older memories do not just pile up forever; they can be superseded, reviewed, or rejected.

## Compared With Other Memory Kernels

Many memory systems are just one of these:

- an append-only vector store
- a note bucket with retrieval on similarity alone
- a prompt cache with no explicit lifecycle

LIMN is different in a few ways:

- it uses a typed decision step before storing memory at all
- it keeps a durable local queue so turns are not lost on crash
- it stores memory states like `AUTHORITATIVE`, `PENDING_CONFIRMATION`, `SUPERSEDED`, and `REJECTED`
- it supports human review instead of assuming every extraction is correct
- it separates model routing from memory routing, so the chat path and the memory path can evolve independently

The tradeoff is that LIMN is more opinionated than a plain vector database. It is built for controlled long-horizon memory with review and replacement, not for dumping every trace into a bag of embeddings.

Fine-tuning the classifier can make this even better by reducing false positives and false negatives in the memory-worthiness gate, which means fewer useless memories stored and fewer good ones sent to human review.

How to fine-tune it:

- use `./bin/labeler` to review turns and mark which ones were worth storing
- run `./bin/export` to turn those reviews into training data
- fine-tune the classifier checkpoint with your preferred training pipeline on that data
- point `LAYA_MODEL_REPO` at the tuned checkpoint so the daemon uses the improved gate

## Requirements

- Go 1.22+
- Python 3.10+
- PostgreSQL with `pgvector` if you want the memory kernel enabled
- OpenAI-compatible model endpoints for:
  - extraction on `8000`
  - embeddings on `8001`
  - fast chat on `8010`
  - heavy chat on `8011`
- Laya service on `8002`

## Quick Start

If your model servers are already running, the simplest macOS flow is:

```bash
chmod +x ./scripts/start-limn-macos.sh ./scripts/stop-limn-macos.sh
./scripts/start-limn-macos.sh
```

That script starts the Laya service, router, and daemon, and expects your model endpoints to already be running.

## Manual Setup

If you want to start each piece yourself:

```bash
export LIMN_SHARED_SECRET=$(openssl rand -hex 32)

cd limnd
go build -o bin/daemon  ./cmd/daemon
go build -o bin/labeler ./cmd/labeler
go build -o bin/export  ./cmd/export
go build -o bin/limn    ./cmd/limn
go build -o bin/router  ./cmd/router
```

Start the Laya service:

```bash
cd ../layarouter
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
USE_TF=0 uvicorn server:app --host 127.0.0.1 --port 8002
```

Start the router:

```bash
cd ../limnd
export LIMN_SHARED_SECRET=$LIMN_SHARED_SECRET
export LIMN_LAYA_URL=http://127.0.0.1:8002/classify
./bin/router
```

Start the daemon:

```bash
cd ../limnd
export LIMN_SHARED_SECRET=$LIMN_SHARED_SECRET
export LIMN_LAYA_MEMORY_URL=http://127.0.0.1:8002/memory-worthiness
./bin/daemon
```

## Admin Commands

Use these commands to review memory state after the daemon has compared a new memory against existing `AUTHORITATIVE` entries.

`pending` shows memories in `PENDING_CONFIRMATION`, including candidates that may supersede an older `AUTHORITATIVE` memory.

Review pending memories:

```bash
cd limnd
export LIMN_POSTGRES_DSN="postgres://limn:CHANGE_ME@localhost:5432/limn_kernel?sslmode=disable"
./bin/limn pending
```

`confirm` promotes a `PENDING_CONFIRMATION` memory to `AUTHORITATIVE`. If it replaces an older memory, the older one is marked `SUPERSEDED`.

Confirm a memory:

```bash
./bin/limn confirm <id>
```

`reject` leaves the current `AUTHORITATIVE` memory in place and marks the pending one as `REJECTED`.

Reject a memory:

```bash
./bin/limn reject <id>
```

Review unlabelled turns:

```bash
./bin/labeler
```

Export reviewed turns for training:

```bash
./bin/export
```

## Configuration

| Variable | Default | Used by |
|---|---|---|
| `LIMN_SHARED_SECRET` | required | daemon, router, Laya service, Pi |
| `LIMN_DAEMON_BIND` | `127.0.0.1:8080` | daemon |
| `LIMN_ROUTER_BIND` | `127.0.0.1:8090` | router |
| `LIMN_LAYA_URL` | `http://localhost:8002/classify` | router |
| `LIMN_LAYA_MEMORY_URL` | `http://localhost:8002/memory-worthiness` | daemon |
| `LAYA_MODEL_REPO` | `convaiinnovations/laya-typed-decisions` | Laya service |
| `LAYA_MEMORY_THRESHOLD` | `0.5` | Laya service |
| `LIMN_WORKER_COUNT` | `2` | daemon |
| `LIMN_POSTGRES_DSN` | required for kernel/admin commands | daemon, `cmd/limn` |
| `USE_TF` | `0` | Laya service |

## Ports

| Port | Service |
|---|---|
| 8000 | extraction model |
| 8001 | embedding model |
| 8002 | Laya classifier service |
| 8010 | fast chat model |
| 8011 | heavy chat model |
| 8080 | LIMN daemon |
| 8090 | LIMN router |

## AI Full Disclosure

LIMN was built with strong assistance from AI coding agents. The ideas, testing, debugging, and final decisions were led by humans, and that shaped the router, daemon, Pi integration, and Laya-based decision flow.

If you are not comfortable using software developed with significant AI assistance, this project is probably not for you.

## Notes

- The daemon uses SQLite for its durable queue and PostgreSQL for the kernel.
- Pi does not have its own memory admin commands; use `limn pending`, `confirm`, and `reject`.
- The router and daemon expect OpenAI-compatible HTTP endpoints.
