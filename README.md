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
            rewrites only the "model" field; every other field
            the client sent is forwarded untouched
                              │
                              ▼
                        model server
                              │
                              ▼
   answer + X-Lemn-Route / X-Lemn-Model / X-Lemn-Probability
                              │
                              ▼
                    Pi ──▶ you   (extension shows the route in the footer)
```

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
                           AND similarity > LEMN_RETRIEVAL_THRESHOLD
                         ORDER BY similarity DESC LIMIT 5
                                                 │
                                                 ▼
                   injected into the turn as a separate message
                   (Pi builds the prompt, so the user's own
                    text is never rewritten)
```

Reads span your project **plus** `global`. Writes never cross scopes.

On error this returns `[]` rather than failing, so an empty result can mean
"nothing matched", "nothing is AUTHORITATIVE yet", or "the embedder is down".
Check the daemon log for `[Retrieval Error]` before concluding the memory is
missing.

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
- `LAYA_MEMORY_THRESHOLD=0.5` — roughly separates decisions from chatter, but
  some genuine preferences score in the low 0.4s and are dropped.
- `LAYA_GLOBAL_THRESHOLD=0.9` — deliberately high. Separation between "project
  fact" and "user preference" was only ~0.04, and rewording the assistant's
  reply moved one case across the line. High means everything stays
  project-scoped, which is the safe failure; promote globals by hand.
- `LEMN_RETRIEVAL_THRESHOLD=0.45` — not Laya, but calibrate it the same way.
  Embedder-specific: with `bge-m3` a question matching its own answer scores
  ~0.5 and unrelated text 0.3–0.4. Swap embedders and re-measure, or retrieval
  silently returns nothing.

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
| `LAYA_ROUTING_THRESHOLD` | `0.38` | Laya service |
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

