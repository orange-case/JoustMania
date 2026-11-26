"""TC-008 on the service tier (T2): the rules default and a remote backend without its key (#1600, S7 of #1422).

AGENT composition, one ``fresh_stack`` per case: real menu, game coordinator and
controller manager (mock backend) from source at HEAD, the Go agent built at
HEAD (no remote backend key), a fresh flag plane, FakeAudio and the OtlpSink (Jaeger's
role: the agent exports its spans there, and the sink tees the services' telemetry to
the agent as the collector does). Routing by confidence (act, held, broken answer) runs
through the fake Claude in ``test_tc008_claude_backend.py`` and the fake Jev in
``test_tc008_jev_backend.py``.

Moved 1:1 from the full-stack ``tests/integration/test_decide_backend_e2e.py``
(e2e-agent shard): the same flags, scripts and assertions; the audio-service
``SetVolume:65%`` span is now the SetVolume(0.65) call FakeAudio records.

Each case runs one menu game on a stack of its own: the agent's interventions are
un-targeted, ``enabled`` and ``interventions_allowed`` are global and the LLM
budget is per process, so cases must not share an agent.
"""

import pytest

from tests.integration.helpers import (
    GameEventCollector,
    get_game_client,
    get_mock_client,
    kill_player_verified,
)
from tests.service.agent_flow import (
    FIRST_CALL_TIMEOUT,
    RULES_DECIDE_TIMEOUT,
    children,
    configure_agent,
    decide_spans,
    intervention_events,
    start_ffa,
    trace_of,
    wait_agent_flags_served,
    wait_decide_span,
    wait_game_cycle_id_sent,
)
from tests.service.harness import AGENT

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = AGENT


def _configure(stack, backend: str, state: str = "act", **kwargs) -> None:
    configure_agent(backend=backend, state=state, **kwargs)
    wait_agent_flags_served(
        stack, backend=backend, state=state, rewrite=lambda: configure_agent(backend=backend, state=state, **kwargs)
    )


@pytest.mark.asyncio
@pytest.mark.uc("UC-040", flow="A1", tc="TC-008")
@pytest.mark.uc("UC-040", flow="A1", tc="TC-007")
@pytest.mark.uc("UC-044", flow="main", steps=[3, 4, 5], tc="TC-008")
@pytest.mark.usefixtures("flag_service")
async def test_rules_backend_default_unchanged(fresh_stack):
    """decision.backend=rules (TC-008 step 8): the rules engine decides, recorded on an
    invoke_agent span (backend=rules, requested=rules, no fallback) that is a SIBLING of
    the rules agent.decision (same parent, same decide.state.digest; #2648: no
    agent.signal_received span in the game trace); the fake is never called."""
    stack = fresh_stack
    # watch: this case observes the rules decision, not its effect.
    _configure(stack, "rules", state="watch")

    game_client, channel = await get_game_client(stack)
    mock_client, mock_channel = await get_mock_client(stack)
    try:
        collector = GameEventCollector(game_client)
        async with collector:
            game_id, serials = await start_ffa(stack, collector)
            # The agent learns game_cycle only from its log record; kill after it was sent.
            await wait_game_cycle_id_sent(stack, game_id)
            # R1 (endurance): an early elimination in a young session makes the rules
            # engine slow the tempo / cue audio, i.e. decide something.
            await kill_player_verified(mock_client, game_client, serials[0], lambda p: not p.alive)

            span = await wait_decide_span(
                stack,
                game_id,
                lambda t: t.get("decide.backend") == "rules",
                "decide.backend=rules",
                within=RULES_DECIDE_TIMEOUT,
            )
            tags = span.attributes
            assert tags.get("decide.backend.requested") == "rules", tags
            assert tags.get("decide.fallback_reason", "") == "", tags
            assert tags.get("decide.outcome") == "act", tags
            # The coordinator's "primary" label is normalised to "real" (#1249).
            assert tags.get("game.kind") == "real", tags
            # GenAI invoke_agent span on the rules path too (#2584, no model called).
            assert span.name == "invoke_agent joustmania-agent", span.name
            assert tags.get("gen_ai.operation.name") == "invoke_agent", tags
            # Sync rules path shape (#2648): invoke_agent and its agent.decision are
            # siblings (one parent, the coordinator's game_cycle, which may still be
            # open so it is not looked up), paired by decide.state.digest; the Export
            # is an event on the decide, not a span; no backend chat span.
            assert span.parent_span_id, "invoke_agent has no parent on the rules path"
            digest = tags.get("decide.state.digest")
            assert digest, tags
            siblings = [
                s
                for s in trace_of(stack, span)
                if s.name == "agent.decision"
                and s.parent_span_id == span.parent_span_id
                and s.attributes.get("decide.state.digest") == digest
            ]
            assert siblings, "no sibling agent.decision with the decide's state digest"
            assert [e for e in span.events if e.name == "agent.signal_received"], span.events
            assert not [s for s in trace_of(stack, span) if s.name == "agent.signal_received"]
            assert not children(stack, span), "the rules invoke_agent span must have no children"
    finally:
        await mock_channel.close()
        await channel.close()


@pytest.mark.asyncio
@pytest.mark.uc("UC-044", flow="A1", tc="TC-008")
@pytest.mark.uc("UC-040", flow="A3", tc="TC-008")
# No key in the agent's environment (the AGENT composition has neither ANTHROPIC_API_KEY nor JEV_API_KEY).
@pytest.mark.parametrize("backend", ["claude", "jev"], ids=["claude-no-key", "jev-no-key"])
@pytest.mark.usefixtures("flag_service")
async def test_selected_backend_missing_key_is_unavailable(fresh_stack, backend):
    """A remote backend selected without its key (UC-044 A1, TC-008 step 1 variant):
    the agent never calls out; the cycle's invoke_agent span is a rules fallback with
    decide.fallback_reason=backend_unavailable (decide.backend=rules, requested=<backend>),
    counted in agent_llm_gated_total{reason=backend_unavailable}, and nothing is applied
    from a remote answer (no chat span, no unblocked agent_intervention)."""
    stack = fresh_stack
    _configure(stack, backend)

    game_client, channel = await get_game_client(stack)
    try:
        collector = GameEventCollector(game_client)
        async with collector:
            game_id, _ = await start_ffa(stack, collector)
            span = await wait_decide_span(
                stack,
                game_id,
                lambda t: t.get("decide.fallback_reason") == "backend_unavailable",
                "decide.fallback_reason=backend_unavailable",
            )
            tags = span.attributes
            assert tags.get("decide.outcome") == "fallback", tags
            assert tags.get("decide.backend") == "rules", tags
            assert tags.get("decide.backend.requested") == backend, tags
            chats = [s for s in children(stack, span) if s.name.startswith("chat ")]
            assert not chats, f"an unavailable backend must not be called: {[s.name for s in chats]}"
            await stack.otlp_sink.wait_metric_async(
                "agent_llm_gated_total",
                {"reason": "backend_unavailable"},
                service="agent",
                timeout=FIRST_CALL_TIMEOUT,
            )
            # No remote answer was ever routed: every decide span of the game is a rules span.
            assert {s.attributes.get("decide.backend") for s in decide_spans(stack, game_id)} == {"rules"}
            assert not [e for e in intervention_events(collector) if e.get("blocked") == "false"], (
                "an unblocked intervention from a backend that cannot answer"
            )
    finally:
        await channel.close()
