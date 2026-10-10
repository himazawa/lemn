# Operations

This guide explains how to set up and operate the experimental prototype.

## Requirements

- Docker Compose (Docker Desktop, Rancher Desktop, or equivalent)
- An OpenAI-compatible model server reachable from the containers
- Pi, if using the included agent integration
- Go 1.22+ and Python 3.10+ only when running services outside Docker

Configure four model roles: heavy chat, fast chat, extraction, and embeddings.
The fast model must reliably emit tool calls. Test it before relying on it. The
extraction model must return valid JSON. The embedding model must match the
vector size in `schema.sql` (currently 1024). Laya downloads its classifier
checkpoint the first time the service starts.

## Docker setup

1. Start an OpenAI-compatible model server and verify its model IDs:

   ```bash
   curl -s http://localhost:8000/v1/models | jq -r '.data[].id'
   ```

  The README examples use port `8000`. Despite its name, `OMLX_URL` can point
  to any compatible model server. Containers use `host.docker.internal` to
  reach the host. Check that this works with your Docker platform and server
  binding. Do not expose an unauthenticated model server to an untrusted
  network.

2. Copy the template and edit required secrets and model IDs:

   ```bash
   cp .env.example .env
   chmod 600 .env
   ```

  Generate `LEMN_SHARED_SECRET` with `openssl rand -hex 32` and
  `POSTGRES_PASSWORD` with `openssl rand -hex 16`. The shared secret lets Pi
  and services authenticate to LEMN. `LEMN_BACKEND_API_KEY` is a separate,
  optional key for your model server. Quote values that contain spaces.

   If the embedding model is not 1024-dimensional, change the `vector(1024)` width in `schema.sql` before first startup. PostgreSQL rejects vectors with the wrong width.

3. Start the services and wait for health checks:

   ```bash
   docker compose up -d --build
   docker compose ps
   docker compose logs -f laya
   ```

   The first Laya startup downloads the classifier checkpoint and may take several minutes.

4. Check service health and classifier responses:

   ```bash
   SECRET=$(grep '^LEMN_SHARED_SECRET=' .env | cut -d= -f2)
   curl -s localhost:8080/healthz
   curl -s localhost:8090/healthz
   curl -s localhost:8002/healthz
   curl -s localhost:8002/classify \
     -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
     -d '{"query":"rename this variable"}'
   ```

   Do not `source .env` before running Compose: exported shell variables override values loaded from `.env`.

5. Install the Pi extension and configure its model provider. Example provider entry for `~/.pi/agent/models.json`:

   ```json
   {
     "providers": {
       "lemn": {
         "baseUrl": "http://localhost:8090/v1",
         "api": "openai-completions",
         "apiKey": "<LEMN_SHARED_SECRET>",
         "compat": { "supportsDeveloperRole": false, "supportsReasoningEffort": false },
         "models": [{ "id": "lemn", "name": "LEMN (routed)", "contextWindow": 131072, "maxTokens": 32768 }]
       }
     }
   }
   ```

  This profile allows a 128k context window and up to 32k generated tokens.
  The backend must support those limits; changing Pi does not resize the model.
  Restart or reload Pi after changing its model profile or installed extension.
  The router requests streamed usage and reports a token-budget stop as `length`
  when the backend supplies completion-token counts. Without usage it cannot
  reliably distinguish truncation from a normal stop. Memory retrieval has a
  five-second timeout so the optional memory hook cannot wait indefinitely.
  Set `contextWindow` to the heavy model's actual context window. Set
  `LEMN_LONG_CONTEXT_TOKENS` below the fast model's context window minus
  `maxTokens`.

   Pi loads TypeScript directly. Put both extension files in a subdirectory because Pi treats every top-level `.ts` file as an extension entry point:

   ```bash
   mkdir -p .pi/extensions/lemn
   cp pi-extension/index.ts pi-extension/retrieval.ts .pi/extensions/lemn/
   export LEMN_SHARED_SECRET=$(grep '^LEMN_SHARED_SECRET=' .env | cut -d= -f2)
   pi -e ./.pi/extensions/lemn/index.ts --model lemn
   ```

  `.pi/extensions/` installs the extension for this project. Use
  `~/.pi/agent/extensions/` to install it for your user. Pi runs on the host
  and does not load `.env` automatically. Set `LEMN_SHARED_SECRET` in Pi's
  environment. Set `LEMN_DAEMON_URL`, `LEMN_PROJECT_ID`, or
  `LEMN_RETRIEVAL_LIMIT` there too if you want to override their defaults.
  For scripted Pi runs, `--no-extensions` disables automatic discovery of
  other extensions. Explicit `-e` paths still load.

6. Review memories and lifecycle:

   ```bash
   docker compose exec daemon lemn pending
   docker compose exec daemon lemn confirm <id>
   docker compose exec daemon lemn reject <id>
   ```

   Inspect proposed relations and dependencies before confirming. `confirm` supports `--relation`, `--target`, `--depends-on`, and `--clear-dependencies`. See the [architecture guide](architecture.md#lifecycle-and-graph-safety) for state and revalidation semantics.

To stop without deleting data, run `docker compose stop`. Resume with `docker compose up -d`. `docker compose down -v` deletes the database and queue volumes, including stored memories.

## Model tool-call check

A fast model that narrates tool calls instead of emitting them can make Pi appear hung. Check both candidate chat models against your model server before configuring them:

```bash
K=your-model-server-key
for M in "<fast-id>" "<heavy-id>"; do
  printf '%-24s ' "$M"
  curl -s http://localhost:8000/v1/chat/completions \
    -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$M\",\"messages\":[{\"role\":\"user\",\"content\":\"What is in this folder?\"}],\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"list\",\"description\":\"List files\",\"parameters\":{\"type\":\"object\",\"properties\":{\"path\":{\"type\":\"string\"}},\"required\":[\"path\"]}}}]}" \
    | python3 -c "import sys,json;m=json.load(sys.stdin)['choices'][0]['message'];print('TOOL CALL' if m.get('tool_calls') else 'NARRATED ONLY')"
done
```

## Source-backed revalidation

Set `LEMN_REVALIDATION_ALLOWED_HOSTS` to a comma-separated allowlist of exact HTTPS host names, for example `docs.example.test,git.example.test`. An empty list disables source fetching; explicit user confirmation remains available.

```bash
docker compose exec daemon lemn revalidate \
  --evidence-quote "The API uses HTTP/2 for requests." \
  --evidence-source "https://docs.example.test/api" <id>
```

When running `./bin/lemn` on the host, export the same allowlist and `LEMN_POSTGRES_DSN` in that shell. Revalidation checks source reachability and quote presence; it does not prove that the source is correct or supports the memory.

## Running without Docker

This starts the services manually. It does not install their dependencies.
Provide a reachable PostgreSQL database, model and embedding endpoints, and
environment settings for each service. Without `LEMN_POSTGRES_DSN`, the daemon
only logs turns. The memory kernel stays disabled.

```bash
export LEMN_SHARED_SECRET=$(openssl rand -hex 32)
export LEMN_POSTGRES_DSN='postgres://lemn:<password>@localhost:5432/lemn_kernel?sslmode=disable'
export LEMN_LAYA_URL=http://127.0.0.1:8002/classify
export LEMN_LAYA_MEMORY_URL=http://127.0.0.1:8002/memory-worthiness
export LEMN_EXTRACTION_URL=http://localhost:8000/v1/chat/completions
export LEMN_EMBEDDING_URL=http://localhost:8001/v1/embeddings
export LEMN_FAST_MODEL_URL=http://localhost:8010/v1/chat/completions
export LEMN_HEAVY_MODEL_URL=http://localhost:8011/v1/chat/completions
export LEMN_FAST_MODEL_NAME='<fast-model-id>'
export LEMN_HEAVY_MODEL_NAME='<heavy-model-id>'
export LEMN_EXTRACTION_MODEL='<extraction-model-id>'
export LEMN_EMBEDDING_MODEL='<embedding-model-id>'
```

Start PostgreSQL and the model endpoints first. In one terminal, build the Go
services from the repository root:

```bash
mkdir -p lemnd/bin
cd lemnd
go build -o bin/daemon ./cmd/daemon
go build -o bin/router ./cmd/router
go build -o bin/lemn ./cmd/lemn
go build -o bin/labeler ./cmd/labeler
go build -o bin/export ./cmd/export
```

In another terminal, install and start Laya:

```bash
cd layarouter
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
USE_TF=0 uvicorn server:app --host 127.0.0.1 --port 8002
```

In separate terminals, start the router and daemon from `lemnd/`:

```bash
./bin/router
```

```bash
./bin/daemon
```

Export the environment settings in each terminal before starting the Go
services. Without `LEMN_POSTGRES_DSN`, the daemon only logs turns and does not
write or retrieve memories. `docker-compose.yml` shows the full container
configuration.

## Scope configuration

Pi uses the Git root's directory name as the default `project_id`. Outside a
Git repository, it uses the current directory name. Repositories with the same
name can share memories by mistake. Set a unique `LEMN_PROJECT_ID` in Pi's
environment to avoid that. The `global` scope is visible from every project;
use it for durable cross-project preferences.

## Database upgrade

For a database created before project scoping was added, back it up first, then
apply:

```sql
ALTER TABLE lemn_memories ADD COLUMN project_id TEXT NOT NULL DEFAULT 'global';
CREATE INDEX lemn_memories_scope_idx ON lemn_memories (project_id, state);
```

Existing rows become global and remain visible from every project. Re-scope
individual rows if that is not intended.

## Admin commands

| Command | Purpose |
|---|---|
| `lemn pending` | List reviewable memories and proposed relations/dependencies |
| `lemn confirm <id> [options]` | Confirm a memory; supports relation and dependency overrides |
| `lemn revalidate <id> [options]` | Restore a quarantined memory with explicit user confirmation or source-backed evidence |
| `lemn reject <id> [--reason text]` | Reject a reviewable memory, or retract an authoritative memory with a required reason; dependent memories are quarantined transactionally |
| `lemn labeler` | Review unlabelled turns from the SQLite queue |
| `lemn export` | Export reviewed turns for training |

Outside Docker, commands need `LEMN_POSTGRES_DSN`. There is no `unsupersede`
command. To undo a mistaken supersession, repair the database carefully. Back
it up and inspect the related edges before changing any state.

For a bad auto-promoted memory, use `lemn reject 211 --reason "Copied an existing
preference rather than learning a new fact"` instead of directly updating SQL.
This preserves provenance and quarantines dependent authoritative claims.
`lemn pending` lists possible duplicates for explicit review; do not confirm a
copy unless its entity, value, polarity, and scope genuinely differ.

### Blocked review queue

`lemn pending` displays creation time and age, with `NEEDS ATTENTION` for
memories older than seven days. Future timestamps have their displayed age
clamped to zero. Age is an escalation signal only: memories are never
automatically deleted because they have been waiting for review.

Dependency details include both stored `depends_on` edges and proposed
dependencies in provenance. `BLOCKED` identifies invalid or missing dependency
IDs, self or proposed relation-target dependencies, non-authoritative states,
and invisible scopes. A project memory may depend on its own project or
`global`; a global memory may depend only on `global`. When present,
`model_equivalent` and `duplicate_review_required` are displayed alongside
duplicate candidates and review provenance. These details are a review-time
snapshot, not a guarantee that confirmation will succeed.

`lemn sweep` remains fail-closed: invalid dependencies prevent promotion, and
errors are reported with a nonzero exit status. The sweep does not repair or
silently discard dependencies. Without `--apply` it is a dry run; applying
earlier actions may invalidate targets used by later actions.

After inspecting the claim, source, scope, and duplicate candidates, explicitly
replace or clear incorrect dependencies, or reject the claim with a reason:

```bash
lemn confirm <id> --depends-on <valid-id>,<valid-id>
lemn confirm <id> --clear-dependencies
lemn reject <id> --reason "Unsupported claim or invalid dependency"
```

Clear dependencies only when the claim is genuinely independent. Memories in
`NEEDS_REVALIDATION` still require `lemn revalidate` with evidence or explicit
user confirmation; `confirm` does not bypass quarantine. No lifecycle or sweep
rules are changed by the visibility labels.

## Configuration reference

Override Compose defaults in `.env`. Some Go defaults differ when you run the
binaries outside Docker; those differences are noted below.

| Variable | Compose default | Used by |
|---|---:|---|
| `LEMN_SHARED_SECRET` | required | daemon, router, Laya, Pi |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | `lemn` / required / `lemn_kernel` | Postgres, daemon DSN |
| `LEMN_DAEMON_BIND` / `LEMN_ROUTER_BIND` | `0.0.0.0:8080` / `0.0.0.0:8090` in Compose; loopback standalone | daemon, router |
| `LEMN_LAYA_URL` / `LEMN_LAYA_MEMORY_URL` | internal Compose URLs | router, daemon |
| `OMLX_URL` | `http://host.docker.internal:8000` | default model/embedding endpoints |
| `LEMN_FAST_MODEL_URL` / `LEMN_HEAVY_MODEL_URL` | derived from `OMLX_URL` | router |
| `LEMN_EXTRACTION_URL` / `LEMN_EMBEDDING_URL` | derived from `OMLX_URL` | daemon |
| `LEMN_FAST_MODEL_NAME` / `LEMN_HEAVY_MODEL_NAME` | unset | router |
| `LEMN_EXTRACTION_MODEL` / `LEMN_EMBEDDING_MODEL` | `qwen2.5-coder` / `bge-m3` | daemon |
| `LEMN_BACKEND_API_KEY` | empty | router, daemon |
| `LEMN_WORKER_COUNT` | `2` | daemon |
| `LEMN_CLASSIFY_TIMEOUT` / `LEMN_BACKEND_IDLE_TIMEOUT` | `5s` / `25m` | router |
| `LEMN_LONG_CONTEXT_TOKENS` | `30000` | router |
| `LEMN_LAYA_TIMEOUT` / `LEMN_EXTRACTION_TIMEOUT` | `60s` / `120s` | daemon |
| `LEMN_RETRIEVAL_THRESHOLD` / `LEMN_RETRIEVAL_FALLBACK_MARGIN` | `0.45` / `0.10` | daemon |
| `LEMN_SUPERSEDE_THRESHOLD` / `LEMN_PROMOTE_THRESHOLD` | `0.82` / `0.9` | daemon |
| `LEMN_ALLOW_CONFIDENCE_PROMOTION` | `false` (unbacked claims require review) | daemon |
| `LEMN_AUTO_SUPERSEDE_THRESHOLD` | `0.95` | daemon |
| `LEMN_REVALIDATION_MAX_EVIDENCE_AGE` | `720h` | daemon, CLI |
| `LEMN_REVALIDATION_ALLOWED_HOSTS` | empty | daemon, CLI |
| `LEMN_SQLITE_PATH` | `/data/lemn_data.db` | daemon queue in Compose |
| `LEMN_RETRIEVAL_LIMIT` | `5` | Pi environment (not Compose) |
| `LEMN_DAEMON_URL` | `http://localhost:8080` | Pi environment (not Compose) |
| `LEMN_PROJECT_ID` | Git root basename | Pi environment (not Compose) |
| `LAYA_MODEL_REPO` | `convaiinnovations/laya-typed-decisions` | Laya |
| `LAYA_ROUTING_THRESHOLD` / `LAYA_MEMORY_THRESHOLD` | `0.38` / `0.45` | Laya |
| `LAYA_GLOBAL_THRESHOLD` / `LAYA_CORRECTION_THRESHOLD` | `0.90` / `0.5` | Laya |
| `ROUTER_PORT` / `DAEMON_PORT` / `LAYA_PORT` | `8090` / `8080` / `8002` | host port mappings |
| `USE_TF` | `0` | Laya |

Standalone model endpoint defaults differ from Compose: router fast/heavy use
ports `8010`/`8011`; extraction and embedding use `8000`/`8001`. See
`docker-compose.yml` and the Go/Python environment readers for all endpoint
options. Standalone daemon and router bind to loopback by default.

## Ports and exposure

Compose publishes router `8090`, daemon `8080`, and Laya `8002`. Docker publishes mapped ports on host interfaces unless an explicit host address such as `127.0.0.1:` is added to the mapping. Endpoints other than health checks require the shared secret. Restrict exposure with host firewall rules or loopback-only port mappings when access should stay local.

The model server is separate. In a one-port/many-model setup, set all endpoint URLs to the same server and set `LEMN_FAST_MODEL_NAME` and `LEMN_HEAVY_MODEL_NAME`; otherwise the router cannot select distinct models. In a per-model setup, use each model's endpoint and leave model-name overrides unset.

## Troubleshooting

| Symptom | Check |
|---|---|
| Requests are slow and consistently use heavy | Check router logs for `failing open`; Laya may be unavailable or classification may exceed `LEMN_CLASSIFY_TIMEOUT`. |
| `/retrieve` returns `[]` unexpectedly | Check daemon `[Retrieval Error]` logs, authoritative state, similarity threshold, and fallback margin. |
| No memories appear | Check daemon `[Gate]` logs and model endpoint availability. |
| Backend returns 404 | Verify the model ID against the server's `/v1/models` response. |
| First turn is slow | Initial model prefill and checkpoint download are expected; enable model-server prompt caching if supported. |
| Compose ignores an edited `.env` value | Ensure it is not overridden by an exported shell variable. |

Laya runs CPU inference. Additional container CPU can reduce classifier latency, but benchmark the effect on your own machine.
