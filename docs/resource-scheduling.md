# Memory Resource Scheduling

## Admission

The router admits one model call at a time across `/v1/chat/completions`,
`/internal/extraction`, `/v1/embeddings`, and `/internal/embeddings`. Foreground
requests wait
with request cancellation support; background requests receive HTTP 503 with
`Retry-After: 30` when another call is active or foreground work is waiting.
The slot stays held until the response body/stream finishes or is cancelled.
No scheduler mutex is held while performing network I/O. Running work is not
foreground work is not preempted. If foreground work arrives during background
extraction or embedding, the router cancels that background request; its durable
job is returned to RETRY without consuming a content-failure attempt. A single
router instance must own this backend; direct oMLX calls,
other routers and independent Bili providers bypass admission.

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
30-60 seconds of jitter without consuming the content-failure attempt budget.
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
and vector space; no second model copy or re-embedding is needed.

Compose routes foreground retrieval through `/v1/embeddings` and worker-created
vectors through `/internal/embeddings`. Both use the shared admission slot with
chat and extraction. This prevents concurrent LEMN requests from stacking model
loads, but does not isolate oMLX from other clients or create more system memory.

The previous TEI CPU-container experiment was stopped and removed: BGE-M3's ONNX
artifacts returned repeated 404s. It was never activated. A separate CPU service
should be reconsidered only with a working artifact, measured Apple Silicon
performance, and vector parity against existing stored memories.

The native endpoint and one live retrieval request were observed to return HTTP
200; retrieval took 0.72 seconds. The returned memories were unrelated global
preferences below threshold, so DPM fact recall remains unverified. Retrieval has
a two-second daemon deadline and Pi a five-second read deadline; test real DPM
queries and stored summaries before claiming recall is fixed.

## Deployment

Run `go test -race ./...` and `node scripts/test_pi_memory_modes.mjs`, then build
the router/daemon images. Pause or finish active Pi requests before recreating
the router so streams are not interrupted:

```sh
docker compose up -d --build router daemon
```

Historical DPM recovery should follow successful live extraction and embedding
checks. Review the candidates in dpm-memory-audit.md rather than creating
authoritative memories from old reports without verification.