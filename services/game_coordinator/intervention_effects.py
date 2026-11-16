"""
Ambient intervention effects for JoustMania (#2634, split out of ``interventions.py``).

The PR E ambient/session handlers (audio cue, volume override and revert,
controller effect, end game) and the agent cue (#818). These are stateless free
functions: they hold no lock and no manager state. ``InterventionManager`` keeps
one-line delegating methods with the original names, so handler registration,
``patch.object`` targets and the late-bound ``_end_game_fn`` are unchanged; the
cue gate (``_cue_enabled``) stays on the manager and is passed in as a callable
so it is re-read on the game loop.

The enforcement chain has already passed when these run, so they only do the
effect (defensively).
"""

import asyncio
import logging
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING

from lib.colors import Colors
from lib.feature_flags import GAME_KIND_SHADOW
from services.game_coordinator.stream_writes import serialized_write

if TYPE_CHECKING:
    from services.game_coordinator.intervention_target import InterventionTarget
    from services.game_coordinator.interventions import InterventionContext, InterventionSpec

logger = logging.getLogger(__name__)


# --- PR E ambient/session helpers -------------------------------------- #

# Effect names the agent may request -> controller_manager GameEffectType enum
# value name. Only the agent-safe subset (signalling / ambient feedback) is
# exposed; per-player lifecycle effects (warning/death/respawn/countdown) and
# admin effects are intentionally NOT addressable via this intervention. The
# mode-capability matrix already blocks send_controller_effect in hidden-role
# modes (LED leak), so this map does not re-encode that policy.
_CONTROLLER_EFFECT_NAMES: dict[str, str] = {
    "rumble": "GAME_EFFECT_RUMBLE",
    "pulse": "GAME_EFFECT_PULSE",
    "flash": "GAME_EFFECT_FLASH",
    "show_battery": "GAME_EFFECT_SHOW_BATTERY",
}


def _coerce_float(value: object) -> float | None:
    """Best-effort float coercion; returns None on failure."""
    try:
        return float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return None


def _parse_effect_payload(payload: str) -> tuple[str, str | None]:
    """Split a controller_effect payload ``"<serial>:<effect>"``.

    Returns ``(serial, effect_name)``. ``serial`` may be empty (broadcast).
    A payload with no ``:`` or an empty effect is malformed -> effect_name None.
    """
    if ":" not in payload:
        return "", None
    serial, effect = payload.split(":", 1)
    serial = serial.strip()
    effect = effect.strip().lower()
    if not effect:
        return serial, None
    return serial, effect


def _resolve_effect(effect_name: str) -> int | None:
    """Map a validated effect name to its GameEffectType enum value, or None."""
    enum_name = _CONTROLLER_EFFECT_NAMES.get(effect_name)
    if enum_name is None:
        return None
    try:
        from proto import controller_manager_pb2

        return getattr(controller_manager_pb2, enum_name)
    except Exception:
        return None


def _effect_targets(game: "InterventionTarget", serial: str) -> list[str]:
    """Resolve controller serials for an effect.

    Empty ``serial`` broadcasts to all alive players; a specific serial routes
    to that one player when present and alive. Unknown / dead serials -> [].
    """
    players = game.players
    if serial:
        player = players.get(serial)
        return [serial] if player is not None and player.alive else []
    return [s for s, p in players.items() if p.alive]


async def handle_audio_cue(ctx: "InterventionContext") -> None:
    """Play ``ctx.payload`` as a sound id through the live game's audio path.

    Unknown/empty sound id or no running game -> log and return.
    """
    sound_id = ctx.payload.strip()
    if not sound_id:
        logger.info("audio_cue: empty sound id, skipping")
        return
    game = ctx.game
    if game is None:
        logger.info(f"audio_cue: no live game audio path for sound '{sound_id}', skipping")
        return
    # _play_sound already swallows unknown-sound / audio errors defensively.
    await game._play_sound(sound_id, priority=2)
    logger.info(f"audio_cue: played sound '{sound_id}'")


async def handle_volume_override(ctx: "InterventionContext") -> None:
    """Set the audio volume via the game's audio client.

    State-shaped: ``ctx.value`` is the float volume. ``none_value`` (-1)
    never reaches here (the manager skips reverts-to-neutral), so reverting
    to the configured game volume is handled in ``handle_volume_revert``,
    invoked by the manager's neutral-revert path.
    """
    volume = _coerce_float(ctx.value)
    if volume is None or volume < 0:
        logger.info(f"volume_override: non-applicable value {ctx.value!r}, skipping")
        return
    await set_volume(ctx.game, volume)
    logger.info(f"volume_override: set volume to {volume}")


async def handle_volume_revert(ctx: "InterventionContext") -> None:
    """Restore the configured game volume when the override reverts to none.

    Dispatched by the manager when ``volume_override`` returns to its
    ``none_value`` (-1). Restores exactly ``games.base.GAME_VOLUME``.
    """
    from services.game_coordinator.games.base import GAME_VOLUME

    await set_volume(ctx.game, GAME_VOLUME)
    logger.info(f"volume_override: reverted volume to game default {GAME_VOLUME}")


async def set_volume(game: "InterventionTarget | None", volume: float) -> None:
    """Send a SetVolume RPC via the live game's audio client (no-op if
    there is no running game / audio client). A shadow game never sends it:
    the audio service is shared and its room volume is the real game's
    (UC-031 BR-006, #1835)."""
    if game is not None and game.game_kind == GAME_KIND_SHADOW:
        logger.info("volume_override: shadow game is silent, skipping")
        return
    audio_client = game.audio_client if game is not None else None
    if audio_client is None:
        logger.info("volume_override: no live game audio client, skipping")
        return
    from proto import audio_pb2

    await audio_client.SetVolume(audio_pb2.SetVolumeRequest(volume=float(volume)))


async def handle_controller_effect(ctx: "InterventionContext") -> None:
    """Send a controller effect via the live game's gameplay stream.

    Payload is ``"<serial>:<effect>"``; an empty serial broadcasts the
    effect to all active (alive) players. Effect names are validated against
    the supported set; unknown effects / malformed payloads -> log + return.
    """
    serial, effect_name = _parse_effect_payload(ctx.payload)
    if effect_name is None:
        logger.info(f"controller_effect: malformed payload {ctx.payload!r}, skipping")
        return
    effect_enum = _resolve_effect(effect_name)
    if effect_enum is None:
        logger.info(f"controller_effect: unknown effect '{effect_name}', skipping")
        return

    game = ctx.game
    stream = game.gameplay_stream if game is not None else None
    if stream is None:
        logger.info("controller_effect: no live gameplay stream, skipping")
        return

    targets = _effect_targets(game, serial)
    if not targets:
        logger.info(f"controller_effect: no active target for serial '{serial}', skipping")
        return

    from proto import controller_manager_pb2

    for target_serial in targets:
        cmd = controller_manager_pb2.GameplayStreamControl(
            game_effect=controller_manager_pb2.GameEffectCommand(
                serial=target_serial,
                effect=effect_enum,
            )
        )
        # Re-read the stream per write: a reopen swaps in a new call and closes the
        # old one, and a game end drops it (UC-030 BR-006).
        stream = game.gameplay_stream
        if stream is None:
            logger.debug("controller_effect: the gameplay stream closed, stopping")
            return
        try:
            await serialized_write(stream, cmd)
        except asyncio.InvalidStateError:
            # grpc.aio: a write that starts after the call finished ("RPC already finished").
            logger.debug("controller_effect: the gameplay stream had closed before the write, stopping")
            return
        except asyncio.CancelledError:
            # The game end cancels the call under a write in flight (UC-030 BR-006):
            # that is a closed stream, not a cancel of this task, which still propagates.
            task = asyncio.current_task()
            if task is not None and task.cancelling():
                raise
            logger.debug("controller_effect: the gameplay stream closed during the write, stopping")
            return
    logger.info(f"controller_effect: sent '{effect_name}' to {len(targets)} controller(s)")


async def handle_end_game(
    ctx: "InterventionContext",
    end_game_fn: "Callable[[str, str], Awaitable[tuple[bool, str]]] | None",
) -> None:
    """End the session this intervention was evaluated for (UC-040 BR-011).

    ``end_game`` is a per-game event list, so the manager only dispatches it
    to the session whose list carries the event, and this handler ends
    exactly that session (``ctx.game_id``) via the servicer's shared
    force-end path. It never
    resolves the primary: before #1310 a shadow session's evaluation ended
    the real game. No-ops safely when the session is gone or no end_game_fn
    is wired.
    """
    if end_game_fn is None:
        logger.info("end_game: no end_game_fn wired, skipping")
        return
    success, error = await end_game_fn(ctx.game_id, "agent_intervention")
    game_label = ctx.game_id or "<primary>"
    if success:
        logger.info(f"end_game: game {game_label} force-ended via agent intervention")
    else:
        logger.info(f"end_game: no-op for game {game_label} ({error})")


async def emit_agent_cue(
    spec: "InterventionSpec",
    target: str | None,
    game: "InterventionTarget | None",
    *,
    cue_enabled: Callable[[], bool],
) -> None:
    """Pulse a distinct 'agent acted' LED on the controller(s) (#818).

    Minimal, non-disruptive: a short magenta LED PULSE (GAME_EFFECT_PULSE
    auto-restores to the base color) on the affected player when the
    intervention is player-targeted, else broadcast to all active players. A
    player-targeted intervention on a now-dead/unknown serial yields no target
    and is skipped. Best-effort — any failure is swallowed so the cue can
    never break the intervention path.

    Never once the game is ending (UC-040 step 13, UC-020 BR-004, #2533): the
    winner rainbow owns the winner's LED, and a pulse would cut it short.
    """
    if not cue_enabled():
        return
    stream = game.gameplay_stream if game is not None else None
    if stream is None:
        return
    try:
        if game.game_ending():
            return
        cue_serial = target if spec.player_targeted and target else ""
        targets = _effect_targets(game, cue_serial)
        if not targets:
            return

        from proto import controller_manager_pb2

        red, green, blue = Colors.Magenta.value  # same magenta as the shield help cue
        # Distinct magenta pulse = "agent acted" — not a game event color.
        # One write after the other: grpc.aio allows one write in flight per call,
        # and a sequential loop never queues N writes ahead of a frame's
        # warning or kill (each waits for at most one cue write). Best-effort: a
        # failed write is logged and never raised into the intervention path.
        for target_serial in targets:
            try:
                await serialized_write(
                    stream,
                    controller_manager_pb2.GameplayStreamControl(
                        game_effect=controller_manager_pb2.GameEffectCommand(
                            serial=target_serial,
                            effect=controller_manager_pb2.GAME_EFFECT_PULSE,
                            color=controller_manager_pb2.RGB(r=red, g=green, b=blue),
                        )
                    ),
                )
            except asyncio.CancelledError:
                # The game end cancels the call under the write: a closed stream, stop.
                task = asyncio.current_task()
                if task is not None and task.cancelling():
                    raise
                return
            except Exception as e:
                logger.debug(f"agent cue write to {target_serial} failed: {e}")
        logger.debug(f"agent cue: pulsed {len(targets)} controller(s) for '{spec.type_id}'")
    except Exception as e:
        logger.debug(f"agent cue emission failed for '{spec.type_id}': {e}")
