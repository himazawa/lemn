# LEMN — Local Epistemic Memory Kernel + Laya Model Router

<img width="1024" height="512" alt="lemn_logo" src="https://github.com/user-attachments/assets/cf0358c6-ebdc-4ded-ade7-5e1660b8b214" />

**Status: experimental prototype, not production-ready.** Current benchmarks are
synthetic functional checks; they do not establish reliable long-term memory
quality or improved task outcomes on real-world projects. Treat recalled
memories as fallible and keep critical project knowledge in version-controlled
documentation.

LEMN is a local memory service for coding agents, plus an optional model router.
Its memory kernel manages the lifecycle of candidate facts: worthiness gating,
extraction, evidence and relation checks, review or promotion, scoped retrieval,
and supersession/revalidation. It is intended to help agents carry useful,
corrected project knowledge across sessions.

The optional router independently sends chat requests to a fast or heavy model.
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
  you ──▶ Pi ──▶ ROUTER :8090
                     │
                     │  "does this span many parts of the codebase,
                     ├─▶ Laya :8002   or is it one lookup / one local edit?"
                     │        │
                     │        ▼  p, compared with LAYA_ROUTING_THRESHOLD
                     │   ┌────┴────┐
                     │   ▼         ▼
                     │  below    at/above
                     │  FAST      HEAVY
                     │   └────┬────┘
                     ▼        ▼
            if configured, rewrites only the "model" field;
            every other client field is forwarded untouched
                              │
                              ▼
                        model server
                              │
                              ▼
  answer + X-Lemn-Route (model/probability headers when available)
                              │
                              ▼
                    Pi ──▶ you   (extension shows the route in the footer)
```

Requests whose estimated prompt size exceeds `LEMN_LONG_CONTEXT_TOKENS` go
directly to the heavy model without classification.

If Laya is unreachable or slow, the router **fails open to the heavy model**.
Misrouting a hard question to the fast model costs answer quality; misrouting an
easy one costs only latency, so the failure direction is deliberate. Watch for
`failing open` in the router log — it means routing has silently stopped
mattering.

The fast model must be able to **emit tool calls**, not just answer well. A
model that narrates ("let me list the directory") instead of calling the tool
ends the turn with no error, which is indistinguishable from a hang.

### 2. Memory write path — runs after every completed turn

```
  Pi ──POST /log {turn, project_id}──▶ DAEMON :8080
                        │
                        ▼
                  SQLite queue          ◀── 202 returned to Pi immediately
                  (durable: survives a crash, retries with backoff)
                        │
                        ▼  background worker claims the job
             Laya  "worth remembering?"  ── below LAYA_MEMORY_THRESHOLD ──▶ drop
                        │ above
                        ▼
             Laya  "true even in an unrelated project?"
                        │        above LAYA_GLOBAL_THRESHOLD ──▶ scope = global
                        │        below ──────────────────────▶ scope = <project>
                        ▼
             EXTRACTION model ──▶ {type, summary, confidence}
                        │            type == "none" ──▶ drop
                        ▼            (the model's own veto)
             EMBEDDER ──▶ vector
                        │
                        ▼
             pgvector: compare with AUTHORITATIVE memories
                       IN THE SAME SCOPE ONLY,
                       above LEMN_SUPERSEDE_THRESHOLD
                        │
   ┌───────────────────┼─────────────────────┬───────────────────────────┐
   ▼                   ▼                     ▼                           ▼
 non-correction     non-correction      correction/relation       valid-target relation proposal
 independent claim  independent claim   not auto-confirmable      with evidence and confidence
 without evidence   with tool evidence   (needs review)             at/above auto-confirm bar
   │                   │                  │                           │
 OBSERVED         AUTHORITATIVE     PENDING_CONFIRMATION ◀────────────┘
 (auto-promoted   (auto-promoted,      │
  if confidence    stamped             │ human confirms
  ≥ LEMN_PROMOTE_  auto_promoted)      ▼
  THRESHOLD)
                                   AUTHORITATIVE + graph changes
                      │
       ┌──────────────┼────────────────────┐
       ▼              ▼                    ▼
  SUPERSEDED     CONTRADICTED      dependents need revalidation
```

"Evidence" means the turn carried a tool call whose diff embeds close to the
claim — that is, the code actually changed in the way the memory says it did.

One branch of the diagram resolves itself without a human. A valid relation
proposal (`supersedes`/`contradicts`) that is backed by relevant tool
evidence **and** whose extraction confidence clears
`LEMN_AUTO_SUPERSEDE_THRESHOLD` (default 0.95) is confirmed automatically in
the same background job, through the same `ConfirmPendingMemory`
transaction `lemn confirm` uses — target flipped, edge written, dependents
cascaded to `NEEDS_REVALIDATION`. The row is stamped `auto_supersede: true`
in provenance so unattended graph mutations are auditable. A valid relation
that misses the evidence or confidence bar, or whose target changes before
confirmation, stays in `PENDING_CONFIRMATION` for human review. A relation
proposal with no valid target is downgraded to an independent claim and follows
the normal promotion rules. The failure direction matches the router's
philosophy: when in doubt, over-review rather than auto-destroy an authoritative
memory.

Nothing on this path blocks your conversation: `/log` returns once the turn is
queued, and every later failure surfaces only in `docker compose logs daemon`.

### 3. Memory read path — runs before every message

```
  Pi ──POST /retrieve {query, project_id}──▶ DAEMON :8080
                                                 │
                                                 ▼
                                             EMBEDDER
                                                 │
                                                 ▼
              SELECT ... WHERE state = 'AUTHORITATIVE'
                           AND project_id IN (<project>, 'global')
                         ORDER BY similarity DESC LIMIT 5   (top-N, ranked)
                                                 │
                                                 ▼
        in Go: keep those > LEMN_RETRIEVAL_THRESHOLD.
        If none clear the bar but the best is within
        LEMN_RETRIEVAL_FALLBACK_MARGIN below it, return the
        top-N flagged below_threshold (near-miss fallback).
        Otherwise return [].
                                                 │
                                                 ▼
                   injected into the turn as a separate message
                   (Pi builds the prompt, so the user's own
                    text is never rewritten)
```

Reads span your project **plus** `global`. Writes never cross scopes.

The threshold is applied in Go, not SQL, so one ranked query serves both the
normal path and the fallback. The **near-miss fallback** exists because a broad
question ("what do you know about this project?") embeds a hair under the bar
against narrow summaries — e.g. a best score of 0.446 against a 0.45 threshold.
Without the fallback that returns `[]` and the model answers "I have no
memory"; with it, the top memories are injected (flagged `below_threshold`, so
the prompt presents them as weaker context). Genuinely unrelated turns — best
match well below `threshold - margin` — still return nothing, so the prompt is
not filled with stale memories.

On error this returns `[]` rather than failing, so an empty result can still
mean "nothing is close enough", "nothing is AUTHORITATIVE yet", or "the
embedder is down". Check the daemon log for `[Retrieval Error]` before
concluding the memory is missing.

### Retrieval depth

The `LIMIT 5` above is the extension's default, not a daemon limit — the
`/retrieve` endpoint accepts any `limit`. The extension reads it from
`LEMN_RETRIEVAL_LIMIT` (default 5). Five is a prompt-budget choice: injected
memories are re-sent on every turn, and reads span the project plus `global`,
so a high limit eventually fills the prompt with stale-but-authoritative noise.

To get a fuller picture without changing the default:

- ask several narrow questions instead of one broad one — ranking is per-query,
  so different questions surface different slices of the authoritative set;
- or query the daemon directly with a higher limit:

```bash
curl -s -X POST localhost:8080/retrieve -H "Authorization: Bearer $LEMN_SHARED_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"query":"<question>","project_id":"<project>","limit":50}'
```

- or list everything unranked with `lemn pending` / a direct `SELECT` on
  `lemn_memories`.

### Memory lifecycle

```
  non-correction independent claim without relevant tool evidence ──▶ OBSERVED
    (auto-promoted if confidence ≥ LEMN_PROMOTE_THRESHOLD,
     else inert until review)
  evidence-backed, non-correction independent claim ──▶ AUTHORITATIVE (auto)
  valid-target relation proposal with tool evidence AND confidence
  ≥ LEMN_AUTO_SUPERSEDE_THRESHOLD ──▶ auto-confirmed in the same job:
    AUTHORITATIVE + target SUPERSEDED/CONTRADICTED (stamped auto_supersede)
  explicit correction without an auto-confirmable relation ──▶ PENDING_CONFIRMATION
  valid-target relation below auto-confirm gate ──▶ PENDING_CONFIRMATION
  relation proposal with invalid target ──▶ downgraded to independent; normal rules apply

  corrections and destructive relations stay pending unless a valid destructive
  relation passes the auto-confirm gate above; dependency edges alone do not
  require review
  ────────────────────────────────────────────────
    OBSERVED / PENDING_CONFIRMATION / legacy CANDIDATE
      ├── lemn confirm ──▶ AUTHORITATIVE
      └── lemn reject  ──▶ REJECTED

    confirm supersedes ──▶ prior target: SUPERSEDED
    confirm contradicts ─▶ prior target: CONTRADICTED
    (either) ─▶ target's dependents: NEEDS_REVALIDATION
    NEEDS_REVALIDATION
      ├── lemn revalidate --evidence-quote <passage>
      │                     --evidence-source <approved-https-url> ──▶ AUTHORITATIVE
      ├── lemn revalidate --user-confirmed ────▶ AUTHORITATIVE
      └── lemn reject ─────────────────────────▶ REJECTED
```

Extraction may propose relations and dependencies only against the scoped list
of current authoritative memories. Confirmation revalidates those IDs and
writes the graph changes atomically. Confirming a `supersedes`/`contradicts`
relation recursively marks the target's authoritative dependents
`NEEDS_REVALIDATION`; they stay out of retrieval until explicitly revalidated
through `lemn revalidate`. Ordinary `confirm` and
`sweep --apply` cannot restore them. Revalidation requires either explicit user
confirmation or a quoted passage that LEMN fetches from an allowlisted HTTPS
host and finds in the visible source text. Fetches reject redirects outside the
allowlist, private and special-use IP addresses, credentials, non-HTTPS URLs,
query-bearing URLs, unsupported content types, bodies above 2 MiB, and quotes
above 16 KiB. LEMN records hashes of the fetched body and cited passage. It uses
HTTP `Last-Modified` and `Age` when present, otherwise fetch time, and rejects
evidence observed before invalidation or older than
`LEMN_REVALIDATION_MAX_EVIDENCE_AGE` (default `720h`, 30 days).
Any retained dependencies must still be authoritative; stale dependencies must
be replaced or cleared. A matching quote proves only that the passage appeared
in the fetched source; it does not prove the source is correct or logically
supports the memory. If the source cannot be fetched or matched, use explicit
user confirmation instead.

Automatic promotion is deliberately narrow, and it is the **extraction model's
own call** that the daemon rubber-stamps: the daemon applies thresholds to the
model's outputs, it does not re-judge the claim. A memory is promoted to
`AUTHORITATIVE` without review when it is backed by relevant tool evidence
(`CANDIDATE`, always promoted) or, unbacked (`OBSERVED`), when the model's own
confidence clears `LEMN_PROMOTE_THRESHOLD` (default 0.9). Confidence-based
auto-promotions are stamped `auto_promote_reason: confidence` in provenance so
you can audit them. Rows created as `CANDIDATE` by older versions can still be
confirmed or rejected normally.

What is actually human-gated is narrower than "everything that touches the
graph". Only **destructive relations** — `supersedes`, `contradicts`, and
explicit corrections — force `PENDING_CONFIRMATION`, and only `lemn confirm`
writes their edges and applies the target's state change (`SUPERSEDED` /
`CONTRADICTED`) plus the recursive `NEEDS_REVALIDATION` of its dependents. The
auto path (`insertMemory`) writes the memory row and nothing else: it never
touches `lemn_edges`. The one exception is the automatic confirmation of
evidence-backed, high-confidence relation proposals (see the write-path note
above): it does not bypass the reviewed path, it *is* the reviewed path with
the human step satisfied by tool evidence plus a stricter confidence bar,
and it runs the identical transaction. Safe independent claims that qualify for
automatic promotion now also pass through this transaction: dependency IDs are
validated and their `depends_on` edges are written atomically with promotion.
If validation fails, the memory remains in its reviewable pre-promotion state
and the daemon logs the failure rather than marking it authoritative without
its dependency graph.

Only `AUTHORITATIVE` memories are ever retrieved. That is the whole point of the
state machine: nothing reaches your prompt until it has either earned automatic
promotion or been confirmed by you.

## Long-Horizon Goal

LEMN is designed for work that spans many turns or sessions. Unlike a flat
append-only note store, it gives memories explicit states and provenance, scopes
them by project, limits prompt injection to retrieved authoritative facts, and
supports review, supersession, contradiction, and dependent-memory revalidation.
The aim is to keep useful context available while making stale claims easier to
replace and inspect.

That is an architectural rationale, not yet a demonstrated long-horizon outcome.
The current synthetic benchmarks exercise retrieval, scope isolation, staged
updates, and the write pipeline on labeled examples. They do not establish that
LEMN improves task success over a maintained `AGENTS.md` across real projects or
long periods. Treat effectiveness as an open evaluation question.

## Compared With Other Memory Kernels

Many memory systems are just one of these:

- an append-only vector store
- a note bucket with retrieval on similarity alone
- a prompt cache with no explicit lifecycle

LEMN is different in a few ways:

- it uses a typed decision step before storing memory at all
- it keeps a durable local queue so turns are not lost on crash
- it stores memory states like `AUTHORITATIVE`, `PENDING_CONFIRMATION`, `SUPERSEDED`, and `REJECTED`
- it auto-promotes only claims that add a leaf (evidence-backed, or high-confidence and unbacked) that propose no destructive relation, and human-reviews every `supersedes`/`contradicts`/correction — except evidence-backed proposals at or above `LEMN_AUTO_SUPERSEDE_THRESHOLD`, which are auto-confirmed through the same transaction a human confirmation uses
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

You need three models. LEMN does not care which ones — any OpenAI-compatible
server and any models it can serve will work. What matters is the *role* each
one fills:

| Role | Must be able to | Sizing guidance |
|---|---|---|
| Heavy chat | handle your hardest questions | the best model you can keep resident |
| Fast chat | answer simple turns **and call tools reliably** | mid-size; see the warning below |
| Extraction | emit small, valid JSON on request | usually the fast model, reused |
| Embeddings | produce sentence embeddings | any; its dimension must match `schema.sql` |

A fourth model, the Laya classifier, downloads automatically into a Docker
volume on first start and needs nothing from you.

> **Do not pick the fast model purely on size.** In a coding agent it has to
> *emit* tool calls, not describe them. An 8B model tested here consistently
> replied "let me list the directory" and then stopped, ending the turn with no
> tool call and no error — which looks exactly like a hang. A 14B of the same
> family called the tool every time. Test your candidate before committing to
> it; the check is in [Tuning](#tuning).

If you have no preference, one working combination on a 48 GB Apple Silicon
machine is a 27B 4-bit as heavy, a 14B 4-bit as fast plus extraction, and
`bge-m3` for embeddings — about 25 GB resident. Treat that as an illustration,
not a requirement.

## Step-by-step setup

### 1. Start your model server

Any OpenAI-compatible server works — oMLX, LM Studio, llama.cpp, vLLM, Ollama.
Keep it on **loopback**. Containers reach the host through `host.docker.internal`,
which resolves to the host's loopback interface — binding `0.0.0.0` exposes your
models to the whole network for no benefit.

```bash
# example only — use whatever server you run
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

Copy them verbatim — whatever your server reports is what goes in `.env`. A
mismatch surfaces as a 404 buried in the daemon log, not as an obvious error.

If your server lists speculative-decoding draft models, do not pick one as a
chat model. They are named after their target, so a draft for a 27B may look
like the 27B itself.

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

`OMLX_URL` is named after one server but is just the base URL of whichever
OpenAI-compatible server you run; every model endpoint is derived from it.

Quote any value containing a space, e.g. `LEMN_HEAVY_MODEL_NAME="Qwen 3.8"`,
so both Compose and your shell parse it.

If your embedding model is not 1024-dimensional, change the `vector(1024)`
column in `schema.sql` to match before first start. Postgres rejects inserts of
the wrong width.

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

For source-backed revalidation, configure `LEMN_REVALIDATION_ALLOWED_HOSTS` as
a comma-separated exact-host allowlist, for example
`docs.example.test,git.example.test`. Docker CLI invocations inherit this from
the daemon service environment; when running `./bin/lemn` locally, export the
same variable in that shell. An empty allowlist disables source fetching; use
`--user-confirmed` if the source cannot be approved or fetched.

```bash
docker compose exec daemon lemn revalidate \
  --evidence-quote "The API uses HTTP/2 for requests." \
  --evidence-source "https://docs.example.test/api" <id>
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
      "models": [{ "id": "lemn", "name": "LEMN (routed)", "contextWindow": 98304, "maxTokens": 8192 }]
    }
  }
}
```

Set `contextWindow` to the heavy model's window. Pi triggers auto-compaction
from this value; left unset it assumes a larger default and sends prompts the
backend rejects as too long. The router sends anything above
`LEMN_LONG_CONTEXT_TOKENS` to the heavy model, so keep that below the fast
model's window minus `maxTokens`.

Install the extension. Pi loads TypeScript directly — there is no build step.
It must go in its own subdirectory, because Pi treats every `*.ts` directly
inside `extensions/` as an extension entry point, and `retrieval.ts` is a helper
module rather than one:

```bash
mkdir -p .pi/extensions/lemn
cp pi-extension/index.ts     .pi/extensions/lemn/index.ts
cp pi-extension/retrieval.ts .pi/extensions/lemn/retrieval.ts
```

`.pi/extensions/` is project-local; use `~/.pi/agent/extensions/` to enable it
everywhere. Then run Pi with the same secret in its environment:

```bash
export LEMN_SHARED_SECRET=$(grep '^LEMN_SHARED_SECRET=' .env | cut -d= -f2)
pi -e ./.pi/extensions/lemn/index.ts --model lemn
```

Add `--no-extensions` when scripting with `-p`. Some globally installed Pi
extensions hold the Node event loop open with servers or timers, which stops
print mode from ever exiting; `-ne` disables discovery while explicit `-e`
paths still load.

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

`pending` lists `OBSERVED`, legacy `CANDIDATE` rows, correction proposals, and
memories that need revalidation. New writes no longer land in `CANDIDATE`:
evidence-backed independent claims are auto-promoted, so the queue only holds
memories that need a judgment call. Review the displayed relation/dependency
proposals before confirming. Use `--relation`, `--target`, `--depends-on`, or
`--clear-dependencies` to correct them; only `AUTHORITATIVE` memories are
retrieved.

Lifecycle:

```bash
docker compose stop      # keep data
docker compose up -d     # resume
docker compose down -v   # wipe memories and the queue
```

## Tuning

### Check your fast model can actually call tools

Do this before anything else. The fast model handles most turns, and if it
narrates tool calls instead of emitting them, Pi ends the turn silently and it
looks like a hang.

```bash
K=your-model-server-key
for M in "<fast-id>" "<heavy-id>"; do
  printf '%-24s ' "$M"
  curl -s http://localhost:8000/v1/chat/completions \
    -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$M\",
         \"messages\":[{\"role\":\"user\",\"content\":\"What is in this folder?\"}],
         \"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"list\",
           \"description\":\"List files in a directory\",
           \"parameters\":{\"type\":\"object\",
             \"properties\":{\"path\":{\"type\":\"string\"}},
             \"required\":[\"path\"]}}}]}" \
    | python3 -c "import sys,json;m=json.load(sys.stdin)['choices'][0]['message'];print('TOOL CALL' if m.get('tool_calls') else 'NARRATED ONLY — too weak')"
done
```

Both should print `TOOL CALL`. If the fast model narrates, either pick a larger
one or disable its reasoning mode — models that "think" often spend the budget
planning the call and then emit the plan as prose.

## Calibrating Laya

Laya answers three yes/no questions, each with its own threshold. None of the
defaults are validated for your workload — the checkpoint was never trained on
these particular questions, so they are informed guesses. Calibration is worth
doing once, and takes about ten minutes.

| Question | Endpoint | Threshold | Decides |
|---|---|---|---|
| spans many parts of the codebase | `/classify` | `LAYA_ROUTING_THRESHOLD` | fast or heavy model |
| worth remembering | `/memory-worthiness` | `LAYA_MEMORY_THRESHOLD` | store the turn or drop it |
| true in an unrelated project | `/memory-worthiness` | `LAYA_GLOBAL_THRESHOLD` | `global` or project scope |

Two properties matter before you start. The probabilities are **compressed** —
observed range is roughly 0.15–0.57, not 0–1, so a threshold of 0.5 is far
harsher than it looks. And they are **sensitive to phrasing length**, so a terse
hard request can score below a wordy easy one.

### Step 1 — write a labelled set

Twelve to twenty examples of your *own* real requests, split into the two
classes. Be honest about the labels; the calibration is only as good as they
are. Keep some short and some long, since length is a confounder.

### Step 2 — measure and pick a threshold

```bash
cd ~/Workspace/limn
SECRET=$(grep '^LEMN_SHARED_SECRET=' .env | cut -d= -f2)

python3 - "$SECRET" <<'PY'
import json, sys, urllib.request
secret = sys.argv[1]

# label your own examples here
CLASS_A = ["rename this variable", "what port does the daemon use"]      # want: fast
CLASS_B = ["find security vulnerabilities in this repo",                 # want: heavy
           "why is our p99 latency spiking only during deploys"]

def p(q):
    r = urllib.request.Request("http://localhost:8002/classify",
        data=json.dumps({"query": q}).encode(),
        headers={"Authorization": f"Bearer {secret}", "Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(r, timeout=60))["probability"]

a = [(q, p(q)) for q in CLASS_A]
b = [(q, p(q)) for q in CLASS_B]
for q, v in sorted(a, key=lambda x: -x[1]): print(f"  A {v:.3f}  {q[:55]}")
print()
for q, v in sorted(b, key=lambda x: x[1]):  print(f"  B {v:.3f}  {q[:55]}")

hi_a, lo_b = max(v for _, v in a), min(v for _, v in b)
print(f"\nmax A = {hi_a:.3f}   min B = {lo_b:.3f}   separation = {lo_b - hi_a:+.3f}")
print(f"threshold = {(hi_a + lo_b) / 2:.2f}" if lo_b > hi_a
      else "OVERLAP — the question cannot separate these; reword it (step 3)")
PY
```

Put the threshold **midway between the highest A and the lowest B**. If the two
classes overlap, no threshold works and you need step 3.

For the memory questions, point the same script at `/memory-worthiness` with
`{"user_message": ..., "assistant_response": ...}` and read `probability` or
`global_probability`.

### Step 3 — reword the question when separation is poor

This matters more than the threshold. The question text is just a string in
[layarouter/server.py](layarouter/server.py), and rewording it changed routing
from unusable to 12/12 correct here.

Compare candidate phrasings directly against the loaded model:

```bash
docker compose exec -T laya python -c "
import os; os.environ.setdefault('USE_TF','0')
import laya
a = laya.load('convaiinnovations/laya-typed-decisions')

CLASS_A = ['rename this variable', 'what port does the daemon use']
CLASS_B = ['find security vulnerabilities in this repo',
           'why is our p99 latency spiking only during deploys']

VARIANTS = {
  'current': 'Does this task require reading and reasoning about many parts of a codebase before answering, as opposed to a single lookup or a localized edit to one spot?',
  'mine':    'YOUR ALTERNATIVE PHRASING HERE',
}

for name, instr in VARIANTS.items():
    q = {'k': {'type': 'noul', 'instructions': instr}}
    f = lambda t: a.predict({'query': t}, q)['answers']['k']['noul']
    hi_a, lo_b = max(f(x) for x in CLASS_A), min(f(x) for x in CLASS_B)
    print(f'{name:<10} maxA={hi_a:.3f} minB={lo_b:.3f} sep={lo_b-hi_a:+.3f}')
"
```

Pick the variant with the largest positive separation, paste it into
`ROUTING_QUESTIONS` or `MEMORY_QUESTIONS`, then `docker compose up -d --build laya`.

For the memory-worthiness question there is a permanent guard:
[layarouter/eval_memory_gate.py](layarouter/eval_memory_gate.py) pins a
labelled set of real traffic (decisions, corrections, and confirmed facts that
must pass; questions, lookups, and acknowledgements that must not) and fails
if a rewording moves the separation below its recorded floor, misclassifies
more than the tolerated budget, or flips an anchor example. Run it after any
change to `MEMORY_QUESTIONS["memory_worthy"]` or `LAYA_MEMORY_THRESHOLD`:

```bash
docker compose exec laya python eval_memory_gate.py --assert
```

What worked, from the variants tested here:

- **Concrete and observable beats abstract.** "Reads many parts of a codebase"
  separated cleanly; "requires deep multi-step reasoning" did not.
- **Counterfactuals work well for scope.** "Would this still be true if the
  project were deleted and the user started an unrelated one?" beat asking
  directly whether something is "about the user".
- **Asking about the speaker backfires.** One phrasing scored project facts
  *higher* than genuine user preferences — inverted separation.

### Step 4 — watch it on real traffic

```bash
docker compose logs daemon | grep '\[Gate\]'        # memory + scope probabilities
docker compose logs router | grep requires_reasoning
```

Every decision is logged with its probability, so after a day of normal use you
have a free labelled set: find the decisions you disagreed with and redo step 2
with those included.

### Step 5 — fine-tune, if calibration is not enough

Rewording and thresholds only reshape what the checkpoint already knows. To
actually change its judgement, train it on your labels:

```bash
docker compose exec -it daemon labeler     # review turns, mark the good ones
docker compose exec daemon export          # writes data/train.jsonl + valid.jsonl
```

Fine-tune the checkpoint on that data with your own pipeline, then point
`LAYA_MODEL_REPO` at the result.

### Known characteristics of the current defaults

- `LAYA_ROUTING_THRESHOLD=0.38` — calibrated over twelve labelled queries,
  12/12 correct. The old 0.5 with the old question routed **nothing** to the
  heavy model.
- `LAYA_MEMORY_THRESHOLD=0.45` — calibrated as a pair with the current
  wording (see step 3 and `eval_memory_gate.py`): on the labelled set the
  concrete phrasing separates decisions/corrections from chatter at ~0.45,
  11/12 correct, where the old abstract wording at 0.5 got 7/12 and dropped
  genuine corrections. The one tolerated miss is an acknowledgement, which
  the extraction layer vetoes downstream. The threshold and the question
  text must be tuned together — moving one without the other undoes the
  calibration.
- `LAYA_GLOBAL_THRESHOLD=0.9` — deliberately high. Separation between "project
  fact" and "user preference" was only ~0.04, and rewording the assistant's
  reply moved one case across the line. High means everything stays
  project-scoped, which is the safe failure; promote globals by hand.
- `LEMN_RETRIEVAL_THRESHOLD=0.45` — not Laya, but calibrate it the same way.
  Embedder-specific: with `bge-m3` a question matching its own answer scores
  ~0.5 and unrelated text 0.3–0.4. Swap embedders and re-measure, or retrieval
  silently returns nothing.
- `LEMN_RETRIEVAL_FALLBACK_MARGIN=0.10` — the near-miss safety net. When no
  memory clears the threshold, the top memories are still injected if the best
  is within this margin below it (best ≥ threshold − margin). This is what lets
  a broad "what do you know about this project?" (best 0.446) return something
  instead of `[]`. It is a *ranking* floor, not a relevance claim: the injected
  memories are flagged `below_threshold` and the prompt tells the model to treat
  them as weaker context. Raise it to inject more aggressively (1.0 = always),
  lower it toward 0 to require a real match.

## End-to-end Memory Benchmark

`scripts/memory_bench.py` creates a scratch Postgres database from `schema.sql`
and starts a one-off daemon pointed at it. It creates a fresh, uniquely named
project scope for each synthetic project in `scripts/memory_bench_corpus.json`,
embeds and inserts initial fixture memories, then compares no memory, that
project's fixed `AGENTS.md`, and real `/retrieve` results from the isolated
daemon. It first asks baseline questions, then simulates learning an updated
daemon. Session names and order come from the corpus `session_order`; scheduled
updates are applied between sessions by marking the old memory `SUPERSEDED` and
inserting a new `AUTHORITATIVE` value. The current replay has five sessions and
six alpha updates: retry limits change 2 to 4 to 5, then a correction says 5 was
only for load tests and production is 3; a migration progresses from DNS
verification, to canary, to a certificate-expiry pause, then renewal and resume.
Alpha's `AGENTS.md` remains static at its initial guidance; beta is an unchanged
control project. Production project and global memories are neither read nor
modified.

The corpus is deterministic retrieval test data inserted directly as memory
rows. The staged update exercises LEMN's persisted state transition and retrieval
of the new fact, but it does **not** test whether the conversation extractor
would notice a changed fact or automatically promote it. The write-path evaluator
below exercises that separately. Per-project AGENTS fixtures are real files under
`scripts/memory_bench_projects/`; the harness reads them and never edits them.

1. Review `scripts/memory_bench_tasks.jsonl`, `scripts/memory_bench_corpus.json`,
   and the fixture `AGENTS.md` files. Keep task rubrics fixed before seeing
   answers. Tasks use a session name in `phase` and a `project_key`; the corpus
   defines session order and each project's updates after a named session.
2. Ensure Docker Compose Postgres, the daemon, the embedding endpoint, and the
  direct model endpoint are running. Use the direct model endpoint, not the
  LEMN router, to hold the model constant.
3. Load `LEMN_SHARED_SECRET` and, if required, `LEMN_BACKEND_API_KEY` from
  `.env`. Validate without making requests or database changes:

  ```bash
  set -a; source .env; set +a
  python3 scripts/memory_bench.py --validate-only
  ```

4. Run randomized paired queries within each session. Sessions execute in the
  corpus order, with scheduled updates applied between them. Each run prints
  its temporary project IDs, inserts only rows stamped with that run's unique
  marker, and deletes those rows in a `finally` cleanup:

  ```bash
  python3 scripts/memory_bench.py --model 4-bit \
    --completions-url http://localhost:9001/v1/chat/completions \
    --embeddings-url http://localhost:9001/v1/embeddings \
    --out /tmp/lemn-memory-bench --repeats 2
  ```

    To focus on the retry correction and migration interruption/recovery, pass
    `--task-ids alpha-retry-session-3,alpha-retry-corrected-session-4,alpha-retry-session-5,migration-interrupted-session-3,migration-still-paused-session-4,migration-resumed-session-5`.
5. Score `blind-results.jsonl` before opening `condition-key.jsonl`. Use each
  task ID and rubric to fill `accuracy_0_or_1`, `factuality_0_to_2`,
  `usefulness_0_to_2`, and notes in `blind-scores.csv`. Then report:

  ```bash
  python3 scripts/memory_bench.py --report /tmp/lemn-memory-bench
  ```

The benchmark stops its one-off daemon and drops the entire scratch database on
normal completion or handled errors. A forced process kill or machine shutdown
can bypass cleanup; leftover databases are named `lemn_bench_<run-id>` and do
not affect production. Check `condition-key.jsonl` for the run ID and drop only
that scratch database if needed. Check the isolated daemon logs when a retrieval
result is empty, since embedding failures are currently returned as an empty
array. This benchmark does not measure tool-using coding tasks or extraction
quality.

### Latest Three-Arm Run

On 2026-10-03, a five-session replay ran 15 tasks under two independent
within-session shuffle seeds (90 responses total), with Qwen 3.8 fixed across
all arms and a separate scratch database/daemon for each run. All arms scored
**15/15 against their arm-specific rubric** in both seeds. LEMN tracked the
retry policy through 2, 4, 5, and the correction to 3 production retries; it
also retained the migration's next action through canary pause, certificate
renewal, and resume. LEMN scored 2.00/2 mean factuality against current state,
versus 1.07 for static `AGENTS.md` (which intentionally retains stale guidance)
and 1.20 for no-memory (which mostly abstained). Mean prompt usage was 291
tokens for LEMN, 272 for AGENTS, and 231 for no memory; mean response times were
6.3s, 6.7s, and 7.5s respectively. LEMN returned context on all 15 tasks.

Arm-rubric accuracy is conditional: AGENTS is judged against its static file,
and no-memory receives credit for abstaining rather than inventing facts. The
factuality score is the indicator of whether answers matched the latest replay
state. This remains one small synthetic scenario repeated under two task-order
seeds, not a real-work benchmark or evidence of statistical superiority. All
updates, including the correction and interruption/recovery, were applied by the
fixture; this does not test conversational extraction, real tool evidence, or
whether the system should infer that work has been interrupted.

### Write-path evaluation

`scripts/memory_write_bench.py` runs labeled turns through the real `/log` worker
path: Laya worthiness gate, extractor, cosine evidence check, relation handling,
and Postgres routing. Every case/repeat gets its own temporary Postgres database
initialized from `schema.sql`, plus a one-off daemon with a temporary SQLite
file. This isolates even `global`-scope writes from production and from other
benchmark cases. The runner stops the daemon and drops the database afterward.
`/jobs/{id}` is an authenticated read-only endpoint used to wait for a durable
job and inspect its extraction result; it never returns the submitted turn
payload.

The labeled cases in `scripts/memory_write_bench_cases.jsonl` cover durable
decisions and preferences, explicit corrections, tool-backed changes, simple
and tool-backed lookups, acknowledgements, recaps, tentative ideas, and
one-answer-only instructions. `--repeats N` runs every case N times in fresh
scopes and reseeds any starting memory for each trial, so one extraction cannot
affect the next. Each repeat starts a fresh daemon/database pair, so repeated
runs take longer. Validate without starting services, then run from the
repository root with Docker Compose, Postgres, Laya, and the model/embedding
endpoint available:

```bash
set -a; source .env; set +a
python3 scripts/memory_write_bench.py --validate-only
python3 scripts/memory_write_bench.py --repeats 3 \
  --out /tmp/lemn-memory-write-bench
```

Use `--case-ids durable-user-preference,one-answer-style-request --repeats 3`
to rerun only preference capture and transient-instruction rejection.

The report separates **gate** precision/recall from **final memory**
precision/recall, and includes extractor vetoes, summary term checks, global
scope checks, relation checks, and descriptive evidence/target cosine
distributions. `saved_scope_passed` and `saved_scope_checked` count only cases
that created an actual memory row; the general scope counters also include
cases rejected earlier by the gate or extractor. It writes detailed per-case
extraction and created-memory provenance to `write-results.json`. These are
hand-labeled examples, so treat the scores as diagnostics rather than a
calibrated quality estimate.

The latest contamination-free run on 2026-10-03 scored 20 cases three times
each (60 turns). **Each trial used its own newly created Postgres database and
one-off daemon**, so even `global`-scope preference memories could not interact
with other fixtures or production. The production global-memory count remained
18, and all scratch databases were dropped afterward.

Results: the worthiness gate passed all 33 expected positives (recall `1.00`)
but also passed 15/27 negatives (precision `0.688`). Extraction saved all 33
positives and rejected all 27 negatives (final precision/recall `1.00` on these
labels); it vetoed 9 gate false positives as `none` and 6 temporary instructions
as `transient_instruction`. All 33 expected global/project-scope decisions
matched, all 27 relation labels matched, and repeat decisions were identical
for gate, final save, and scope. Exact summary text consistency was `0.917`.

The raw Laya global-scope classifier still scored every tested durable
cross-project preference below its `0.90` cutoff; wording variants did not
separate the labeled positives and negatives at that threshold. In the expanded
16-turn raw evaluator, the deployed and candidate prompts each scored 7/16;
none justified changing the threshold. The explicit cue override recovered
durable preferences expressed with "whatever repo", "regardless of repo",
"any repo", and "all my future projects". Treat this as a narrow, explicitly
worded path, not proof that arbitrary preference phrasing is handled.

A balanced real-write-path follow-up ran those four paraphrases, a saved
project-local preference, two durable cross-project technical decisions, and a
transient instruction, three fresh-scope trials each. All 21 expected durable
memories were saved, all 3 temporary instructions were vetoed, and all 21
persisted global/project scope labels matched. Decisions were identical across
repeats. This is still a small synthetic set; the raw classifier missed the
durable preferences, so the observed improvement is from the explicit cue
override rather than Laya calibration.

Cosine thresholds were unchanged. Across this run, evidence cosine was
`0.784`–`0.861` in 9 observations and target similarity was `0.880`–`0.908` in 6
observations. These counts are still too small and too synthetic to calibrate
either cutoff. The suite exercises extraction and routing on labeled turns but
is not a representative traffic sample or a statistical quality guarantee.

After isolating every trial in its own scratch database, the full 20-case x
three-repeat run on 2026-10-02 scored 60 turns: gate precision `0.688`, recall
`1.00`; final memory precision and recall were both `1.00`. All 33 labeled
positive turns were saved, all 27 negative turns were rejected or vetoed, all
27 expected relation decisions matched, and all 33 expected global/project
scope decisions matched. The gate still admitted 15 negative turns; the
extractor vetoed 9 as `none` and the transient-instruction guard vetoed 6. The
global cue override fired on 15 turns. Repeat decisions were identical for
gate, save/no-save, and scope on every case; exact summary text consistency was
`0.917`. Cosine thresholds remain unchanged: tool evidence ranged `0.784`–`0.861`
and target similarity `0.880`–`0.908` in 9 and 6 observed comparisons,
respectively.

This run used one disposable database per case/repeat, so explicit `global`
memories could not interact across fixtures or with production. After cleanup,
the production database still had 18 global memories and there were zero
`lemn_writebench_*` databases remaining. These are still synthetic, hand-labeled
cases; the repeated consistency result measures these exact examples, not
generalization to natural traffic.

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

Scope is chosen automatically. `/memory-worthiness` answers a second typed question, `globally_applicable`, asking whether the turn describes a durable preference of the *user* that holds across every codebase, rather than a fact belonging to one repository. A narrow explicit-language rule also selects global scope when the user's own message combines durable-preference language (`prefer`, `preference`, or `for future`) with an unambiguous cross-project phrase (`across all projects`, `across my projects`, or `in every project`). One-answer/task-only phrases block that override. The raw Laya probability is retained for measurement. Otherwise, the Laya result selects `global` or the caller's `project_id`. Tune the cutoff with `LAYA_GLOBAL_THRESHOLD`, and watch the daemon's `[Gate]` log lines to see both probabilities per turn.

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
| `./bin/lemn pending` | Lists `OBSERVED`, legacy `CANDIDATE`, pending, and `NEEDS_REVALIDATION` memories with proposed relations and dependencies |
| `./bin/lemn confirm <id> [options]` | Promotes a reviewed memory; supports `--relation`, `--target`, `--depends-on id,id`, and `--clear-dependencies` to correct proposals before atomic validation and application |
| `./bin/lemn revalidate [--evidence-quote passage --evidence-source https_url \| --user-confirmed] [--summary text] [--depends-on id,id \| --clear-dependencies] <id>` | Restores a quarantined dependent only after fetching an allowlisted HTTPS source and finding the quoted passage, or explicit confirmation; records source/body and quote hashes, verifies dependencies, and can revise the claim or dependency set |
| `./bin/lemn reject <id>` | Marks a reviewable memory `REJECTED`, leaving current authoritative memories in place |
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
| `LEMN_BACKEND_IDLE_TIMEOUT` | `25m` | router |
| `LEMN_LONG_CONTEXT_TOKENS` | `30000` | router |
| `LEMN_EXTRACTION_URL` | `http://localhost:8000/v1/chat/completions` | daemon |
| `LEMN_EXTRACTION_MODEL` | `qwen2.5-coder` | daemon |
| `LEMN_EMBEDDING_URL` | `http://localhost:8001/v1/embeddings` | daemon |
| `LEMN_EMBEDDING_MODEL` | `bge-m3` | daemon |
| `LEMN_RETRIEVAL_THRESHOLD` | `0.45` | daemon |
| `LEMN_RETRIEVAL_FALLBACK_MARGIN` | `0.10` | daemon |
| `LEMN_RETRIEVAL_LIMIT` | `5` | Pi extension |
| `LEMN_SUPERSEDE_THRESHOLD` | `0.82` | daemon |
| `LEMN_PROMOTE_THRESHOLD` | `0.9` | daemon |
| `LEMN_AUTO_SUPERSEDE_THRESHOLD` | `0.95` | daemon |
| `LEMN_REVALIDATION_MAX_EVIDENCE_AGE` | `720h` | daemon / `cmd/lemn` |
| `LEMN_REVALIDATION_ALLOWED_HOSTS` | empty (source verification disabled) | daemon / `cmd/lemn` |
| `LEMN_LAYA_TIMEOUT` | `60s` | daemon |
| `LEMN_EXTRACTION_TIMEOUT` | `120s` | daemon |
| `LEMN_SQLITE_PATH` | `./lemn_data.db` | daemon, labeler, export |
| `LEMN_PROJECT_ID` | git root name | Pi extension |
| `LEMN_DAEMON_URL` | `http://localhost:8080` | Pi extension |
| `LAYA_MODEL_REPO` | `convaiinnovations/laya-typed-decisions` | Laya service |
| `LAYA_ROUTING_THRESHOLD` | `0.38` | Laya service |
| `LAYA_MEMORY_THRESHOLD` | `0.45` | Laya service |
| `LAYA_GLOBAL_THRESHOLD` | `0.90` | Laya service |
| `LAYA_CORRECTION_THRESHOLD` | `0.5` | Laya service |
| `LEMN_WORKER_COUNT` | `2` | daemon |
| `LEMN_POSTGRES_DSN` | required for kernel/admin commands | daemon, `cmd/lemn` |
| `USE_TF` | `0` | Laya service |

If you change `LEMN_EMBEDDING_MODEL`, update the `vector(1024)` column width in `schema.sql` to match the new model's output dimension — Postgres rejects inserts of the wrong width.

## Ports

| Port | Service |
|---|---|
| 8002 | Laya classifier service |
| 8080 | LEMN daemon (`/log`, `/jobs/{id}`, `/retrieve`) |
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
- Pi does not have its own memory admin commands; use `lemn pending`, `confirm`, `revalidate`, and `reject`.
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
| `/retrieve` returns `[]` for a question that should match | The best match fell below `threshold − LEMN_RETRIEVAL_FALLBACK_MARGIN` (genuinely unrelated), or nothing is `AUTHORITATIVE` yet, or the embedder is down. Raise `LEMN_RETRIEVAL_FALLBACK_MARGIN` to catch near misses; check the daemon log for `[Retrieval Error]` to rule out the embedder |
| Memories never appear at all | Check `docker compose logs daemon` for `[Gate]` lines. No line means the job failed before the gate; the model server is the usual cause |
| `404` from the backend | A `*_MODEL_NAME` does not match an id from `/v1/models` |
| First turn takes a minute, later ones are fast | Normal prompt prefill. Enable your model server's prompt cache |
| Config edits appear ignored | You ran `source .env` in that shell; exported variables override the `.env` file for Compose |

## License

Unless otherwise noted, LEMN's original source code and documentation are
licensed under the GNU Affero General Public License, version 3 only
(AGPL-3.0-only); see [LICENSE](LICENSE). AGPL permits commercial use and does
not require payment merely because a project earns money. Third-party
dependencies, model checkpoints, container images, and external services are
governed by their own terms.

