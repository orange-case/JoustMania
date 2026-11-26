"""The countdown and result lines go out as CRITICAL in every mode (UC-031 A2).

Only CRITICAL steals a busy channel, so a line sent lower can be dropped on a full mix.
A source scan pins every ``_play_sound`` call inside the countdown, end-of-game and
cancel functions, so a new mode or a reverted priority fails here.
"""

import ast
from pathlib import Path

import pytest

from proto import audio_pb2
from services.game_coordinator.games import sound_priority

GAMES_DIR = Path(__file__).resolve().parent.parent / "games"
CRITICAL_FUNCTIONS = {
    "_countdown",
    "_end_game_impl",
    "_play_team_victory_sound",
    "_show_tie_cue",
    "_abort_on_mass_disconnect",
}
# (file, function, sound expression) of every line that must be CRITICAL today
EXPECTED = {
    ("game_loop.py", "_countdown", "Sound.SFX_BEEP_LOUD"),
    ("game_loop.py", "_countdown", "Sound.SFX_START3"),
    ("game_loop.py", "_abort_on_mass_disconnect", "Sound.MENU_VOX_STATUS_CANCELLED"),
    ("teams_base.py", "_countdown", "Sound.SFX_BEEP_LOUD"),
    ("teams_base.py", "_countdown", "Sound.SFX_START3"),
    ("teams_base.py", "_play_team_victory_sound", "sound"),
    ("elimination.py", "_show_tie_cue", "sound"),
    ("ffa.py", "_end_game_impl", "Sound.VOX_CONGRATULATIONS"),
    ("fight_club.py", "_end_game_impl", "Sound.VOX_CONGRATULATIONS"),
    ("nonstop_joust.py", "_end_game_impl", "Sound.VOX_CONGRATULATIONS"),
    ("tournament.py", "_end_game_impl", "Sound.VOX_CONGRATULATIONS"),
    ("traitor.py", "_end_game_impl", "Sound.VOX_TRAITOR_WIN"),
    ("werewolf.py", "_end_game_impl", "Sound.VOX_WEREWOLF_WIN"),
    ("werewolf.py", "_end_game_impl", "Sound.VOX_HUMAN_WIN"),
    ("zombie.py", "_end_game_impl", "Sound.VOX_HUMAN_VICTORY"),
    ("zombie.py", "_end_game_impl", "Sound.VOX_ZOMBIE_VICTORY"),
    ("swapper.py", "_end_game_impl", "sound"),
}


def _sites() -> list[tuple[str, str, str, str]]:
    """(file, function, sound, priority) of every _play_sound call in a critical function."""
    found = []
    for path in sorted(GAMES_DIR.glob("*.py")):
        for fn in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
            if not isinstance(fn, ast.FunctionDef | ast.AsyncFunctionDef) or fn.name not in CRITICAL_FUNCTIONS:
                continue
            for node in ast.walk(fn):
                if isinstance(node, ast.Call) and getattr(node.func, "attr", None) == "_play_sound" and node.args:
                    priority = next((ast.unparse(k.value) for k in node.keywords if k.arg == "priority"), "")
                    found.append((path.name, fn.name, ast.unparse(node.args[0]), priority))
    return found


SITES = _sites()


@pytest.mark.uc("UC-031", flow="A2")
def test_every_expected_countdown_and_result_line_is_found():
    assert {(f, fn, s) for f, fn, s, _ in SITES} >= EXPECTED


@pytest.mark.uc("UC-031", flow="A2")
@pytest.mark.parametrize(
    ("file", "function", "sound", "priority"), SITES, ids=[f"{f}:{fn}:{s}" for f, fn, s, _ in SITES]
)
def test_countdown_and_result_lines_are_critical(file, function, sound, priority):
    assert priority == "sound_priority.CRITICAL", f"{file} {function} plays {sound} at {priority or 'the default'}"


def test_the_constant_is_the_proto_critical():
    assert sound_priority.CRITICAL == audio_pb2.CRITICAL
