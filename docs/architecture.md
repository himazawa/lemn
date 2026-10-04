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
Extraction first reads only the new turn, never the existing memory list. A
separate dependency resolver then reads the immutable claim and authoritative
project/global candidates; it cannot rewrite the claim. Repeated existing facts
are vetoed rather than counted as independent evidence. Explicit confirmations
of new durable decisions are distinct from conversational acknowledgments or
memory-ID administration. Failed dependency or relation decisions fail closed.
The extractor can reject a turn or propose relations and dependencies. Tool
evidence means a tool-call diff has a high embedding similarity to the claim.
This is a supporting signal, not proof that the diff confirms the claim.

Non-correction independent claims can be promoted automatically. Evidence-backed
claims are promoted unless a possible duplicate requires review. Unbacked
`OBSERVED` claims require human review by default, even at confidence 1.0.
`LEMN_ALLOW_CONFIDENCE_PROMOTION=true` explicitly enables the old confidence
policy using `LEMN_PROMOTE_THRESHOLD`; it is not a calibration guarantee.
Independent claims are checked against authoritative same-scope plus global
memories at the existing 0.90 near-restatement threshold. Matches become
`PENDING_CONFIRMATION` with candidate IDs; similarity never automatically merges
claims or supersedes another scope. Sweep also honors this review gate.
Explicit corrections and destructive
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

### Review states

```text
New independent claim
  | no relevant tool evidence
  v
OBSERVED -- human confirm (confidence promotion opt-in only) --> AUTHORITATIVE
  |                                               ^
  | reject or sweep rejection                    |
  v                                               |
REJECTED                                          |
                                                  |
Evidence-backed independent claim                 |
  +-- promotion ------------------------------- > AUTHORITATIVE
                                                  ^
Correction or destructive relation               |
  v                                               |
PENDING_CONFIRMATION -- human confirm ------------+
  |                       or eligible auto-confirm
  +-- reject --> REJECTED

NEEDS_REVALIDATION -- fresh evidence or user confirmation --> AUTHORITATIVE
          |
          +-- reject --> REJECTED
```

`CANDIDATE` is the intermediate state for evidence-backed claims and is usually
promoted automatically. Older rows may still be reviewed as candidates. An
invalid relation target is downgraded to an independent claim. Ordinary
confirmation cannot restore a `NEEDS_REVALIDATION` memory.

Successful confirmations record a human/automatic review actor and timestamp.
Authoritative memories can be explicitly retracted with `reject --reason`;
retraction is transactional, records its reason, and recursively quarantines
authoritative dependents, including project memories depending on global ones.
Confirmation refuses dependencies that its own destructive relation would
invalidate and rechecks them after cascading. Sweep does not silently drop
stale dependencies to manufacture an independent claim.

### Graph effects of confirmation

```text
Confirm memory A as superseding or contradicting memory B:

  A --supersedes / contradicts edge--> B
  A: reviewable state ----------------> AUTHORITATIVE
  B: AUTHORITATIVE -------------------> SUPERSEDED / CONTRADICTED
  B's authoritative dependents -------> NEEDS_REVALIDATION (recursive)

  A's depends_on edges are checked and written in the same transaction.
```

Confirmation validates the target and dependencies, then commits the state
changes and graph edges in one transaction. It checks dependencies before
flipping the target so the confirming memory can depend on other memories that
the same transaction will invalidate. Only `AUTHORITATIVE` memories are
retrieved.

Revalidation returns a quarantined memory to `AUTHORITATIVE` only after explicit
user confirmation or verification of a cited passage from an allowlisted HTTPS
host. It also checks that dependencies remain authoritative, unless they are
replaced or cleared. Source fetching rejects redirects outside the allowlist,
private and special-use IP addresses, non-HTTPS URLs, credentials,
query-bearing URLs, unsupported content types, oversized bodies, and oversized
quotes. LEMN records hashes of the fetched body and quote and checks evidence
age. A matching quote proves only that the source contained those words. It
does not prove the source is correct or supports the memory.

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
