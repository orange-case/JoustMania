"""Menu boot and lifecycle edge cases left after the boot lookup and the readiness re-check (#2938)."""

import asyncio
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from opentelemetry import trace

from lib.types import Games
from proto import menu_pb2
from services.menu.handlers.base import ControllerState
from services.menu.idle_monitor import IdleMonitor


async def _connect(servicer, *serials: str) -> None:
    for serial in serials:
        await servicer.on_connect(serial)


async def _until(predicate) -> None:
    for _ in range(200):
        if predicate():
            return
        await asyncio.sleep(0.01)
    raise AssertionError("condition not reached")


class TestStartDuringLobbyEntry:
    """UC-010 A8: a start that lands while the lobby is being entered runs after the entry, never inside it."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A8")
    @pytest.mark.uc("UC-010", flow="A11", steps=[2])
    async def test_start_waits_for_the_lobby_entry(self, real_servicer):
        calls = []
        music_gate = asyncio.Event()

        async def start_lobby_music():
            calls.append("lobby_music_starting")
            await music_gate.wait()
            calls.append("lobby_music_started")

        async def prepare():
            calls.append("prepare_game_start")
            return False

        real_servicer.audio.start_lobby_music.side_effect = start_lobby_music
        real_servicer.idle_monitor.start.side_effect = lambda: calls.append("idle_monitor_started")
        await _connect(real_servicer, "a", "b")

        with patch.object(real_servicer, "_prepare_game_start", side_effect=prepare):
            entry = asyncio.create_task(real_servicer.enter_lobby())
            await _until(lambda: "lobby_music_starting" in calls)

            assert await real_servicer._start_game("", source="operator_pad", controllers=["a", "b"])
            await asyncio.sleep(0.3)
            assert "prepare_game_start" not in calls

            music_gate.set()
            await entry
            await real_servicer._game_lifecycle_task

        assert calls == ["lobby_music_starting", "lobby_music_started", "idle_monitor_started", "prepare_game_start"]


def _game_that_ends(servicer, seen_states: list):
    async def run_stream(*_args, **_kwargs):
        seen_states.append(servicer.state)
        await servicer._restart_lobby()

    return run_stream


class TestStartOnAStoppedMenu:
    """UC-010 BR-014, A11: a web or pad start on a STOPPED menu runs its game and leaves the menu STOPPED."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A11", steps=[3], br=["BR-014"])
    async def test_pad_force_start_keeps_a_stopped_menu_stopped(self, real_servicer):
        await _connect(real_servicer, "a", "b")
        assert real_servicer.state == menu_pb2.MenuState.STOPPED
        seen_states = []
        game = _game_that_ends(real_servicer, seen_states)

        with (
            patch.object(real_servicer.settings_ops, "force_start_roster", AsyncMock(return_value=["a", "b"])),
            patch.object(real_servicer, "_run_game_event_stream", side_effect=game),
        ):
            await real_servicer._pad_force_start(trace.get_current_span())
            await real_servicer._game_lifecycle_task

        assert seen_states == [menu_pb2.MenuState.STOPPED]
        assert real_servicer.state == menu_pb2.MenuState.STOPPED
        real_servicer.idle_monitor.start.assert_not_called()

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A12", steps=[2])
    @pytest.mark.uc("UC-010", flow="A16", steps=[3])
    async def test_a_menu_started_during_that_game_is_running_after_it(self, real_servicer):
        """The boot with menu_auto_start on marks the menu started under a game begun in its window."""
        await _connect(real_servicer, "a", "b")
        seen_states = []
        game_gate = asyncio.Event()

        async def run_stream(*_args, **_kwargs):
            seen_states.append(real_servicer.state)
            await game_gate.wait()
            await real_servicer._restart_lobby()

        with patch.object(real_servicer, "_run_game_event_stream", side_effect=run_stream):
            await real_servicer._start_game("", source="web", controllers=["a", "b"])
            await _until(lambda: seen_states)
            assert real_servicer.start_menu_during_game()
            game_gate.set()
            await real_servicer._game_lifecycle_task

        assert seen_states == [menu_pb2.MenuState.STOPPED]
        assert real_servicer.state == menu_pb2.MenuState.RUNNING
        real_servicer.idle_monitor.start.assert_called_once_with()


class _Provider:
    """The user flag provider's bounded wait: answers ``answer`` once ``ready`` is set."""

    def __init__(self):
        self.ready = asyncio.Event()
        self.ready.set()
        self.answer = True

    async def __call__(self, _domain, *_a, **_k):
        await self.ready.wait()
        return self.answer


@pytest.fixture
def provider():
    fake = _Provider()
    with patch("services.menu.game_lifecycle.wait_for_provider_ready", side_effect=fake):
        yield fake


def _persisted_fight_club(servicer) -> None:
    servicer._user_selection_restored = False
    servicer.user_prefs_client.get_string_value.side_effect = lambda key, default, *_a, **_k: (
        Games.FightClub.name if key == "current_game" else default
    )
    servicer.user_prefs_client.get_string_value.reset_mock()


class TestBootWindowStartWithoutAGame:
    """UC-010 A12, A16: a boot-window start that ends without a game or a restore enters the lobby after it."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A16", steps=[3])
    @pytest.mark.uc("UC-010", flow="A12", steps=[2], br=["BR-001"])
    async def test_roster_abort_under_a_deferred_start_menu_enters_the_lobby(self, real_servicer, provider):
        real_servicer._user_selection_restored = False
        real_servicer.user_prefs_client.get_string_value.side_effect = lambda key, default, *_a, **_k: (
            Games.FightClub.name if key == "current_game" else default
        )
        await _connect(real_servicer, "a")

        assert await real_servicer._start_game("", source="web")
        # The boot decides to skip its lobby entry under the live lifecycle (server._boot_entry).
        assert real_servicer.start_menu_during_game()
        await real_servicer._game_lifecycle_task

        assert real_servicer.current_selection == Games.FightClub
        assert real_servicer.state == menu_pb2.MenuState.RUNNING
        real_servicer.audio.start_lobby_music.assert_awaited_once_with()
        real_servicer.idle_monitor.start.assert_called()
        published = [event for event, _data in (c.args for c in real_servicer.event_publisher.publish.await_args_list)]
        assert "menu_started" not in published
        assert published.count("game_start_cancelled") == 1

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A16", steps=[3])
    async def test_a_boot_window_game_waits_for_the_provider_and_its_mode(self, real_servicer, provider):
        _persisted_fight_club(real_servicer)
        await _connect(real_servicer, "a", "b")
        started_modes = []

        async def run_stream(_stub, request, *_args, **_kwargs):
            started_modes.append(request.start_config.game_name)

        provider.ready.clear()
        with patch.object(real_servicer, "_run_game_event_stream", side_effect=run_stream):
            await real_servicer._start_game("", source="operator_pad", controllers=["a", "b"])
            await asyncio.sleep(0.3)
            assert started_modes == []
            provider.ready.set()
            await real_servicer._game_lifecycle_task

        assert started_modes == [Games.FightClub.name]
        assert real_servicer._user_selection_restored

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A16", steps=[3], br=["BR-003"])
    async def test_a_provider_that_is_not_ready_leaves_the_selection_unread(self, real_servicer, provider):
        _persisted_fight_club(real_servicer)
        provider.answer = False
        await _connect(real_servicer, "a", "b")

        await real_servicer.ensure_user_selection()

        assert real_servicer.current_selection == Games.JoustFFA
        assert not real_servicer._user_selection_restored
        real_servicer.user_prefs_client.get_string_value.assert_not_called()

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A16", steps=[3])
    @pytest.mark.uc("UC-015", br=["BR-008"])
    async def test_pad_force_start_names_the_persisted_mode(self, real_servicer, provider):
        _persisted_fight_club(real_servicer)
        await _connect(real_servicer, "a", "b")

        with (
            patch.object(real_servicer.settings_ops, "force_start_roster", AsyncMock(return_value=["a", "b"])),
            patch.object(real_servicer, "_start_game", AsyncMock(return_value=True)),
        ):
            result = await real_servicer._pad_force_start(trace.get_current_span())

        requested = [
            c.args[1]["game_name"]
            for c in real_servicer.event_publisher.publish.await_args_list
            if c.args[0] == "game_requested"
        ]
        assert requested == [Games.FightClub.name]
        assert result.new == Games.FightClub.name


class TestRecoveryAgainstLobbyEntry:
    """UC-010 A8, BR-008: a reset_menu restore and a lobby entry never run at the same time."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A8", br=["BR-008"])
    async def test_recovery_restore_waits_for_the_lobby_entry(self, real_servicer):
        calls = []
        music_gate = asyncio.Event()

        async def start_lobby_music():
            calls.append("lobby_music_starting")
            await music_gate.wait()
            calls.append("lobby_music_started")

        real_servicer.audio.start_lobby_music.side_effect = start_lobby_music
        restart = real_servicer._restart_lobby

        async def restart_lobby():
            calls.append("restore")
            await restart()

        await _connect(real_servicer, "a", "b")
        with (
            patch.object(real_servicer, "_restart_lobby", side_effect=restart_lobby),
            patch.object(real_servicer, "_primary_game_active", AsyncMock(return_value=False)),
        ):
            entry = asyncio.create_task(real_servicer.enter_lobby())
            await _until(lambda: "lobby_music_starting" in calls)
            assert await real_servicer._start_game("", source="operator_pad", controllers=["a", "b"])
            await asyncio.sleep(0.2)

            reset = asyncio.create_task(real_servicer._handle_reset_menu())
            await asyncio.sleep(0.2)
            assert "restore" not in calls
            start_menu = await real_servicer.StartMenu(menu_pb2.StartMenuRequest(), MagicMock())

            music_gate.set()
            await entry
            assert await reset is None

        assert (start_menu.success, start_menu.error) == (True, "")
        assert calls.index("lobby_music_started") < calls.index("restore")
        assert real_servicer.state == menu_pb2.MenuState.RUNNING

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A8", br=["BR-008"])
    @pytest.mark.uc("UC-010", flow="A12", steps=[1])
    async def test_start_menu_between_the_cancel_and_the_restore_is_deferred(self, real_servicer):
        """The stuck task is already cancelled and the restore not assigned yet: only the recovery is live."""
        cancelled_gate = asyncio.Event()
        cancel_publishing = asyncio.Event()

        async def publish(event_type, _data):
            if event_type == "game_start_cancelled":
                cancel_publishing.set()
                await cancelled_gate.wait()

        real_servicer.event_publisher.publish.side_effect = publish
        real_servicer._game_lifecycle_task = asyncio.create_task(asyncio.sleep(3600))
        await _connect(real_servicer, "a", "b")

        with (
            patch.object(real_servicer, "_primary_game_active", AsyncMock(return_value=False)),
            patch.object(real_servicer, "enter_lobby", wraps=real_servicer.enter_lobby) as enter_lobby,
        ):
            reset = asyncio.create_task(real_servicer._handle_reset_menu())
            await asyncio.wait_for(cancel_publishing.wait(), timeout=5)
            assert not real_servicer._lifecycle_live()
            assert real_servicer._recovery_running()

            response = await real_servicer.StartMenu(menu_pb2.StartMenuRequest(), MagicMock())

            assert (response.success, response.error) == (True, "")
            assert real_servicer._start_menu_deferred
            enter_lobby.assert_not_called()
            cancelled_gate.set()
            assert await reset is None

        enter_lobby.assert_not_called()
        assert real_servicer.state == menu_pb2.MenuState.RUNNING
        real_servicer.idle_monitor.start.assert_called()


class TestDroppedStartGeneration:
    """services.md §3.1, UC-010 A15: a dropped pending start was a live lifecycle, so it moves the generation by 2."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-015")
    @pytest.mark.uc("UC-010", flow="A15", steps=[2])
    async def test_dropped_ready_start_counts_as_a_lifecycle(self, real_servicer):
        real_servicer.state = menu_pb2.MenuState.RUNNING
        await _connect(real_servicer, "a", "b")
        real_servicer.state_manager.controller_states["a"] = ControllerState.READY
        prepare = AsyncMock(return_value=False)

        with patch.object(real_servicer, "_prepare_game_start", prepare):
            assert await real_servicer._start_game("a", ready_start=True)
            live = (real_servicer._lifecycle_live(), real_servicer.lifecycle_generation)
            refused = await real_servicer._start_game("b", source="web", controllers=["a", "b"])
            await real_servicer._game_lifecycle_task

        assert live == (True, 1)
        assert refused is False
        assert (real_servicer._lifecycle_live(), real_servicer.lifecycle_generation) == (False, 2)
        prepare.assert_not_awaited()


def _monitor(state_manager) -> IdleMonitor:
    return IdleMonitor(
        state_manager=state_manager,
        get_idle_enabled=lambda: True,
        get_idle_timeout=lambda: 15,
        get_sentinel_count=lambda: 0,
        get_rotation_minutes=lambda: 5,
    )


class TestIdleBelowTheMinimum:
    """UC-010 BR-018: an A14 lobby (everyone ready, below the minimum) goes idle with its READY players."""

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A14", br=["BR-018"])
    @pytest.mark.uc("UC-010", flow="A2", steps=[1, 2])
    async def test_ready_players_below_the_minimum_go_idle_and_wake_unready(self, real_servicer):
        manager = real_servicer.state_manager
        await _connect(real_servicer, "a", "b", "c")
        manager.set_game_mode(Games.Traitor)
        for serial in ("a", "b", "c"):
            manager.controller_states[serial] = ControllerState.READY
        monitor = _monitor(manager)

        await monitor._enter_idle_mode()

        assert monitor.is_idle_active
        assert manager.controller_states == dict.fromkeys(("a", "b", "c"), ControllerState.IDLE)

        await manager.transition_to("a", ControllerState.CONNECTED)
        await monitor.wake_all("a")

        assert manager.controller_states == dict.fromkeys(("a", "b", "c"), ControllerState.CONNECTED)
        assert manager.ready_controllers == set()

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A2", steps=[1], br=["BR-018"])
    async def test_ready_players_waiting_for_others_stay_ready(self, real_servicer):
        manager = real_servicer.state_manager
        await _connect(real_servicer, "a", "b", "c")
        manager.set_game_mode(Games.Traitor)
        manager.controller_states["a"] = ControllerState.READY
        manager.controller_states["b"] = ControllerState.READY
        monitor = _monitor(manager)

        await monitor._enter_idle_mode()

        assert manager.controller_states == {
            "a": ControllerState.READY,
            "b": ControllerState.READY,
            "c": ControllerState.IDLE,
        }

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-010", flow="A14", steps=[4], br=["BR-017"])
    async def test_select_stays_locked_in_a_lobby_below_the_minimum(self, real_servicer):
        """The way out of A14 is un-readying, the pad or the idle timeout; a READY Select changes nothing."""
        real_servicer.state = menu_pb2.MenuState.RUNNING
        manager = real_servicer.state_manager
        await _connect(real_servicer, "a", "b", "c")
        await real_servicer.select_game_mode(Games.Traitor.name)
        for serial in ("a", "b", "c"):
            manager.controller_states[serial] = ControllerState.READY
        real_servicer.event_publisher.publish.reset_mock()

        await manager.handle_button_event("a", "select", True, event_time=10.0)

        assert real_servicer.current_selection == Games.Traitor
        assert manager.current_game_mode == Games.Traitor
        real_servicer.event_publisher.publish.assert_not_awaited()
