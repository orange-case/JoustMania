"""The screens of the pad's OLED (UC-016 BR-001): 8 lines of 21 characters, line 1 inverted.

Pure: no I/O. The pad service composes every screen; the firmware only draws lines (BR-002).
A value the status does not tell shows as ``?``, never a guess. The pages of protocol 2 and
higher (BR-006, BR-007) are composed in `render.py` from the shared tables here.
"""

from __future__ import annotations

import re
from collections.abc import Sequence
from dataclasses import dataclass

from lib import setting_ladders
from lib.setting_ladders import AUTOMATIC, KNOB_LADDERS, VOLUME_LADDER, preview_value
from services.operator_pad.state import AGENT_OFF, AGENT_UNKNOWN, AGENT_WATCH, agent_state

WIDTH = 21
LINES = 8
UNKNOWN = "?"

# BR-002 line attributes (bit 0 from protocol 1, bits 1-2 from 2, bit 3 from 3).
INVERTED, LARGE, UNDERLINE, MEDIUM = 0x01, 0x02, 0x04, 0x08

# BR-002, BR-006 glyph bytes (protocol 2: 0x7F-0x84, protocol 3: 0x85).
BAR_FULL, BAR_EMPTY = "\x7f", "\x80"
ICON_INTERNET, ICON_AGENT, ICON_INSTRUCTIONS, ICON_LOCK = "\x81", "\x82", "\x83", "\x84"
GLYPH_LAST = 0x85

Line = tuple[str, int]  # (21 characters, attributes); attributes 1 / True = inverted (protocol 1)


def clean(text: str) -> str:
    """Data strings (names, tags) in printable ASCII only: anything else is ``?`` (glyph bytes included)."""
    return "".join(c if 0x20 <= ord(c) <= 0x7E else "?" for c in text)


def fit(text: str, width: int = WIDTH, glyphs: bool = False) -> str:
    """BR-002: ASCII 0x20 to 0x7E (with ``glyphs`` also 0x7F to 0x85), anything else ``?``; cut and padded."""
    last = GLYPH_LAST if glyphs else 0x7E
    text = "".join(c if 0x20 <= ord(c) <= last else "?" for c in text)
    return text[:width].ljust(width)


@dataclass(frozen=True)
class Screen:
    lines: tuple[Line, ...]
    # Protocol 3: the marker bar (row, rungs, filled, marker or None, labels), sent after the lines.
    bar: tuple | None = None
    # Protocol 4: the wheel, three (text, now) slots (above, selected, below), sent after the lines.
    wheel: tuple | None = None

    @staticmethod
    def of(texts: Sequence[str], title: bool = True) -> Screen:
        padded = list(texts)[:LINES] + [""] * (LINES - len(texts))
        return Screen(tuple((fit(text), title and i == 0) for i, text in enumerate(padded)))

    def text(self, line: int) -> str:
        """Line ``line`` (1 to 8) without its trailing padding."""
        return self.lines[line - 1][0].rstrip()


# BR-001 short forms, so that every line fits in 21 characters.
SHORT_FORMS: dict[str, dict[str, str]] = {
    "sensitivity": {AUTOMATIC: "AUTO", **{v: str(i) for i, v in enumerate(setting_ladders.SENSITIVITY_LADDER)}},
    "num_teams": {
        AUTOMATIC: "AUTO",
        **{v: str(i + setting_ladders.TEAMS_MIN) for i, v in enumerate(setting_ladders.TEAMS_LADDER)},
    },
    "shield.activation": {AUTOMATIC: "AUTO", "instant": "INST", "trigger": "TRIG", "hold": "HOLD"},
    "audio_volume.master": {"mute": "MUTE", "quiet": "QUIET", "normal": "NORM", "loud": "LOUD", "max": "MAX"},
    "agent_level": {"none": "NONE", "ambient": "AMBIENT", "standard": "STANDARD", "full": "FULL"},
    "decision.backend": {"rules": "RULES", "claude": "CLAUDE", "openai": "OPENAI", "jev": "JEV"},
    "game_instructions": {"on": "ON", "off": "OFF"},
    "force_all_start": {"on": "ON", "off": "OFF"},
    "interventions": {"on": "ON", "off": "OFF"},
}

# BR-006 mode table: the full name (`pretty_name` in capitals without a leading `JOUST `, at most 14
# characters) wherever the pad names a mode, the short name (at most 5) only in the status bar.
MODE_NAMES: dict[str, tuple[str, str]] = {
    "JoustFFA": ("FREE-FOR-ALL", "FFA"),
    "JoustTeams": ("TEAMS", "TEAMS"),
    "JoustRandomTeams": ("RANDOM TEAMS", "RTEAM"),
    "Traitor": ("TRAITORS", "TRAIT"),
    "Werewolf": ("WEREWOLF", "WOLF"),
    "Zombies": ("ZOMBIES", "ZOMBI"),
    "Swapper": ("SWAPPER", "SWAP"),
    "FightClub": ("FIGHT CLUB", "FIGHT"),
    "Tournament": ("TOURNAMENT", "TOURN"),
    "NonStop": ("NON STOP JOUST", "NSTOP"),
}


def mode_name(selection: str | None, short_name: bool = False) -> str:
    """A mode's full (or short) name; a mode the table does not list is its name in capitals."""
    if not selection:
        return UNKNOWN
    names = MODE_NAMES.get(selection)
    if names is None:
        name = clean(selection.upper())
        return name[:5] if short_name else name[:14]
    return names[1] if short_name else names[0]


# The setting screen's title per layer option (BR-001).
SETTING_LABELS = {
    "current_game": "GAME",
    "sensitivity": "SENSITIVITY",
    "agent_level": "AGENT LEVEL",
    "num_teams": "TEAMS",
    "shield.activation": "SHIELD MODE",
    "decision.backend": "BACKEND",
    "game_instructions": "INSTRUCTIONS",
    "audio_volume.master": "VOLUME",
    "force_all_start": "FORCE ALL",
    "interventions": "INTERVENTIONS",
}

# UC-015 BR-009 words for a command that ended without a commit (UC-016 A3).
NOT_POSSIBLE = "NOT POSSIBLE"
NO_CONNECTION = "NO CONNECTION"
FAILED_WORD = "FAILED"
CANCELLED_WORD = "CANCELLED"
MAXIMUM = "MAXIMUM"
MINIMUM = "MINIMUM"
RESULT_WORDS = {
    "refused": NOT_POSSIBLE,
    "game_active": NOT_POSSIBLE,
    "bad_option": NOT_POSSIBLE,
    "menu_unavailable": NO_CONNECTION,
    "failed": FAILED_WORD,
    "flag_service_unavailable": FAILED_WORD,
    "partial": FAILED_WORD,
    "cancelled": CANCELLED_WORD,
    "stale": CANCELLED_WORD,
}


def short(option: str, value: str | None) -> str:
    """A value in its BR-001 short form; anything not in the table in capitals; unknown is ``?``."""
    if not value:
        return UNKNOWN
    if option == "current_game":
        return mode_name(value)
    return SHORT_FORMS.get(option, {}).get(value, clean(value.upper()))


def result_word(result: str, up: bool = True) -> str:
    """UC-015 BR-009 word for a result label; ``FAILED`` for any error without one (BR-001)."""
    if result == "at_limit":
        return MAXIMUM if up else MINIMUM
    return RESULT_WORDS.get(result, FAILED_WORD)


# --- status screen ----------------------------------------------------------------------------


_RELEASE_TAG = re.compile(r"[vV]?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)")


def version_text(version: str | None) -> str:
    """FR-241: ``V`` and the release tag (one ``V`` for a tag that has its own), else ``DEV``.

    `latest`, `main` or a sha (IMAGE_TAG overrides for dev and dry runs) are not releases.
    """
    match = _RELEASE_TAG.fullmatch(clean(version or ""))
    if match is None or match.group(1) == "0.0.0-dev":
        return "DEV"
    return f"V{match.group(1)}"


def _knob_value(status, option: str, effective: str | None) -> str:
    """``AUTO`` for the `automatic` rung, else the value the menu uses (also with the key missing)."""
    if status.operator_settings.get(option) == AUTOMATIC:
        return "AUTO"
    return short(option, effective)


def agent_text(status) -> str:
    """UC-016 BR-011 agent states: ``OFF``, ``WATCH`` / ``ON`` and the level, ``?`` by BR-013's unknown rules."""
    state = agent_state(status)
    if state == AGENT_UNKNOWN:
        return UNKNOWN
    if state == AGENT_OFF:
        return "OFF"
    word = "WATCH" if state == AGENT_WATCH else "ON"
    return f"{word} {short('agent_level', status.agent_level)}"


def tap_value(option: str, status) -> str:
    """BR-001 tap screen: the value read for a K2 / K3 / K5 / K7 tap, short form or ``?``."""
    if status is None:
        return UNKNOWN
    if option == "agent_level":
        return agent_text(status)
    if option == "decision.backend":
        return short(option, status.agent_backend)
    return _knob_value(status, option, _flag_variant(status, option))


def _flag_variant(status, option: str) -> str | None:
    ladder = setting_ladders.FLAG_LADDERS[option]
    index = status.sensitivity if option == "sensitivity" else status.num_teams - setting_ladders.TEAMS_MIN
    return ladder[index] if 0 <= index < len(ladder) else None


def status_screen(status, *, connection_ok: bool, locked: bool, version: str | None) -> Screen:
    """BR-001 status screen from the last `GetMenuStatus` read (None: nothing read yet)."""
    if not connection_ok:
        last = NO_CONNECTION
    elif locked:
        last = "LOCKED"
    else:
        last = version_text(version)
    if status is None:
        return Screen.of(
            [
                "JOUSTMANIA ?",
                UNKNOWN,
                "READY ?",
                "AGENT ?",
                "BACKEND ?",
                "SENS ? TEAMS ?",
                "SHIELD ? VOL ?",
                last,
            ]
        )
    # BR-001: the status does not tell starting, running and ending apart.
    state = "GAME" if status.lifecycle_live else "LOBBY"
    agent = f"AGENT {agent_text(status)}"
    sens = _knob_value(status, "sensitivity", _flag_variant(status, "sensitivity"))
    teams = _knob_value(status, "num_teams", _flag_variant(status, "num_teams"))
    # Line 7 has no line 6 rule: an unknown operator value is `?` (BR-001).
    operator_shield = status.operator_settings.get("shield.activation")
    if operator_shield is None:
        shield = UNKNOWN
    else:
        shield = "AUTO" if operator_shield == AUTOMATIC else short("shield.activation", status.shield_activation)
    return Screen.of(
        [
            f"JOUSTMANIA {state}",
            mode_name(status.selection),
            f"READY {status.ready_count}/{status.connected_count}",
            agent,
            f"BACKEND {short('decision.backend', status.agent_backend)}",
            f"SENS {sens} TEAMS {teams}",
            f"SHIELD {shield} VOL {short('audio_volume.master', status.audio_volume_master)}",
            last,
        ]
    )


# --- setting screen ---------------------------------------------------------------------------


def _ladder_line(option: str, new: str | None) -> str:
    """The ladder with the new value marked, only where it fits on one line (BR-001)."""
    ladder = KNOB_LADDERS.get(option)
    if ladder is None or new not in ladder:
        return ""
    text = " ".join(f"[{short(option, v)}]" if v == new else short(option, v) for v in ladder)
    return text if len(text) <= WIDTH else ""


def _at_end(option: str, press_status, net_steps: int) -> bool:
    """The last step was clamped: the net steps reach past the end of the range."""
    if net_steps == 0:
        return False
    new = preview_value(option, press_status, net_steps)
    before = preview_value(option, press_status, net_steps - (1 if net_steps > 0 else -1))
    return new.value is not None and (new.at_limit or new.value == before.value)


def setting_screen(
    option: str, press_status, net_steps: int, *, lobby_only: bool, plan=None, confirmed: bool = False
) -> Screen:
    """BR-001 setting screen: NOW from the press read, NEW the value the release would commit.

    ``plan`` (UC-016 BR-014, `page_keys.Plan`): NOW and NEW of a page key, refused or a raise to confirm.
    """
    label = SETTING_LABELS.get(option, option.upper())
    now_value = plan.now if plan is not None else preview_value(option, press_status, 0).value
    now = short(option, now_value)
    if (lobby_only and press_status is not None and press_status.lifecycle_live) or (plan is not None and plan.refused):
        # A4: no new value; the release is refused.
        return Screen.of([label, "", f"NOW {now}", "", "", "", "", NOT_POSSIBLE])
    new_value = plan.new if plan is not None else preview_value(option, press_status, net_steps).value
    last = "RELEASE TO SET"
    if plan is not None and plan.raises and not confirmed:
        last = PRESS_TO_CONFIRM
    elif _at_end(option, press_status, net_steps):
        last = MAXIMUM if net_steps > 0 else MINIMUM
    return Screen.of(
        [label, "", f"NOW {now}", "", f"NEW {short(option, new_value)}", _ladder_line(option, new_value), "", last]
    )


# --- volume and result screens ----------------------------------------------------------------


def volume_screen(variant: str | None, *, limit: str | None = None) -> Screen:
    """BR-001 volume screen: the variant the turn would commit and a bar of the five rungs."""
    index = VOLUME_LADDER.index(variant) if variant in VOLUME_LADDER else -1
    bar = " ".join("###" if i <= index else "..." for i in range(len(VOLUME_LADDER)))
    last = "MUTED" if variant == "mute" else limit or ""
    return Screen.of(["VOLUME", "", short("audio_volume.master", variant), "", bar, "", "", last])


# --- page keys (UC-016 BR-013, BR-015, BR-016: protocol 1's 8-line layout) ---------------------

RESET_Q, CONFIRM_Q = "RESET?", "CONFIRM?"
RELEASE_TO_RESET, PRESS_TO_CONFIRM = "RELEASE TO RESET", "PRESS TO CONFIRM"
TRANSITION_TEXT = "AGENT IS K5"


def reset_hint(refused: bool, awaiting: bool) -> str:
    return NOT_POSSIBLE if refused else (PRESS_TO_CONFIRM if awaiting else RELEASE_TO_RESET)


def reset_screen(label: str, option: str, now: str | None, new: str | None, hint: str) -> Screen:
    return Screen.of([label, "", f"NOW {short(option, now)}", "", f"NEW {short(option, new)}", "", "", hint])


def lock_screen(locked: bool) -> Screen:
    """BR-015: the K4 hold past `operator_pad.hold_lock_ms`; ``locked`` is the state now (it unlocks)."""
    word = "UNLOCK" if locked else "LOCK"
    return Screen.of(["PAD", "", f"{word}?", "", "", "", "", f"RELEASE TO {word}"])


def result_screen(label: str, text: str) -> Screen:
    """BR-001 result screen: the label of the command, then the committed value or the BR-009 word."""
    return Screen.of([label, "", text])
