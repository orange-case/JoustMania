"""UC-100 BR-012 / UC-041 A8 (#2722, #2783): the coordinator's agent-only log events.

Every game sends ``game.start`` and ``game.end`` in the root span's context, each carrying
``orangecase.audience`` = ``agent``; no flag gates them (#2783) and a process without OTLP
logging sends none. No moment is copied to the agent in any shape (it reads deaths from the
elimination-order gauge, UC-041 BR-009).

Real SDK tracer and logger providers with in-memory exporters (not the global ones).
"""

from unittest.mock import AsyncMock, create_autospec, patch

import pytest
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib import moments, otel_logging
from services.game_coordinator.games import base
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.tests.test_death_detection_uc030 import _game
from services.game_coordinator.tests.test_game_session import _make_session

AUDIENCE = {"orangecase.audience": "agent"}
RETIRED_FLAG = "telemetry.agent_log_events"
SHAPES = ["span_event", "span", "log_event"]


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
    """Stands in for the flag readers: answers the moment shape, records every key read."""

    def __init__(self, shape="span_event"):
        self.shape = shape
        self.keys: list[str] = []

    def string(self, domain, key, default, game_id=None):  # noqa: ARG002
        self.keys.append(key)
        return self.shape if key == "telemetry.moment_shape" else default

    def boolean(self, domain, key, default, game_id=None):  # noqa: ARG002
        self.keys.append(key)
        return default


async def _run_session(sdk, reads=None, run=None, build_error=None):
    session = _make_session()
    game = create_autospec(FFAGame, instance=True)
    game.run.side_effect = run
    game.get_game_name.return_value = "FFA"
    reads = reads or _FlagReads()
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


# --- the session: no flag, no root attribute, start and end in every game -----------------


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="main", steps=[3], br=["BR-002", "BR-012"])
async def test_session_reads_no_agent_log_events_flag_and_records_none_on_the_root(sdk):
    reads = _FlagReads()

    _, root, _ = await _run_session(sdk, reads)

    assert RETIRED_FLAG not in reads.keys
    assert reads.keys.count("telemetry.moment_shape") == 1
    assert RETIRED_FLAG not in root.attributes


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.uc("UC-041", flow="A8", steps=[1, 2, 4])
async def test_every_game_sends_game_start_and_game_end_in_the_root_context(sdk):
    session, root, _ = await _run_session(sdk)

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

    await _run_session(sdk, run=_boom)

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
        await _run_session(sdk, run=_force_ended)

    assert len(sdk.records("game.end")) == 1


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", flow="A1", br=["BR-012"])
async def test_without_otlp_logging_nothing_is_sent_and_the_game_runs(sdk, monkeypatch):
    monkeypatch.setattr(otel_logging, "_logger_provider", None)

    _, root, game = await _run_session(sdk)

    assert RETIRED_FLAG not in root.attributes
    game.run.assert_awaited_once()
    assert sdk.records() == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", steps=[4])
async def test_a_game_never_built_sends_no_agent_record(sdk):
    """game.end only pairs with a sent game.start."""
    await _run_session(sdk, build_error=ValueError("unknown mode"))

    assert sdk.records() == []


# --- the moments: never copied to the agent ----------------------------------------------


def _ffa(sdk, shape):
    game, _ = _game(3)
    game._play_sound = AsyncMock()
    game.moment_shape = shape
    for serial, player in game.players.items():
        player.span = sdk.tracer.start_span("game.player", attributes={"player.serial": serial})
    return game


async def _warn(game, serial="p0"):
    player = game.players[serial]
    warn, _death = game._compute_effective_thresholds(player)
    player.warning_until = 0.0
    await game._warn_player(serial, warn + 0.1, warn)


@pytest.mark.asyncio
@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.uc("UC-041", br=["BR-009"])
@pytest.mark.parametrize("shape", SHAPES)
async def test_no_moment_is_copied_to_the_agent(sdk, shape):
    """The warning and the death go out only in the game's shape, never marked."""
    game = _ffa(sdk, shape)

    await _warn(game)
    assert await game._kill_player("p0", 3.0) is True

    for name in ("death_warning", "player_death"):
        records = sdk.records(name)
        assert len(records) == (1 if shape == "log_event" else 0)
        assert all("orangecase.audience" not in r.attributes for r in records)


@pytest.mark.asyncio
@pytest.mark.nfr("NFR-060")
@pytest.mark.uc("UC-100", flow="A1", br=["BR-012"])
@pytest.mark.parametrize("shape", ["span_event", "span"])
async def test_without_otlp_logging_the_death_still_lands(sdk, monkeypatch, shape):
    monkeypatch.setattr(otel_logging, "_logger_provider", None)
    game = _ffa(sdk, shape)

    await _warn(game)
    assert await game._kill_player("p0", 3.0) is True

    assert sdk.records() == []


def test_a_new_game_has_no_agent_log_events_switch():
    game, _ = _game(2)

    assert not hasattr(game, "agent_log_events")


@pytest.mark.asyncio
@pytest.mark.uc("UC-041", flow="A8", br=["BR-004"])
async def test_game_start_is_flushed_and_game_end_and_moments_are_not(sdk):
    flushed: list[str] = []
    with patch.object(moments, "_flush_in_background", flushed.append):
        await _run_session(sdk)
        game = _ffa(sdk, "span_event")
        await _warn(game)
        assert await game._kill_player("p0", 3.0) is True

    assert flushed == ["game.start"]
