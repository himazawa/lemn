# Memory Resource Scheduling

## Admission

The router admits one model call at a time across `/v1/chat/completions` and
the authenticated `/internal/extraction` endpoint. Foreground requests wait
with request cancellation support; background requests receive HTTP 503 with
`Retry-After: 30` when another call is active or foreground work is waiting.
The slot stays held until the response body/stream finishes or is cancelled.
No scheduler mutex is held while performing network I/O. Running work is not
preempted. A single router instance must own this backend; direct oMLX calls,
other routers and independent Bili providers bypass admission.

Compose sends daemon extraction and relation classification through the router.
`LEMN_EXTRACTION_URL` in the environment still selects the router's downstream
backend, not the daemon's broker URL. Multiple durable workers may remain enabled;
backend busy responses defer jobs rather than launching concurrent model loads.

## Durable Recovery

HTTP 429/502/503/504/507 and network/deadline failures return jobs to RETRY with
30-60 seconds of jitter without consuming the content-failure attempt budget.
This preserves jobs through extended outages; it does not promise eventual
completion if the backend never recovers. Other extraction failures retain the
existing bounded attempts. Old FAILED jobs are not replayed automatically.
Foreground demand can delay background work indefinitely; monitor the queue.

Pi shows enqueue failures and retrieval outages. `/lemn-job` checks the last
acknowledged job in the current Pi process, distinguishing waiting, processing,
failure, extractor rejection and completion. Completion is not a promise of
AUTHORITATIVE status. Restart Pi to load the updated extension.

## Isolated Embeddings

The optional CPU service uses TEI with BAAI/bge-m3 and CLS pooling. It does not
share oMLX's Metal cache. On Apple Silicon this amd64 image is emulated; benchmark
latency and VM memory before activating it. Model weights still consume system
RAM. The first startup downloads model weights.

Start without changing the active embedding endpoint:

```sh
docker compose -f docker-compose.yml -f docker-compose.embeddings.yml up -d embeddings
```

With backend credentials exported in your own terminal, validate compatibility:

```sh
cd lemnd
LEMN_PARITY_OLD_URL=http://127.0.0.1:9001/v1/embeddings \
LEMN_PARITY_OLD_MODEL=bge-m3-mlx-fp16 \
LEMN_PARITY_NEW_URL=http://127.0.0.1:8001/v1/embeddings \
go test ./internal/lemn -run TestEmbeddingBackendParity -v -count=1
```

The parity test requires matching dimensions and cosine >= 0.995 on four sample
texts. This is a smoke test, not full corpus validation: also compare isolated
vectors against existing stored summaries and benchmark question retrieval before
cutover. If parity fails, do not activate the new service; re-embedding existing
memories requires a separately reviewed migration.

After compatibility and latency checks pass, activate with all three files:

```sh
docker compose -f docker-compose.yml -f docker-compose.embeddings.yml \
  -f docker-compose.embeddings-active.yml up -d daemon
```

Keep using the same files on subsequent updates. Reverting to base Compose restores
the configured old endpoint. Never mix vector spaces silently. Retrieval currently
has a two-second service deadline and Pi a five-second read deadline; the isolated
service must meet that budget on the target machine or those deadlines need a
measured adjustment. LOG_LEVEL=warn suppresses TEI startup argument logging because
the upstream INFO log includes its API key. Rotate any key previously exposed.

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