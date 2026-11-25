"""The screens of protocol 2 and higher (UC-016 BR-006, BR-007): one frame, rendered per protocol version.

Pure: no I/O. A frame is a title bar (line 0, inverted), three rows (protocol 3 or higher: medium
rows of 16 characters; protocol 2: large lines of 10) with the divider under the last, and the
status bar (line 7). A ladder setting is a marker bar (protocol 3 or higher), a wheel setting a
wheel (protocol 4; the agent level's does not wrap), anything else NOW and NEW rows. Every bar
and wheel is within the firmware's limits for every input (BR-002), so the pad never has to refuse one.
"""

from __future__ import annotations

import dataclasses

from lib.setting_ladders import (
    AGENT_POLICY_LEVELS,
    BACKENDS,
    KNOB_LADDERS,
    MODE_LADDER,
    SHIELD_KNOB_LADDER,
    VOLUME_LADDER,
    WRAPPING,
    preview_value,
)
from services.operator_pad.pages import NET_OK, SHIELD_WORDS, PageContext, page_rows, value_text
from services.operator_pad.pages import PAGES as PAGE_NAMES
from services.operator_pad.screens import (
    CONFIRMED,
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
    PRESS_KNOB,
    PRESS_TO_CONFIRM,
    RELEASE_TO_RESET,
    RESET_Q,
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
V2_HINTS = {
    RELEASE_TO_SET: "RELEASE",
    RELEASE_TO_RESET: "RELEASE",
    PRESS_TO_CONFIRM: "PRESS KNOB",
}  # protocol 2 rows hold 10 characters

# BR-007 marker bars: the ladder (one rung per value) and the rung labels (at most 26 bytes joined).
LADDER_BARS: dict[str, tuple[list[str], tuple[str, ...]]] = {
    "sensitivity": (KNOB_LADDERS["sensitivity"], ("AUTO", "0", "1", "2", "3", "4")),
    "num_teams": (KNOB_LADDERS["num_teams"], ("AUTO", "2", "3", "4", "5", "6")),
    "audio_volume.master": (VOLUME_LADDER, ("MUTE", "", "", "", "MAX")),
}

# BR-007 wheels: the options in ladder order and their full names. Only `WRAPPING` options wrap;
# the agent level stays clamped (the kill switch: never `none` to `full` in one step).
WHEEL_LADDERS: dict[str, list[str]] = {
    "current_game": MODE_LADDER,
    "decision.backend": BACKENDS,
    "shield.activation": SHIELD_KNOB_LADDER,
    "agent_level": AGENT_POLICY_LEVELS,
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


def page_row(label: str, value: str, width: int) -> str:
    """A page row (BR-006): label left and value right when both fit with a space, else the value alone.

    The value is cut only when it is longer than the row by itself.
    """
    if not value:
        return label[:width]
    if label and len(label) + 1 + len(value) <= width:
        return label + value.rjust(width - len(label))
    return value[:width].rjust(width)


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
        agent = ICON_AGENT if agent_state.startswith(("ON", "WATCH")) else (UNKNOWN if agent_state == UNKNOWN else " ")
        instructions = ICON_INSTRUCTIONS if status.instructions else " "
    left = f"{ICON_INTERNET if ctx.internet == NET_OK else ' '} {mode} {ready}"
    right = f"{agent}{instructions} {ICON_LOCK if ctx.locked else ' '}"
    return fit(left + right.rjust(WIDTH - len(left)), glyphs=True), 0


def _frame(
    version: int,
    title: str,
    right: str,
    rows: list[tuple[str, str]],
    bottom: Line,
    row=lr,
    highlight: int | None = None,
) -> list[Line]:
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr(title, right, WIDTH, min_label=WIDTH - len(right) - 1)), INVERTED)
    for r in range(3):
        label, value = rows[r] if r < len(rows) else ("", "")
        divider = UNDERLINE if r == 2 else 0  # BR-006: the divider above the status bar
        if r == highlight:
            divider |= INVERTED  # UC-016 BR-006: the row of the key held
        if version >= 3:
            lines[1 + 2 * r] = (fit(row(label, value, MEDIUM_WIDTH), glyphs=True), MEDIUM | divider)
        else:
            lines[1 + 2 * r] = (fit(row(label, value, LARGE_WIDTH), glyphs=True), LARGE | divider)
    lines[7] = bottom
    return lines


def page_screen(
    version: int, page: int, status, ctx: PageContext, connection_ok: bool, highlight: int | None = None
) -> Screen:
    """BR-006: a page in the frame, its place `n/7` top right; ``highlight`` the row of a held key."""
    rows = page_rows(page, status, ctx, version)
    title = PAGE_NAMES[page]
    place = f"{page + 1}/{len(PAGE_NAMES)}"
    bottom = status_line(status, ctx, connection_ok)
    return Screen(tuple(_frame(version, title, place, rows, bottom, row=page_row, highlight=highlight)))


def _bar_screen(title: str, right: str, hint: str, bar: tuple, bottom: Line) -> Screen:
    """Protocol 3: the marker bar at bar row 0 over rows 1 and 2, the hint in row 3 over the divider."""
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr(title, right, WIDTH, min_label=WIDTH - len(right) - 1)), INVERTED)
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


def _wheel_screen(title: str, right: str, option: str, now: str | None, new: str, bottom: Line) -> Screen:
    """Protocol 4: lines 1-5 empty, line 6 the divider, then the three wheel slots around NEW.

    A wheel that does not wrap leaves the slot past either end empty (the firmware draws nothing).
    """
    ladder = WHEEL_LADDERS[option]
    i = ladder.index(new)
    if option in WRAPPING:
        entries = [ladder[(i + d) % len(ladder)] for d in (-1, 0, 1)]
    else:
        entries = [ladder[i + d] if 0 <= i + d < len(ladder) else None for d in (-1, 0, 1)]
    wheel = tuple(("", False) if entry is None else (wheel_name(option, entry), entry == now) for entry in entries)
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr(title, right, WIDTH, min_label=WIDTH - len(right) - 1)), INVERTED)
    lines[6] = (fit(""), UNDERLINE)
    lines[7] = bottom
    return Screen(tuple(lines), wheel=wheel)


def setting_screen(
    version: int,
    option: str,
    press_status,
    net_steps: int,
    *,
    lobby_only: bool,
    bottom: Line,
    plan=None,
    confirmed: bool = False,
    blink_on: bool = True,
) -> Screen:
    """BR-007: a setting being changed: marker bar, wheel or NOW and NEW rows, by protocol.

    ``plan`` (UC-016 BR-014, `page_keys.Plan`): NOW and NEW of a page key; refused shows `NOT
    POSSIBLE`; an unconfirmed raise `PRESS KNOB` in a blinking title bar (``blink_on``: its inverted
    half) and, on rows, `PRESS TO CONFIRM`; a confirmed one `CONFIRMED`.
    """
    title = SETTING_LABELS.get(option, clean(option.upper()))
    now = plan.now if plan is not None else preview_value(option, press_status, 0).value
    refused = plan is not None and plan.refused
    if refused or (lobby_only and press_status is not None and press_status.lifecycle_live):
        return _rows_screen(version, title, EDIT, option, now, None, NOT_POSSIBLE, bottom, refused=True)
    new = plan.new if plan is not None else preview_value(option, press_status, net_steps).value
    limit = (MAXIMUM if net_steps > 0 else MINIMUM) if _at_end(option, press_status, net_steps) else None
    raises = plan is not None and plan.raises
    awaiting = raises and not confirmed
    right = PRESS_KNOB if awaiting else (CONFIRMED if raises else (limit or EDIT))
    if version >= 3 and option in LADDER_BARS:
        hint = PRESS_TO_CONFIRM if awaiting else RELEASE_TO_SET
        screen = _bar_screen(title, right, hint, _bar_args(option, now, new), bottom)
    elif version >= 4 and option in WHEEL_LADDERS and new in WHEEL_LADDERS[option]:
        screen = _wheel_screen(title, right, option, now, new, bottom)
    else:
        hint = PRESS_TO_CONFIRM if awaiting else (RELEASE_TO_SET if raises else (limit or RELEASE_TO_SET))
        screen = _rows_screen(version, title, right if raises else EDIT, option, now, new, hint, bottom)
    return _blink(screen, awaiting and not blink_on)


def reset_screen(version: int, option: str, plan, confirmed: bool, bottom: Line, blink_on: bool = True) -> Screen:
    """UC-016 BR-007 reset: `RESET?`, NOW and the default, the hint; a raise as on the setting screen."""
    title = SETTING_LABELS.get(option, clean(option.upper()))
    raises = plan.raises and not plan.refused
    awaiting = raises and not confirmed
    hint = NOT_POSSIBLE if plan.refused else (PRESS_TO_CONFIRM if awaiting else RELEASE_TO_RESET)
    if version < 3 and plan.refused:
        hint = "NOT POSS."  # 10 characters; NEW stays, so no `NOT` / `POSSIBLE` split
    right = PRESS_KNOB if awaiting else (CONFIRMED if raises else RESET_Q)
    screen = _rows_screen(version, title, right, option, plan.now, plan.new, hint, bottom)
    return _blink(screen, awaiting and not blink_on)


def _blink(screen: Screen, off: bool) -> Screen:
    """UC-016 BR-014: the off half of the blinking title bar is drawn normal, not inverted."""
    if not off:
        return screen
    text, attrs = screen.lines[0]
    return dataclasses.replace(screen, lines=((text, attrs & ~INVERTED), *screen.lines[1:]))


def lock_screen(version: int, locked: bool, bottom: Line) -> Screen:
    """UC-016 BR-015: the K4 hold past `operator_pad.hold_lock_ms`; ``locked`` is the state now."""
    word = "UNLOCK" if locked else "LOCK"
    lines: list[Line] = [(fit(""), 0)] * LINES
    lines[0] = (fit(lr("PAD", f"{word}?", WIDTH, min_label=WIDTH)), INVERTED)
    width, size = (MEDIUM_WIDTH, MEDIUM) if version >= 3 else (LARGE_WIDTH, LARGE)
    lines[3] = (fit(_centre(f"{word}?", width)), size)
    lines[6] = (fit(_centre(f"RELEASE TO {word}", WIDTH)), UNDERLINE)  # small font: 17 characters
    lines[7] = bottom
    return Screen(tuple(lines))


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
