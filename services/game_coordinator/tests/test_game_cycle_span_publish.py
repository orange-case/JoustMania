"""The game_cycle span the agent parents its in-game spans under (#1325, UC-041 BR-004).

game_cycle is a child of gameplay_phase; the agent learns its id from the agent-only
``game.phase.start`` record (test_agent_log_phase_start_uc041.py), not from a gauge
(the ``game_trace_correlation`` gauge is retired, #2783).
"""

from unittest.mock import AsyncMock, patch

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from services.game_coordinator import metrics
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.tests.conftest import EventCollector, MockControllerManagerService


@pytest.fixture
def span_exporter(monkeypatch):
    """Route base.py's module tracer to a real SDK tracer with an in-memory exporter."""
    # conftest disables the SDK via OTEL_SDK_DISABLED, which a TracerProvider reads at
    # construction; lift it for this provider only.
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    with patch("services.game_coordinator.games.base.tracer", provider.get_tracer("test")):
        yield exporter


def _fast_ffa_game() -> FFAGame:
    """An FFA game whose countdown, warm-up and loop are stubbed so run() is quick."""
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=3),
        event_publisher=EventCollector().publish,
        audio_client=None,
        game_id="game_cycle_publish",
        initial_players=[type("P", (), {"serial": f"mock_controller_{i}"})() for i in range(3)],
    )
    game._countdown = AsyncMock()
    game._warmup_ema = AsyncMock()
    game._game_loop = AsyncMock()
    return game


@pytest.mark.uc("UC-035", flow="main", steps=[5])
@pytest.mark.uc("UC-041", br=["BR-004"])
@pytest.mark.asyncio
async def test_run_opens_game_cycle_as_a_child_of_gameplay_phase(span_exporter):
    game = _fast_ffa_game()

    await game.run()

    spans = {s.name: s for s in span_exporter.get_finished_spans()}
    cycle = spans["game_cycle"]
    gameplay = spans["gameplay_phase"]
    assert cycle.parent.span_id == gameplay.context.span_id
    assert cycle.context.span_id != gameplay.context.span_id


@pytest.mark.uc("UC-041", br=["BR-004"])
def test_the_trace_correlation_gauge_is_retired():
    """#2783: the coordinator publishes no game_trace_correlation gauge."""
    assert not hasattr(metrics, "game_trace_correlation")


class _SpanEmittingAudioClient:
    """An audio stub whose RPCs open a CLIENT span in the current context, like the gRPC instrumentation."""

    async def SetVolume(self, request):  # noqa: N802 - gRPC stub name
        from proto import audio_pb2
        from services.game_coordinator.games import base

        with base.tracer.start_as_current_span("AudioService/SetVolume", kind=trace.SpanKind.CLIENT):
            pass
        return audio_pb2.SetVolumeResponse(success=True)


@pytest.mark.uc("UC-041", br=["BR-004"])
@pytest.mark.asyncio
async def test_post_countdown_set_volume_span_is_child_of_game_cycle(span_exporter):
    """The ramp to game volume after the countdown parents under game_cycle, not gameplay_phase (#2671)."""
    game = _fast_ffa_game()
    game.audio_client = _SpanEmittingAudioClient()

    await game.run()

    spans = span_exporter.get_finished_spans()
    cycle = next(s for s in spans if s.name == "game_cycle")
    # Two SetVolume calls: the countdown level during initialization, then the game level after the countdown.
    set_volume = max((s for s in spans if s.name == "AudioService/SetVolume"), key=lambda s: s.start_time)
    assert set_volume.parent.span_id == cycle.context.span_id
