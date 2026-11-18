"""UC-100 (#2658 slice 3): the death follows the game's moment shape (BR-007).

The base kill path records the one ``player_death`` of a death (UC-030 step 9) through
``lib.moments.record_moment``: a span event by default (today's attributes plus the
moment identity, BR-004, NFR-060), a span ``game.player_death`` with start = end, a log
event in the player span's context, or both with one ``moment.id`` (BR-001, BR-003,
BR-005). The record keeps its place in ``_run_kill``: after the second gate check (a
declined kill records nothing), before the mode's handler (the mode extras describe
the state as if the player were already out). A failing log channel never raises into
the kill path, so a kill is never half-applied.

Real SDK tracer and logger providers with in-memory exporters (not the global ones).
"""

import logging
import sys
from pathlib import Path
from unittest.mock import patch

import pytest
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

test_dir = Path(__file__).parent
sys.path.insert(0, str(test_dir.parent.parent.parent))
sys.path.insert(0, str(test_dir))

from conftest import async_noop  # noqa: E402
from test_death_detection_uc030 import _game  # noqa: E402
from test_fight_club_elimination_order import FC_METRICS, _seated_game  # noqa: E402
from test_respawn_elimination_order import _nonstop, _swapper, _zombies  # noqa: E402

from lib import moments, otel_logging  # noqa: E402
from proto import controller_manager_pb2  # noqa: E402
from services.game_coordinator.games import base  # noqa: E402
from services.game_coordinator.games.base import Player  # noqa: E402
from services.game_coordinator.games.ffa import FFAGame  # noqa: E402

DEATH = "player_death"
TODAY = {"accel_magnitude", "sensitivity", "sensitivity_factor", "music_speed", "alive_remaining", "death.reason"}
IDENTITY = {"moment.id", "player.serial", "game.id", "game.kind"}
SHAPES = ["span_event", "span", "log_event", "both"]


class _Sdk:
    """Local SDK providers: finished spans, log records, and a tracer patched into ``games.base``."""

    def __init__(self):
        self.spans = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(self.spans))
        self.tracer = provider.get_tracer("test_death_moment")
        self.logs = InMemoryLogRecordExporter()
        self.log_provider = LoggerProvider()
        self.log_provider.add_log_record_processor(SimpleLogRecordProcessor(self.logs))

    def player_span(self, serial: str):
        return self.tracer.start_span("player_lifecycle", attributes={"player.serial": serial})

    def death_logs(self) -> list:
        return [r.log_record for r in self.logs.get_finished_logs() if r.log_record.event_name == DEATH]

    def death_spans(self) -> list:
        return [s for s in self.spans.get_finished_spans() if s.name == f"game.{DEATH}"]


@pytest.fixture
def sdk(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # conftest disables the SDK; these are local providers
    s = _Sdk()
    monkeypatch.setattr(otel_logging, "_logger_provider", s.log_provider)
    with patch.object(base, "tracer", s.tracer):
        yield s


def _events(span, name: str = DEATH) -> list[dict]:
    """Attributes of the ``name`` span events on a (live or ended) SDK span."""
    return [dict(e.attributes) for e in span.events if e.name == name]


def _ffa(sdk, count: int = 3, shape: str = "span_event"):
    game, _ = _game(count)
    game._play_sound = async_noop
    game.moment_shape = shape
    for serial, player in game.players.items():
        player.span = sdk.player_span(serial)
    return game


def _with_sdk_spans(game, sdk, shape: str):
    game.moment_shape = shape
    for serial, player in game.players.items():
        player.span = sdk.player_span(serial)


# --- T1-T4: one record per death, in the game's shape ----------------------------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-003", "BR-004", "BR-007"])
@pytest.mark.uc("UC-030", flow="main", steps=[9])
async def test_default_shape_keeps_the_span_event_and_adds_the_identity(sdk):
    game = _ffa(sdk)
    game.players["p0"].name = "Blue Phoenix"
    span = game.players["p0"].span

    assert await game._kill_player("p0", 3.0) is True

    (death,) = _events(span)
    assert set(death) == TODAY | IDENTITY | {"player.name"}
    assert death["moment.id"] == "test_uc030/p0/1"
    assert (death["player.serial"], death["player.name"]) == ("p0", "Blue Phoenix")
    assert (death["game.id"], death["game.kind"]) == ("test_uc030", "real")
    assert death["death.reason"] == "motion"
    assert sdk.death_logs() == [] and sdk.death_spans() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-003", "BR-007", "BR-010"])
async def test_span_shape_sends_a_point_in_time_game_player_death_span(sdk):
    game = _ffa(sdk, shape="span")
    span = game.players["p0"].span

    assert await game._kill_player("p0", 3.0) is True

    assert _events(span) == []
    (moment,) = sdk.death_spans()
    assert moment.start_time == moment.end_time
    assert moment.parent.span_id == span.get_span_context().span_id
    assert moment.context.trace_id == span.get_span_context().trace_id
    assert set(moment.attributes) == TODAY | IDENTITY
    assert moment.attributes["moment.id"] == "test_uc030/p0/1"
    assert sdk.death_logs() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-001", "BR-003", "BR-007"])
async def test_log_event_shape_sends_a_log_record_in_the_player_span_context(sdk):
    game = _ffa(sdk, shape="log_event")
    span = game.players["p0"].span

    assert await game._kill_player("p0", 3.0) is True

    assert _events(span) == []
    (record,) = sdk.death_logs()
    assert set(record.attributes) == TODAY | IDENTITY
    assert record.attributes["moment.id"] == "test_uc030/p0/1"
    assert record.span_id == span.get_span_context().span_id
    assert record.trace_id == span.get_span_context().trace_id
    assert sdk.death_spans() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A6", br=["BR-003", "BR-005", "BR-007"])
@pytest.mark.uc("UC-030", flow="main", steps=[9])
async def test_both_shape_sends_one_moment_id_as_span_event_and_log_event(sdk):
    game = _ffa(sdk, shape="both")
    span = game.players["p0"].span

    assert await game._kill_player("p0", 3.0) is True

    (event,) = _events(span)
    (record,) = sdk.death_logs()
    assert dict(record.attributes) == event
    assert event["moment.id"] == "test_uc030/p0/1"
    (stamp,) = [e.timestamp for e in span.events if e.name == DEATH]
    assert record.timestamp == stamp


# --- T5: one moment id per moment, across warnings, deaths and recreated players -------


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003"])
async def test_warnings_and_the_death_count_one_sequence_also_for_a_recreated_player(sdk):
    game = _ffa(sdk)
    span = game.players["p0"].span

    for _ in range(2):
        player = game.players["p0"]
        warn, _death = game._compute_effective_thresholds(player)
        player.warning_until = 0.0
        await game._warn_player("p0", warn + 0.1, warn)
    game.players["p0"] = Player(serial="p0", team=0, alive=True, color=(255, 0, 0), span=span)
    assert await game._kill_player("p0", 3.0) is True

    assert [w["moment.id"] for w in _events(span, "death_warning")] == ["test_uc030/p0/1", "test_uc030/p0/2"]
    assert [d["moment.id"] for d in _events(span)] == ["test_uc030/p0/3"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003", "BR-007"])
@pytest.mark.uc("UC-026", flow="main", steps=[2], br=["BR-004"])
async def test_each_nonstop_death_is_its_own_moment(sdk):
    game = await _nonstop(3)
    _with_sdk_spans(game, sdk, "log_event")
    serial = next(iter(game.players))

    for _ in range(2):
        assert await game._kill_player(serial, 5.0) is True
        assert await game._respawn_player(serial) is True

    ids = [r.attributes["moment.id"] for r in sdk.death_logs()]
    assert ids == [f"test_nonstop_no_elim/{serial}/1", f"test_nonstop_no_elim/{serial}/2"]


# --- T6: the mode extras ride on every shape, death.eliminated stays a bool -----------


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003", "BR-007"])
@pytest.mark.uc("UC-026", flow="main", steps=[2], br=["BR-004"])
async def test_nonstop_respawn_death_log_carries_its_extras(sdk):
    game = await _nonstop(3)
    _with_sdk_spans(game, sdk, "log_event")
    serial = next(iter(game.players))

    assert await game._kill_player(serial, 5.0) is True

    (record,) = sdk.death_logs()
    assert record.attributes["death.eliminated"] is False
    assert (record.attributes["kills"], record.attributes["deaths"], record.attributes["score"]) == (0, 1, 0)
    assert _events(game.players[serial].span) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003", "BR-007"])
@pytest.mark.uc("UC-023", flow="main", steps=[3], br=["BR-007"])
async def test_zombies_conversion_log_says_not_eliminated(sdk):
    game = await _zombies(6)
    _with_sdk_spans(game, sdk, "log_event")
    human = game.human_serials[0]

    assert await game._kill_player(human, 3.0) is True

    (record,) = sdk.death_logs()
    assert record.attributes["death.eliminated"] is False
    assert record.attributes["player.serial"] == human
    await game.cleanup()


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003", "BR-007"])
@pytest.mark.uc("UC-024", flow="main", steps=[2], br=["BR-005"])
async def test_swapper_swap_log_carries_the_team_extras(sdk):
    game = await _swapper(4)
    _with_sdk_spans(game, sdk, "log_event")
    serial = "mock_controller_0"
    old_span = game.players[serial].span

    assert await game._kill_player(serial, 3.0) is True

    (record,) = sdk.death_logs()
    assert record.attributes["death.eliminated"] is False
    assert {"team", "team_eliminated"} <= set(record.attributes)
    assert record.span_id == old_span.get_span_context().span_id
    await game.cleanup()


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-003", "BR-007"])
@pytest.mark.uc("UC-025", br=["BR-006"])
async def test_fight_club_round_loss_log_says_not_eliminated(sdk):
    game = await _seated_game(4, "test_fc_death_moment")
    _with_sdk_spans(game, sdk, "log_event")
    loser = game.current_fighter

    with patch(FC_METRICS):
        assert await game._kill_player(loser, 5.0) is True

    (record,) = sdk.death_logs()
    assert record.attributes["death.eliminated"] is False
    assert record.attributes["moment.id"] == f"test_fc_death_moment/{loser}/1"
    from test_fight_club import _end_round_timer

    _end_round_timer(game)


# --- T7: a kill declined after the explosion records nothing, in every shape ---------


class _GateClosesDuringTheExplosion(FFAGame):
    accept = True

    def _accepts_kill(self, serial: str) -> bool:  # noqa: ARG002
        return self.accept


@pytest.mark.asyncio
@pytest.mark.uc("UC-030", flow="main", steps=[9])
@pytest.mark.uc("UC-100", flow="main", steps=[4], br=["BR-007"])
@pytest.mark.parametrize("shape", SHAPES)
async def test_a_kill_declined_after_the_explosion_records_no_moment(sdk, shape):
    game = _ffa(sdk, shape=shape)
    game.__class__ = _GateClosesDuringTheExplosion

    async def explode(*_args, **_kwargs):
        game.accept = False  # e.g. the outcome latched while the explosion played

    game._play_sound = explode
    span = game.players["p0"].span

    assert await game._kill_player("p0", 3.0) is False

    assert _events(span) == []
    assert sdk.death_logs() == [] and sdk.death_spans() == []
    assert game._ground_truth.event_attributes("p0") == {"player.serial": "p0"}


# --- T8: a broken log channel never half-applies a kill -------------------------------


class _BrokenProcessor:
    def on_emit(self, log_record):  # noqa: ARG002
        raise RuntimeError("log processor broken")

    emit = on_emit

    def shutdown(self):
        pass

    def force_flush(self, timeout_millis=30000):  # noqa: ARG002
        return True


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-004"], cond=["Post-S-2", "Post-F-1"])
@pytest.mark.uc("UC-030", flow="main", steps=[9])
@pytest.mark.parametrize("shape", ["log_event", "both"])
async def test_a_broken_log_channel_never_half_applies_a_kill(sdk, monkeypatch, capsys, shape):
    """The provider raises on emit and the root OTLP handler (level ERROR) writes to the same provider.

    That is ``init_logging`` with ``log_level=error``: a report through Python ``logging``
    would raise again from the handler (#2658 opposer S1). Two deaths, one report.
    """
    from opentelemetry.sdk._logs import LoggingHandler

    broken = LoggerProvider()
    broken.add_log_record_processor(_BrokenProcessor())
    monkeypatch.setattr(otel_logging, "_logger_provider", broken)
    monkeypatch.setattr(moments, "_failure_reported", False)
    handler = LoggingHandler(level=logging.ERROR, logger_provider=broken)
    root = logging.getLogger()
    root.addHandler(handler)
    game = _ffa(sdk, count=4, shape=shape)
    try:
        for serial in ("p0", "p1"):
            assert await game._kill_player(serial, 3.0) is True
    finally:
        root.removeHandler(handler)

    for serial in ("p0", "p1"):
        assert game.players[serial].alive is False  # FFA's _kill_player_impl ran
    assert game.gameplay_stream.effects(controller_manager_pb2.GAME_EFFECT_PLAYER_DEATH) == ["p0", "p1"]
    assert game.dead_count == 2
    assert capsys.readouterr().err.count("moment could not be recorded") == 1
