"""The pages of the pad's screen (UC-016 BR-006) and their timing (BR-009). Pure: no I/O, no clock.

`page_rows` gives a page's three (label, value) rows from the last `GetMenuStatus` read; `Pager`
says which page shows: the knob scrolls it (BR-008), it turns on its own while the pad is idle and
returns to `HOME` once after the last input (BR-009). Every time is passed in (seconds).
"""

from __future__ import annotations

from dataclasses import dataclass

from lib import setting_ladders
from lib.setting_ladders import AGENT_POLICY_LEVELS, AUTOMATIC, SENSITIVITY_LADDER, VOLUME_LADDER
from services.operator_pad.screens import (
    BAR_EMPTY,
    BAR_FULL,
    UNKNOWN,
    V2_SHORT_FORMS,
    _flag_variant,
    _knob_value,
    agent_text,
    mode_name,
    short,
    version_text,
)

PAGES = ("HOME", "GAME", "START", "AGENT", "INTERV", "RETRO", "SYSTEM")
HOME = 0

# BR-010 internet states and the SYSTEM page's NET row.
NET_OK, NET_DOWN, NET_UNKNOWN, NET_OFF = "ok", "down", "?", "off"
NET_TEXT = {NET_OK: "OK", NET_DOWN: "DOWN", NET_UNKNOWN: UNKNOWN, NET_OFF: "OFF"}

# BR-006 full shield words where 16 characters allow (protocol 3 and higher).
SHIELD_WORDS = {AUTOMATIC: "AUTO", "instant": "INSTANT", "trigger": "TRIGGER", "hold": "HOLD"}


def bar(filled: int, rungs: int) -> str:
    """BR-006: one glyph per rung, full up to and including the value's rung."""
    return BAR_FULL * filled + BAR_EMPTY * (rungs - filled)


def value_text(option: str, value: str | None, version: int) -> str:
    """A setting's value as a row shows it (BR-006): the bar where it has one, else full word or short form."""
    if not value:
        return UNKNOWN
    if option == "current_game":
        return mode_name(value, short_name=version < 3)
    if option == "audio_volume.master" and value in VOLUME_LADDER:
        return bar(VOLUME_LADDER.index(value) + 1, len(VOLUME_LADDER))
    if option == "sensitivity" and value in SENSITIVITY_LADDER:
        return bar(SENSITIVITY_LADDER.index(value) + 1, len(SENSITIVITY_LADDER))
    if option == "agent_level" and value in AGENT_POLICY_LEVELS:
        return bar(AGENT_POLICY_LEVELS.index(value) + 1, len(AGENT_POLICY_LEVELS))
    if option == "shield.activation" and version >= 3 and value in SHIELD_WORDS:
        return SHIELD_WORDS[value]
    if option == setting_ladders.RATE_OPTION:
        return _rate(value)
    if version < 3 and value in V2_SHORT_FORMS.get(option, {}):
        return V2_SHORT_FORMS[option][value]
    return short(option, value)


def _rate(value: str | None) -> str:
    return f"{value}/MIN" if value is not None and value.isascii() and value.isdigit() else UNKNOWN


@dataclass(frozen=True)
class PageContext:
    """What a page shows besides the status: the pad's own state."""

    locked: bool = False
    internet: str = NET_UNKNOWN
    version: str | None = None


def page_rows(page: int, status, ctx: PageContext, version: int) -> list[tuple[str, str]]:
    """BR-006: the (label, value) rows of a page; protocol 2 gets the short labels in brackets of the table."""
    v3 = version >= 3

    def label(long: str, short_label: str) -> str:
        return long if v3 else short_label

    name = PAGES[page]
    if name == "SYSTEM":
        version_value = version_text(ctx.version)
        if not v3 and version_value.startswith("V"):
            version_value = version_value[1:]  # the label is the `V` already
        return [
            (label("VERSION", "V"), version_value),
            ("NET", NET_TEXT.get(ctx.internet, UNKNOWN)),
            ("LOCK", "ON" if ctx.locked else "OFF"),
        ]
    unknown = status is None
    if name == "HOME":
        if unknown:
            return [("MODE", UNKNOWN), (label("VOLUME", "VOL"), UNKNOWN), ("SENS", UNKNOWN)]
        sens = (
            "AUTO"
            if status.operator_settings.get("sensitivity") == AUTOMATIC
            else value_text("sensitivity", _flag_variant(status, "sensitivity"), version)
        )
        return [
            ("MODE", value_text("current_game", status.selection, version)),
            (label("VOLUME", "VOL"), value_text("audio_volume.master", status.audio_volume_master, version)),
            ("SENS", sens),
        ]
    if name == "GAME":
        if unknown:
            return [("TEAMS", UNKNOWN), (label("SHIELD", "SHLD"), UNKNOWN), (label("INSTRUCTIONS", "INSTR"), UNKNOWN)]
        operator_shield = status.operator_settings.get("shield.activation")
        if operator_shield is None:
            shield = UNKNOWN  # BR-001 status line 6: an unknown operator value is `?`
        elif operator_shield == AUTOMATIC:
            shield = "AUTO"
        else:
            shield = value_text("shield.activation", status.shield_activation, version)
        return [
            ("TEAMS", _knob_value(status, "num_teams", _flag_variant(status, "num_teams"))),
            (label("SHIELD", "SHLD"), shield),
            (label("INSTRUCTIONS", "INSTR"), "ON" if status.instructions else "OFF"),
        ]
    if name == "START":
        return [(label("FORCE ALL", "FORCE"), UNKNOWN if unknown else ("ON" if status.force_all_start else "OFF"))]
    if name == "AGENT":
        agent = agent_text(status)
        level = None if unknown else status.agent_level
        return [
            ("AGENT", agent.split(" ")[0]),  # ON, WATCH, OFF or ?
            # BR-006: the level also while the agent is stopped; `?` only when the level itself is unknown.
            ("LEVEL", value_text("agent_level", level if level in AGENT_POLICY_LEVELS else None, version)),
            ("GOAL", _row_value(setting_ladders.GOAL_OPTION, status, version)),
        ]
    if name == "INTERV":
        return [
            (label("BACKEND", "AI"), UNKNOWN if unknown else short("decision.backend", status.agent_backend)),
            ("RATE", _row_value(setting_ladders.RATE_OPTION, status, version)),
            ("GROUP", _row_value(setting_ladders.GROUP_OPTION, status, version)),
        ]
    # RETRO
    return [
        (label("RETRO AI", "RETRO"), _row_value(setting_ladders.RETRO_BACKEND_OPTION, status, version)),
        (label("EFFORT", "EFF"), _row_value(setting_ladders.EFFORT_OPTION, status, version)),
        ("BRIEF", _row_value(setting_ladders.BRIEF_OPTION, status, version)),
    ]


def _row_value(option: str, status, version: int) -> str:
    """A row of iteration 4 from status fields 22 to 27: `?` until the menu reports it (never a zero default)."""
    return value_text(option, agent_row_value(option, status), version)


def agent_row_value(option: str, status) -> str | None:
    """The value in force of an agent row as the menu reports it; None while empty or missing (unknown)."""
    value = getattr(status, setting_ladders.AGENT_ROW_STATUS_FIELDS[option], "") if status is not None else ""
    if not isinstance(value, str) or not value:
        return None
    if option == setting_ladders.GOAL_OPTION and value not in setting_ladders.GOAL_LADDER:
        return None  # only the five declared variants mean anything on the pad
    return value


class Pager:
    """BR-008 / BR-009: the page showing, the auto-paging and the return to `HOME` (times in seconds)."""

    def __init__(self, now: float = 0.0) -> None:
        self.reset(now)

    def reset(self, now: float) -> None:
        """Link up: `HOME`, cycling, no input yet (so no return to `HOME`)."""
        self.page = HOME
        self.cycling = True
        self.shown_at = now
        self._last_input: float | None = None
        self._home_armed = False

    def step(self, steps: int, now: float) -> None:
        """A knob page step: wraps, stops the cycling."""
        self.page = (self.page + steps) % len(PAGES)
        self.cycling = False
        self.shown_at = now

    def input(self, now: float) -> None:
        """Any key or knob input (press, release, turn): arms the one return to `HOME`."""
        self._last_input = now
        self._home_armed = True
        self.shown_at = now

    def shown(self, now: float) -> None:
        """The page shows again (an overlay ended): a full auto-paging period from now."""
        self.shown_at = now

    def tick(self, now: float, idle: bool, autopage_s: float, home_s: float) -> bool:
        """Apply the due return to `HOME` and auto-page while idle; True when the page changed."""
        if not idle:
            return False  # never move the page under a held key or a screen over it
        changed = False
        if self._home_armed and self._last_input is not None and now - self._last_input >= home_s:
            self._home_armed = False
            if self.page != HOME or not self.cycling:
                self.page, self.cycling, self.shown_at = HOME, True, now
                changed = True
        if self.cycling and autopage_s > 0 and now - self.shown_at >= autopage_s:
            self.page = (self.page + 1) % len(PAGES)
            self.shown_at = now
            changed = True
        return changed

    def next_wake(self, idle: bool, autopage_s: float, home_s: float) -> float | None:
        """When `tick` next has something to do, or None."""
        if not idle:
            return None
        deadlines = []
        if self._home_armed and self._last_input is not None:
            deadlines.append(self._last_input + home_s)
        if self.cycling and autopage_s > 0:
            deadlines.append(self.shown_at + autopage_s)
        return min(deadlines) if deadlines else None
