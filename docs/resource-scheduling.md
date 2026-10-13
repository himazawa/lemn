# Memory Resource Scheduling

## Admission

The router admits one model call at a time across `/v1/chat/completions`,
`/internal/extraction`, `/v1/embeddings`, and `/internal/embeddings`. Foreground
requests wait
with request cancellation support; background requests receive HTTP 503 with
`Retry-After: 30` when another call is active or foreground work is waiting.
The slot stays held until the response body/stream finishes or is cancelled.
No scheduler mutex is held while performing network I/O. Running foreground
work is not preempted. If foreground work arrives during background
extraction or embedding, the router cancels that background request; its durable
job is returned to RETRY without consuming a content-failure attempt. A single
router instance must own this backend; direct oMLX calls,
other routers and proxies that call the backend directly bypass admission.

Compose sends daemon extraction, relation classification, and embeddings through
the router. Pi retrieval queries use foreground `/v1/embeddings`; durable worker
embedding calls use `/internal/embeddings` and yield to foreground traffic.
`LEMN_EXTRACTION_URL` in the environment still selects the router's downstream
backend, not the daemon's broker URL. Multiple durable workers may remain enabled;
backend busy responses defer jobs rather than launching concurrent model loads.
The daemon authenticates to the broker with `LEMN_SHARED_SECRET`; the router
authenticates to oMLX with `LEMN_BACKEND_API_KEY`.

## Durable Recovery

HTTP 429/502/503/504/507, cancellation, and network/deadline failures return jobs to RETRY with
exponential backoff (30s doubling to 8m, plus up to 30s jitter) without consuming the content-failure attempt budget.
This preserves jobs through extended outages; it does not promise eventual
completion if the backend never recovers. Other extraction failures retain the
existing bounded attempts. Old FAILED jobs are not replayed automatically.
Foreground demand can delay background work indefinitely; monitor the queue.

Pi shows enqueue failures and retrieval outages. `/lemn-job` checks the last
acknowledged job in the current Pi process, distinguishing waiting, processing,
failure, extractor rejection and completion. Completion is not a promise of
AUTHORITATIVE status. Restart Pi to load the updated extension.

An explicit user message beginning with `remember` bypasses the Laya worthiness
and extraction-model calls and is stored as a user-declared claim. It is not
independent verification. Similar authoritative candidates still trigger the
existing duplicate review; backend embedding/database failures keep the job
recoverable. Acknowledged enqueue means queued, not that this processing finished.

## Native Embeddings

The existing native oMLX server serves `bge-m3-mlx-fp16` (XLM-RoBERTa, BGE-M3
CLS pooling and L2 normalization, 1024 dimensions). LEMN retains this exact model
and vector space; no second model copy or re-embedding is needed. A different
model requires measured retrieval parity and, if incompatible, an explicit
re-embedding migration; matching dimensions alone are not enough.

Compose routes foreground retrieval through `/v1/embeddings` and worker-created
vectors through `/internal/embeddings`. Both use the shared admission slot with
chat and extraction. This prevents concurrent LEMN requests from stacking model
loads, but does not isolate oMLX from other clients or create more system memory.
A separate CPU embedding service needs measured Apple Silicon performance and
vector parity against existing stored memories before it replaces this endpoint.

Retrieval has a ten-second daemon deadline (`LEMN_RETRIEVAL_TIMEOUT`) and Pi a
five-second read deadline.

## Deployment

Run `go test -race ./...` and `node scripts/test_pi_memory_modes.mjs`, then build
the router/daemon images. Pause or finish active Pi requests before recreating
the router so streams are not interrupted:

```sh
docker compose up -d --build router daemon
```