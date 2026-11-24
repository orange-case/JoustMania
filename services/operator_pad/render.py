"""The screens of protocol 2 and higher (UC-016 BR-006, BR-007): one frame, rendered per protocol version.

Pure: no I/O. A frame is a title bar (line 0, inverted), three rows (protocol 3 or higher: medium
rows of 16 characters; protocol 2: large lines of 10) with the divider under the last, and the
status bar (line 7). A ladder setting is a marker bar (protocol 3 or higher), a wheel setting a
wheel (protocol 4), anything else NOW and NEW rows. Every bar and wheel is within the firmware's
limits for every input (BR-002), so the pad never has to refuse one.
"""

from __future__ import annotations

from lib.setting_ladders import (
    AGENT_POLICY_LEVELS,
    BACKENDS,
    KNOB_LADDERS,
    MODE_LADDER,
    SHIELD_KNOB_LADDER,
    VOLUME_LADDER,
    preview_value,
)
from services.operator_pad.pages import NET_OK, SHIELD_WORDS, PageContext, page_rows, value_text
from services.operator_pad.pages import PAGES as PAGE_NAMES
from services.operator_pad.screens import (
    ICON_AGENT,
    ICON_INSTRUCTIONS,
    ICON_INTERNET,
    ICON_LOCK,
    INVERTED,
    LARGE,
    LINES,
    MAXIMUM,
    MEDIUM,
    MINIMUM,
    NO_CONNECTION,
    NOT_POSSIBLE,
    SETTING_LABELS,
    UNDERLINE,
    UNKNOWN,
    WIDTH,
    Line,
    Screen,
    _at_end,
    agent_text,
    clean,
    fit,
    mode_name,
    short,
)

MAX_VERSION = 4  # BR-002: a version above 4 is used as 4
MEDIUM_WIDTH, LARGE_WIDTH = 16, 10
EDIT, MUTED = "EDIT", "MUTED"
RELEASE_TO_SET = "RELEASE TO SET"
V2_HINTS = {RELEASE_TO_SET: "RELEASE"}  # protocol 2 rows hold 10 characters

# BR-007 marker bars: the ladder (one rung per value) and the rung labels (at most 26 bytes joined).
LADDER_BARS: dict[str, tuple[list[str], tuple[str, ...]]] = {
    "sensitivity": (KNOB_LADDERS["sensitivity"], ("AUTO", "0", "1", "2", "3", "4")),
    "num_teams": (KNOB_LADDERS["num_teams"], ("AUTO", "2", "3", "4", "5", "6")),
    "agent_level": (AGENT_POLICY_LEVELS, ("NONE", "AMB", "STD", "FULL")),
    "audio_volume.master": (VOLUME_LADDER, ("MUTE", "", "", "", "MAX")),
}

# BR-007 wheels: the options in ladder order and their full names.
WHEEL_LADDERS: dict[str, list[str]] = {
    "current_game": MODE_LADDER,
    "decision.backend": BACKENDS,
    "shield.activation": SHIELD_KNOB_LADDER,
}


def wheel_name(option: str, value: str) -> str:
    if option == "current_game":
        return mode_name(value)
    if option == "shield.activation":
        return SHIELD_WORDS.get(value, clean(value.upper()))
    return short(option, value)


def lr(label: str, value: str, width: int, min_label: int = 4) -> str:
    """Label flush left, value flush right, always one space between (BR-006).

    The value is cut so that at least ``min_label`` characters of the label stay; then the label
    is cut to what is left. A row without a value is its label alone.
    """
    if not value:
        return label[:width]
    if not label:
        return value[:width].rjust(width)
    value = value[: max(1, width - min(len(label), min_label) - 1)]
    label = label[: width - len(value) - 1]
    return label + " " * (width - len(label) - len(value)) + value


def _centre(text: str, width: int) -> str:
    return text[:width].center(width)


def status_line(status, ctx: PageContext, connection_ok: bool) -> Line:
    """BR-006 status bar: internet, mode short name, ready/connected, agent, instructions, lock."""
    if not connection_ok:
        return fit(_centre(NO_CONNECTION, WIDTH)), INVERTED
    if status is None:
        mode, ready, agent, instructions = UNKNOWN, "?/?", UNKNOWN, " "
    else:
        mode = mode_name(status.selection, short_name=True)
        ready = f"{status.ready_count}/{status.connected_count}"
        agent_state = agent_text(status)
        agent = ICON_AGENT if agent_state.startswith("ON") else (UNKNOWN if agent_state == UNKNOWN else " ")
        instructions = ICON_INSTRUCTIONS if status.instructions else " "
    left = f"{ICON_INTERNET if ctx.internet == NET_OK else ' '} {mode} {ready}"
    right = f"{agent}{instructions} {ICON_LOCK if ctx.locked else ' '}"
    return fit(left + right.rjust(WIDTH - len(left)), glyphs=True), 0


def _frame(version: int, title: str, right: str, rows: list[tuple[str, str]], bottom: Line) -> list[Line]:
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr(title, right, WIDTH, min_label=WIDTH)), INVERTED)
    for r in range(3):
        label, value = rows[r] if r < len(rows) else ("", "")
        divider = UNDERLINE if r == 2 else 0  # BR-006: the divider above the status bar
        if version >= 3:
            lines[1 + 2 * r] = (fit(lr(label, value, MEDIUM_WIDTH), glyphs=True), MEDIUM | divider)
        else:
            lines[1 + 2 * r] = (fit(lr(label, value, LARGE_WIDTH), glyphs=True), LARGE | divider)
    lines[7] = bottom
    return lines


def page_screen(version: int, page: int, status, ctx: PageContext, connection_ok: bool) -> Screen:
    """BR-006: a page in the frame, its place `n/7` top right."""
    rows = page_rows(page, status, ctx, version)
    title = PAGE_NAMES[page]
    return Screen(
        tuple(_frame(version, title, f"{page + 1}/{len(PAGE_NAMES)}", rows, status_line(status, ctx, connection_ok)))
    )


def _bar_screen(title: str, right: str, hint: str, bar: tuple, bottom: Line) -> Screen:
    """Protocol 3: the marker bar at bar row 0 over rows 1 and 2, the hint in row 3 over the divider."""
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr(title, right, WIDTH, min_label=WIDTH)), INVERTED)
    lines[5] = (fit(hint), MEDIUM | UNDERLINE)
    lines[7] = bottom
    return Screen(tuple(lines), bar=bar)


def _bar_args(option: str, now: str | None, new: str | None) -> tuple:
    ladder, labels = LADDER_BARS[option]
    marker = ladder.index(now) if now in ladder else None
    filled = ladder.index(new) + 1 if new in ladder else 0
    return (0, len(ladder), filled, marker, labels)


def _rows_screen(
    version: int,
    title: str,
    right: str,
    option: str,
    now: str | None,
    new: str | None,
    hint: str,
    bottom: Line,
    *,
    refused: bool = False,
) -> Screen:
    """BR-007 NOW and NEW rows; NEW is empty when refused (A4)."""
    rows = [("NOW", value_text(option, now, version)), ("NEW", "" if refused else value_text(option, new, version))]
    if version < 3 and len(hint) > LARGE_WIDTH:
        if refused and " " in hint:
            first, rest = hint.split(" ", 1)  # BR-007: `NOT` / `POSSIBLE` over rows 2 and 3
            rows[1] = (first, "")
            hint = rest
        else:
            hint = V2_HINTS.get(hint, hint)
    rows.append((hint, ""))
    return Screen(tuple(_frame(version, title, right, rows, bottom)))


def _wheel_screen(title: str, option: str, now: str | None, new: str, bottom: Line) -> Screen:
    """Protocol 4: lines 1-5 empty, line 6 the divider, then the three wheel slots around NEW."""
    ladder = WHEEL_LADDERS[option]
    i = ladder.index(new)
    entries = [ladder[(i + d) % len(ladder)] for d in (-1, 0, 1)]
    wheel = tuple((wheel_name(option, entry), entry == now) for entry in entries)
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr(title, EDIT, WIDTH, min_label=WIDTH)), INVERTED)
    lines[6] = (fit(""), UNDERLINE)
    lines[7] = bottom
    return Screen(tuple(lines), wheel=wheel)


def setting_screen(
    version: int, option: str, press_status, net_steps: int, *, lobby_only: bool, bottom: Line
) -> Screen:
    """BR-007: a layer setting being changed: marker bar, wheel or NOW and NEW rows, by protocol."""
    title = SETTING_LABELS.get(option, clean(option.upper()))
    now = preview_value(option, press_status, 0).value
    if lobby_only and press_status is not None and press_status.lifecycle_live:
        return _rows_screen(version, title, EDIT, option, now, None, NOT_POSSIBLE, bottom, refused=True)
    new = preview_value(option, press_status, net_steps).value
    limit = (MAXIMUM if net_steps > 0 else MINIMUM) if _at_end(option, press_status, net_steps) else None
    if version >= 3 and option in LADDER_BARS:
        return _bar_screen(title, limit or EDIT, RELEASE_TO_SET, _bar_args(option, now, new), bottom)
    if version >= 4 and option in WHEEL_LADDERS and new in WHEEL_LADDERS[option]:
        return _wheel_screen(title, option, now, new, bottom)
    return _rows_screen(version, title, EDIT, option, now, new, limit or RELEASE_TO_SET, bottom)


def volume_screen(version: int, now: str | None, new: str | None, limit: str | None, bottom: Line) -> Screen:
    """BR-007 volume: the marker bar (NOW = the level when the turn began), `MUTED` / limit top right."""
    right = MUTED if new == "mute" else (limit or EDIT)
    title = SETTING_LABELS["audio_volume.master"]
    if version >= 3:
        return _bar_screen(title, right, "", _bar_args("audio_volume.master", now, new), bottom)
    return _rows_screen(version, title, right, "audio_volume.master", now, new, "", bottom)


def result_screen(version: int, label: str, text: str, bottom: Line) -> Screen:
    """BR-007 result and tap: the label in the title bar, the value or word centred in row 2."""
    width = MEDIUM_WIDTH if version >= 3 else LARGE_WIDTH
    rows = [("", ""), ("", ""), ("", "")]
    if len(text) > width and " " in text and version < 3:
        first, rest = text.split(" ", 1)  # `NOT` / `POSSIBLE`, `NO` / `CONNECTION`
        rows[1], rows[2] = (_centre(first, width), ""), (_centre(rest, width), "")
    elif text:
        rows[1] = (_centre(text, width), "")
    return Screen(tuple(_frame(version, label, "", rows, bottom)))
