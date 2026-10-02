"""
LEMN Laya Classifier Service
----------------------------
Wraps the Laya typed-decisions checkpoint
(convaiinnovations/laya-typed-decisions) behind a small HTTP endpoint
the Go router calls before every chat completion.

Uses Laya's "noul" question type: a single forward pass returns a
calibrated P(true) for one yes/no question, rather than free-text
generation — there's nothing to parse or hallucinate.

IMPORTANT CAVEAT: the 0.5 default threshold below is a starting point,
not a validated cutoff. The published benchmark numbers for
`convaiinnovations/laya-typed-decisions` do not validate these custom
`requires_reasoning` or `memory_worthy` questions, which were written for
this project rather than trained directly into the model. Log routing and
memory-classification probabilities, inspect real traffic, and tune the
thresholds on your own labeled examples. The repo includes a hand-label /
export path for that, but the current extraction pipeline is still not a
validated calibrated classifier.

Run with USE_TF=0 set — transformers probes for TensorFlow at import
time, and if TF is installed its runtime can deadlock Laya's model load.
"""

import os
import secrets as secrets_module

os.environ.setdefault("USE_TF", "0")

from fastapi import Depends, FastAPI, Header, HTTPException
from pydantic import BaseModel
import laya

MODEL_REPO = os.environ.get("LAYA_MODEL_REPO", "convaiinnovations/laya-typed-decisions")
THRESHOLD = float(os.environ.get("LAYA_ROUTING_THRESHOLD", "0.38"))
# Calibrated as a pair with the memory_worthy wording below (see
# eval_memory_gate.py): on the labelled set the wording separates the classes
# at ~0.45, so the default sits at the measured midpoint, not at 0.5.
MEMORY_THRESHOLD = float(os.environ.get("LAYA_MEMORY_THRESHOLD", "0.45"))
GLOBAL_THRESHOLD = float(os.environ.get("LAYA_GLOBAL_THRESHOLD", "0.67"))
CORRECTION_THRESHOLD = float(os.environ.get("LAYA_CORRECTION_THRESHOLD", "0.5"))

SHARED_SECRET = os.environ.get("LEMN_SHARED_SECRET")
if not SHARED_SECRET:
    raise RuntimeError(
        "LEMN_SHARED_SECRET is not set — refusing to start unauthenticated. "
        "Set it to the same value configured on the router and daemon."
    )


def require_auth(authorization: str = Header(default="")) -> None:
    """Constant-time bearer-token check, mirroring the Go authmw package."""
    prefix = "Bearer "
    if not authorization.startswith(prefix):
        raise HTTPException(status_code=401, detail="missing bearer token")
    token = authorization[len(prefix):]
    if not secrets_module.compare_digest(token, SHARED_SECRET):
        raise HTTPException(status_code=401, detail="invalid bearer token")


app = FastAPI(title="LEMN Laya Classifier")

agent = laya.load(MODEL_REPO)

# Split per endpoint: agent.predict evaluates every question it is handed, so a
# combined dict would make each routing call pay for the memory questions too.
ROUTING_QUESTIONS = {
    "requires_reasoning": {
        "type": "noul",
        # Chosen by measurement over a labelled set. Asking about "deep multi-step
        # reasoning" tracked how verbosely a request was phrased, not how hard it
        # was, and left easy and hard queries overlapping. Asking about codebase
        # breadth separates them, and matches what actually predicts failure on a
        # small model: tasks that span many files.
        "instructions": (
            "Does this task require reading and reasoning about many parts of a "
            "codebase before answering, as opposed to a single lookup or a "
            "localized edit to one spot?"
        ),
    }
}

MEMORY_QUESTIONS = {
    "memory_worthy": {
        "type": "noul",
        # Chosen by measurement (eval_memory_gate.py) over a labelled set of
        # real traffic. The previous abstract wording ("stable, reusable
        # information ... likely to matter") left the worth and transient
        # classes overlapping (separation -0.158); this concrete, contrastive
        # phrasing — and its 0.45 threshold, as a pair — separates them
        # (11/12 correct; the one miss is an acknowledgement that the
        # extraction layer vetoes). Run eval_memory_gate.py --assert after
        # any change here.
        "instructions": (
            "Does this turn establish or change something concrete — a "
            "decision made, a fact confirmed, a preference stated, a mistake "
            "corrected, a configuration set — as opposed to merely requesting "
            "information or acknowledging a previous answer?"
        ),
    },
    "globally_applicable": {
        "type": "noul",
        # Counterfactual phrasing, chosen by measurement: asking directly whether
        # something is "about the user" scored project facts *higher* than real
        # preferences on this checkpoint. Separation here is narrow (~0.04), so
        # LAYA_GLOBAL_THRESHOLD is deliberately set above the observed midpoint.
        "instructions": (
            "Would this statement still be completely true and useful if the "
            "current project were deleted and the user started an entirely "
            "unrelated new project from scratch?"
        ),
    },
    "is_correction": {
        "type": "noul",
        # Primary detector for turns that correct, replace, or reject something
        # stated earlier. The daemon also keeps a phrasing backstop (regex) and
        # ORs the two: the regex misses past tense and implicit corrections,
        # Laya misses nothing it can't see, and a false positive only costs one
        # review-queue item. Starting threshold; calibrate on labelled traffic
        # like the other questions, remembering the probabilities are compressed.
        "instructions": (
            "Is the user correcting, replacing, or rejecting something that was "
            "previously claimed, decided, configured, or preferred?"
        ),
    },
}


class ClassifyRequest(BaseModel):
    query: str


class ClassifyResponse(BaseModel):
    requires_reasoning: bool
    probability: float
    threshold: float


class MemoryWorthinessRequest(BaseModel):
    user_message: str
    assistant_response: str


class MemoryWorthinessResponse(BaseModel):
    memory_worthy: bool
    probability: float
    threshold: float
    globally_applicable: bool
    global_probability: float
    global_threshold: float
    is_correction: bool
    correction_probability: float
    correction_threshold: float


def _noul(result: dict, question: str) -> float:
    """Pull one probability out of Laya's ``{"answers": {...}}`` envelope."""
    try:
        return float(result["answers"][question]["noul"])
    except (KeyError, TypeError) as exc:
        raise HTTPException(
            status_code=502,
            detail=f"unexpected Laya response shape for {question!r}: {exc}",
        ) from exc


@app.post("/classify", response_model=ClassifyResponse, dependencies=[Depends(require_auth)])
def classify(req: ClassifyRequest) -> ClassifyResponse:
    state = {"query": req.query}
    result = agent.predict(state, ROUTING_QUESTIONS)
    probability = _noul(result, "requires_reasoning")
    return ClassifyResponse(
        requires_reasoning=probability >= THRESHOLD,
        probability=probability,
        threshold=THRESHOLD,
    )


@app.post("/memory-worthiness", response_model=MemoryWorthinessResponse, dependencies=[Depends(require_auth)])
def memory_worthiness(req: MemoryWorthinessRequest) -> MemoryWorthinessResponse:
    state = {
        "query": "User: %s\nAssistant: %s" % (req.user_message, req.assistant_response),
    }
    result = agent.predict(state, MEMORY_QUESTIONS)
    probability = _noul(result, "memory_worthy")
    global_probability = _noul(result, "globally_applicable")
    correction_probability = _noul(result, "is_correction")
    return MemoryWorthinessResponse(
        memory_worthy=probability >= MEMORY_THRESHOLD,
        probability=probability,
        threshold=MEMORY_THRESHOLD,
        globally_applicable=global_probability >= GLOBAL_THRESHOLD,
        global_probability=global_probability,
        global_threshold=GLOBAL_THRESHOLD,
        is_correction=correction_probability >= CORRECTION_THRESHOLD,
        correction_probability=correction_probability,
        correction_threshold=CORRECTION_THRESHOLD,
    )


@app.get("/healthz")
def healthz():
    return {
        "status": "ok",
        "model_repo": MODEL_REPO,
        "routing_threshold": THRESHOLD,
        "memory_threshold": MEMORY_THRESHOLD,
        "global_threshold": GLOBAL_THRESHOLD,
        "correction_threshold": CORRECTION_THRESHOLD,
    }
