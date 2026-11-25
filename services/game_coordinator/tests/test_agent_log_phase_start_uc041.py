"""UC-041 A8 / BR-004 (#2766): ``game.phase.start`` carries the phase span ids to the agent.

With ``telemetry.agent_log_events`` on, the game sends one agent-only ``game.phase.start``
when ``gameplay_phase`` opens and one when ``game_cycle`` opens, each in that span's
context and only when that span is sampled. A mode without ``game_cycle`` sends only
the first. Off, nothing is sent.

Real SDK tracer and logger providers with in-memory exporters (not the global ones).
"""

from unittest.mock import AsyncMock, patch

import pytest
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF

from lib import otel_logging
from services.game_coordinator.games import base
from services.game_coordinator.games.zombie import ZombieGame
from services.game_coordinator.tests.conftest import EventCollector, MockControllerManagerService
from services.game_coordinator.tests.test_game_cycle_span_publish import _fast_ffa_game

PHASE_START = "game.phase.start"


class _Sdk:
    def __init__(self, sampler=None):
        self.spans = InMemorySpanExporter()
        provider = TracerProvider(sampler=sampler) if sampler is not None else TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(self.spans))
        self.tracer = provider.get_tracer("test_agent_log_phase_start")
        self.logs = InMemoryLogRecordExporter()
        self.log_provider = LoggerProvider()
        self.log_provider.add_log_record_processor(SimpleLogRecordProcessor(self.logs))

    def records(self, name=PHASE_START) -> list:
        return [r.log_record for r in self.logs.get_finished_logs() if name is None or r.log_record.event_name == name]

    def span(self, name):
        (span,) = [s for s in self.spans.get_finished_spans() if s.name == name]
        return span


def _install(monkeypatch, sampler=None) -> _Sdk:
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # conftest disables the SDK; these are local providers
    s = _Sdk(sampler)
    monkeypatch.setattr(otel_logging, "_logger_provider", s.log_provider)
    monkeypatch.setattr(base, "tracer", s.tracer)
    return s


@pytest.fixture
def sdk(monkeypatch):
    return _install(monkeypatch)


@pytest.fixture
def unsampled_sdk(monkeypatch):
    return _install(monkeypatch, ALWAYS_OFF)


def _ffa(agent_log_events):
    game = _fast_ffa_game()
    game.game_kind = "shadow"
    game.agent_log_events = agent_log_events
    return game


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", br=["BR-004"])
async def test_flag_on_sends_one_phase_start_per_phase_span_in_its_context(sdk):
    game = _ffa(agent_log_events=True)

    await game.run()

    records = sdk.records()
    assert [r.attributes["game.phase.span"] for r in records] == ["gameplay_phase", "game_cycle"]
    for record in records:
        span = sdk.span(record.attributes["game.phase.span"])
        assert dict(record.attributes) == {
            "game.id": game.game_id,
            "game.kind": "shadow",
            "game.phase.span": span.name,
            "orangecase.audience": "agent",
        }
        assert (record.trace_id, record.span_id) == (span.context.trace_id, span.context.span_id)
        assert record.trace_flags.sampled


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", br=["BR-004"])
async def test_flag_off_sends_no_phase_start(sdk):
    game = _ffa(agent_log_events=False)

    await game.run()

    assert sdk.records(None) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", br=["BR-004"])
async def test_unsampled_phase_spans_send_no_phase_start(unsampled_sdk):
    game = _ffa(agent_log_events=True)

    await game.run()

    assert unsampled_sdk.records(None) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", br=["BR-004"])
async def test_mode_without_game_cycle_sends_only_gameplay_phase(sdk):
    game = ZombieGame(
        controller_manager_client=MockControllerManagerService(num_controllers=3),
        event_publisher=EventCollector().publish,
        game_id="zombie_phase_start",
        initial_players=[type("P", (), {"serial": f"mock_controller_{i}"})() for i in range(3)],
    )
    game.agent_log_events = True
    game._game_loop = AsyncMock()

    with patch.object(game, "_run_tracer", return_value=sdk.tracer):
        await game._run_streamed_gameplay()

    (record,) = sdk.records()
    gameplay = sdk.span("gameplay_phase")
    assert record.attributes["game.phase.span"] == "gameplay_phase"
    assert record.attributes["game.id"] == "zombie_phase_start"
    assert (record.trace_id, record.span_id) == (gameplay.context.trace_id, gameplay.context.span_id)
