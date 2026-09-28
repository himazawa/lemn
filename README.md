# LEMN — Local Epistemic Memory Kernel + Laya Model Router

LEMN is a small local system for two related jobs:

1. route chat requests to a fast or heavy model
2. decide which turns are worth storing as long-term memory

Everything runs on your machine. Nothing leaves it.

It uses:

- a Go router for model selection
- a Go daemon for logging, extraction, and memory routing
- a Python Laya service for typed decisions
- PostgreSQL + pgvector for the memory kernel
- a Pi extension that logs turns and injects retrieved memories

The four services run in Docker. Your language models stay on the host, where
they can reach the GPU.

## Components

| Component | Language | Purpose |
|---|---|---|
| `lemnd/cmd/router` | Go | OpenAI-compatible proxy that routes each request to fast or heavy chat models |
| `lemnd/cmd/daemon` | Go | Durably ingests turns, decides whether they are memory-worthy, and routes approved memories into the kernel |
| `lemnd/cmd/lemn` | Go | Admin CLI for reviewing pending memories |
| `lemnd/cmd/labeler` | Go | Interactive tool for reviewing unlabelled turns |
| `lemnd/cmd/export` | Go | Exports reviewed turns to JSONL for training |
| `layarouter/server.py` | Python | Laya typed-decision service for `/classify` and `/memory-worthiness` |
| `pi-extension/` | TypeScript | Pi hooks for memory read/write integration |
| `schema.sql` | SQL | PostgreSQL + pgvector schema for the kernel |
| `docker-compose.yml` | YAML | Runs Postgres, Laya, the router, and the daemon |

## How it works

LEMN sits between your coding agent and your model server. It has three paths:
one for chat, one for writing memory, one for reading it.

### 1. Chat path — runs on every message

```
  you ──▶ Pi ──▶ ROUTER :8090 ──▶ Laya :8002   "requires_reasoning?"
                     │                 │
                     │        ┌────────┴────────┐
                     │        ▼                 ▼
                     │   p < threshold     p >= threshold
                     │    FAST model        HEAVY model
                     │        └────────┬────────┘
                     ▼                 ▼
                 router rewrites the "model" field and forwards
                 the request otherwise untouched
                                       │
                                       ▼
                                model server ──▶ answer ──▶ Pi ──▶ you
```

If Laya is unreachable or slow, the router **fails open to the heavy model**.
Misrouting a hard question to the fast model costs answer quality; misrouting an
easy one costs only latency, so the failure direction is deliberate.

### 2. Memory write path — runs after every completed turn

```
  Pi ──POST /log──▶ DAEMON :8080
                        │
                        ▼
                  SQLite queue          ◀── 202 returned to Pi immediately
                  (durable: survives a crash, retries with backoff)
                        │
                        ▼  background worker claims the job
             Laya  "memory_worthy?" ───────── no ──▶ drop
                        │ yes
                        ▼
             Laya  "globally_applicable?" ──▶ scope = global | <project>
                        │
                        ▼
             EXTRACTION model ──▶ {type, summary, confidence}
                        │            type == "none" ──▶ drop
                        ▼
             EMBEDDER ──▶ 1024-dim vector
                        │
                        ▼
             pgvector: compare with AUTHORITATIVE memories
                       IN THE SAME SCOPE ONLY
                        │
   ┌────────────────────┼─────────────────────┬────────────────────────┐
   ▼                    ▼                     ▼                        ▼
 evidence +          evidence +            anything            user said "replace",
 exactly 1 match     0 matches             else                "forget", "no longer"
      │                   │                   │                        │
  SUPERSEDE          AUTHORITATIVE        CANDIDATE          PENDING_CONFIRMATION
 (old → SUPERSEDED)   (live at once)    (needs promotion)      (needs review)
```

"Evidence" means the turn carried a tool call whose diff embeds close to the
claim — that is, the code actually changed in the way the memory says it did.

### 3. Memory read path — runs before every message

```
  Pi ──POST /retrieve {query, project_id}──▶ DAEMON
                                                 │
                                                 ▼
                                             EMBEDDER
                                                 │
                                                 ▼
              SELECT ... WHERE state = 'AUTHORITATIVE'
                           AND project_id IN (<project>, 'global')
                           AND similarity > LEMN_RETRIEVAL_THRESHOLD
                         ORDER BY similarity DESC LIMIT 5
                                                 │
                                                 ▼
                   injected into the turn as a separate message
```

Reads span your project **plus** `global`. Writes never cross scopes.

### Memory lifecycle

```
  a new memory lands in one of four states
  ────────────────────────────────────────
    evidence + exactly 1 match in scope ──▶ AUTHORITATIVE  (target → SUPERSEDED)
    evidence + no match                 ──▶ AUTHORITATIVE
    correction wording, but ambiguous   ──▶ PENDING_CONFIRMATION
    no tool-call evidence               ──▶ CANDIDATE

  and moves only on your say-so afterwards
  ────────────────────────────────────────
    PENDING_CONFIRMATION ──lemn confirm──▶ AUTHORITATIVE
    PENDING_CONFIRMATION ──lemn reject───▶ REJECTED
    CANDIDATE            ──SQL update────▶ AUTHORITATIVE

    AUTHORITATIVE ──a newer memory supersedes it──▶ SUPERSEDED
                                                    + 'supersedes' edge
                                                    + dependents marked
                                                      NEEDS_REVALIDATION
```

Only `AUTHORITATIVE` memories are ever retrieved. That is the whole point of the
state machine: nothing reaches your prompt until it has either earned automatic
promotion or been confirmed by you.

## Why This Helps Long-Horizon Use

LEMN is useful when the conversation has to stay coherent across many turns, sessions, or even days.

It helps because it does not treat memory as a single flat log. It separates raw turn capture, memory-worthiness gating, extraction, retrieval, and supersession, so the system can keep useful information while still replacing stale or duplicated memories over time.

That matters for long-horizon work because the prompt only needs the current authoritative view of the user, the project, or the task. Older memories do not just pile up forever; they can be superseded, reviewed, or rejected.

## Compared With Other Memory Kernels

Many memory systems are just one of these:

- an append-only vector store
- a note bucket with retrieval on similarity alone
- a prompt cache with no explicit lifecycle

LEMN is different in a few ways:

- it uses a typed decision step before storing memory at all
- it keeps a durable local queue so turns are not lost on crash
- it stores memory states like `AUTHORITATIVE`, `PENDING_CONFIRMATION`, `SUPERSEDED`, and `REJECTED`
- it supports human review instead of assuming every extraction is correct
- it separates model routing from memory routing, so the chat path and the memory path can evolve independently

The tradeoff is that LEMN is more opinionated than a plain vector database. It is built for controlled long-horizon memory with review and replacement, not for dumping every trace into a bag of embeddings.

Fine-tuning the classifier can make this even better by reducing false positives and false negatives in the memory-worthiness gate, which means fewer useless memories stored and fewer good ones sent to human review.

How to fine-tune it:

- use `./bin/labeler` to review turns and mark which ones were worth storing
- run `./bin/export` to turn those reviews into training data
- fine-tune the classifier checkpoint with your preferred training pipeline on that data
- point `LAYA_MODEL_REPO` at the tuned checkpoint so the daemon uses the improved gate

## Requirements

- Docker (Docker Desktop, Rancher Desktop, or equivalent)
- An OpenAI-compatible model server running **on the host**, not in a container —
  models need GPU/Metal access that containers cannot provide
- Pi, if you want the agent integration
- Go 1.22+ and Python 3.10+ only if you run the services outside Docker

You need three models:

| Role | Purpose | Suggestion |
|---|---|---|
| Heavy chat | hard questions | a 27B–32B 4-bit model |
| Fast chat | simple edits and lookups, and extraction | an 8B 4-bit model |
| Embeddings | similarity for memory | `bge-m3` (1024-dim) |

A fourth model, the Laya classifier, is downloaded automatically into a Docker
volume on first start. Extraction can reuse the fast model, so only three need
to be resident.

## Step-by-step setup

### 1. Start your model server

Keep it on **loopback**. Containers reach the host through `host.docker.internal`,
which resolves to the host's loopback interface — binding `0.0.0.0` exposes your
models to the whole network for no benefit.

```bash
# example: oMLX
omlx serve --model-dir ~/.omlx/models --hot-cache-max-size 20% \
  --paged-ssd-cache-dir ~/.omlx/cache
```

Enable the server's prompt/KV cache if it has one. Pi resends a large system
prompt every turn, and without caching that prefill dominates response time —
on one machine it was the difference between 52s and 0.5s per turn.

### 2. Get the exact model IDs

```bash
curl -s http://localhost:8000/v1/models | jq -r '.data[].id'
```

Copy them verbatim. A mismatch here surfaces as a 404 buried in the daemon log,
not as an obvious error. If your server lists speculative-decoding draft models,
do not pick one as a chat model — they are named after their target model and
are easy to confuse.

### 3. Create `.env`

```bash
cd limn
cat > .env <<EOF
LEMN_SHARED_SECRET=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -hex 16)
POSTGRES_USER=lemn
POSTGRES_DB=lemn_kernel

DAEMON_PORT=8080
ROUTER_PORT=8090
LAYA_PORT=8002

OMLX_URL=http://host.docker.internal:8000
LEMN_BACKEND_API_KEY=

LEMN_HEAVY_MODEL_NAME=<heavy id from step 2>
LEMN_FAST_MODEL_NAME=<fast id from step 2>
LEMN_EXTRACTION_MODEL=<fast id from step 2>
LEMN_EMBEDDING_MODEL=<embedding id from step 2>
EOF
chmod 600 .env
```

`LEMN_SHARED_SECRET` authenticates Pi to LEMN. `LEMN_BACKEND_API_KEY`
authenticates LEMN to your model server — leave it empty if the server needs no
key. They are deliberately different secrets.

Quote any value containing a space, e.g. `LEMN_HEAVY_MODEL_NAME="Qwen 3.8"`,
so both Compose and your shell parse it.

### 4. Start the stack

```bash
docker compose up -d --build
docker compose ps
```

Four containers: `postgres`, `laya`, `router`, `daemon`. Wait for all to report
`healthy` — the first `laya` start downloads its checkpoint, so give it a few
minutes and watch with `docker compose logs -f laya`.

### 5. Verify

```bash
SECRET=$(grep '^LEMN_SHARED_SECRET=' .env | cut -d= -f2)

curl -s localhost:8080/healthz    # daemon -> ok
curl -s localhost:8090/healthz    # router -> ok
curl -s localhost:8002/healthz    # laya   -> json with thresholds
```

Do **not** `source .env` before running `docker compose` — exported shell
variables override `.env` file values, and edits appear to be ignored.

Check the classifier separates easy from hard:

```bash
curl -s localhost:8002/classify -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' -d '{"query":"rename this variable"}'

curl -s localhost:8002/classify -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"query":"Design a sharding strategy for a write-heavy Postgres cluster"}'
```

Store a memory end to end:

```bash
curl -s -X POST localhost:8080/log -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' -d '{
    "id":"t1","project_id":"demo",
    "user_message":"We decided to drop Redis and use Postgres LISTEN/NOTIFY for the job queue.",
    "assistant_response":"Understood, removing the Redis dependency.",
    "tool_calls":[{"name":"edit","diff_text":"- redis.Client\n+ pq.Listener"}]}'

docker compose logs daemon --tail=20 | grep '\[Gate\]'
docker compose exec postgres psql -U lemn -d lemn_kernel \
  -c 'select id, state, project_id, category, summary from lemn_memories;'
```

Then promote and retrieve it:

```bash
docker compose exec daemon lemn pending
docker compose exec daemon lemn confirm <id>

curl -s -X POST localhost:8080/retrieve -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"query":"what is our job queue built on?","project_id":"demo","limit":5}'
```

### 6. Connect Pi

Register the router as a model in `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "lemn": {
      "baseUrl": "http://localhost:8090/v1",
      "api": "openai-completions",
      "apiKey": "<your LEMN_SHARED_SECRET>",
      "compat": { "supportsDeveloperRole": false, "supportsReasoningEffort": false },
      "models": [{ "id": "lemn", "name": "LEMN (routed)" }]
    }
  }
}
```

Install the extension. Pi loads TypeScript directly — there is no build step:

```bash
mkdir -p .pi/extensions
cp pi-extension/index.ts     .pi/extensions/lemn.ts
cp pi-extension/retrieval.ts .pi/extensions/retrieval.ts
```

`.pi/extensions/` is project-local; use `~/.pi/agent/extensions/` to enable it
everywhere. Then run Pi with the same secret in its environment:

```bash
export LEMN_SHARED_SECRET=<same value as .env>
pi --model lemn
```

The extension derives `project_id` from the git root, so each repository gets
its own memory space automatically. Override with `LEMN_PROJECT_ID`.

### 7. Use it

Nothing else to do — the extension retrieves before each message and logs after
each turn. Your ongoing work is reviewing what it wants to remember:

```bash
docker compose exec daemon lemn pending      # State | Project | Category
docker compose exec daemon lemn confirm <id>
docker compose exec daemon lemn reject <id>
```

`pending` lists only `PENDING_CONFIRMATION`. Turns without tool-call evidence
land in `CANDIDATE`, which is neither listed there nor retrievable:

```bash
docker compose exec postgres psql -U lemn -d lemn_kernel \
  -c "select id, project_id, summary from lemn_memories where state='CANDIDATE';"

docker compose exec postgres psql -U lemn -d lemn_kernel \
  -c "update lemn_memories set state='AUTHORITATIVE' where id=<id>;"
```

Lifecycle:

```bash
docker compose stop      # keep data
docker compose up -d     # resume
docker compose down -v   # wipe memories and the queue
```

## Tuning

The thresholds are starting points, not validated cutoffs. Collect real data
before changing them:

```bash
docker compose logs daemon | grep '\[Gate\]'       # memory + scope probabilities
docker compose logs router | grep requires_reasoning
```

Edit `.env`, then restart only the affected service with
`docker compose up -d laya`.

Two things worth knowing from real measurements:

- `LEMN_RETRIEVAL_THRESHOLD` is embedder-specific. With `bge-m3`, a question
  matching its own answer scores around 0.5, while unrelated text scores
  0.3–0.4 — so the default is 0.45. A different embedder needs re-measuring, or
  retrieval silently returns nothing.
- `LAYA_GLOBAL_THRESHOLD` is the least reliable of the three. The `global` vs
  project distinction was never trained into the checkpoint, and separation
  between the two classes is narrow. Setting it high is the safe failure:
  everything stays project-scoped, and you promote globals by hand.

For a real improvement rather than threshold nudging, label your own turns and
fine-tune:

```bash
docker compose exec -it daemon labeler
docker compose exec daemon export
```

## Running without Docker

If you want to start each piece yourself:


```bash
export LEMN_SHARED_SECRET=$(openssl rand -hex 32)

cd lemnd
go build -o bin/daemon  ./cmd/daemon
go build -o bin/labeler ./cmd/labeler
go build -o bin/export  ./cmd/export
go build -o bin/lemn    ./cmd/lemn
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
cd ../lemnd
export LEMN_SHARED_SECRET=$LEMN_SHARED_SECRET
export LEMN_LAYA_URL=http://127.0.0.1:8002/classify
./bin/router
```

Start the daemon:

```bash
cd ../lemnd
export LEMN_SHARED_SECRET=$LEMN_SHARED_SECRET
export LEMN_LAYA_MEMORY_URL=http://127.0.0.1:8002/memory-worthiness
./bin/daemon
```

## Memory Scoping

Every memory carries a `project_id`. The Pi extension derives it from the git repository root (falling back to the working directory name), so each repo gets its own memory space without configuration. Override it with `LEMN_PROJECT_ID`.

The scope `global` is special: those memories apply everywhere. Use it for durable user-level preferences rather than project facts.

Scope is chosen automatically. `/memory-worthiness` answers a second typed question, `globally_applicable`, asking whether the turn describes a durable preference of the *user* that holds across every codebase, rather than a fact belonging to one repository. If yes the memory is stored as `global`; otherwise it takes the caller's `project_id`. Tune the cutoff with `LAYA_GLOBAL_THRESHOLD`, and watch the daemon's `[Gate]` log lines to see both probabilities per turn.

The read and write rules are deliberately asymmetric:

- **Retrieval** returns memories from the current project **plus** `global`.
- **Supersession** only ever matches within a memory's own scope.

That asymmetry is the point. A fact learned in one project must never mark another project's memory `SUPERSEDED`, because the two can be textually near-identical while both remaining true. Requests that omit `project_id` fall back to `global`.

Upgrading a database created before scoping existed:

```sql
ALTER TABLE lemn_memories ADD COLUMN project_id TEXT NOT NULL DEFAULT 'global';
CREATE INDEX lemn_memories_scope_idx ON lemn_memories (project_id, state);
```

Existing rows become `global`, so they stay visible from every project. Re-scope them individually if that is not what you want.

## Admin Commands

Step 7 covers the Docker form (`docker compose exec daemon lemn ...`). Outside
Docker the same commands need the DSN in the environment:

```bash
cd lemnd
export LEMN_POSTGRES_DSN="postgres://lemn:CHANGE_ME@localhost:5432/lemn_kernel?sslmode=disable"
```

| Command | Effect |
|---|---|
| `./bin/lemn pending` | Lists `PENDING` and `PENDING_CONFIRMATION` memories, with their project and any supersession target |
| `./bin/lemn confirm <id>` | Promotes to `AUTHORITATIVE`; if it names exactly one target, that target becomes `SUPERSEDED` and its dependents `NEEDS_REVALIDATION` |
| `./bin/lemn reject <id>` | Marks the pending memory `REJECTED`, leaving the current authoritative one in place |
| `./bin/labeler` | Interactive review of unlabelled turns from the SQLite queue |
| `./bin/export` | Writes reviewed turns to `data/train.jsonl` and `data/valid.jsonl` |

There is no `unsupersede`. Restoring a memory that was superseded by mistake
takes SQL:

```sql
UPDATE lemn_memories SET state = 'AUTHORITATIVE' WHERE id = <id>;
DELETE FROM lemn_edges WHERE target_id = <id> AND relationship = 'supersedes';
```

## Configuration

| Variable | Default | Used by |
|---|---|---|
| `LEMN_SHARED_SECRET` | required | daemon, router, Laya service, Pi |
| `LEMN_DAEMON_BIND` | `127.0.0.1:8080` | daemon |
| `LEMN_ROUTER_BIND` | `127.0.0.1:8090` | router |
| `LEMN_LAYA_URL` | `http://localhost:8002/classify` | router |
| `LEMN_LAYA_MEMORY_URL` | `http://localhost:8002/memory-worthiness` | daemon |
| `LEMN_FAST_MODEL_URL` | `http://localhost:8010/v1/chat/completions` | router |
| `LEMN_HEAVY_MODEL_URL` | `http://localhost:8011/v1/chat/completions` | router |
| `LEMN_FAST_MODEL_NAME` | unset | router |
| `LEMN_HEAVY_MODEL_NAME` | unset | router |
| `LEMN_BACKEND_API_KEY` | unset | router, daemon |
| `LEMN_CLASSIFY_TIMEOUT` | `5s` | router |
| `LEMN_EXTRACTION_URL` | `http://localhost:8000/v1/chat/completions` | daemon |
| `LEMN_EXTRACTION_MODEL` | `qwen2.5-coder` | daemon |
| `LEMN_EMBEDDING_URL` | `http://localhost:8001/v1/embeddings` | daemon |
| `LEMN_EMBEDDING_MODEL` | `bge-m3` | daemon |
| `LEMN_RETRIEVAL_THRESHOLD` | `0.45` | daemon |
| `LEMN_SUPERSEDE_THRESHOLD` | `0.82` | daemon |
| `LEMN_LAYA_TIMEOUT` | `60s` | daemon |
| `LEMN_EXTRACTION_TIMEOUT` | `120s` | daemon |
| `LEMN_SQLITE_PATH` | `./lemn_data.db` | daemon, labeler, export |
| `LEMN_PROJECT_ID` | git root name | Pi extension |
| `LEMN_DAEMON_URL` | `http://localhost:8080` | Pi extension |
| `LAYA_MODEL_REPO` | `convaiinnovations/laya-typed-decisions` | Laya service |
| `LAYA_ROUTING_THRESHOLD` | `0.5` | Laya service |
| `LAYA_MEMORY_THRESHOLD` | `0.5` | Laya service |
| `LAYA_GLOBAL_THRESHOLD` | `0.67` | Laya service |
| `LEMN_WORKER_COUNT` | `2` | daemon |
| `LEMN_POSTGRES_DSN` | required for kernel/admin commands | daemon, `cmd/lemn` |
| `USE_TF` | `0` | Laya service |

If you change `LEMN_EMBEDDING_MODEL`, update the `vector(1024)` column width in `schema.sql` to match the new model's output dimension — Postgres rejects inserts of the wrong width.

## Ports

| Port | Service |
|---|---|
| 8002 | Laya classifier service |
| 8080 | LEMN daemon (`/log`, `/retrieve`) |
| 8090 | LEMN router (OpenAI-compatible, point Pi here) |

Model endpoints are wherever your server runs. LEMN supports both layouts:

**Port per model** — the historical default (`8000` extraction, `8001`
embeddings, `8010` fast, `8011` heavy). Set the `*_URL` variables and leave the
`*_MODEL_NAME` ones unset; the client's `model` field is forwarded unchanged.

**One port, many models** — how oMLX and LM Studio work, selecting the model
from the request body. Point every `*_URL` at the same address and set
`LEMN_FAST_MODEL_NAME` and `LEMN_HEAVY_MODEL_NAME`, otherwise the router cannot
tell fast from heavy and routing silently becomes a no-op. When those are set,
the router rewrites only the `model` field and passes every other field through
untouched.

## AI Full Disclosure

LEMN was built with strong assistance from AI coding agents. The ideas, testing, debugging, and final decisions were led by humans, and that shaped the router, daemon, Pi integration, and Laya-based decision flow.

If you are not comfortable using software developed with significant AI assistance, this project is probably not for you.

## Notes

- The daemon uses SQLite for its durable queue and PostgreSQL for the kernel.
- Pi does not have its own memory admin commands; use `lemn pending`, `confirm`, and `reject`.
- The router and daemon expect OpenAI-compatible HTTP endpoints.
- `/log` returns `202` as soon as the turn is queued. Everything after that —
  gating, extraction, embedding, kernel routing — happens in a background
  worker, so failures show up in `docker compose logs daemon`, not in the
  response.
- The router forwards the request body unchanged apart from the `model` field,
  so streaming, tools, and provider-specific options pass straight through.
- Giving Docker more CPU helps: Laya is CPU-only and competes with the GPU work
  on your host. Going from 2 to 6 cores cut classification from 0.50s to 0.22s
  on one machine.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Every request feels slow and answers are oddly thorough | Laya is timing out and the router is failing open to heavy. Check `docker compose logs router` for `failing open`, and raise `LEMN_CLASSIFY_TIMEOUT` |
| `/retrieve` always returns `[]` | `LEMN_RETRIEVAL_THRESHOLD` is too high for your embedder, or the memory is still `CANDIDATE` rather than `AUTHORITATIVE` |
| Memories never appear at all | Check `docker compose logs daemon` for `[Gate]` lines. No line means the job failed before the gate; the model server is the usual cause |
| `404` from the backend | A `*_MODEL_NAME` does not match an id from `/v1/models` |
| First turn takes a minute, later ones are fast | Normal prompt prefill. Enable your model server's prompt cache |
| Config edits appear ignored | You ran `source .env` in that shell; exported variables override the `.env` file for Compose |

