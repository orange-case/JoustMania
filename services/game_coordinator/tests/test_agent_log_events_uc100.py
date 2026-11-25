"""UC-100 BR-012 / UC-041 A8 (#2722): the coordinator's agent-only log events.

``telemetry.agent_log_events`` is read once per game next to the moment shape, recorded
on the game root and held for the game. On, the coordinator sends ``game.start`` and
``game.end`` in the root span's context and, in the ``span_event`` and ``span`` shapes, a
log copy of each warning and death; every such record carries ``orangecase.audience`` =
``agent``. Off (the default), nothing new is sent.

Real SDK tracer and logger providers with in-memory exporters (not the global ones).
"""

from unittest.mock import AsyncMock, create_autospec, patch

import pytest
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib import otel_logging
from services.game_coordinator.games import base
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.tests.test_death_detection_uc030 import _game
from services.game_coordinator.tests.test_game_session import _make_session

AUDIENCE = {"orangecase.audience": "agent"}
SHAPES = ["span_event", "span", "log_event", "both"]


class _Sdk:
    def __init__(self):
        self.spans = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(self.spans))
        self.tracer = provider.get_tracer("test_agent_log_events")
        self.logs = InMemoryLogRecordExporter()
        self.log_provider = LoggerProvider()
        self.log_provider.add_log_record_processor(SimpleLogRecordProcessor(self.logs))

    def records(self, name=None) -> list:
        return [r.log_record for r in self.logs.get_finished_logs() if name is None or r.log_record.event_name == name]


@pytest.fixture
def sdk(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # conftest disables the SDK; these are local providers
    s = _Sdk()
    monkeypatch.setattr(otel_logging, "_logger_provider", s.log_provider)
    with patch.object(base, "tracer", s.tracer):
        yield s


class _FlagReads:
    """Stands in for the flag readers: answers the two per-game flags, counts their reads."""

    def __init__(self, shape="span_event", agent_log_events=False):
        self.shape = shape
        self.agent_log_events = agent_log_events
        self.reads = {"telemetry.moment_shape": 0, "telemetry.agent_log_events": 0}

    def string(self, domain, key, default, game_id=None):  # noqa: ARG002
        if key == "telemetry.moment_shape":
            self.reads[key] += 1
            return self.shape
        return default

    def boolean(self, domain, key, default, game_id=None):  # noqa: ARG002
        if key == "telemetry.agent_log_events":
            self.reads[key] += 1
            return self.agent_log_events
        return default


async def _run_session(sdk, reads, run=None, build_error=None):
    session = _make_session()
    game = create_autospec(FFAGame, instance=True)
    game.run.side_effect = run
    game.get_game_name.return_value = "FFA"
    with (
        patch("services.game_coordinator.game_session.tracer", sdk.tracer),
        patch("services.game_coordinator.game_session.GameFactory") as factory,
        patch("lib.feature_flags.read_string_flag", reads.string),
        patch("lib.feature_flags.read_bool_flag", reads.boolean),
    ):
        factory.create_game.return_value = game
        factory.create_game.side_effect = build_error
        await session._run_game_loop_async()
    (root,) = [s for s in sdk.spans.get_finished_spans() if s.parent is None]
    return session, root, game


# --- the session: one read per game, recorded on the root, held for the game ----------


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[3], br=["BR-002", "BR-012"])
@pytest.mark.parametrize("value", [True, False])
async def test_session_reads_the_flag_once_and_records_it_on_the_game_root(sdk, value):
    reads = _FlagReads(agent_log_events=value)

    _, root, game = await _run_session(sdk, reads)

    assert reads.reads == {"telemetry.moment_shape": 1, "telemetry.agent_log_events": 1}
    assert root.attributes["telemetry.agent_log_events"] is value
    assert game.agent_log_events is value


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-012"])
async def test_flag_off_sends_no_new_record(sdk):
    await _run_session(sdk, _FlagReads(agent_log_events=False))

    assert sdk.records() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.uc("UC-041", flow="A8", steps=[1, 2, 4])
async def test_flag_on_sends_game_start_and_game_end_in_the_root_context(sdk):
    session, root, _ = await _run_session(sdk, _FlagReads(agent_log_events=True))

    (start,) = sdk.records("game.start")
    (end,) = sdk.records("game.end")
    assert dict(start.attributes) == {
        "game.id": session.game_id,
        "game.kind": session.game_kind,
        "game.mode": "FFA",
        "game.name": session.game_name,
        **AUDIENCE,
    }
    assert dict(end.attributes) == {"game.id": session.game_id, "game.kind": session.game_kind, **AUDIENCE}
    for record in (start, end):
        assert (record.trace_id, record.span_id) == (root.context.trace_id, root.context.span_id)
    assert start.timestamp <= end.timestamp


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", steps=[4])
async def test_game_end_is_sent_once_also_when_the_game_fails(sdk):
    def _boom():
        raise RuntimeError("mode failed")

    await _run_session(sdk, _FlagReads(agent_log_events=True), run=_boom)

    assert len(sdk.records("game.start")) == 1
    assert len(sdk.records("game.end")) == 1


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", steps=[4])
async def test_game_end_is_sent_once_when_the_game_is_force_ended(sdk):
    """A force-end makes the mode's run return early; the session's end is the same path."""
    holder = {}

    async def _force_ended():
        session = holder["session"]
        with session._state_lock:
            session.game_running = False

    orig = _make_session

    def _capture(*args, **kwargs):
        holder["session"] = orig(*args, **kwargs)
        return holder["session"]

    with patch(f"{__name__}._make_session", _capture):
        await _run_session(sdk, _FlagReads(agent_log_events=True), run=_force_ended)

    assert len(sdk.records("game.end")) == 1


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A1", br=["BR-012"])
async def test_without_otlp_logging_the_flag_reads_off_and_nothing_is_sent(sdk, monkeypatch):
    monkeypatch.setattr(otel_logging, "_logger_provider", None)

    _, root, game = await _run_session(sdk, _FlagReads(agent_log_events=True))

    assert root.attributes["telemetry.agent_log_events"] is False
    assert game.agent_log_events is False
    assert sdk.records() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", steps=[4])
async def test_a_game_never_built_sends_no_agent_record(sdk):
    """game.end only pairs with a sent game.start."""
    await _run_session(sdk, _FlagReads(agent_log_events=True), build_error=ValueError("unknown mode"))

    assert sdk.records() == []


# --- the moments: a marked copy in the span shapes only -----------------------------------


def _ffa(sdk, shape, agent_log_events):
    game, _ = _game(3)
    game._play_sound = AsyncMock()
    game.moment_shape = shape
    game.agent_log_events = agent_log_events
    for serial, player in game.players.items():
        player.span = sdk.tracer.start_span("player_lifecycle", attributes={"player.serial": serial})
    return game


async def _warn(game, serial="p0"):
    player = game.players[serial]
    warn, _death = game._compute_effective_thresholds(player)
    player.warning_until = 0.0
    await game._warn_player(serial, warn + 0.1, warn)


def _moment(sdk, span, name, shape):
    """The moment's attributes and time as its own record in ``shape`` carries them."""
    if shape == "span":
        (moment,) = [s for s in sdk.spans.get_finished_spans() if s.name == f"game.{name}"]
        return dict(moment.attributes), moment.start_time
    (event,) = [e for e in span.events if e.name == name]
    return dict(event.attributes), event.timestamp


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.uc("UC-041", flow="A8", steps=[3])
@pytest.mark.parametrize("shape", ["span_event", "span"])
async def test_span_shapes_send_a_marked_log_copy_of_the_warning_and_the_death(sdk, shape):
    game = _ffa(sdk, shape, agent_log_events=True)
    span = game.players["p0"].span

    await _warn(game)
    assert await game._kill_player("p0", 3.0) is True

    for name in ("death_warning", "player_death"):
        (copy,) = sdk.records(name)
        expected, moment_time = _moment(sdk, span, name, shape)
        assert dict(copy.attributes) == {**expected, **AUDIENCE}
        assert copy.attributes["moment.id"] == expected["moment.id"]
        assert copy.timestamp == moment_time
        assert (copy.trace_id, copy.span_id) == (span.get_span_context().trace_id, span.get_span_context().span_id)


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.parametrize("shape", ["log_event", "both"])
async def test_log_shapes_send_no_copy(sdk, shape):
    game = _ffa(sdk, shape, agent_log_events=True)

    await _warn(game)
    assert await game._kill_player("p0", 3.0) is True

    for name in ("death_warning", "player_death"):
        (record,) = sdk.records(name)
        assert "orangecase.audience" not in record.attributes


@pytest.mark.asyncio
@pytest.mark.nfr("NFR-060")
@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.parametrize("shape", ["span_event", "span"])
async def test_flag_off_sends_no_moment_copy(sdk, shape):
    game = _ffa(sdk, shape, agent_log_events=False)

    await _warn(game)
    assert await game._kill_player("p0", 3.0) is True

    assert sdk.records() == []


def test_a_new_game_holds_the_flag_off():
    game, _ = _game(2)

    assert game.agent_log_events is False
