"""Ground truth of a challenge game (UC-100 step 6, A5, BR-006; FR-219; #2658).

At a normal game end the game root span carries one ``challenge.ground_truth``
span event per player with the time of each warning and of each death, taken from
the clock of the moment records (the OTel SDK's ``time_ns``). A force-ended game
or one whose ``run()`` raised records none.
"""

import asyncio
import sys
from contextlib import ExitStack
from pathlib import Path
from unittest.mock import patch

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

test_dir = Path(__file__).parent
sys.path.insert(0, str(test_dir.parent.parent.parent))
sys.path.insert(0, str(test_dir))

from conftest import EventCollector, MockControllerManagerService, async_noop  # noqa: E402

from lib.types import GameEvent  # noqa: E402
from services.game_coordinator.games import base, ground_truth, nonstop_joust  # noqa: E402
from services.game_coordinator.games.base import GameState  # noqa: E402
from services.game_coordinator.games.ffa import FFAGame  # noqa: E402
from services.game_coordinator.games.nonstop_joust import NonstopJoustGame  # noqa: E402
from services.game_coordinator.runtime_config import GamePerformanceConfig, get_config_manager  # noqa: E402

EVENT = "challenge.ground_truth"
WARNINGS = "challenge.warning_times_ns"
DEATHS = "challenge.death_times_ns"
SERIALS = [f"mock_controller_{i}" for i in range(3)]


class _Stream:
    def __init__(self):
        self.messages = []

    async def write(self, message):
        self.messages.append(message)


@pytest.fixture
def tracer(monkeypatch):
    """An SDK tracer behind the game modules, no countdown and a short rainbow."""
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    sdk_tracer = provider.get_tracer("test_ground_truth")
    sdk_tracer.exporter = exporter
    config = GamePerformanceConfig(countdown_phase_duration_ms=0, winner_rainbow_duration_ms=50)
    with ExitStack() as stack:
        for module in (base, nonstop_joust):
            stack.enter_context(patch.object(module, "tracer", sdk_tracer))
        stack.enter_context(patch.object(get_config_manager(), "get_config", return_value=config))
        yield sdk_tracer


class _FrozenClock:
    """One wall clock for the SDK's span events and the ground truth, set by the test."""

    def __init__(self, monkeypatch):
        self.now = 1_700_000_000_000_000_000
        monkeypatch.setattr("opentelemetry.sdk.trace.time_ns", lambda: self.now)
        monkeypatch.setattr(ground_truth, "time_ns", lambda: self.now)

    def at(self, seconds: float) -> int:
        self.now = 1_700_000_000_000_000_000 + int(seconds * 1e9)
        return self.now


async def _game(cls, tracer, num_players=3):
    client = MockControllerManagerService(num_controllers=num_players)
    game = cls(
        controller_manager_client=client,
        event_publisher=EventCollector().publish,
        audio_client=None,
        game_id=f"test_ground_truth_{cls.__name__.lower()}",
    )
    await game._initialize_players_impl(client.controllers)
    game.gameplay_stream = _Stream()
    game.state = GameState.RUNNING
    game.running = True
    game._play_sound = async_noop
    for serial, player in game.players.items():
        player.span = tracer.start_span("player_lifecycle", attributes={"player.serial": serial})
    game._game_root_span = tracer.start_span("root")
    return game


def _truth(root) -> dict[str, dict]:
    """{serial: attributes} of the root's ground-truth events (one per serial asserted)."""
    events = [e for e in root.events if e.name == EVENT]
    by_serial = {e.attributes["player.serial"]: dict(e.attributes) for e in events}
    assert len(by_serial) == len(events), "one ground-truth event per player"
    return by_serial


def _stamps(span, name) -> list[int]:
    return [e.timestamp for e in span.events if e.name == name]


class TestGroundTruthAtNormalEnd:
    @pytest.mark.uc("UC-100", flow="main", steps=[6], br=["BR-006"])
    @pytest.mark.asyncio
    async def test_ffa_records_warnings_and_death_with_the_record_timestamps(self, tracer, monkeypatch):
        """Warned twice then dead, warned once and the winner, never warned: times = the moment records'."""
        clock = _FrozenClock(monkeypatch)
        game = await _game(FFAGame, tracer)
        p0, p1, p2 = SERIALS
        spans = {s: game.players[s].span for s in SERIALS}

        clock.at(1.0)
        await game._warn_player(p0, 1.8, 1.5)
        clock.at(2.5)
        await game._warn_player(p0, 1.9, 1.5)
        clock.at(3.0)
        await game._warn_player(p1, 1.7, 1.5)
        clock.at(4.0)
        assert await game._kill_player(p0, 3.0) is True
        clock.at(5.0)
        assert await game._kill_player(p2, 3.0) is True
        game._latch_game_outcome()
        game._record_ground_truth()

        truth = _truth(game._game_root_span)
        assert set(truth) == set(SERIALS)
        assert truth[p0][WARNINGS] == (clock.at(1.0), clock.at(2.5))
        assert truth[p0][WARNINGS] == tuple(_stamps(spans[p0], "death_warning"))
        assert truth[p0][DEATHS] == tuple(_stamps(spans[p0], "player_death")) == (clock.at(4.0),)
        assert truth[p1][WARNINGS] == tuple(_stamps(spans[p1], "death_warning")) == (clock.at(3.0),)
        assert DEATHS not in truth[p1], "a player with no death has no death attribute"
        assert truth[p2][DEATHS] == (clock.at(5.0),)
        assert WARNINGS not in truth[p2]

    @pytest.mark.uc("UC-100", flow="main", steps=[6], br=["BR-006"])
    @pytest.mark.asyncio
    async def test_nonstop_records_every_death(self, tracer, monkeypatch):
        """A mode with respawns records each death time of a player, in order."""
        clock = _FrozenClock(monkeypatch)
        game = await _game(NonstopJoustGame, tracer)
        serial = SERIALS[0]
        for second in (1.0, 2.0, 3.0):
            clock.at(second)
            assert await game._kill_player(serial, 5.0) is True
            assert await game._respawn_player(serial) is True
        game._latch_game_outcome()
        game._record_ground_truth()

        deaths = _truth(game._game_root_span)[serial][DEATHS]
        assert deaths == (clock.at(1.0), clock.at(2.0), clock.at(3.0))
        assert deaths == tuple(_stamps(game.players[serial].span, "player_death"))

    @pytest.mark.uc("UC-100", flow="main", steps=[6], br=["BR-006"])
    @pytest.mark.asyncio
    async def test_real_ffa_run_records_ground_truth_on_the_root(self, tracer):
        """A real run() to a winner: the root gets one event per player, deaths at the player_death stamps."""
        events = EventCollector()
        game = FFAGame(
            controller_manager_client=MockControllerManagerService(
                num_controllers=3, death_schedule={0.3: 1, 0.6: 2}, infinite=True
            ),
            event_publisher=events.publish,
            audio_client=None,
            game_id="test_ground_truth_run",
            initial_players=[type("P", (), {"serial": s})() for s in SERIALS],
        )
        with tracer.start_as_current_span("Joust Free-for-All") as root:
            await asyncio.wait_for(game.run(), timeout=20)

        assert root.attributes["game.outcome"] == "winner"
        truth = _truth(root)
        assert set(truth) == set(SERIALS)
        player_spans = {
            s.attributes["player.serial"]: s
            for s in tracer.exporter.get_finished_spans()
            if s.name.startswith("player") and "player.serial" in (s.attributes or {})
        }
        for serial in SERIALS[1:]:
            (death,) = truth[serial][DEATHS]
            (stamp,) = _stamps(player_spans[serial], "player_death")
            # Same clock, read next to the record: well under BR-011's 0.1 s.
            assert abs(death - stamp) < 10_000_000, serial
        assert DEATHS not in truth[SERIALS[0]]


class TestNoGroundTruthWithoutNormalEnd:
    @pytest.mark.uc("UC-100", flow="A5", br=["BR-006"])
    @pytest.mark.uc("UC-027", flow="main", steps=[3])
    @pytest.mark.asyncio
    async def test_force_ended_run_records_none(self, tracer):
        events = EventCollector()
        game = FFAGame(
            controller_manager_client=MockControllerManagerService(num_controllers=3, infinite=True),
            event_publisher=events.publish,
            audio_client=None,
            game_id="test_ground_truth_force_end",
            initial_players=[type("P", (), {"serial": s})() for s in SERIALS],
        )
        with tracer.start_as_current_span("Joust Free-for-All") as root:
            task = asyncio.create_task(game.run())
            for _ in range(200):
                if events.get_events_of_type(GameEvent.GAME_STARTED):
                    break
                await asyncio.sleep(0.05)
            await game._warn_player(SERIALS[0], 1.8, 1.5)
            game.force_end("operator_stop")
            await asyncio.wait_for(task, timeout=10)

        assert root.attributes["game.outcome"] == "force_ended"
        assert [e for e in root.events if e.name == EVENT] == []

    @pytest.mark.uc("UC-100", flow="A5", br=["BR-006"])
    @pytest.mark.asyncio
    async def test_run_that_raised_records_none(self, tracer):
        game = FFAGame(
            controller_manager_client=MockControllerManagerService(
                num_controllers=3, death_schedule={0.3: 1, 0.6: 2}, infinite=True
            ),
            event_publisher=EventCollector().publish,
            audio_client=None,
            game_id="test_ground_truth_raised",
            initial_players=[type("P", (), {"serial": s})() for s in SERIALS],
        )

        def _boom():
            raise RuntimeError("teardown failed")

        game._teardown_after_outcome = _boom
        with tracer.start_as_current_span("Joust Free-for-All") as root:
            with pytest.raises(RuntimeError):
                await asyncio.wait_for(game.run(), timeout=20)

        assert [e for e in root.events if e.name == EVENT] == []

    @pytest.mark.uc("UC-100", flow="A5", br=["BR-006"])
    @pytest.mark.asyncio
    async def test_force_ended_outcome_records_none_even_if_asked(self, tracer):
        game = await _game(FFAGame, tracer)
        await game._warn_player(SERIALS[0], 1.8, 1.5)
        game.force_end("operator_stop")
        game._latch_game_outcome()
        game._record_ground_truth()
        assert [e for e in game._game_root_span.events if e.name == EVENT] == []
