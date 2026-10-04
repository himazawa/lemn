# Architecture

LEMN is an experimental, local-first memory service for coding agents, with an
optional model router. This guide describes how it works. It does not claim
that LEMN has proven long-term benefits.

## Components

| Component | Purpose |
|---|---|
| `lemnd/cmd/router` | OpenAI-compatible proxy that selects a fast or heavy chat model |
| `lemnd/cmd/daemon` | Durable turn intake, background extraction, memory writes and retrieval |
| `lemnd/cmd/lemn` | CLI for reviewing, confirming, rejecting and revalidating memories |
| `lemnd/cmd/labeler`, `cmd/export` | Label turns and export training data |
| `layarouter/server.py` | Laya typed-decision service |
| `pi-extension/` | Pi integration for memory retrieval and turn logging |
| PostgreSQL + pgvector | Memory rows and relationship edges |
| SQLite | Durable background-job queue |

## Chat routing

```text
Pi -> Router -> Laya classification -> fast or heavy model -> Pi
                    |                    ^
                    +-- failure/timeout --+ (router selects heavy)

Oversized prompts go directly to the heavy model without classification.
```

The router rewrites the `model` field only when a model name is configured; it otherwise forwards the request body. It returns route metadata in response headers. If classification fails or times out, it fails open to the heavy model. The fast model must emit tool calls reliably; narrating a tool call can look like a hang to the user.

## Memory write path

Pi sends completed turns to the daemon. The daemon saves each turn to its
durable queue before it acknowledges the request. Background workers then
check whether the turn is worth saving, choose its scope, extract candidate
memories, check for tool evidence, and route the results. These steps do not
block the conversation. Later failures appear in the daemon logs.

```text
Pi -> daemon -> SQLite queue -> Laya gates -> extraction -> embedding
                                                  |
                                                  v
                                      scoped authoritative candidates
                                                  |
                         +------------------------+----------------------+
                         |                        |                      |
              independent, no evidence  independent, relevant evidence  destructive relation
                         |                        |                      |
                      OBSERVED             AUTHORITATIVE             review or auto-confirm
```

`LAYA_MEMORY_THRESHOLD` decides whether a turn should be considered for
extraction. A separate decision selects `global` or the turn's project scope.
The extractor can reject a turn or propose relations and dependencies. Tool
evidence means a tool-call diff has a high embedding similarity to the claim.
This is a supporting signal, not proof that the diff confirms the claim.

Non-correction independent claims can be promoted automatically. Evidence-backed
claims are promoted; unbacked `OBSERVED` claims are promoted when extraction
confidence meets `LEMN_PROMOTE_THRESHOLD`. Explicit corrections and destructive
relations (`supersedes` or `contradicts`) normally need review. They can be
auto-confirmed only when the target is valid, relevant tool evidence exists,
and confidence meets `LEMN_AUTO_SUPERSEDE_THRESHOLD`. Automatic confirmation
uses the same transaction as human confirmation. If a relation target is
invalid, LEMN treats the claim as independent and applies the normal promotion
rules.

## Memory read path

Before each turn, the Pi extension asks the daemon for memories that match the
query and project ID. The daemon embeds the query and ranks `AUTHORITATIVE`
memories from that project and `global` scope. Go applies the similarity
threshold and near-miss fallback. If no result clears the threshold, but the
best result is within `LEMN_RETRIEVAL_FALLBACK_MARGIN`, LEMN returns the top
results with `below_threshold` set. Otherwise it returns an empty list. Pi adds
the memories as a separate message and leaves the user's text unchanged.

Retrieval failures return an empty result instead of failing the conversation.
An empty result may mean no close authoritative memories, or it may mean
retrieval failed. Check daemon logs for `[Retrieval Error]`.

## Lifecycle and graph safety

```text
OBSERVED / CANDIDATE / PENDING_CONFIRMATION
  | confirm                         | reject
  v                                 v
AUTHORITATIVE                     REJECTED
  |
  | superseded or contradicted by a confirmed memory
  v
SUPERSEDED / CONTRADICTED
  |
  +-- authoritative dependents -> NEEDS_REVALIDATION
                                      | revalidate or reject
                                      v
                             AUTHORITATIVE / REJECTED
```

Only `AUTHORITATIVE` memories are retrieved. Confirmation validates relation targets and dependencies and writes memory state and graph changes transactionally. Confirming a `supersedes` or `contradicts` relation recursively moves the target's authoritative dependents to `NEEDS_REVALIDATION`; ordinary confirmation and `sweep --apply` do not restore them.

Revalidation requires explicit user confirmation or a cited passage fetched from an allowlisted HTTPS host. Fetching rejects redirects outside the allowlist, private and special-use IP addresses, non-HTTPS URLs, credentials, query-bearing URLs, unsupported content types, oversized bodies, and oversized quotes. LEMN records hashes of the fetched body and cited passage and checks evidence age. A matching quote proves only that the passage appeared at the fetched source; it does not prove that the source is correct or supports the memory. Dependencies must remain authoritative or be replaced/cleared.

## Scope

Every memory has a `project_id`. Pi uses the Git root's directory name. If the
directory is not in a Git repository, Pi uses the current directory name.
Repositories with the same name can share a scope by mistake. Set
`LEMN_PROJECT_ID` in Pi's environment to choose a unique or shared scope.

`global` memories are visible across projects and should be reserved for durable cross-project preferences. Retrieval reads the caller's project plus `global`; relation matching and writes remain scope-local. The global classifier is deliberately conservative and includes a narrow explicit-language cue override.

For databases created before project scoping, see the [operations guide](operations.md#database-upgrade).

## Privacy and network traffic

LEMN can keep conversation inference and storage local, but “local-first” does not mean that no network traffic occurs. First startup downloads the Laya checkpoint; model and embedding requests go to the configured OpenAI-compatible endpoints, which may be remote; source-backed revalidation fetches allowlisted HTTPS URLs. Docker Compose publishes service ports on host interfaces unless the mappings are bound to loopback. Operational endpoints use the shared secret, but health checks are unauthenticated. Review and restrict network exposure for your deployment.

## Design goal and limits

LEMN models memories as scoped claims with provenance and an explicit lifecycle, rather than as an unstructured vector bucket. That is the design rationale, not a demonstrated long-horizon outcome. Current synthetic benchmarks do not establish improved task success over maintained repository instructions or a no-memory baseline. See the [evaluation guide](evaluation.md).
