"""Menu boot and lifecycle edge cases left after the boot lookup and the readiness re-check (#2938)."""

import asyncio
from unittest.mock import AsyncMock, patch

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

        with (
            patch.object(real_servicer.settings_ops, "force_start_roster", AsyncMock(return_value=["a", "b"])),
            patch.object(real_servicer, "_run_game_event_stream", side_effect=_game_that_ends(real_servicer, seen_states)),
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
