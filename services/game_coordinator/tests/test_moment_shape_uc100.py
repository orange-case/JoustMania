"""UC-100 (#2658 slice 1): the warning follows the game's moment shape.

- the session reads ``telemetry.moment_shape`` once per game inside the game root span,
  records the shape in effect on it and hands it to the game (BR-002, A1);
- the game's ``death_warning`` goes through ``lib.moments.record_moment`` with today's
  attributes plus the identity keys (BR-003, BR-004, NFR-060);
- ``moment.id`` is unique per warning within the game, kept by the game per serial;
- a flag change mid-game has no effect (A2): the frame path never reads the flag.
"""

from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib import otel_logging
from services.game_coordinator.games.base import Player
from services.game_coordinator.tests.test_death_detection_uc030 import _game, _span_events
from services.game_coordinator.tests.test_game_session import _make_session, _tracer_mock

TODAY = {"accel_magnitude", "threshold", "sensitivity", "sensitivity_factor", "music_speed"}
IDENTITY = {"moment.id", "player.serial", "game.id", "game.kind"}


class _FlagReads:
    """Stands in for ``lib.feature_flags.read_string_flag``: answers one moment shape, counts reads."""

    def __init__(self, value):
        self.value = value
        self.moment_reads = 0

    def __call__(self, domain, key, default, game_id=None):
        if key == "telemetry.moment_shape":
            self.moment_reads += 1
            return self.value
        return default


@pytest.fixture
def otlp_logs(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # conftest disables the SDK; these are local providers
    exporter = InMemoryLogRecordExporter()
    provider = LoggerProvider()
    provider.add_log_record_processor(SimpleLogRecordProcessor(exporter))
    monkeypatch.setattr(otel_logging, "_logger_provider", provider)
    return exporter


async def _warn(game, serial="p0"):
    """One warning for ``serial`` (the warning window is cleared so the next call warns again)."""
    player = game.players[serial]
    warn, _death = game._compute_effective_thresholds(player)
    player.warning_until = 0.0
    await game._warn_player(serial, warn + 0.1, warn)


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A1", br=["BR-003", "BR-004"])
async def test_default_shape_keeps_the_span_event_and_adds_the_identity_keys():
    game, _ = _game(2)
    game.players["p0"].name = "Blue Phoenix"

    await _warn(game)

    (warning,) = _span_events(game.players["p0"].span, "death_warning")
    assert set(warning) == TODAY | IDENTITY | {"player.name"}
    assert warning["moment.id"] == "test_uc030/p0/1"
    assert warning["player.serial"] == "p0"
    assert warning["player.name"] == "Blue Phoenix"
    assert warning["game.id"] == "test_uc030"
    assert warning["game.kind"] == "real"


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003"])
async def test_moment_id_is_unique_per_warning_also_when_the_player_is_recreated():
    game, _ = _game(2)

    await _warn(game, "p0")
    await _warn(game, "p0")
    await _warn(game, "p1")
    old_span = game.players["p0"].span
    game.players["p0"] = Player(serial="p0", team=0, alive=True, color=(255, 0, 0))
    game.players["p0"].span = old_span
    await _warn(game, "p0")

    ids = [w["moment.id"] for w in _span_events(old_span, "death_warning")]
    assert ids == ["test_uc030/p0/1", "test_uc030/p0/2", "test_uc030/p0/3"]
    assert [w["moment.id"] for w in _span_events(game.players["p1"].span, "death_warning")] == ["test_uc030/p1/1"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-003"])
async def test_span_shape_sends_a_game_death_warning_span_and_no_span_event(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    game, _ = _game(2)
    game.moment_shape = "span"

    with patch("services.game_coordinator.games.base.tracer", provider.get_tracer("test")):
        await _warn(game)

    assert _span_events(game.players["p0"].span, "death_warning") == []
    (moment,) = exporter.get_finished_spans()
    assert moment.name == "game.death_warning"
    assert set(moment.attributes) == TODAY | IDENTITY
    assert moment.attributes["moment.id"] == "test_uc030/p0/1"
    assert moment.end_time - moment.start_time == 500_000_000


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A6", br=["BR-003", "BR-005"])
async def test_both_shape_sends_one_moment_id_as_span_event_and_log_event(otlp_logs):
    game, _ = _game(2)
    game.moment_shape = "both"

    await _warn(game)

    (event,) = _span_events(game.players["p0"].span, "death_warning")
    (record,) = [r.log_record for r in otlp_logs.get_finished_logs()]
    assert record.event_name == "death_warning"
    assert dict(record.attributes) == event
    assert record.attributes["moment.id"] == event["moment.id"] == "test_uc030/p0/1"


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A2", br=["BR-002"])
async def test_flag_change_mid_game_has_no_effect(otlp_logs):
    """The game holds its shape: the warning path never reads the flag."""
    game, _ = _game(2)
    game.moment_shape = "log_event"
    reads = _FlagReads("span")

    with patch("lib.feature_flags.read_string_flag", reads):
        await _warn(game)
        await _warn(game)

    assert reads.moment_reads == 0
    assert [r.log_record.event_name for r in otlp_logs.get_finished_logs()] == ["death_warning", "death_warning"]
    assert _span_events(game.players["p0"].span, "death_warning") == []


async def _run_session(flag_value):
    session = _make_session()
    tracer = _tracer_mock()
    root = tracer.start_as_current_span.return_value.__enter__.return_value
    game = MagicMock()
    game.run = AsyncMock()
    reads = _FlagReads(flag_value)
    with (
        patch("services.game_coordinator.game_session.tracer") as mock_tracer,
        patch("services.game_coordinator.game_session.GameFactory") as factory,
        patch("lib.feature_flags.read_string_flag", reads),
    ):
        mock_tracer.start_as_current_span = tracer.start_as_current_span
        factory.create_game.return_value = game
        await session._run_game_loop_async()
    return root, game, reads


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[3], br=["BR-002"])
@pytest.mark.parametrize("shape", ["span_event", "span", "log_event", "both"])
@pytest.mark.usefixtures("otlp_logs")
async def test_session_reads_the_shape_once_and_records_it_on_the_game_root(shape):
    root, game, reads = await _run_session(shape)

    assert reads.moment_reads == 1
    assert root.attributes["telemetry.moment_shape"] == shape
    assert game.moment_shape == shape


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A1", br=["BR-004"])
@pytest.mark.parametrize("value", ["", "spans", None])
@pytest.mark.usefixtures("otlp_logs")
async def test_absent_or_unknown_flag_runs_the_game_as_span_event(value):
    root, game, _ = await _run_session(value)

    assert root.attributes["telemetry.moment_shape"] == "span_event"
    assert game.moment_shape == "span_event"


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A1", br=["BR-001"])
async def test_without_otlp_logging_log_event_runs_and_is_recorded_as_span_event(monkeypatch):
    monkeypatch.setattr(otel_logging, "_logger_provider", None)

    root, game, _ = await _run_session("log_event")

    assert root.attributes["telemetry.moment_shape"] == "span_event"
    assert game.moment_shape == "span_event"
