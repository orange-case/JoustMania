"""UC-103 (#2574): per-sample spans (BR-001, FR-224) and the coordinator's delivered count (BR-003, FR-246)."""

from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib.span_count import ATTR_DELIVERED, SPAN_COUNT, DeliveredSpans
from proto import controller_manager_pb2
from services.game_coordinator.games.sample_spans import SPAN_SAMPLE
from services.game_coordinator.tests.test_death_detection_uc030 import STILL, _game
from services.game_coordinator.tests.test_game_session import _make_session, _tracer_mock


@pytest.fixture
def exporter(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # conftest disables the SDK; this provider is local
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    exporter.tracer = provider.get_tracer("test")
    return exporter


async def _samples(exporter, per_sample_spans: bool, frames: int = 3):
    game, _ = _game(2)
    game.per_sample_spans = per_sample_spans
    player_span = exporter.tracer.start_span("game.player")
    game.players["p0"].span = player_span
    with patch("services.game_coordinator.games.base.tracer", exporter.tracer):
        for _ in range(frames):
            await game._process_controller_state(controller_manager_pb2.GameplayData(serial="p0", accel=STILL))
    return game, player_span, [s for s in exporter.get_finished_spans() if s.name == SPAN_SAMPLE]


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="main", steps=[3], br=["BR-001"])
async def test_one_span_per_processed_sample_under_the_player_span(exporter):
    game, player_span, samples = await _samples(exporter, per_sample_spans=True)

    assert len(samples) == 3
    _warn, death = game._compute_effective_thresholds(game.players["p0"])
    for span in samples:
        assert span.parent.span_id == player_span.get_span_context().span_id
        assert span.attributes["player.serial"] == "p0"
        assert span.attributes["game.id"] == "test_uc030"
        assert span.attributes["accel_magnitude"] == pytest.approx(1.0)
        assert span.attributes["threshold"] == pytest.approx(death)
        assert "smoothed_accel" in span.attributes


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", cond=["Post-F-3"], br=["BR-001"])
async def test_no_sample_span_with_the_flag_off(exporter):
    _game_, _span, samples = await _samples(exporter, per_sample_spans=False)
    assert samples == []


async def _run_session(per_sample: bool, outcome: str = "winner"):
    session = _make_session()
    tracer = _tracer_mock()
    root = tracer.start_as_current_span.return_value.__enter__.return_value
    game = MagicMock()
    game.run = AsyncMock()
    game.game_outcome.return_value = outcome
    record = AsyncMock()
    with (
        patch("services.game_coordinator.game_session.tracer") as mock_tracer,
        patch("services.game_coordinator.game_session.GameFactory") as factory,
        patch("lib.feature_flags.read_bool_flag", return_value=per_sample) as reads,
        patch.object(session, "_record_span_count", record),
    ):
        mock_tracer.start_as_current_span = tracer.start_as_current_span
        factory.create_game.return_value = game
        await session._run_game_loop_async()
    flag_reads = [c for c in reads.call_args_list if c.args[1] == "telemetry.per_sample_spans"]
    return root, game, flag_reads, record


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="main", steps=[1, 3], br=["BR-001"])
@pytest.mark.parametrize("value", [True, False])
async def test_the_session_reads_the_flag_once_and_records_it_on_the_root(value):
    root, game, flag_reads, _ = await _run_session(value)

    assert len(flag_reads) == 1
    assert root.attributes["telemetry.per_sample_spans"] is value
    assert game.per_sample_spans is value


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
async def test_a_normal_end_records_the_count_after_the_root():
    root, _game_, _reads, record = await _run_session(False, outcome="winner")
    record.assert_awaited_once_with(root)


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="A1", br=["BR-003"])
async def test_a_force_ended_game_has_no_count():
    _root, _game_, _reads, record = await _run_session(False, outcome="force_ended")
    record.assert_not_awaited()


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
async def test_the_count_span_and_gauge_carry_the_exported_spans(exporter):
    session = _make_session()
    counts = DeliveredSpans()
    counts.active = True
    root = exporter.tracer.start_span("Joust Free-for-All")
    trace_id = root.get_span_context().trace_id
    counts.watch(trace_id)
    counts._counts[trace_id] = 7  # what the counting exporter saw
    root.end()
    flush = MagicMock(return_value=True)

    with (
        patch("services.game_coordinator.game_session.DELIVERED", counts),
        patch("services.game_coordinator.game_session.flush_spans", flush),
        patch("services.game_coordinator.game_session.tracer", exporter.tracer),
        patch("services.game_coordinator.game_session.metrics.set_trace_spans_delivered") as gauge,
    ):
        await session._record_span_count(root)

    flush.assert_called_once()
    (count,) = [s for s in exporter.get_finished_spans() if s.name == SPAN_COUNT]
    assert count.parent.span_id == root.get_span_context().span_id
    assert count.attributes[ATTR_DELIVERED] == 7
    gauge.assert_called_once_with("game_abc123", 7)
    assert not counts.is_watching(trace_id)


@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
def test_the_gauge_keeps_only_the_recent_games():
    from services.game_coordinator import metrics

    with (
        patch.object(metrics, "trace_spans_delivered") as gauge,
        patch.object(metrics, "_trace_spans_delivered_games", []),
    ):
        for i in range(metrics.TRACE_SPANS_DELIVERED_KEEP + 2):
            metrics.set_trace_spans_delivered(f"g{i}", i)
    assert [c.args for c in gauge.remove.call_args_list] == [("g0",), ("g1",)]
