"""Which row a page key changes, and how (UC-016 BR-011 to BR-014). Pure: no I/O, no clock.

A row key binds at its press to a row of a page (`binding`); `edit_plan` and `reset_plan` say what
its release would commit from the status read at the press, whether that raises what the agent may
do (BR-014: a confirmation first) and whether it is refused because a value is unknown.
"""

from __future__ import annotations

from dataclasses import dataclass

from lib import setting_ladders
from lib.setting_ladders import AGENT_POLICY_LEVELS, AGENT_RESUME_FALLBACK_LEVEL, AUTOMATIC, ON_OFF_LADDER, Preview
from services.operator_pad.config import K1, K2, K3, K4, K6, K7
from services.operator_pad.pages import PAGES
from services.operator_pad.state import AGENT_OFF, AGENT_ON, AGENT_WATCH, agent_state

EDIT, RESET_ROLE = "edit", "reset"
ROW_KEYS: dict[str, tuple[int, str]] = {
    K1: (0, EDIT),
    K2: (0, RESET_ROLE),
    K3: (1, EDIT),
    K4: (1, RESET_ROLE),
    K6: (2, EDIT),
    K7: (2, RESET_ROLE),
}

INTERVENTIONS = "interventions"  # AGENT row 1: the write gate, `interventions_switch` (not a flag option)
STEP, BACKEND, SWITCH = "step", "backend", "switch"
AUTO, DEFAULT = "auto", "default"
FALLBACK_PAGE = PAGES.index("HOME")  # BR-016


@dataclass(frozen=True)
class RowSpec:
    option: str
    edit: str  # STEP (`step_setting`), BACKEND (`set_backend`) or SWITCH (`interventions_switch`)
    reset: str | None  # AUTO, DEFAULT or None (no reset key)
    lobby_only: bool


def _row(option: str, edit: str = STEP, reset: str | None = None, lobby_only: bool = True) -> RowSpec:
    return RowSpec(option, edit, reset, lobby_only)


# BR-012, row by row; None = a read-only or empty row (no keys).
ROWS: dict[str, tuple[RowSpec | None, RowSpec | None, RowSpec | None]] = {
    "HOME": (
        _row("current_game"),
        _row("audio_volume.master", lobby_only=False),
        _row("sensitivity", reset=AUTO),
    ),
    "GAME": (
        _row("num_teams", reset=AUTO),
        _row("shield.activation", reset=AUTO),
        _row("game_instructions", reset=DEFAULT),
    ),
    "START": (_row("force_all_start", reset=DEFAULT), None, None),
    "AGENT": (
        _row(INTERVENTIONS, edit=SWITCH, lobby_only=False),
        _row("agent_level", reset=DEFAULT, lobby_only=False),
        None,
    ),
    "INTERV": (_row("decision.backend", edit=BACKEND, reset=DEFAULT, lobby_only=False), None, None),
    "RETRO": (None, None, None),
    "SYSTEM": (None, None, None),
}


@dataclass(frozen=True)
class Binding:
    """A row key's row, fixed at the press for the whole hold (BR-011)."""

    page: int
    row: int
    role: str
    spec: RowSpec | None
    fallback: bool = False  # BR-016: no screen shows the page

    @property
    def active(self) -> bool:
        """The key does something: an edit of an editable row, a reset of a row with a reset."""
        return self.spec is not None and (self.role == EDIT or self.spec.reset is not None)


def binding(key: str, page: int, fallback: bool = False) -> Binding | None:
    """The row ``key`` changes on ``page``; None for K5, K8 and the knob."""
    if key not in ROW_KEYS:
        return None
    row, role = ROW_KEYS[key]
    return Binding(page, row, role, ROWS[PAGES[page]][row], fallback)


def rank(level: str | None) -> int | None:
    return AGENT_POLICY_LEVELS.index(level) if level in AGENT_POLICY_LEVELS else None


def gate(status) -> str | None:
    value = status.agent_interventions_enabled_state if status is not None else ""
    return value if value in ON_OFF_LADDER else None


def row_preview(option: str, status, steps: int) -> Preview:
    """The value a release of ``steps`` commits; the interventions switch is a clamped two-state."""
    if option != INTERVENTIONS:
        return setting_ladders.preview_value(option, status, steps)
    now = gate(status)
    if now is None:
        if steps == 0:
            return Preview(None)
        return Preview("on" if steps > 0 else "off")  # ON is known whatever the gate was (BR-014)
    new, at_limit = setting_ladders.clamp_step(ON_OFF_LADDER, now, steps)
    return Preview(new, at_limit)


@dataclass(frozen=True)
class Plan:
    """What a release would commit: NOW, NEW, a raise to confirm (BR-014), refused (NOT POSSIBLE)."""

    now: str | None
    new: str | None
    raises: bool = False
    refused: bool = False


def edit_plan(spec: RowSpec, status, steps: int) -> Plan:
    now = row_preview(spec.option, status, 0).value
    new = row_preview(spec.option, status, steps).value
    if spec.option == "agent_level":
        if rank(now) is None:
            return Plan(now, None, refused=True)  # BR-014: never confirmed blind
        return Plan(now, new, raises=(rank(new) or 0) > (rank(now) or 0))
    if spec.option == INTERVENTIONS:
        if agent_state(status) == AGENT_OFF:
            return Plan(now, None, refused=True)  # BR-012: the resume restores the gate of the stop
        return Plan(now, new, raises=new == "on" and now != "on")
    return Plan(now, new)


def reset_base(status) -> str | None:
    """The agent level a level reset replaces (BR-013): live for ON / WATCH, saved for OFF."""
    state = agent_state(status)
    if state in (AGENT_ON, AGENT_WATCH):
        return status.agent_level
    if state == AGENT_OFF:
        return status.agent_saved_level or AGENT_RESUME_FALLBACK_LEVEL
    return None


def default_of(option: str, status) -> str | None:
    """The NEW of a reset: `automatic`, or the menu's `setting_defaults` value; None while unknown."""
    if option in setting_ladders.AUTOMATIC_OPTIONS:
        return AUTOMATIC
    if status is None:
        return None
    try:
        return status.setting_defaults.get(option) or None
    except AttributeError:
        return None


def reset_plan(spec: RowSpec, status) -> Plan:
    new = default_of(spec.option, status)
    if spec.option != "agent_level":
        return Plan(row_preview(spec.option, status, 0).value, new)
    base = reset_base(status)
    if rank(base) is None or rank(new) is None:
        return Plan(base, new, refused=True)  # BR-013, BR-014: unknown or off the ladder
    return Plan(base, new, raises=rank(new) > rank(base))
