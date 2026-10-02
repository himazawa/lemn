"""
Regression check for Laya's memory-worthiness question.

The `memory_worthy` question in server.py is a hand-written string evaluated
by a checkpoint that was never trained on it, so rewording it can move every
probability in the pipeline. This script pins the labelled behaviour down so
future rewordings are measured, not guessed.

The labelled set is real traffic from this project: turns that should be
stored (decisions, corrections, confirmed facts, preferences) and turns that
should be dropped (broad questions, lookups, acknowledgements). Anchors are
the examples that motivated the current wording — they must classify
correctly at the deployed threshold or the eval fails.

Usage (inside the laya container, or anywhere with the model repo):

    python eval_memory_gate.py              # measure every variant, print table
    python eval_memory_gate.py --assert     # additionally fail (exit 1) if the
                                            # deployed question regresses

"--assert" checks three things against the question currently in server.py:
  1. anchors:    every anchored example must classify correctly at the
                 deployed LAYA_MEMORY_THRESHOLD
  2. accuracy:   at most MAX_MISCLASSIFIED examples may misclassify
  3. separation: min(WORTH) - max(NOT) must stay at or above SEPARATION_FLOOR

The floor and misclassification budget were recorded from the measured table
when the current wording was adopted. The one tolerated miss is the "ack"
example ("Yes, go ahead" / "Done."), which scores just above the threshold;
its cost is a single extraction call that the extraction model vetoes with
type:none, which the pipeline is designed to absorb.

Run it after any change to MEMORY_QUESTIONS["memory_worthy"] in server.py.
"""

import os

os.environ.setdefault("USE_TF", "0")

import sys

import laya

MODEL_REPO = os.environ.get("LAYA_MODEL_REPO", "convaiinnovations/laya-typed-decisions")
THRESHOLD = float(os.environ.get("LAYA_MEMORY_THRESHOLD", "0.5"))

# (name, user_message, assistant_response, want_worthy, is_anchor)
#
# Anchors are the examples the wording must get right, full stop. The rest of
# the set contributes to the separation measurement. Labels are honest
# judgments about this project's traffic, not what the model currently says.
LABELED = [
    # --- should be stored -------------------------------------------------
    # The motivating failure: a confirmed correction of a stale memory,
    # dropped at 0.38 by the previous wording.
    ("secret-correction",
     "We confirmed the shared secret is never stored in the database, it only comes from the LEMN_SHARED_SECRET environment variable",
     "Correct — the lemn_memories table has no secret column; LEMN_SHARED_SECRET is read from the environment at startup. Memory #2 claiming a secret API key is stored in the memory DB is stale.",
     True, True),
    ("redis-decision",
     "We decided to drop Redis and use Postgres LISTEN/NOTIFY for the job queue.",
     "Understood, removing the Redis dependency.",
     True, False),
    ("threshold-change",
     "Lower LEMN_SUPERSEDE_THRESHOLD to 0.80 in the .env file",
     "Done — the daemon will now propose supersession matches above 0.80 similarity.",
     True, False),
    ("flag-fix",
     "The lemn confirm command ignores flags placed after the memory ID",
     "Fixed — the flag parsing now accepts flags before or after the IDs and applies them to the whole batch.",
     True, False),
    ("user-preference",
     "I prefer terse answers without preamble",
     "Got it, I'll keep answers short from now on.",
     True, False),
    ("embedding-fact",
     "We are using bge-m3 for embeddings, 1024 dimensions",
     "Noted — the schema's vector(1024) column matches that model.",
     True, False),
    # --- should be dropped ------------------------------------------------
    ("broad-question",
     "Tell me about the project in the current folder",
     "It is a local memory system with a Go router, a daemon, and a Postgres kernel.",
     False, True),
    ("lookup-question",
     "Can you list the currently authoritative memories?",
     "Here are the five authoritative memories in this project.",
     False, False),
    ("ack",
     "Yes, go ahead",
     "Done.",
     False, False),
    ("dir-listing",
     "What is in this directory?",
     "There is a README, a docker-compose file, and two source directories.",
     False, False),
    ("routine-edit",
     "Rename this variable to something clearer",
     "Renamed it.",
     False, False),
    ("thanks",
     "Thanks, that worked",
     "Great!",
     False, False),
]

# Recorded from the measured table when the current wording was adopted
# (v2_establishes @ threshold 0.45: measured separation -0.0854; the negative
# sign comes from the one tolerated "ack" miss). A rewording that shrinks
# the gap below this is a regression even if no single anchor flips.
SEPARATION_FLOOR = -0.09
MAX_MISCLASSIFIED = 1


def deployed_question() -> str:
    import server
    return server.MEMORY_QUESTIONS["memory_worthy"]["instructions"]


# Candidate rewordings to measure alongside the deployed question. Lessons
# from the routing-question calibration: concrete and observable beats
# abstract; counterfactuals work; asking about the speaker backfires.
VARIANTS = {
    "v1_counterfactual": (
        "If the user returned to this project next week with no memory of this "
        "conversation, would losing this specific turn cost them information "
        "they cannot easily re-derive from the code, the docs, or a fresh "
        "question?"
    ),
    "v2_establishes": (
        "Does this turn establish or change something concrete — a decision "
        "made, a fact confirmed, a preference stated, a mistake corrected, a "
        "configuration set — as opposed to merely requesting information or "
        "acknowledging a previous answer?"
    ),
    "v3_short": (
        "Does this turn contain a stable decision, fact, preference, or "
        "correction that would still matter in a future conversation, as "
        "opposed to a transient request or routine exchange?"
    ),
}


def measure(agent, instructions: str) -> dict:
    """Return {name: probability} for every labelled example."""
    question = {"k": {"type": "noul", "instructions": instructions}}
    out = {}
    for name, user, assistant, _want, _anchor in LABELED:
        state = {"query": "User: %s\nAssistant: %s" % (user, assistant)}
        out[name] = agent.predict(state, question)["answers"]["k"]["noul"]
    return out


def report(variant_name: str, probs: dict) -> None:
    worth = [p for n, _, _, want, _ in LABELED if want for p in [probs[n]]]
    notw = [p for n, _, _, want, _ in LABELED if not want for p in [probs[n]]]
    print(f"\n== {variant_name} ==")
    for name, _u, _a, want, anchor in LABELED:
        p = probs[name]
        verdict = "WORTH" if p >= THRESHOLD else "drop "
        mark = " *" if anchor else "   "
        flag = "" if (p >= THRESHOLD) == want else "  <-- MISCALLED"
        print(f"  {mark} {p:.3f} {verdict} {name[:26]:<26}{flag}")
    if worth and notw:
        hi_not, lo_worth = max(notw), min(worth)
        print(f"  max(NOT)={hi_not:.3f}  min(WORTH)={lo_worth:.3f}  "
              f"separation={lo_worth - hi_not:+.3f}  threshold={THRESHOLD}")


def main() -> int:
    assert_mode = "--assert" in sys.argv
    agent = laya.load(MODEL_REPO)

    variants = {"deployed": deployed_question()}
    variants.update(VARIANTS)

    results = {}
    for name, instructions in variants.items():
        results[name] = measure(agent, instructions)
        report(name, results[name])

    if not assert_mode:
        return 0

    # Regression gate on the deployed question.
    probs = results["deployed"]
    worth = [probs[n] for n, _, _, want, _ in LABELED if want]
    notw = [probs[n] for n, _, _, want, _ in LABELED if not want]
    separation = min(worth) - max(notw)
    misclassified = [
        (n, probs[n]) for n, _u, _a, want, _ in LABELED
        if (probs[n] >= THRESHOLD) != want]

    failures = []
    if separation + 1e-9 < SEPARATION_FLOOR:
        failures.append(
            f"separation {separation:+.3f} fell below floor {SEPARATION_FLOOR:+.3f}")
    if len(misclassified) > MAX_MISCLASSIFIED:
        failures.append(
            f"{len(misclassified)} misclassified examples exceed budget of "
            f"{MAX_MISCLASSIFIED}: "
            + ", ".join(f"{n} (p={p:.3f})" for n, p in misclassified))
    for name, _u, _a, want, anchor in LABELED:
        if anchor and (probs[name] >= THRESHOLD) != want:
            failures.append(
                f"anchor {name!r} misclassified at threshold {THRESHOLD} (p={probs[name]:.3f})")

    if failures:
        print("\nREGRESSION in deployed memory-worthiness gate:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print(f"\nOK: deployed gate holds (separation {separation:+.3f}, "
          f"{len(misclassified)}/{len(LABELED)} misclassified, "
          f"all anchors correct at threshold {THRESHOLD})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
