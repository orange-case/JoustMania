"""Settings utilities for the Menu service."""

import logging

from lib.setting_ladders import MODE_LADDER, step_ladder
from lib.types import Games

logger = logging.getLogger(__name__)


# Game modes available in the menu: the mode ladder shared with the operator pad (#2106).
# Excludes unimplemented modes (Commander, Ninja) and meta mode (Random)
GAME_MODES: list[str] = MODE_LADDER


def validate_mode_keys(mapping: dict[str, object], map_name: str) -> None:
    """Fail at import when a per-mode map has a key that is not a Games member name (#1815)."""
    unknown = set(mapping) - {g.name for g in Games}
    if unknown:
        raise ValueError(f"{map_name} has keys that are not Games members: {sorted(unknown)}")


DEFAULT_GAME_MODE: Games = Games.JoustFFA
DEFAULT_VOICE_ACTOR: str = "gen_female"  # menu_voice default (UC-031 A4, #2107)


def get_next_game_mode(current: Games, forward: bool = True) -> Games:
    """
    Get the next game mode in the cycle.

    Args:
        current: Games enum value
        forward: True to cycle forward, False to cycle backward

    Returns:
        Next Games enum value
    """
    # The pad's `step_setting` stepping, so every surface walks the same wheel.
    next_name, _ = step_ladder("current_game", GAME_MODES, current.name, 1 if forward else -1)
    return Games.from_name(next_name) or DEFAULT_GAME_MODE
