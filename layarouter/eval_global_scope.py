"""Measure Laya's global-scope classifier on durable and project-local facts."""

import os

os.environ.setdefault("USE_TF", "0")

import server

MODEL_REPO = os.environ.get("LAYA_MODEL_REPO", "convaiinnovations/laya-typed-decisions")
THRESHOLD = float(os.environ.get("LAYA_GLOBAL_THRESHOLD", "0.90"))

# (name, user_message, assistant_response, expected_global)
LABELED = [
    (
        "concise-across-projects",
        "Across all projects, I prefer concise answers without a preamble.",
        "Understood. I'll keep answers concise and skip preambles across projects.",
        True,
    ),
    (
        "metric-units-across-projects",
        "Please remember that I prefer metric units in every project, not just this repository.",
        "Got it. I'll use metric units across projects.",
        True,
    ),
    (
        "review-risks-across-projects",
        "For future code reviews across my projects, lead with concrete risks before the summary.",
        "Understood. Future reviews will start with concrete risks.",
        True,
    ),
    (
        "repository-test-convention",
        "For this repository, keep new API tests as integration tests using the test database.",
        "Understood. I'll follow that convention for this repository.",
        False,
    ),
    (
        "one-answer-style-request",
        "For this answer only, keep it short and skip the preamble.",
        "Sure. I'll keep this reply brief.",
        False,
    ),
    (
        "project-retry-setting",
        "We decided this service retries failed requests up to four times.",
        "Understood. The service will retry failed requests up to four times.",
        False,
    ),
    (
        "lasting-preference-no-matter-project",
        "No matter which project we're in, use ISO dates in logs; that's my lasting preference.",
        "Understood. I'll use ISO dates in logs in any project.",
        True,
    ),
    (
        "examples-in-any-codebase",
        "Please remember that I like examples before abstractions in any codebase.",
        "Got it. I'll lead with examples across your codebases.",
        True,
    ),
    (
        "repository-unit-tests",
        "In this repository, prefer unit tests for small pure helpers.",
        "Understood. I'll use unit tests for small pure helpers in this repository.",
        False,
    ),
    (
        "temporary-with-global-phrase",
        "Across all projects, for this answer only, use bullet points.",
        "Sure, I'll use bullet points for this answer.",
        False,
    ),
]


def deployed_question() -> str:
    return server.MEMORY_QUESTIONS["globally_applicable"]["instructions"]


VARIANTS = {
    "deployed": deployed_question(),
    "behavior-vs-project": (
        "Does this state a durable way the user wants the assistant to behave in every project, "
        "or is it a fact, decision, or convention that applies only to the current project?"
    ),
    "explicit-cross-project": (
        "Would the user want this same information remembered and applied in every unrelated project? "
        "Answer yes only for a durable user preference that applies across projects; answer no for "
        "project facts, project decisions, and one-time instructions."
    ),
    "user-preference-counterfactual": (
        "If the user started a completely unrelated project tomorrow, should this memory still guide "
        "the assistant there? Say yes only for a lasting user preference; say no for facts or rules "
        "that belong to this project or this one answer."
    ),
}


def evaluate(agent, instructions: str) -> dict[str, float]:
    question = {"global_scope": {"type": "noul", "instructions": instructions}}
    scores = {}
    for name, user, assistant, _expected in LABELED:
        result = agent.predict({"query": f"User: {user}\nAssistant: {assistant}"}, question)
        scores[name] = float(result["answers"]["global_scope"]["noul"])
    return scores


def main() -> None:
    agent = server.laya.load(MODEL_REPO)
    for variant, instruction in VARIANTS.items():
        scores = evaluate(agent, instruction)
        positive = [scores[name] for name, _, _, expected in LABELED if expected]
        negative = [scores[name] for name, _, _, expected in LABELED if not expected]
        predictions = {
            name: score >= THRESHOLD
            for name, score in scores.items()
        }
        correct = sum(predictions[name] == expected for name, _, _, expected in LABELED)
        separation = min(positive) - max(negative)
        print(f"\n== {variant} (threshold={THRESHOLD:.2f}) ==")
        for name, _, _, expected in LABELED:
            score = scores[name]
            predicted = predictions[name]
            marker = "" if predicted == expected else "  <-- MISCALCULATED"
            print(f"  p={score:.4f} predicted_global={predicted!s:<5} expected={expected!s:<5} {name}{marker}")
        print(f"  accuracy={correct}/{len(LABELED)} separation={separation:+.4f}")


if __name__ == "__main__":
    main()
