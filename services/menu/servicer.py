"""Menu gRPC Servicer for JoustMania."""

import asyncio
import contextlib
import json
import logging
import time

from openfeature.flag_evaluation import Reason
from openfeature.provider.no_op_provider import PASSED_IN_DEFAULT
from opentelemetry import trace

from lib.setting_ladders import (
    AGENT_STATE_LADDER,
    AUTOMATIC,
    AUTOMATIC_OPTIONS,
    EFFORT_OPTION,
    GOAL_LADDER,
    GOAL_OPTION,
    GROUP_OPTION,
    RATE_OPTION,
    RETRO_BACKEND_OPTION,
    SHIELD_KNOB_LADDER,
)
from lib.types import Games
from proto import menu_pb2, menu_pb2_grpc
from services.menu.game_lifecycle import GameLifecycleMixin, _bounded_restore_step
from services.menu.handlers.base import ControllerState
from services.menu.menu_setup import build_menu
from services.menu.settings_ops import (
    AGENT_ENABLED_FLAG,
    AGENT_POLICY_FLAG,
    BAD_OPTION,
    DECISION_BACKEND_FLAG,
    FLAG_SERVICE_UNAVAILABLE,
    GAME_ACTIVE,
    LOBBY_ONLY_OPTIONS,
    OK,
    RESET_LOBBY_ONLY_OPTIONS,
    RESETTABLE_OPTIONS,
    SHIELD_ACTIVATION_FLAG,
    STALE,
    Result,
    is_failure,
)
from services.menu.utils.settings import DEFAULT_GAME_MODE, DEFAULT_VOICE_ACTOR, GAME_MODES, get_next_game_mode

logger = logging.getLogger(__name__)


def _variant(details) -> str:
    """The resolved variant name of an OpenFeature evaluation, empty when the client fell back to the default."""
    return details.variant or ""


def _enabled_state(details) -> str:
    """`on`, `off` or `unknown` for an agent on/off flag's evaluation (services.md §3.1, UC-015 BR-013, #2101).

    Unknown is any evaluation that did not come from a resolved flag: an error code, reason ERROR
    (provider not ready, flag not found, ...), no variant (the client fell back to the default),
    or the no-op provider's passed-in default before a provider is set.
    """
    if details.error_code is not None or details.reason == Reason.ERROR:
        return "unknown"
    if not details.variant or details.variant == PASSED_IN_DEFAULT:
        return "unknown"
    return "on" if details.value is True else "off"


def _agent_state(details) -> str:
    """`enabled` as `off`, `watch`, `act` or `unknown` (unresolved, or a value outside the three states)."""
    return details.value if _resolved(details) and details.value in AGENT_STATE_LADDER else "unknown"


def _resolved(details) -> bool:
    """The evaluation came from a resolved flag (the rule of `_enabled_state`)."""
    if details.error_code is not None or details.reason == Reason.ERROR:
        return False
    return isinstance(details.variant, str) and details.variant not in ("", PASSED_IN_DEFAULT)


def _resolved_variant(read) -> str:
    """An agent row's variant name from the in-process client, empty (unknown) when unresolved."""
    try:
        details = read()
    except Exception as e:
        logger.warning(f"Reading an agent row failed: {e}")
        return ""
    return details.variant if _resolved(details) else ""


def _objectives(agent) -> str:
    """`objectives` as one of its declared variants, empty (unknown) otherwise."""
    variant = _resolved_variant(lambda: agent.get_object_details(GOAL_OPTION, {}))
    return variant if variant in GOAL_LADDER else ""


def _two_state(read) -> str:
    """`on` / `off` of a resolved on/off agent row, empty (unknown) otherwise."""
    try:
        state = _enabled_state(read())
    except Exception as e:
        logger.warning(f"Reading an agent row failed: {e}")
        return ""
    return state if state in ("on", "off") else ""


def _rate_text(agent) -> str:
    """policy.max_interventions_per_minute as an integer decimal (`8`, never `8.0`), empty when unknown."""
    try:
        details = agent.get_integer_details(RATE_OPTION, 0)
    except Exception as e:
        logger.warning(f"Reading {RATE_OPTION} failed: {e}")
        return ""
    value = details.value
    if not _resolved(details) or isinstance(value, bool) or not isinstance(value, int | float) or value != int(value):
        return ""
    return str(int(value))


class MenuServicer(GameLifecycleMixin, menu_pb2_grpc.MenuServiceServicer):
    """
    Menu gRPC servicer.

    Manages menu UI and interactions:
    - Start/stop menu
    - Process input
    - Stream events
    """

    def __init__(self):
        """Initialize menu service."""
        self.state = menu_pb2.MenuState.STOPPED
        self.current_selection: Games = Games.JoustFFA
        # Selectable game modes: the pad's mode step and the dashboard share one source of truth (#1030).
        # GAME_MODES already excludes unimplemented/meta modes.
        self.game_options: list[str] = list(GAME_MODES)

        build_menu(self)

        # Track game lifecycle task for clean shutdown. Every non-None assignment counts one
        # lifecycle start; lifecycle_generation derives from that count (UC-015 A5).
        self._lifecycle_starts = 0
        self._lifecycle_task: asyncio.Future | None = None
        self._game_lifecycle_task = None
        # True while the lifecycle task restores the lobby after a game (UC-010 A8 lobby-restart window).
        self._lobby_restarting = False
        self._reset_recovery: asyncio.Future | None = None
        # Set when a lobby restore begins; a failed start never restores twice (UC-010 A3, #2379).
        self._lobby_restore_started = False
        # Set when a StartMenu during the lifecycle undid a StopMenu (UC-010 A12, #2449).
        self._start_menu_deferred = False
        # game_id of the game the lifecycle follows once its start is confirmed; None before (UC-010 BR-009).
        self._followed_game_id: str | None = None

        logger.info("Menu service initialized")

    # Game lifecycle task and its generation (UC-015 A5, services.md §3.1)

    @property
    def _game_lifecycle_task(self) -> asyncio.Future | None:
        """The game lifecycle task: a game starting or running, or a lobby restore."""
        return self._lifecycle_task

    @_game_lifecycle_task.setter
    def _game_lifecycle_task(self, task: asyncio.Future | None) -> None:
        # The start count moves in the same statement as the task, so no read sees one without the other.
        if task is not None:
            self._lifecycle_starts += 1
        self._lifecycle_task = task

    @property
    def lifecycle_generation(self) -> int:
        """Goes up when a lifecycle task starts and again when it ends (the lobby is back).

        Derived at read time from the start count and the task's own done state, so a snapshot never
        pairs a lifecycle with an older generation: the end counts in the same event-loop step as the
        task finishing, before any done callback, also for a task cancelled before it ran. Odd while
        live, even while idle; starts at 0 with the menu process.
        """
        return 2 * self._lifecycle_starts - (1 if self._lifecycle_live() else 0)

    # Callbacks of SettingsOps (the pad's mode step and force start)

    def set_menu_state(self, state) -> None:
        """Set the menu state."""
        self.state = state

    def get_game_options(self) -> list[str]:
        """Get list of available game options."""
        return list(self.game_options)

    async def select_game_mode(
        self, game_name: str, *, source: str = "operator_pad", serial: str | None = None, announce: bool = False
    ) -> bool:
        """Select a game mode by name (SettingsOps and StateManager callback).

        The single selection path (#1272, #1285): the operator pad, lobby SELECT,
        ``ProcessInput`` select and the web ``select_game`` / ``start_game``
        commands all land in the SAME state the coordinator start path reads
        (#1030): the servicer's ``current_selection``, the state_manager's game
        mode, the lobby colour of every
        CONNECTED / READY controller (UC-011 BR-005), a ``selection_changed``
        event and the ``current_game`` default persisted to flagd. ``announce`` adds the mode's
        feedback of the option table (voice and lobby pulse, services.md §3.1), the same on every surface.

        Returns False (and changes nothing) for an unknown or unplayable mode.
        """
        game_mode = Games.from_name(game_name)
        if game_mode is None or game_name not in GAME_MODES:
            logger.warning(f"Unknown game mode selected via {source}: {game_name}")
            return False
        self.current_selection = game_mode
        self.state_manager.set_game_mode(game_mode)
        # UC-011 step 6, BR-005 (#2105): every lobby controller takes the new mode's colour.
        await self.state_manager.show_lobby_colors()
        payload = {"game_name": game_mode.name, "source": source}
        if serial is not None:
            payload["serial"] = serial
        await self.event_publisher.publish("selection_changed", payload)
        await asyncio.to_thread(self.user_prefs_writer.update_default_variant, "current_game", game_mode.name)
        if announce:
            await self.settings_ops.announce("current_game", game_mode.name)
        logger.info(f"Game selected via {source}: {game_mode.name}")
        return True

    async def start_game(self, serial: str, source: str, controllers: list[str] | None = None) -> bool:
        """Start a coordinator game through the ready/web start path (#1029); False when refused.

        ``controllers`` is an explicit roster (a force start honours force_all_start).
        """
        return await self._start_game(serial, source=source, controllers=controllers)

    # ControllerEventCallbacks protocol implementation

    def get_menu_state(self) -> int:
        """Get current menu state (ControllerEventCallbacks)."""
        return self.state

    async def on_connect(self, serial: str) -> None:
        """Handle controller connect event (ControllerEventCallbacks)."""
        await self.state_manager.on_controller_connected(serial)

    async def on_disconnect(self, serial: str) -> None:
        """Handle controller disconnect event (ControllerEventCallbacks)."""
        await self.state_manager.on_controller_disconnected(serial)
        # UC-010 A15: the leaver may have been the last one not ready.
        if self.state == menu_pb2.MenuState.RUNNING and not self._lifecycle_live():
            ready = sorted(self.state_manager.ready_controllers)
            if ready:
                await self.ready_handler.try_auto_start(ready[0])

    def update_battery(self, serial: str, battery: int) -> None:
        """Update battery level for a controller (ControllerEventCallbacks)."""
        self.state_manager.update_battery(serial, battery)

    def get_connected_serials(self) -> set[str]:
        """Get set of serials the menu considers connected (ControllerEventCallbacks)."""
        return self.state_manager.connected_controllers

    async def on_stream_reconnected(self) -> None:
        """Clear state on stream reconnect so initial events re-populate cleanly (ControllerEventCallbacks)."""
        await self.state_manager.on_stream_reconnected()

    async def on_button(self, serial: str, button: str, is_press: bool, event_time: float | None = None) -> None:
        """Handle button event (ControllerEventCallbacks)."""
        # Reset idle timer on any button activity
        self.idle_monitor.reset_activity()

        await self.state_manager.handle_button_event(serial, button, is_press, event_time)

    # Controller state properties (delegate to state_manager as single source of truth)

    @property
    def ready_controllers(self) -> set[str]:
        """Controllers in READY state (delegates to state_manager)."""
        return self.state_manager.ready_controllers

    @property
    def connected_controllers(self) -> set[str]:
        """All connected controllers (delegates to state_manager)."""
        return self.state_manager.connected_controllers

    @property
    def ready_controller_count(self) -> int:
        """Number of ready controllers (computed from state_manager)."""
        return len(self.state_manager.ready_controllers)

    def _clear_ready_state(self) -> None:
        """Clear ready controllers.

        Transitions all READY controllers back to CONNECTED state.
        Used when game starts or menu stops.
        """
        # Transition ready controllers back to connected
        for serial in self.state_manager.ready_controllers:
            self.state_manager.controller_states[serial] = ControllerState.CONNECTED
        logger.debug("Ready state cleared")

    # gRPC service methods (names defined by proto)

    async def StartMenu(self, _request, _context):
        """Start the menu."""
        span = trace.get_current_span()
        try:
            if self._lifecycle_live():
                # UC-010 A12 (#2449): no lobby mid-game, whatever the state; the lifecycle's end applies
                # the request (checked before RUNNING, so the answer during a lifecycle is always success).
                if self.start_menu_during_game():
                    await self.event_publisher.publish("menu_started", {})
                logger.info("StartMenu during a game: the lobby restore will start the menu")
                return menu_pb2.StartMenuResponse(success=True, error="")

            if self.state == menu_pb2.MenuState.RUNNING:
                return menu_pb2.StartMenuResponse(success=False, error="Menu already running")

            await self.enter_lobby()

            logger.info("Menu started")
            span.set_attribute("menu.state", "RUNNING")

            return menu_pb2.StartMenuResponse(success=True, error="")

        except Exception as e:
            span.record_exception(e)
            span.set_status(trace.Status(trace.StatusCode.ERROR, str(e)))
            logger.error(f"StartMenu error: {e}", exc_info=True)
            return menu_pb2.StartMenuResponse(success=False, error=str(e))

    async def enter_lobby(self) -> None:
        """Enter the lobby: the one lobby-entry path for StartMenu and the boot auto-start (D-M2, #1505).

        Sets RUNNING, un-readies everyone, restores the persisted ``current_game``
        (an unplayable value falls back to JoustFFA), starts lobby music and the
        idle monitor, and publishes ``menu_started``.
        """
        self.state = menu_pb2.MenuState.RUNNING
        self._clear_ready_state()

        # Load settings from flagd (user domain)
        self.voice_actor = self.user_prefs_client.get_string_value("menu_voice", DEFAULT_VOICE_ACTOR)
        current_game_name = self.user_prefs_client.get_string_value("current_game", DEFAULT_GAME_MODE.name)
        game = Games.from_name(current_game_name)
        self.current_selection = game if game and game.name in GAME_MODES else DEFAULT_GAME_MODE
        self.state_manager.set_game_mode(self.current_selection)
        # UC-010 A11 (#2175): every controller the menu knows is CONNECTED again and shows the restored
        # mode's dim lobby colour; bounded like the post-game restore (BR-010).
        await _bounded_restore_step("controller re-registration", self.state_manager.reset)

        await _bounded_restore_step("lobby music", self.audio.start_lobby_music)
        self.idle_monitor.start()
        await self.event_publisher.publish("menu_started", {})

    async def StopMenu(self, _request, _context):
        """Stop the menu."""
        span = trace.get_current_span()
        try:
            if self.state == menu_pb2.MenuState.STOPPED:
                return menu_pb2.StopMenuResponse(success=False, error="Menu already stopped")

            self.state = menu_pb2.MenuState.STOPPED

            # Stop idle monitor
            self.idle_monitor.stop()

            # UC-010 A11 (#2175): un-ready the lobby but keep the roster. The button stream stays open
            # and the controller manager re-sends no connect events, so a cleared roster would let a
            # subset of the controllers auto start after the next StartMenu (BR-001, step 4).
            self._clear_ready_state()
            self.state_manager.return_all_to_connected()

            await self.event_publisher.publish("menu_stopped", {})

            logger.info("Menu stopped")
            span.set_attribute("menu.state", "STOPPED")

            return menu_pb2.StopMenuResponse(success=True, error="")

        except Exception as e:
            span.record_exception(e)
            span.set_status(trace.Status(trace.StatusCode.ERROR, str(e)))
            logger.error(f"StopMenu error: {e}", exc_info=True)
            return menu_pb2.StopMenuResponse(success=False, error=str(e))

    async def ProcessInput(self, request, _context):
        """Process menu input."""
        span = trace.get_current_span()
        span.set_attribute("input.type", request.input_type)

        try:
            input_type = request.input_type
            data = dict(request.data)

            if input_type == "web_command" and data.get("command", "") in self.PAD_COMMANDS:
                success, error, value = await self._handle_pad_command(data, span)
                return menu_pb2.ProcessInputResponse(success=success, error=error, value=value)
            rejection = None
            if input_type == "button_press":
                rejection = await self._handle_button_input(data, span)
            elif input_type == "web_command":
                rejection = await self._handle_web_command(data, span)
            elif input_type == "reset_menu":
                rejection = await self._handle_reset_menu()
            if rejection is not None:
                return menu_pb2.ProcessInputResponse(success=False, error=rejection)

            return menu_pb2.ProcessInputResponse(success=True, error="")

        except Exception as e:
            span.record_exception(e)
            span.set_status(trace.Status(trace.StatusCode.ERROR, str(e)))
            logger.error(f"ProcessInput error: {e}", exc_info=True)
            return menu_pb2.ProcessInputResponse(success=False, error=str(e))

    def read_operator_settings_at_start(self) -> None:
        """Boot: read the operator-set values once in the background (UC-015 BR-018), so the first
        K6 hold after a menu restart starts from a known value instead of a silent preview."""
        self.settings_ops.schedule_operator_refresh()

    async def GetMenuStatus(self, _request, _context):
        """Read-only menu snapshot (services.md §3.1): menu memory and in-process flag clients only.

        Never the HTTP flag writer (0.5 s per flag). A flag the client cannot resolve reads as its
        default: empty variant names, and the agent state `unknown` (`agent_enabled` false).
        """
        # UC-015 BR-018: the operator's shield value is read in the background, never in this call.
        self.settings_ops.schedule_operator_refresh()
        return self._menu_status()

    def _menu_status(self) -> menu_pb2.GetMenuStatusResponse:
        """Build the GetMenuStatus snapshot (synchronous: in-process reads only)."""
        game = self.game_settings_client
        user = self.user_prefs_client
        agent = self.agent_settings_client
        enabled_state = _agent_state(agent.get_string_details(AGENT_ENABLED_FLAG, "off"))
        return menu_pb2.GetMenuStatusResponse(
            menu_state=self.state,
            lifecycle_live=self._lifecycle_live(),
            lifecycle_generation=self.lifecycle_generation,
            selection=self.current_selection.name,
            ready_count=self.ready_controller_count,
            connected_count=len(self.connected_controllers),
            agent_enabled=enabled_state in ("watch", "act"),
            agent_enabled_state=enabled_state,
            agent_level=_variant(agent.get_string_details(AGENT_POLICY_FLAG, "")),
            agent_backend=_variant(agent.get_string_details(DECISION_BACKEND_FLAG, "")),
            sensitivity=game.get_integer_value("sensitivity", 2),
            num_teams=game.get_integer_value("num_teams", 2),
            instructions=bool(user.get_boolean_value("game_instructions", False)),
            force_all_start=bool(game.get_boolean_value("force_all_start", False)),
            audio_volume_master=_variant(user.get_float_details("audio_volume.master", 1.0)),
            shield_activation=_variant(game.get_string_details(SHIELD_ACTIVATION_FLAG, "")),
            operator_settings=dict(self.settings_ops.operator_settings),
            agent_objectives=_objectives(agent),
            agent_max_interventions_per_minute=_rate_text(agent),
            agent_group_grant_state=_two_state(lambda: agent.get_boolean_details(GROUP_OPTION, False)),
            inference_backend=_resolved_variant(lambda: agent.get_string_details(RETRO_BACKEND_OPTION, "")),
            inference_effort=_resolved_variant(lambda: agent.get_string_details(EFFORT_OPTION, "")),
            agent_operator_brief_variant=self.settings_ops.brief_variant or "",
            setting_defaults=dict(self.settings_ops.setting_defaults),
            pad_commands=self.settings_ops.pad_commands(),
        )

    async def ListGameModes(self, _request, _context):
        """List the playable game modes (UC-011 BR-001) for the dashboard selector."""
        return menu_pb2.ListGameModesResponse(game_modes=list(GAME_MODES), default_mode=DEFAULT_GAME_MODE.name)

    async def StreamMenuEvents(self, _request, context):
        """Stream menu events in real-time."""
        subscriber_id = f"menu_events_{time.time()}"
        span = trace.get_current_span()
        span.set_attribute("subscriber.id", subscriber_id)

        event_queue = await self.event_publisher.subscribe(subscriber_id)

        try:
            # Headers now = "subscriber registered" (#1613); inside the try so a client that leaves
            # during the send still runs the unsubscribe below.
            await context.send_initial_metadata(())
            while not context.cancelled():
                try:
                    event = await asyncio.wait_for(event_queue.get(), timeout=1.0)
                    yield event
                except TimeoutError:
                    continue
                except Exception as e:
                    logger.error(f"Stream error for {subscriber_id}: {e}")
                    break
        finally:
            await self.event_publisher.unsubscribe(subscriber_id)

    # Input handlers

    async def _handle_button_input(self, data: dict, span) -> str | None:
        """Handle button_press input type; the error code when the start is refused (UC-010 A8), else None."""
        button = data.get("button", "")
        span.set_attribute("button", button)

        if button == "trigger":
            if not await self._start_game(serial="process_input", source="button_input"):
                return GAME_ACTIVE

        elif button == "select":
            next_mode = get_next_game_mode(self.current_selection)
            await self.select_game_mode(next_mode.name, source="button_input", announce=True)
        return None

    async def _handle_web_command(self, data: dict, span) -> str | None:
        """Handle web_command input type.

        Returns an error message when the command is rejected, else None.
        """
        command = data.get("command", "")
        span.set_attribute("command", command)

        if command == "start_game":
            requested_mode = data.get("mode", "")
            game_mode = None
            if requested_mode:
                game_mode = Games.from_name(requested_mode)
                if game_mode is None or game_mode.name not in GAME_MODES:
                    return await self._reject_web_start_mode(requested_mode, span)
                if self._lifecycle_live():
                    # UC-010 A8 (#2177 review): a refused start changes nothing, not even the selection.
                    await self._reject_start_while_active("web", "web", game_mode)
                    return GAME_ACTIVE
                await self.select_game_mode(game_mode.name, source="web")
            if not await self._start_game(serial="web", source="web", game_mode=game_mode):
                # UC-010 A8, UC-015 A1 (#2176): the caller sees the refusal, not success=true.
                return GAME_ACTIVE

        elif command == "select_game":
            game_name = data.get("game_name", "")
            if await self.select_game_mode(game_name, source="web", announce=True):
                # UC-011 A1: the dashboard also listens for this event.
                selected_name = Games.from_name(game_name).name
                await self.event_publisher.publish(
                    "game_selection_changed", {"game_name": selected_name, "source": "web"}
                )
        return None

    # Operator pad commands (#2050, UC-015, services.md §3.1): no serial, no LEDs, error codes.
    PAD_COMMANDS = frozenset(
        {
            "force_start",
            "step_setting",
            "preview_setting",
            "agent_switch",
            "set_backend",
            "reset_setting",
            "agent_state",
        }
    )

    async def _handle_pad_command(self, data: dict, span) -> tuple[bool, str, str]:
        """Run one pad command; (success, error code, committed value). A failed write plays the failure cue (BR-009).

        The value (services.md §3.1 `ProcessInputResponse.value`): what a `step_setting` or `set_backend`
        committed, for the pad's result screen (UC-016 BR-001); empty for every other command and on an error.
        """
        command = data.get("command", "")
        span.set_attribute("command", command)
        if command == "force_start":
            result = await self._pad_force_start(span)
        elif command == "step_setting":
            result = await self._pad_step_setting(data)
        elif command == "preview_setting":
            result = await self._pad_preview_setting(data)
        elif command == "agent_switch":
            result = await self._pad_agent_switch(data)
        elif command == "reset_setting":
            result = await self._pad_reset_setting(data)
        elif command == "agent_state":
            result = await self.settings_ops.agent_state(data.get("state", ""), data.get("from"))
            if result.error is None:
                await self.settings_ops.announce_agent_state(result.old, result.new)
        else:
            result = await self.settings_ops.set_backend(data.get("backend", ""), data.get("from"))
        error = result.error or ""
        # UC-016 BR-001: the committed value of a step, a backend write, a reset or the agent switch.
        named = command in ("step_setting", "set_backend", "reset_setting", "agent_state")
        value = (result.new or "") if error in ("", OK) and named else ""
        if error in ("", OK) and not value and command in ("reset_setting", "agent_state"):
            # A pad cannot tell an empty-valued success from an older menu's unknown-command success.
            logger.error(f"Pad command {command} succeeded without a value")
            error = FLAG_SERVICE_UNAVAILABLE
        if error and error != OK:
            span.set_attribute("error", error)
            logger.warning(f"Pad command {command} refused: {error}")
        if is_failure(error):
            # UC-015 A3: fail closed, never queued or retried; the host hears two loud beeps.
            await self.settings_ops.play_failure_cue()
        success = error in ("", OK)
        return success, error, value if success else ""

    async def _pad_agent_switch(self, data: dict) -> Result:
        """agent_switch: the stop (UC-015 BR-002); a ``direction`` other than ``stop`` is ``bad_option``."""
        direction = data.get("direction", "")
        result = await self.settings_ops.agent_switch(direction)
        if result.error is None:
            await self.settings_ops.announce("agent_switch", direction)
        return result

    async def _pad_reset_setting(self, data: dict) -> Result:
        """reset_setting: lobby-only refusal for all but agent_level and decision.backend, then one clear."""
        option = data.get("option", "")
        if option not in RESETTABLE_OPTIONS:
            return Result(None, None, BAD_OPTION)
        if option in RESET_LOBBY_ONLY_OPTIONS and self._lifecycle_live():
            return Result(None, None, GAME_ACTIVE)
        result = await self.settings_ops.reset_setting(option, data.get("expect") or None, data.get("from"))
        if result.error is None:
            await self.settings_ops.announce(option, result.new)
        return result

    async def _pad_step_setting(self, data: dict) -> Result:
        """step_setting: lobby-only refusal (UC-015 A1), then one clamped or wrapped write (A7, BR-005)."""
        option = data.get("option", "")
        try:
            steps = int(data.get("steps", "1") or "1")
        except ValueError:
            return Result(None, None, BAD_OPTION)
        # UC-016 BR-004: `cue=none` drops only the volume's step sound (the pad's screen shows the level).
        cue = data.get("cue")
        if cue is not None and (cue != "none" or option != "audio_volume.master"):
            return Result(None, None, BAD_OPTION)
        if option in LOBBY_ONLY_OPTIONS and self._lifecycle_live():
            return Result(None, None, GAME_ACTIVE)
        result = await self.settings_ops.step_setting(
            option, data.get("direction", ""), steps, from_value=data.get("from")
        )
        if result.error is None:
            await self.settings_ops.announce(option, result.new, quiet=cue == "none")
        return result

    async def _pad_preview_setting(self, data: dict) -> Result:
        """preview_setting: the K1 / K6 live preview (UC-015 BR-017, #2106, #2215); a pulse, nothing written.

        Writes no flag, changes no selection and plays no voice: only the BR-015 pulse of the mode's
        colour (or the BR-018 pulse of the shield mode's) on every lobby controller. Refused like a
        step while the lifecycle is live (A1).
        """
        option = data.get("option", "")
        if option == SHIELD_ACTIVATION_FLAG:
            shield_mode = data.get("mode", "")
            if shield_mode not in SHIELD_KNOB_LADDER:
                return Result(None, None, BAD_OPTION)
            if self._lifecycle_live():
                return Result(None, None, GAME_ACTIVE)
            await self.settings_ops.show_shield_mode_on_lobby(shield_mode)
            return Result(None, shield_mode)
        if option in AUTOMATIC_OPTIONS:
            # K2 / K5 (#2245): only the `automatic` rung has an LED cue; the values have none.
            if data.get("mode", "") != AUTOMATIC:
                return Result(None, None, BAD_OPTION)
            if self._lifecycle_live():
                return Result(None, None, GAME_ACTIVE)
            await self.settings_ops.show_automatic_on_lobby()
            return Result(None, AUTOMATIC)
        if option != "current_game":
            return Result(None, None, BAD_OPTION)
        mode = Games.from_name(data.get("mode", ""))
        if mode is None or mode.name not in GAME_MODES:
            return Result(None, None, BAD_OPTION)
        if self._lifecycle_live():
            return Result(None, None, GAME_ACTIVE)
        await self.settings_ops.show_mode_on_lobby(mode.name)
        return Result(None, mode.name)

    async def _pad_force_start(self, span) -> Result:
        """force_start: UC-014 steps 3-5 with the live roster (UC-015 BR-008), the pad's K8 tap."""
        if self._lifecycle_live():
            return Result(None, None, GAME_ACTIVE)
        controllers = await self.settings_ops.force_start_roster(span)
        if self._lifecycle_live():
            # A start won the race during the roster lookup (ready auto start, another caller).
            return Result(None, None, GAME_ACTIVE)
        previous_state = self.state
        self.set_menu_state(menu_pb2.MenuState.GAME_STARTING)
        await self.event_publisher.publish(
            "game_requested",
            {
                "game_name": self.current_selection.name,
                "source": "operator_pad",
                "controllers": json.dumps(controllers),
            },
        )
        if not await self._start_game(serial="", source="operator_pad", controllers=controllers):
            if self.state == menu_pb2.MenuState.GAME_STARTING:
                self.set_menu_state(previous_state)
            return Result(None, None, GAME_ACTIVE)
        return Result(None, self.current_selection.name)

    async def _reject_web_start_mode(self, requested_mode: str, span) -> str:
        """Reject a web start_game naming an unknown or unimplemented mode (UC-011 BR-002).

        Publishes ``game_start_rejected`` and returns the error; the selection
        stays and no game starts.
        """
        error = f"Unknown or unimplemented game mode: {requested_mode}"
        logger.warning(f"Web start_game rejected: {error}")
        span.set_attribute("error", error)
        await self.event_publisher.publish(
            "game_start_rejected", {"game_name": requested_mode, "source": "web", "error": error}
        )
        return error

    async def _handle_reset_menu(self) -> str | None:
        """Handle reset_menu input type; the error code of an ignored reset, None otherwise.

        UC-010 BR-008 (#2054): reset_menu is the operator's recovery for a lifecycle task that
        never finishes. A live task whose game is not active on the coordinator is cancelled and
        the lobby restored; a live task with an active game is kept (`game_active`).
        """
        if self._lobby_restarting or self._recovery_running():
            # A lobby restore (this handler's or a lifecycle's post-game one) finishes by itself;
            # cancelling it mid state_manager.reset() would drop controllers not yet re-registered.
            # A recovery still unwinding the stuck task has not set _lobby_restarting yet.
            logger.info("reset_menu ignored: a lobby restore is already running")
            return STALE
        task = self._game_lifecycle_task
        if task is not None and not task.done():
            if await self._primary_game_active():
                logger.warning("reset_menu ignored: a game is starting or running on the coordinator")
                return GAME_ACTIVE
            if self._game_lifecycle_task is not task or task.done() or self._recovery_running():
                # The lifecycle finished (or a new start replaced it) during the lookup: nothing to recover.
                logger.info("reset_menu ignored: the lifecycle task changed during the game-state lookup")
                return STALE
            # Shielded: the caller's deadline cancels this handler, and a cancel mid-recovery would
            # leave the lifecycle cancelled with no restore, or cut state_manager.reset() short.
            # The loop holds tasks weakly: keep the recovery referenced after the caller has gone.
            self._reset_recovery = asyncio.ensure_future(self._recover_stuck_lifecycle(task))
            await asyncio.shield(self._reset_recovery)
            return None
        if self.state == menu_pb2.MenuState.GAME_STARTING:
            self.state = menu_pb2.MenuState.RUNNING
            await self.event_publisher.publish("game_start_cancelled", {})
            logger.info("Menu reset to RUNNING state (game start cancelled)")
        return None

    def _recovery_running(self) -> bool:
        return self._reset_recovery is not None and not self._reset_recovery.done()

    async def _recover_stuck_lifecycle(self, task: asyncio.Future) -> None:
        try:
            logger.warning("reset_menu: cancelling a game lifecycle task with no active game")
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task
            await self.event_publisher.publish("game_start_cancelled", {})
            # The restore runs as the lifecycle task, so a start in this window gets the A8
            # lobby-restart refusal instead of interleaving with the lobby restore.
            restore = asyncio.create_task(self._restart_lobby())
            self._game_lifecycle_task = restore
            try:
                await restore
            finally:
                if self._game_lifecycle_task is restore:
                    self._game_lifecycle_task = None
        finally:
            self._reset_recovery = None

    # Lifecycle methods

    async def start_button_monitor(self):
        """Start the controller event loop."""
        await self.controller_events.start()

    async def stop_button_monitor(self):
        """Stop the controller event loop."""
        await self.controller_events.stop()

    @property
    def button_monitor_running(self) -> bool:
        """Check if button monitor is running."""
        return self.controller_events.is_running

    async def start_game_event_monitor(self):
        """Start the game event monitoring task (no-op, kept for compatibility)."""
        # Game events are now handled by _start_game() stream directly
        # No separate persistent subscription needed
        logger.info("Game event monitor: events handled by game stream")

    async def stop_game_event_monitor(self):
        """Stop the game lifecycle task if running."""
        if self._game_lifecycle_task is not None and not self._game_lifecycle_task.done():
            logger.info("Cancelling game lifecycle task...")
            self._game_lifecycle_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._game_lifecycle_task
            self._game_lifecycle_task = None
            logger.info("Game lifecycle task cancelled")
        else:
            logger.info("No active game lifecycle task to stop")

    async def shutdown(self):
        """Cleanup resources on shutdown."""
        logger.info("Shutting down Menu service...")

        # Stop idle monitor
        self.idle_monitor.stop()

        # Cancel game lifecycle task before closing channels
        if self._game_lifecycle_task is not None and not self._game_lifecycle_task.done():
            logger.info("Cancelling game lifecycle task during shutdown...")
            self._game_lifecycle_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._game_lifecycle_task
            self._game_lifecycle_task = None

        logger.info("Closing gRPC channels...")
        await self.controller_channel.close()
        await self.game_coordinator_channel.close()
        await self.audio_channel.close()
        logger.info("Menu service gRPC channels closed")

    async def _primary_game_active(self) -> bool:
        """Whether the coordinator's primary game is starting or running (UC-010 BR-008).

        Fails closed: an error or an unsuccessful answer counts as active, so reset_menu never
        cancels a live game's stream on a lookup failure.
        """
        from proto import game_coordinator_pb2, game_coordinator_pb2_grpc

        stub = game_coordinator_pb2_grpc.GameCoordinatorServiceStub(self.game_coordinator_channel)
        try:
            response = await stub.GetGameState(game_coordinator_pb2.GetGameStateRequest(), timeout=2.0)
        except Exception as e:
            logger.warning(f"GetGameState failed, treating the game as active: {e}")
            return True
        if not response.success:
            logger.warning(f"GetGameState unsuccessful, treating the game as active: {response.error}")
            return True
        return response.game_info.state in (
            game_coordinator_pb2.GameState.STARTING,
            game_coordinator_pb2.GameState.RUNNING,
        )
