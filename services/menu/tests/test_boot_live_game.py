"""UC-010 A16 (#2911): a menu that restarts while a game runs follows that game instead of entering the lobby."""

import asyncio
from unittest.mock import patch

import pytest

from proto import game_coordinator_pb2, menu_pb2
from services.menu import game_lifecycle
from services.menu.settings_ops import GAME_ACTIVE

DOWN = "down"


class _BootStub:
    """Fake coordinator stub: GetGameState walks ``answers`` (the last repeats); a ``DOWN`` answer raises.

    A game event stream answers its headers at once and delivers ``game_ended`` once ``end`` is set.
    """

    def __init__(self, *answers):
        self.answers = list(answers)
        self.state_requests = 0
        self.stream_requests = []
        self.end = asyncio.Event()

    def __call__(self, _channel):
        return self

    async def GetGameState(self, _request, **_kwargs):  # noqa: N802 - gRPC stub name
        self.state_requests += 1
        answer = self.answers.pop(0) if len(self.answers) > 1 else self.answers[0]
        if answer == DOWN:
            raise ConnectionError("coordinator unavailable")
        state, game_id = answer
        return game_coordinator_pb2.GetGameStateResponse(
            success=True,
            game_info=game_coordinator_pb2.GameInfo(state=game_coordinator_pb2.GameState.Value(state), game_id=game_id),
        )

    def StreamGameEvents(self, request, metadata=None):  # noqa: N802 - gRPC stub name
        self.stream_requests.append(request)
        stub = self

        class Stream:
            def cancel(self):
                pass

            async def initial_metadata(self):
                return ()

            async def _events(self):
                await stub.end.wait()
                yield game_coordinator_pb2.GameEvent(event_type="game_ended", game_id="g1", data={"winner": "s1"})
                await asyncio.Event().wait()

            def __aiter__(self):
                return self._events()

        return Stream()


@pytest.fixture
def stub():
    return _BootStub()


@pytest.fixture
def patched(stub):
    with (
        patch("proto.game_coordinator_pb2_grpc.GameCoordinatorServiceStub", stub),
        patch.object(game_lifecycle, "BOOT_GAME_LOOKUP_RETRY_SECONDS", 0.01),
    ):
        yield stub


def _published(servicer) -> list[tuple]:
    return [c.args for c in servicer.event_publisher.publish.await_args_list]


def _assert_no_lobby(servicer):
    servicer.controller_events.start.assert_not_awaited()
    servicer.audio.start_lobby_music.assert_not_awaited()
    servicer.idle_monitor.start.assert_not_called()
    assert "menu_started" not in [p[0] for p in _published(servicer)]


async def _lifecycle_end(servicer):
    await asyncio.wait_for(asyncio.shield(servicer._game_lifecycle_task), timeout=5)


@pytest.mark.asyncio
@pytest.mark.uc("UC-010", flow="A16", steps=[1, 4], br=["BR-008", "BR-009"])
async def test_live_game_at_boot_is_followed_without_lobby(real_servicer, patched):
    patched.answers = [("RUNNING", "g1")]

    assert await real_servicer.boot_follows_live_game() is True

    assert real_servicer.state == menu_pb2.MenuState.GAME_STARTING
    assert real_servicer._lifecycle_live()
    await asyncio.sleep(0.05)
    assert [r.game_id for r in patched.stream_requests] == ["g1"]
    assert real_servicer._followed_game_id == "g1"
    _assert_no_lobby(real_servicer)

    # A8: every start path is refused while the adopted game runs; StartMenu enters no lobby (A12).
    assert await real_servicer.start_game("web", source="web") is False
    assert ("game_start_rejected", {"game_name": "JoustFFA", "source": "web", "reason": "game_active"}) in _published(
        real_servicer
    )
    started = await real_servicer.StartMenu(menu_pb2.StartMenuRequest(), None)
    assert started.success
    reset = await real_servicer.ProcessInput(menu_pb2.ProcessInputRequest(input_type="reset_menu"), None)
    assert reset.error == GAME_ACTIVE
    _assert_no_lobby(real_servicer)
    assert len(patched.stream_requests) == 1

    patched.end.set()
    await _lifecycle_end(real_servicer)


@pytest.mark.asyncio
@pytest.mark.uc("UC-010", flow="A16", steps=[5, 6])
async def test_adopted_game_end_returns_to_the_lobby(real_servicer, patched):
    patched.answers = [("RUNNING", "g1")]
    assert await real_servicer.boot_follows_live_game() is True

    patched.end.set()
    await _lifecycle_end(real_servicer)

    assert real_servicer.state == menu_pb2.MenuState.RUNNING
    assert not real_servicer._lifecycle_live()
    real_servicer.controller_events.start.assert_awaited_once()
    real_servicer.audio.start_lobby_music.assert_awaited_once()
    real_servicer.idle_monitor.start.assert_called_once()
    assert ("game_ended", {"winner": "s1"}) in _published(real_servicer)


@pytest.mark.asyncio
@pytest.mark.uc("UC-010", flow="A16", steps=[4, 5], br=["BR-009"])
async def test_game_that_ended_before_the_subscription_restores_the_lobby(real_servicer, patched):
    """The game ends between the boot lookup and the subscription: the re-check finds it gone (A9)."""
    patched.answers = [("RUNNING", "g1"), ("ENDED", "g1")]

    assert await real_servicer.boot_follows_live_game() is True
    await _lifecycle_end(real_servicer)

    assert real_servicer.state == menu_pb2.MenuState.RUNNING
    assert ("game_ended", {"game_id": "g1", "reason": "ended_while_disconnected"}) in _published(real_servicer)
    real_servicer.audio.start_lobby_music.assert_awaited_once()


@pytest.mark.asyncio
@pytest.mark.uc("UC-010", flow="A16", steps=[3])
@pytest.mark.parametrize("state_name", ["IDLE", "ENDED"])
async def test_no_live_game_at_boot_leaves_the_lobby_to_the_boot(real_servicer, patched, state_name):
    patched.answers = [(state_name, "")]

    assert await real_servicer.boot_follows_live_game() is False

    assert patched.stream_requests == []
    assert not real_servicer._lifecycle_live()
    assert real_servicer.state == menu_pb2.MenuState.STOPPED
    assert real_servicer.boot_start_requested is False


@pytest.mark.asyncio
@pytest.mark.uc("UC-010", flow="A16", steps=[1, 2, 3], br=["BR-008"])
async def test_unreachable_coordinator_fails_closed_until_it_answers(real_servicer, patched):
    patched.answers = [DOWN]

    boot = asyncio.create_task(real_servicer.boot_follows_live_game())
    await asyncio.sleep(0.1)

    assert not boot.done()
    assert patched.state_requests > 1, "the lookup is not retried"
    assert await real_servicer.start_game("web", source="web") is False
    reset = await real_servicer.ProcessInput(menu_pb2.ProcessInputRequest(input_type="reset_menu"), None)
    assert reset.error == GAME_ACTIVE
    started = await real_servicer.StartMenu(menu_pb2.StartMenuRequest(), None)
    assert started.success
    assert real_servicer.boot_start_requested is True
    assert real_servicer.state == menu_pb2.MenuState.STOPPED
    _assert_no_lobby(real_servicer)

    patched.answers = [("IDLE", "")]
    assert await asyncio.wait_for(boot, timeout=5) is False
    assert not real_servicer._lifecycle_live()
    assert real_servicer._boot_lookup_running is False
