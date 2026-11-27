"""UC-103 (#2574): per-sample spans (BR-001, FR-224) and the coordinator's delivered count (BR-003, FR-246)."""

import threading
import time
from types import SimpleNamespace
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


class _Flags:
    """``read_bool_flag`` / ``read_string_flag`` stand-ins: the round's two flags, defaults otherwise."""

    def __init__(self, per_sample: bool, challenge: str) -> None:
        self.per_sample, self.challenge = per_sample, challenge
        self.per_sample_reads = 0

    def bool_flag(self, domain, key, default, game_id=None):
        if key == "telemetry.per_sample_spans":
            self.per_sample_reads += 1
            return self.per_sample
        return default

    def string_flag(self, domain, key, default, game_id=None):
        return self.challenge if key == "stowaway.challenge" else default


async def _run_session(per_sample: bool, outcome: str = "winner", challenge: str = "none", flush=None):
    session = _make_session()
    tracer = _tracer_mock()
    root = tracer.start_as_current_span.return_value.__enter__.return_value
    game = MagicMock()
    game.run = AsyncMock()
    game.game_outcome.return_value = outcome
    flags = _Flags(per_sample, challenge)
    counts = DeliveredSpans()
    counts.active = True
    with (
        patch("services.game_coordinator.game_session.tracer") as mock_tracer,
        patch("services.game_coordinator.game_session.GameFactory") as factory,
        patch("lib.feature_flags.read_bool_flag", flags.bool_flag),
        patch("lib.feature_flags.read_string_flag", flags.string_flag),
        patch("services.game_coordinator.game_session.DELIVERED", counts),
        patch("services.game_coordinator.game_session.flush_spans", flush or MagicMock(return_value=True)),
        patch("services.game_coordinator.game_session.metrics.set_trace_spans_delivered") as gauge,
    ):
        mock_tracer.start_as_current_span = tracer.start_as_current_span
        factory.create_game.return_value = game
        started = time.monotonic()
        await session._run_game_loop_async()
        elapsed = time.monotonic() - started
        if flush is None and session.span_count_thread is not None:
            session.span_count_thread.join(5)
    return SimpleNamespace(
        root=root, game=game, flags=flags, session=session, counts=counts, gauge=gauge, elapsed=elapsed
    )


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="main", steps=[1, 3], br=["BR-001"])
@pytest.mark.parametrize("value", [True, False])
async def test_the_session_reads_the_flag_once_and_records_it_on_the_root(value):
    run = await _run_session(value)

    assert run.flags.per_sample_reads == 1
    assert run.root.attributes["telemetry.per_sample_spans"] is value
    assert run.game.per_sample_spans is value


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", cond=["Post-F-3"], br=["BR-003"])
async def test_an_ordinary_game_takes_no_count():
    run = await _run_session(False)

    assert run.session.span_count_thread is None
    assert not run.counts.is_watching(run.root.get_span_context().trace_id)
    run.gauge.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
@pytest.mark.parametrize(("per_sample", "challenge"), [(True, "none"), (False, "span-rounds")])
async def test_a_span_round_records_the_count_after_a_normal_end(per_sample, challenge):
    run = await _run_session(per_sample, challenge=challenge)

    run.gauge.assert_called_once_with("game_abc123", 0)
    assert not run.counts.is_watching(run.root.get_span_context().trace_id)


@pytest.mark.asyncio
@pytest.mark.uc("UC-103", flow="A1", br=["BR-003"])
async def test_a_force_ended_game_has_no_count():
    run = await _run_session(True, outcome="force_ended")

    assert run.session.span_count_thread is None
    assert not run.counts.is_watching(run.root.get_span_context().trace_id)
    run.gauge.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.nfr("NFR-062")
@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
async def test_a_slow_flush_never_delays_the_game_end():
    release = threading.Event()
    flush = MagicMock(side_effect=lambda: release.wait(10))

    run = await _run_session(True, flush=flush)
    try:
        assert run.elapsed < 1.0
        assert run.session.span_count_thread.is_alive()  # still waiting on the flush
        run.gauge.assert_not_called()
    finally:
        release.set()
        run.session.span_count_thread.join(5)
    run.gauge.assert_called_once_with("game_abc123", 0)


@pytest.mark.uc("UC-103", flow="A1", br=["BR-003"])
def test_a_game_that_returned_early_leaves_no_watched_trace():
    session = _make_session()
    counts = DeliveredSpans()
    counts.active = True
    counts.watch(42)
    session._counted_trace_id = 42
    with patch("services.game_coordinator.game_session.DELIVERED", counts):
        session._discard_span_count()
    assert not counts.is_watching(42)
    assert session._counted_trace_id is None


@pytest.mark.uc("UC-103", flow="main", steps=[5], br=["BR-003"])
def test_the_count_span_and_gauge_carry_the_exported_spans(exporter):
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
        session._record_span_count(root, trace_id)

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
