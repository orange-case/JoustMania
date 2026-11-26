"""Shared steps of the agent journeys on the service tier (AGENT composition, #1600).

TC-006 step 5, TC-008 and TC-009 (decided through the fake Claude, ``AGENT_CLAUDE_DECIDE``). Ported from the full-stack
``tests/integration/test_decide_backend_e2e.py`` (deleted with #1600): the same
flags, scripts and budgets, with Jaeger replaced by the stack's ``OtlpSink`` (the
agent exports its spans there) and audio-service by ``FakeAudio``.

Every agent-set flag is a flag-service overlay that the ``flag_service`` fixture
deletes at teardown; every journey runs on ``fresh_stack`` anyway, because
``enabled``, ``interventions_allowed`` and the agent's LLM budget are global.
"""

from __future__ import annotations

from collections.abc import Callable

from openfeature.evaluation_context import EvaluationContext

from tests.integration.flag_service import default as flag_service_default
from tests.integration.helpers import (
    GameEventCollector,
    async_poll_until,
    flagd_probe_client,
    get_game_client,
    poll_until,
)
from tests.service.harness.fakes import SpanRecord
from tests.service.lobby_flow import ready_up_until_started, start_lobby, wait_running

AGENT = "agent"
AGENT_SERVICE = "agent"  # the agent's OTEL_SERVICE_NAME (harness/agent.py)

FAKE_BASE_URL_VARIANT = "fake"  # rewritten to the stack's fake (harness/stack.py)
CLAUDE_MODEL = "claude-haiku-4-5-20251001"  # the shipped decision.claude.model default
CHAT_SPAN = f"chat {CLAUDE_MODEL}"

# The scripted intervention: session-scoped (target "none"), numeric magnitude, in the
# DEFAULT ``ambient`` interventions_allowed variant, observable on the game stream as
# agent_intervention(type=adjust_volume); its handler calls audio SetVolume (#1250).
SCRIPTED_INTERVENTION = "adjust_volume"
SCRIPTED_FLAG = "volume_override"
INTERVENTIONS_ALLOWED_VARIANT = "ambient"
SCRIPTED_MAGNITUDE = 0.5  # over adjust_volume's [0.3, 1.0] bounds (decide.ResolveMagnitude)
EXPECTED_VALUE = 0.65
HIGH_CONFIDENCE = 0.95
LOW_CONFIDENCE = 0.3
ACT_THRESHOLD = 0.8  # decision.claude.act_confidence_threshold default variant (ci/agent.json)
# llm.eligible_game_kinds: the SHIPPED default, left untouched. Menu games reach the agent
# as game_kind "real" (#1249), so the remote cases also prove a menu game is eligible.
ELIGIBLE_GAME_KINDS = "real"

# Remote call budget per decide call: the kill-switch case rewrites the ``patient`` variant of
# decision.claude.timeout_ms to this in its stack's baseline (the flag takes declared variants
# only), long so the held call is still in flight after the switch flips. Below the agent's
# HTTP backstop (30 s), and the fake's hold below it.
DECIDE_TIMEOUT_MS = 25000
DECIDE_TIMEOUT_VARIANT = ("agent", "decision.claude.timeout_ms", "patient")
DECIDE_MIN_INTERVAL_MS = 1500

# Polling budgets (worst-case bounds, not paid waits).
FIRST_CALL_TIMEOUT = 60.0  # game start -> OTLP -> agent cycle -> first remote call
SPAN_TIMEOUT = 45.0  # span end -> agent batch export -> sink
APPLY_TIMEOUT = 30.0  # agent write -> flagd -> coordinator applies
RULES_DECIDE_TIMEOUT = 90.0  # a rules decision needs a rule to fire
# Observation window of the "nothing applied" assertions: several remote decision
# cadences (decision.min_interval_ms = 1.5 s) plus a flagd push. The window polls for
# a VIOLATION and fails at once on one; when the correct behaviour is "nothing
# happens", a bounded observation is the test.
ABSENCE_WINDOW = 6.0


# --------------------------------------------------------------------------- #
# Agent flags
# --------------------------------------------------------------------------- #
def configure_agent(
    *,
    backend: str,
    state: str = "act",
    allowed: str = INTERVENTIONS_ALLOWED_VARIANT,
    jev_fake: bool = False,
    claude_fake: bool = False,
    timeout_variant: str | None = None,
) -> None:
    """Every agent-set flag a journey needs, as flag-service overlays.

    ``jev_fake`` points ``decision.jev.base_url`` at the stack's fake Jev and ``claude_fake``
    ``decision.claude.base_url`` at the stack's fake Claude; ``timeout_variant`` names the
    ``decision.claude.timeout_ms`` variant (the shipped default when None). ``state`` is
    ``enabled``: ``act`` applies decisions, ``watch`` decides and writes nothing.
    """
    fs = flag_service_default()
    fs.put(AGENT, "enabled", variant=state)
    fs.put(AGENT, "interventions_allowed", variant=allowed)
    fs.put(AGENT, "decision.backend", variant=backend)
    if jev_fake:
        fs.put(AGENT, "decision.jev.base_url", variant=FAKE_BASE_URL_VARIANT)
    if claude_fake:
        fs.put(AGENT, "decision.claude.base_url", variant=FAKE_BASE_URL_VARIANT)
    if timeout_variant is not None:
        fs.put(AGENT, "decision.claude.timeout_ms", variant=timeout_variant)
    fs.put(AGENT, "decision.min_interval_ms", value=DECIDE_MIN_INTERVAL_MS)
    # The remote-call budget: since #1788 the env AGENT_LLM_MAX_REQUESTS_PER_MINUTE is
    # only a ceiling (min(env, flag)), so the flag itself is lifted. `journey` (600/min, a
    # CI-only variant in services/flagd/ci/agent.json) matches the old env value: `stage` (30)
    # would run long remote journeys into llm_budget_exhausted at DECIDE_MIN_INTERVAL_MS.
    fs.put(AGENT, "llm.max_requests_per_minute", variant="journey")


def set_enabled(state: str) -> None:
    """Write ``enabled`` (off / watch / act); a stop is a write of ``off``, never a clear."""
    flag_service_default().put(AGENT, "enabled", variant=state)


def wait_agent_flags_served(
    stack,
    *,
    backend: str,
    state: str,
    rewrite: Callable[[], None] | None = None,
) -> None:
    """flagd serves the agent flags of the journey (typed getters: the #903 trap)."""
    ctx = EvaluationContext()
    with flagd_probe_client(stack, AGENT) as client:
        served = poll_until(
            lambda: (
                client.get_string_value("decision.backend", "", ctx) == backend
                and client.get_string_value("enabled", "", ctx) == state
                and client.get_string_value("llm.eligible_game_kinds", "", ctx) == ELIGIBLE_GAME_KINDS
            ),
            rewrite=rewrite,
        )
    assert served, f"flagd did not serve decision.backend={backend} enabled={state}"


def scripted_flag_value(game_id: str):
    """The value the agent wrote for the scripted intervention in ``game_id``, or None."""
    view = flag_service_default().get("interventions", SCRIPTED_FLAG)
    entry = (view.get("games") or {}).get(game_id)
    return None if entry is None else entry.get("value")


# --------------------------------------------------------------------------- #
# Game
# --------------------------------------------------------------------------- #
async def start_ffa(stack, collector: GameEventCollector, players: int = 4) -> tuple[str, list[str]]:
    """A menu-started JoustFFA game on ``players`` mock controllers; (game id, serials).

    The menu is restarted and the mode selected BEFORE the controllers are added
    (#1575: controllers added first can connect before the restart and be lost).
    """
    serials = await start_lobby(stack, count=players)
    game_id = await ready_up_until_started(stack, serials, collector, known_ids=set())
    game_client, channel = await get_game_client(stack)
    try:
        await wait_running(game_client, game_id)
    finally:
        await channel.close()
    return game_id, serials


async def wait_game_cycle_id_sent(stack, game_id: str) -> None:
    """Wait until the game_cycle id left the coordinator: the agent parents on it (UC-041 BR-004)."""
    await stack.otlp_sink.wait_log_async(
        event_name="game.phase.start", attrs={"game.id": game_id, "game.phase.span": "game_cycle"}, timeout=15.0
    )


def intervention_events(collector: GameEventCollector) -> list[dict]:
    return [
        dict(e.data) for e in collector.get_events("agent_intervention") if e.data.get("type") == SCRIPTED_INTERVENTION
    ]


async def assert_nothing_applied(collector: GameEventCollector, game_id: str) -> None:
    """Over ABSENCE_WINDOW: the scripted intervention neither reached the game nor was written for it."""

    def violation() -> bool:
        return bool(intervention_events(collector)) or scripted_flag_value(game_id) is not None

    assert not await async_poll_until(violation, ABSENCE_WINDOW), (
        f"{SCRIPTED_INTERVENTION} reached the game although nothing should act: "
        f"events {intervention_events(collector)}, {SCRIPTED_FLAG}={scripted_flag_value(game_id)}"
    )


# --------------------------------------------------------------------------- #
# Fake backend
# --------------------------------------------------------------------------- #
def decide_calls(stack, game_id: str) -> list[dict]:
    """The decide calls the fake Claude got for ``game_id`` (parsed from the rendered prompt)."""
    return [r for r in stack.fake_claude.requests("decide") if r.get("game_id") == game_id]


async def wait_first_call(stack, game_id: str) -> list[dict]:
    fake = stack.fake_claude
    ok = await async_poll_until(lambda: bool(decide_calls(stack, game_id)), FIRST_CALL_TIMEOUT, interval=0.5)
    assert ok, f"the agent never called fake-claude for game {game_id} within {FIRST_CALL_TIMEOUT}s: {fake.log_tail()}"
    calls = decide_calls(stack, game_id)
    assert calls[0].get("questions"), (
        "fake-claude parsed no decide questions from the prompt: the rendered prompt "
        "(services/agent/decide/llmio/llmio.go Render) no longer matches "
        f"tests/service/harness/fakes/decide_answer.py _QUESTION_RE: {calls[0]}"
    )
    return calls


# --------------------------------------------------------------------------- #
# Agent spans (exported by the agent to the stack's OtlpSink)
# --------------------------------------------------------------------------- #
def agent_spans(stack, operation: str, game_id: str) -> list[SpanRecord]:
    """Every agent span named ``operation`` for ``game_id``."""
    return stack.otlp_sink.spans(operation, {"game.id": game_id}, service=AGENT_SERVICE)


def decide_spans(stack, game_id: str) -> list[SpanRecord]:
    return agent_spans(stack, "invoke_agent joustmania-agent", game_id)


def trace_of(stack, span: SpanRecord) -> list[SpanRecord]:
    return [s for s in stack.otlp_sink.spans() if s.trace_id == span.trace_id]


def children(stack, span: SpanRecord, name: str | None = None) -> list[SpanRecord]:
    """Direct children of ``span`` in its trace, optionally filtered by name."""
    return [s for s in trace_of(stack, span) if s.parent_span_id == span.span_id and (name is None or s.name == name)]


def parent(stack, span: SpanRecord) -> SpanRecord | None:
    return next((s for s in trace_of(stack, span) if s.span_id == span.parent_span_id), None)


def evaluation_events(span: SpanRecord) -> list[dict]:
    return [e.attributes for e in span.events if e.name == "gen_ai.evaluation.result"]


def _decide_tags(span: SpanRecord) -> dict:
    return {
        k: v
        for k, v in span.attributes.items()
        if k.startswith("decide.") and k not in ("decide.questions", "decide.answers")
    }


async def wait_decide_span(
    stack, game_id: str, predicate: Callable[[dict], bool], what: str, within: float = SPAN_TIMEOUT
) -> SpanRecord:
    """The first invoke_agent span for ``game_id`` whose attributes satisfy ``predicate``."""
    found: list[SpanRecord] = []

    def check() -> bool:
        found[:] = [s for s in decide_spans(stack, game_id) if predicate(s.attributes)]
        return bool(found)

    if not await async_poll_until(check, within, interval=0.5):
        seen = [_decide_tags(s) for s in decide_spans(stack, game_id)]
        raise AssertionError(f"no invoke_agent span for {game_id} with {what} within {within}s; saw: {seen}")
    return found[0]
