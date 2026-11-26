"""Setting ladders shared by the menu and the operator pad (UC-015 BR-004, BR-005, A7; #2106).

One ordering and one stepping rule (clamped, or wrapping for `WRAPPING`) for every setting the pad
steps, so the value the pad computes
from `GetMenuStatus` plus the knob steps (the live preview, #2106) is the value the menu's
`step_setting` commits on release. The menu steps the flag ladders (sensitivity, teams,
instructions, volume) through `FlagServiceWriter.step_variant`, which reads the variant order
from the flag service; these ladders are that order, and a menu test pins them to the flag
files (`services/flagd/*.json`). The agent rows (`AGENT_ROW_LADDERS`, UC-016 BR-012) are stepped by the
menu from the value in force, in their canonical format, and never restart outside their ladder.

Pure module: no I/O, no service imports.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from typing import Protocol

from lib.types import Games

# --- Ladders --------------------------------------------------------------------------------

# UC-011 BR-001: the selectable modes, in `Games` order. Unimplemented (Commander, Ninja) and the
# meta mode (Random) are not selectable.
_UNSELECTABLE_MODES = {Games.Commander, Games.Ninja, Games.Random}
MODE_LADDER: list[str] = [g.name for g in Games if g not in _UNSELECTABLE_MODES]

# `game/sensitivity` variants low to high by value (0 = ultra_slow ... 4 = ultra_fast).
SENSITIVITY_LADDER: list[str] = ["ultra_slow", "slow", "medium", "fast", "ultra_fast"]
# `game/num_teams` variants low to high by value (2 ... 6).
TEAMS_LADDER: list[str] = ["two", "three", "four", "five", "six"]
TEAMS_MIN = 2
# On / off options step along this order, so "up" means on (UC-015 BR-004).
ON_OFF_LADDER: list[str] = ["off", "on"]
# `user/audio_volume.master` variants low to high (flags.md §9); `mute` is the pad's mute (BR-006).
VOLUME_LADDER: list[str] = ["mute", "quiet", "normal", "loud", "max"]

# `game/shield.activation` variants (UC-015 BR-018, #2215): the shield activation modes (flags.md §4).
SHIELD_ACTIVATION_LADDER: list[str] = ["instant", "trigger", "hold"]
# The pad's K6 knob ladder (BR-018): `automatic` (no operator value: the flag file's default or an
# agent promotion applies) first, then the modes; wraps (`WRAPPING`). Committing `automatic` clears
# the persisted operator value, any other rung writes it.
AUTOMATIC = "automatic"
SHIELD_AUTOMATIC = AUTOMATIC
SHIELD_KNOB_LADDER: list[str] = [AUTOMATIC, *SHIELD_ACTIVATION_LADDER]

# UC-015 BR-004 / BR-019: the intervention levels, most restrictive first; clamped, never wraps.
AGENT_POLICY_LEVELS: list[str] = ["none", "ambient", "standard", "full"]
# UC-016 BR-011, BR-012: the `AGENT` row 1 switch, least first (`off`: `enabled` off; `watch`: the gate off).
AGENT_STATE_LADDER: list[str] = ["off", "watch", "on"]

# UC-044 decision backends, in knob order (UC-015 BR-004; wraps, `WRAPPING`).
BACKENDS: list[str] = ["rules", "claude", "openai", "jev"]
# UC-016 BR-014 (D12): the backends that call a paid remote API; `rules` is free.
PAID_BACKENDS: frozenset[str] = frozenset({"claude", "openai", "jev"})

# UC-016 BR-012 (iteration 4): the agent rows the pad edits. Each ladder is in the option's canonical
# format (services.md §3.1), the one the status, `from`, `expect`, `setting_defaults` and `value` use,
# never the flag file's declared order.
# The pad's GOAL row edits `objectives` (D17); `goal.profile` follows it by default and stays Flagsmith-only.
GOAL_OPTION = "objectives"
RATE_OPTION = "policy.max_interventions_per_minute"
GROUP_OPTION = "policy.group_grant.enabled"
RETRO_BACKEND_OPTION = "inference.backend"
EFFORT_OPTION = "inference.effort"
BRIEF_OPTION = "decision.operator_brief"
GOAL_LADDER: list[str] = ["endurance_focused", "balanced_focused", "accelerate_focused", "chaos_focused", "mixed"]
# The rate by value, as integer decimals; `RATE_VARIANTS` names the declared variant of each rung.
RATE_LADDER: list[str] = ["1", "2", "4", "8", "20"]
RATE_VARIANTS: dict[str, str] = {"1": "conservative", "2": "default", "4": "aggressive", "8": "lively", "20": "frenzy"}
RETRO_BACKEND_LADDER: list[str] = ["stub", "claude"]
EFFORT_LADDER: list[str] = ["low", "medium", "high", "xhigh", "max"]
BRIEF_LADDER: list[str] = ["off", "unkillable", "protect_weakest"]
# A brief text equal to no preset byte for byte (a script's or Flagsmith's free text): never stepped or cleared.
BRIEF_CUSTOM = "custom"
AGENT_ROW_LADDERS: dict[str, list[str]] = {
    GOAL_OPTION: GOAL_LADDER,
    RATE_OPTION: RATE_LADDER,
    GROUP_OPTION: ON_OFF_LADDER,
    RETRO_BACKEND_OPTION: RETRO_BACKEND_LADDER,
    EFFORT_OPTION: EFFORT_LADDER,
    BRIEF_OPTION: BRIEF_LADDER,
}
# A value in force off these ladders is refused, never moved to the first rung (D4, D11).
AGENT_ROW_OPTIONS = frozenset(AGENT_ROW_LADDERS)
# UC-016 BR-014 (D3): a step up these ladders raises what the agent may do or spend.
RAISE_LADDER_OPTIONS: frozenset[str] = frozenset({RATE_OPTION, GROUP_OPTION, RETRO_BACKEND_OPTION, EFFORT_OPTION})

# Options of the menu's `step_setting` whose ladder is a flag's variants (`step_variant`), and the agent rows.
FLAG_LADDERS: dict[str, list[str]] = {
    "sensitivity": SENSITIVITY_LADDER,
    "num_teams": TEAMS_LADDER,
    "force_all_start": ON_OFF_LADDER,
    "game_instructions": ON_OFF_LADDER,
    "audio_volume.master": VOLUME_LADDER,
    **AGENT_ROW_LADDERS,
}

# The persisted pad settings with an `automatic` rung (UC-015 BR-018, generalised by #2222): their knob
# ladder is `automatic` followed by the value ladder (clamped, except the shield mode, `WRAPPING`).
# Committing `automatic` clears the persisted operator value (the flag file's default or an agent
# promotion applies again), any other rung writes it, and the knob starts at the operator's value, or
# when there is none at the effective value's rung (the shield mode: at `automatic`; `knob_start`).
# Left out on purpose (the two-state toggles and the volume have no meaningful "no operator value"):
# force_all_start, game_instructions, audio_volume.master.
SHIELD_OPTION = "shield.activation"
KNOB_LADDERS: dict[str, list[str]] = {
    "sensitivity": [AUTOMATIC, *SENSITIVITY_LADDER],
    "num_teams": [AUTOMATIC, *TEAMS_LADDER],
    SHIELD_OPTION: SHIELD_KNOB_LADDER,
}
AUTOMATIC_OPTIONS = frozenset(KNOB_LADDERS)

# UC-015 BR-005, UC-016 BR-007: the options whose ladder wraps (past the last option to the first and
# back), with or without the pad's display. Named one by one, never a default: the agent level and the
# saved-level ladder stay clamped (a kill switch that wraps from `none` to `full` is unsafe), and so do
# the sensitivity, the teams, the volume and the toggles.
WRAPPING: frozenset[str] = frozenset({"current_game", "decision.backend", SHIELD_OPTION, GOAL_OPTION})


def is_raise(option: str, old: str | None, new: str | None) -> bool | None:
    """UC-016 BR-014: whether changing ``option`` from ``old`` to ``new`` raises what the agent may do or spend.

    None when it cannot be judged (a value unknown or off the ladder): the caller refuses, never confirms blind.
    Options that are never a raise (the goal, the brief, the game settings) answer False.
    """
    if option == "decision.backend":
        if new not in BACKENDS:
            return None
        if new not in PAID_BACKENDS:
            return False
        return None if old not in BACKENDS else old not in PAID_BACKENDS
    if option == "agent_level":
        ladder = AGENT_POLICY_LEVELS
    elif option in RAISE_LADDER_OPTIONS:
        ladder = AGENT_ROW_LADDERS[option]
    else:
        return False
    if old not in ladder or new not in ladder:
        return None
    return ladder.index(new) > ladder.index(old)


def knob_start(option: str, operator: str | None, effective: str | None) -> str | None:
    """The rung the knob starts at (BR-018): the operator's value, else the effective value's rung.

    With no operator value (`automatic`) the knob starts where the setting stands now, so "up" means
    more; `automatic` is then the bottom rung, reached by turning down past the lowest value. An
    effective value outside the value ladder starts at `automatic`. The shield mode (a wheel of kinds,
    not a scale; UC-016 BR-007) starts at `automatic` whenever there is no operator value. None = the
    operator value is unknown (never guessed). Shared by the menu's commit and the pad's preview.
    """
    ladder = KNOB_LADDERS[option]
    if operator not in ladder:
        return None
    if operator == AUTOMATIC and effective in ladder and option != SHIELD_OPTION:
        return effective
    return operator


# --- Stepping -------------------------------------------------------------------------------


def clamp_step(ladder: list[str], current: str | None, steps: int, *, restart_outside: bool = False) -> tuple:
    """``steps`` moves along ``ladder`` (negative = back), clamped at both ends: (new, at_limit).

    ``at_limit`` is true when a move was asked for but the value cannot change (already at the end
    it moves towards, A7). A current value outside the ladder (or None) never reports ``at_limit``:
    - ``restart_outside=False`` (the mode and agent ladders, the menu's `_ladder_step`): the first
      move lands on ``ladder[0]`` and the remaining moves go on from there;
    - ``restart_outside=True`` (the flag ladders, `FlagServiceWriter.step_variant`): it lands on
      ``ladder[0]`` whatever the number of moves.
    Zero steps return ``(current, False)``.
    """
    if steps == 0:
        return current, False
    last = len(ladder) - 1
    if current not in ladder:
        if restart_outside:
            return ladder[0], False
        index = 0
        moves = abs(steps) - 1
    else:
        index = ladder.index(current)
        moves = abs(steps)
    target = index + moves if steps > 0 else index - moves
    new = ladder[min(max(target, 0), last)]
    return new, new == current


def wrap_step(ladder: list[str], current: str | None, steps: int) -> str | None:
    """``steps`` moves along ``ladder``, wrapping at both ends; never a limit.

    A net that is a multiple of ``len(ladder)`` lands on ``current``: the caller's no-op (nothing
    written, no cue). A current value outside the ladder (or None): the first move lands on
    ``ladder[0]`` and the remaining moves go on from there, as `clamp_step` without ``restart_outside``.
    """
    if steps == 0:
        return current
    if current not in ladder:
        return ladder[(steps - 1 if steps > 0 else steps + 1) % len(ladder)]
    return ladder[(ladder.index(current) + steps) % len(ladder)]


def step_ladder(
    option: str, ladder: list[str], current: str | None, steps: int, *, restart_outside: bool = False
) -> tuple:
    """(new, at_limit) for ``option``: `wrap_step` for `WRAPPING`, else `clamp_step` (A7).

    A wrapping option never reports ``at_limit``; ``new == current`` without ``at_limit`` is a no-op.
    """
    if option in WRAPPING:
        return wrap_step(ladder, current, steps), False
    return clamp_step(ladder, current, steps, restart_outside=restart_outside)


# --- Preview: the value a release would commit ------------------------------------------------


class _Status(Protocol):
    """The fields of `GetMenuStatusResponse` (services.md §3.1) a preview reads."""

    selection: str
    sensitivity: int
    num_teams: int
    instructions: bool
    force_all_start: bool
    audio_volume_master: str
    agent_level: str
    agent_enabled_state: str
    agent_backend: str
    shield_activation: str
    operator_settings: Mapping[str, str]
    agent_objectives: str
    agent_max_interventions_per_minute: str
    agent_group_grant_state: str
    inference_backend: str
    inference_effort: str
    agent_operator_brief_variant: str


@dataclass(frozen=True)
class Preview:
    """The variant a commit of the net steps would write, and whether it stops at a limit (A7).

    ``value`` is None when the status does not say where the step starts (unknown, never guessed).
    ``at_limit`` matches the menu's ``at_limit`` error: nothing would be written. A wrapping option whose
    net steps come back to the start has ``value`` = the start and no ``at_limit``: the menu's no-op.
    """

    value: str | None
    at_limit: bool = False


UNKNOWN = Preview(None)

# Options `preview_value` knows: the `step_setting` options plus the backend (`set_backend`).
PREVIEW_OPTIONS = frozenset({*FLAG_LADDERS, "current_game", "agent_level", "decision.backend", SHIELD_OPTION})

# The status field (services.md §3.1, fields 22 to 27) each agent row reads, in its canonical format.
AGENT_ROW_STATUS_FIELDS: dict[str, str] = {
    GOAL_OPTION: "agent_objectives",
    RATE_OPTION: "agent_max_interventions_per_minute",
    GROUP_OPTION: "agent_group_grant_state",
    RETRO_BACKEND_OPTION: "inference_backend",
    EFFORT_OPTION: "inference_effort",
    BRIEF_OPTION: "agent_operator_brief_variant",
}


def _flag_variant(option: str, status: _Status) -> str | None:
    """The status field of a flag option as its variant name, None when out of range."""
    if option == "sensitivity":
        index = status.sensitivity
        return SENSITIVITY_LADDER[index] if 0 <= index < len(SENSITIVITY_LADDER) else None
    if option == "num_teams":
        index = status.num_teams - TEAMS_MIN
        return TEAMS_LADDER[index] if 0 <= index < len(TEAMS_LADDER) else None
    if option == "game_instructions":
        return "on" if status.instructions else "off"
    if option == "force_all_start":
        return "on" if status.force_all_start else "off"
    if option == "audio_volume.master":
        return status.audio_volume_master or None
    return None


def preview_value(option: str, status: _Status | None, net_steps: int) -> Preview:
    """The value a release of ``net_steps`` on ``option`` commits, from the status read at the press.

    The same ladder and stepping (`step_ladder`) as the menu's `step_setting` and the pad's backend
    step, so the preview and the commit agree for every start and every step count (a menu test checks all).
    Lobby-only refusals (A1) and failed writes are the caller's: this only computes the target.
    """
    if status is None or option not in PREVIEW_OPTIONS:
        return UNKNOWN
    if option in KNOB_LADDERS:
        # BR-018: the knob starts at the operator's value, else at the effective value's rung.
        ladder = KNOB_LADDERS[option]
        effective = status.shield_activation if option == SHIELD_OPTION else _flag_variant(option, status)
        start = knob_start(option, status.operator_settings.get(option, ""), effective)
        if start is None:
            return UNKNOWN  # the menu could not read whether the operator set it
        new, at_limit = step_ladder(option, ladder, start, net_steps)
        return Preview(new, at_limit)
    if option in AGENT_ROW_OPTIONS:
        current = getattr(status, AGENT_ROW_STATUS_FIELDS[option], "")
        if current not in AGENT_ROW_LADDERS[option]:
            return UNKNOWN  # unknown, `custom` or off the ladder: the menu refuses the step (bad_option)
        new, at_limit = step_ladder(option, AGENT_ROW_LADDERS[option], current, net_steps)
        return Preview(new, at_limit)
    if option in FLAG_LADDERS:
        current = _flag_variant(option, status)
        if current is None:
            return UNKNOWN
        new, at_limit = step_ladder(option, FLAG_LADDERS[option], current, net_steps, restart_outside=True)
        return Preview(new, at_limit)
    if option == "current_game":
        if not status.selection:
            return UNKNOWN
        new, at_limit = step_ladder(option, MODE_LADDER, status.selection, net_steps)
        return Preview(new, at_limit)
    if option == "decision.backend":
        if status.agent_backend not in BACKENDS:
            return UNKNOWN  # the pad refuses a step from an unknown backend
        new, at_limit = step_ladder(option, BACKENDS, status.agent_backend, net_steps)
        return Preview(new, at_limit)
    # agent_level: the live level whatever `enabled` is, as the menu's `_step_agent_level` (UC-015 BR-004).
    level = status.agent_level
    if not level:
        return UNKNOWN  # the menu fails closed with it unreadable
    new, at_limit = step_ladder(option, AGENT_POLICY_LEVELS, level, net_steps)
    return Preview(new, at_limit)
