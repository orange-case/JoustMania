"""One settings option table for every input surface (#2801, UC-011 BR-006, services.md §3.1).

The option table in services.md is checked against the code's ladders, end rules and lobby-only
set, and every surface that changes the mode gives the same feedback as the pad's `step_setting`.
"""

import re
from pathlib import Path

import pytest

from lib.setting_ladders import AGENT_POLICY_LEVELS, BACKENDS, FLAG_LADDERS, KNOB_LADDERS, MODE_LADDER, WRAPPING
from proto import controller_manager_pb2, menu_pb2
from services.menu import settings_ops
from services.menu.handlers.base import ControllerState
from services.menu.settings_ops import LOBBY_ONLY_OPTIONS, STEP_OPTIONS
from services.menu.tests.test_pad_commands import _pulses, flags, pad, servicer  # noqa: F401 - fixtures
from services.menu.utils.led import GAME_MODE_COLORS
from services.menu.utils.settings import GAME_MODES

SERVICES_MD = Path(__file__).resolve().parents[3] / "docs" / "specs" / "services.md"


def _table() -> dict[str, tuple[list[str], str, str]]:
    """option -> (ladder, ends, lobby only), from the table after the "Settings option table" heading."""
    text = SERVICES_MD.read_text(encoding="utf-8")
    start = text.index("**Settings option table")
    rows = {}
    in_table = False
    for line in text[start:].splitlines():
        if line.startswith("| Option |"):
            in_table = True
            continue
        if not in_table or line.startswith("|---"):
            continue
        if not line.startswith("|"):
            break
        cells = [cell.strip() for cell in line.strip("|").split("|")]
        rows[cells[0].strip("`")] = (re.findall(r"`([^`]+)`", cells[1]), cells[2], cells[3])
    return rows


def _code_ladder(option: str) -> list[str]:
    if option == "current_game":
        return MODE_LADDER
    if option == "agent_level":
        return AGENT_POLICY_LEVELS
    if option == "decision.backend":
        return BACKENDS
    return KNOB_LADDERS.get(option) or FLAG_LADDERS[option]


@pytest.mark.uc("UC-011", flow="main", br=["BR-006"])
def test_the_table_lists_every_steppable_option():
    assert set(_table()) == {*STEP_OPTIONS, "decision.backend"}


@pytest.mark.uc("UC-011", flow="main", br=["BR-006"])
@pytest.mark.parametrize("option", [*STEP_OPTIONS, "decision.backend"])
def test_each_row_matches_the_code(option):
    ladder, ends, lobby_only = _table()[option]
    assert ladder == _code_ladder(option)
    assert ends == ("wraps" if option in WRAPPING else "clamped")
    assert lobby_only == ("yes" if option in LOBBY_ONLY_OPTIONS else "no")


async def _select_from(surface: str, menu) -> None:
    if surface == "pad":
        assert (await pad(menu, "step_setting", option="current_game", direction="up", steps=1)).success
    elif surface == "lobby_select":
        menu.state = menu_pb2.MenuState.RUNNING
        await menu.on_button("dim", "select", True)
    elif surface == "button_input":
        request = menu_pb2.ProcessInputRequest(input_type="button_press", data={"button": "select"})
        assert (await menu.ProcessInput(request, None)).success
    else:
        assert (await pad(menu, "select_game", game_name=GAME_MODES[1])).success


@pytest.mark.asyncio
@pytest.mark.uc("UC-011", flow="A2", br=["BR-006"])
@pytest.mark.uc("UC-011", flow="A1", br=["BR-006"])
@pytest.mark.parametrize("surface", ["pad", "lobby_select", "button_input", "web"])
async def test_every_surface_gives_the_same_mode_feedback(servicer, surface):  # noqa: F811 - fixture
    servicer.state_manager.controller_states = {
        "dim": ControllerState.CONNECTED,
        "ready": ControllerState.READY,
        "idle": ControllerState.IDLE,
    }
    if surface == "lobby_select":
        # UC-010 BR-017: lobby SELECT counts only while nobody is READY.
        del servicer.state_manager.controller_states["ready"]

    await _select_from(surface, servicer)

    new = GAME_MODES[1]
    assert servicer.current_selection.name == new
    servicer.state_manager.audio.play_game_mode_voice.assert_awaited_once_with(new)
    pulse = (controller_manager_pb2.GAME_EFFECT_PULSE, GAME_MODE_COLORS[new], settings_ops.MODE_PULSE_MS)
    pulsed = ["dim"] if surface == "lobby_select" else ["dim", "ready"]
    assert sorted(_pulses(servicer)) == [(serial, *pulse) for serial in pulsed]
