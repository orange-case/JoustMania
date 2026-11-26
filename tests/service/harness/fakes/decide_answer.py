"""Builds a scriptable decide answer from a rendered decide prompt (services/agent/decide/llmio Render).

The answer is BUILT FROM THE REQUEST: the questions, their options and ranges are parsed out of the
rendered user prompt, so every choice is an option the agent actually offered and every score lies
inside the asked range. Stdlib only. Used by ``FakeClaude`` (the claude decide backend).
"""

from __future__ import annotations

import json
import re

# Session-scoped interventions take target "none"; the journeys only script session-scoped ones
# (see decide_hook.go playerTargetedInterventions).
TARGET_NONE = "none"
OPTION_NOOP = "noop"

# Rendered prompt shapes (services/agent/decide/llmio/llmio.go Render).
_GAME_ID_RE = re.compile(r"^game: id=(\S+)", re.MULTILINE)
_QUESTION_RE = re.compile(r"^- id: (\S+)\n  kind: (\w+)\n  (options|range): (\[.*\])$", re.MULTILINE)


def parse_game_id(user_prompt: str) -> str:
    match = _GAME_ID_RE.search(user_prompt)
    return match.group(1) if match else ""


def parse_questions(user_prompt: str) -> list[dict]:
    """The questions of a rendered decide prompt, in order.

    Options are rendered with Go's strconv.Quote, which is valid JSON for the ASCII ids the agent
    uses; a range is ``[lo, hi]``.
    """
    questions = []
    for qid, kind, field, raw in _QUESTION_RE.findall(user_prompt):
        q = {"id": qid, "kind": kind}
        if field == "options":
            q["options"] = json.loads(raw)
        else:
            q["range"] = json.loads(raw)
        questions.append(q)
    return questions


def _choice_answer(qid: str, options: list[str], wanted: str, confidence: float) -> dict:
    choice = wanted if wanted in options else options[0]
    probabilities = [{"option": choice, "p": 0.9}]
    others = [o for o in options if o != choice]
    if others:
        probabilities.append({"option": others[0], "p": 0.1})
    else:
        probabilities[0]["p"] = 1.0
    return {"id": qid, "choice": choice, "score": 0, "probabilities": probabilities, "confidence": confidence}


def build_answer(questions: list[dict], script: dict) -> tuple[dict, str]:
    """The structured answer for ``questions`` under ``script``.

    Returns (answer, chosen intervention). If the scripted intervention is not among the offered
    options (not in interventions_allowed) the answer is noop, which the request log exposes so the
    test fails with a clear reason.
    """
    confidence = float(script["confidence"])
    answers = []
    chosen = ""
    for q in questions:
        if q["kind"] == "choice":
            if q["id"] == "ffa.intervention":
                wanted = script["intervention"]
            elif q["id"] == "ffa.target":
                wanted = TARGET_NONE
            else:
                wanted = q["options"][0]
            answer = _choice_answer(q["id"], q["options"], wanted, confidence)
            if q["id"] == "ffa.intervention":
                chosen = answer["choice"]
            answers.append(answer)
        else:
            lo, hi = q["range"]
            score = min(max(float(script["magnitude"]), lo), hi)
            answers.append({"id": q["id"], "choice": "", "score": score, "probabilities": [], "confidence": confidence})
    if chosen == "":
        chosen = OPTION_NOOP
    return {"answers": answers, "rationale": f"fake scripted {chosen} at confidence {confidence}"}, chosen
