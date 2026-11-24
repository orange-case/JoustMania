"""Configuration of the operator pad from the `operator` flag set (UC-015, flags.md §10a).

Every key is read at use time through the in-process OpenFeature client; a key the client cannot
resolve, or a malformed value, falls back to the code default below (the `operator.json` baseline).
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass, field

logger = logging.getLogger(__name__)

FLAG_DOMAIN = "operator"

# Logical key names of the pad (UC-015 BR-011).
K1, K2, K3, K4, K5, K6, K7, K8 = "K1", "K2", "K3", "K4", "K5", "K6", "K7", "K8"
KEYS = (K1, K2, K3, K4, K5, K6, K7, K8)
KNOB_PRESS = "KNOB_PRESS"
KNOB_TURN_A = "KNOB_TURN_A"  # counter-clockwise (left): down
KNOB_TURN_B = "KNOB_TURN_B"  # clockwise (right): up
KNOB_NAMES = (KNOB_PRESS, KNOB_TURN_A, KNOB_TURN_B)

DEFAULT_DEVICE = "cb00:1337"
DEFAULT_TAP_MS = 400
DEFAULT_HOLD_AGENT_MS = 1000
DEFAULT_HOLD_END_MS = 1500
DEFAULT_HOLD_LOCK_MS = 3000
DEFAULT_KNOB_EVENTS_PER_STEP = 4
KNOB_EVENTS_PER_STEP_RANGE = (1, 12)
DEFAULT_PREVIEW_DEBOUNCE_MS = 150  # BR-017: wait after the last knob step before the preview speaks
PREVIEW_DEBOUNCE_MS_RANGE = (0, 1000)
DEFAULT_DISPLAY_POLL_MS = 500  # UC-016 BR-003: the status poll while the display link is up
DISPLAY_POLL_MS_RANGE = (200, 5000)
DEFAULT_DISPLAY_RESULT_MS = 1500  # UC-016 BR-001: how long a result or the volume screen shows
DISPLAY_RESULT_MS_RANGE = (300, 10000)
DEFAULT_DISPLAY_AUTOPAGE_MS = 5000  # UC-016 BR-009: the pages turn on their own; 0 = off
DISPLAY_AUTOPAGE_MS_RANGE = (1000, 60000)
DEFAULT_DISPLAY_HOME_MS = 10000  # UC-016 BR-009: back to HOME after the last input
DISPLAY_HOME_MS_RANGE = (2000, 120000)
DEFAULT_INTERNET_CHECK_TARGET = "google.com:443"  # UC-016 BR-010; "" = off
DEFAULT_INTERNET_CHECK_MS = 30000
INTERNET_CHECK_MS_RANGE = (5000, 300000)
DEFAULT_KEYMAP = (
    "K1=F13,K2=F14,K3=F18,K4=F19,K5=F20,K6=F21,K7=F22,K8=F23,KNOB_PRESS=F15,KNOB_TURN_A=F16,KNOB_TURN_B=F17"
)

# Linux input key codes of the function keys (linux/input-event-codes.h).
_FKEY_CODES = {
    **{f"F{n}": 58 + n for n in range(1, 11)},  # F1=59 .. F10=68
    "F11": 87,
    "F12": 88,
    **{f"F{n}": 170 + n for n in range(13, 25)},  # F13=183 .. F24=194
}

_DEVICE_RE = re.compile(r"^([0-9a-fA-F]{4}):([0-9a-fA-F]{4})$")


def key_code(token: str) -> int | None:
    """``F13`` -> 183, ``KEY_F13`` -> 183, ``183`` -> 183; anything else -> None."""
    token = token.strip().upper()
    token = token.removeprefix("KEY_")
    if token.isdigit():
        return int(token)
    return _FKEY_CODES.get(token)


def parse_keymap(raw: str) -> dict[int, str] | None:
    """``K1=F13,...`` -> {183: "K1", ...}; None when malformed, a name is unknown or a code repeats."""
    mapping: dict[int, str] = {}
    for part in raw.split(","):
        name, sep, token = part.partition("=")
        name = name.strip().upper()
        code = key_code(token) if sep else None
        if code is None or (name not in KEYS and name not in KNOB_NAMES) or code in mapping:
            return None
        if name in mapping.values():
            return None
        mapping[code] = name
    return mapping


_PORT_RE = re.compile(r"^[0-9]{1,5}$")


def parse_target(raw: str) -> tuple[str, int] | None:
    """``host:port`` or ``[v6]:port`` -> (host, port); "" -> None (off); malformed -> ValueError.

    A host with a ``:`` outside brackets is malformed (an IPv6 address needs them).
    """
    raw = raw.strip()
    if not raw:
        return None
    host, sep, port = raw.rpartition(":")
    if not sep or not _PORT_RE.match(port) or not 1 <= int(port) <= 65535:
        raise ValueError(raw)
    if host.startswith("[") and host.endswith("]"):
        host = host[1:-1]
        if not host or ":" not in host:
            raise ValueError(raw)
    elif not host or ":" in host or "[" in host or "]" in host:
        raise ValueError(raw)
    if any(c.isspace() for c in host) or not host.isascii():
        raise ValueError(raw)
    return host, int(port)


def parse_device(raw: str) -> tuple[str, str] | None:
    """``cb00:1337`` -> ("cb00", "1337"); "" (variant ``unset``) or malformed -> None (match nothing)."""
    match = _DEVICE_RE.match(raw.strip())
    if match is None:
        return None
    return match.group(1).lower(), match.group(2).lower()


@dataclass(frozen=True)
class Timings:
    """Gesture timings in ms (UC-015 BR-007, BR-011)."""

    tap_ms: int = DEFAULT_TAP_MS
    hold_ms: dict[str, int] = field(
        default_factory=lambda: {K3: DEFAULT_HOLD_AGENT_MS, K4: DEFAULT_HOLD_END_MS, K8: DEFAULT_HOLD_LOCK_MS}
    )
    knob_events_per_step: int = DEFAULT_KNOB_EVENTS_PER_STEP  # BR-005: raw knob events per step


class PadConfig:
    """Reads the `operator` flag set at use time (flags.md §10a)."""

    def __init__(self, client) -> None:
        self._client = client
        self._warned: dict[str, str] = {}  # key -> the last warning, so a bad value warns once per change
        self._target_raw = DEFAULT_INTERNET_CHECK_TARGET  # the last good `internet_check_target`

    def _int(self, key: str, default: int) -> int:
        try:
            value = self._client.get_integer_value(key, default)
        except Exception as e:  # an unreadable flag keeps the default
            logger.warning(f"Reading {key} failed, using {default}: {e}")
            return default
        if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
            logger.warning(f"Malformed {key}={value!r}, using {default}")
            return default
        return value

    def knob_events_per_step(self) -> int:
        """Raw knob events that make one step (BR-005); the default outside 1 to 12 or when malformed."""
        value = self._int("operator_pad.knob_events_per_step", DEFAULT_KNOB_EVENTS_PER_STEP)
        low, high = KNOB_EVENTS_PER_STEP_RANGE
        if not low <= value <= high:
            logger.warning(
                f"operator_pad.knob_events_per_step={value} outside {low} to {high}, "
                f"using {DEFAULT_KNOB_EVENTS_PER_STEP}"
            )
            return DEFAULT_KNOB_EVENTS_PER_STEP
        return value

    def _str(self, key: str, default: str) -> str:
        try:
            value = self._client.get_string_value(key, default)
        except Exception as e:
            logger.warning(f"Reading {key} failed, using the default: {e}")
            return default
        return value if isinstance(value, str) else default

    def enabled(self) -> bool:
        try:
            return bool(self._client.get_boolean_value("operator_pad.enabled", True))
        except Exception as e:
            logger.warning(f"Reading operator_pad.enabled failed, keeping the pad on: {e}")
            return True

    def preview_enabled(self) -> bool:
        """`operator_pad.preview_enabled` (BR-017); unreadable keeps the preview on (it writes nothing)."""
        try:
            return bool(self._client.get_boolean_value("operator_pad.preview_enabled", True))
        except Exception as e:
            logger.warning(f"Reading operator_pad.preview_enabled failed, keeping the preview on: {e}")
            return True

    def preview_debounce_ms(self) -> int:
        """`operator_pad.preview_debounce_ms` (BR-017); 0 is valid, the default outside 0 to 1000 or malformed."""
        key, default = "operator_pad.preview_debounce_ms", DEFAULT_PREVIEW_DEBOUNCE_MS
        try:
            value = self._client.get_integer_value(key, default)
        except Exception as e:
            logger.warning(f"Reading {key} failed, using {default}: {e}")
            return default
        low, high = PREVIEW_DEBOUNCE_MS_RANGE
        if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
            logger.warning(f"Malformed or out of range {key}={value!r} (valid {low} to {high}), using {default}")
            return default
        return value

    def display_enabled(self) -> bool:
        """`operator_pad.display_enabled` (UC-016 Pre-3, A1); unreadable keeps the display on (it writes nothing)."""
        try:
            return bool(self._client.get_boolean_value("operator_pad.display_enabled", True))
        except Exception as e:
            self._warn_once("operator_pad.display_enabled", f"Reading operator_pad.display_enabled failed: {e}")
            return True

    def display_poll_ms(self) -> int:
        """`operator_pad.display_poll_ms` (UC-016 BR-003); the default outside 200 to 5000 or malformed."""
        return self._ranged_int("operator_pad.display_poll_ms", DEFAULT_DISPLAY_POLL_MS, DISPLAY_POLL_MS_RANGE)

    def display_result_ms(self) -> int:
        """`operator_pad.display_result_ms` (UC-016 BR-001); the default outside 300 to 10000 or malformed."""
        return self._ranged_int("operator_pad.display_result_ms", DEFAULT_DISPLAY_RESULT_MS, DISPLAY_RESULT_MS_RANGE)

    def display_autopage_ms(self) -> int:
        """`operator_pad.display_autopage_ms` (UC-016 BR-009): 0 (off) or 1000 to 60000, else the default."""
        key, default = "operator_pad.display_autopage_ms", DEFAULT_DISPLAY_AUTOPAGE_MS
        try:
            value = self._client.get_integer_value(key, default)
        except Exception as e:
            self._warn_once(key, f"Reading {key} failed, using {default}: {e}")
            return default
        low, high = DISPLAY_AUTOPAGE_MS_RANGE
        if isinstance(value, bool) or not isinstance(value, int) or not (value == 0 or low <= value <= high):
            self._warn_once(
                key, f"Malformed or out of range {key}={value!r} (valid 0 or {low} to {high}), using {default}"
            )
            return default
        self._warned.pop(key, None)
        return value

    def display_home_ms(self) -> int:
        """`operator_pad.display_home_ms` (UC-016 BR-009); the default outside 2000 to 120000 or malformed."""
        return self._ranged_int("operator_pad.display_home_ms", DEFAULT_DISPLAY_HOME_MS, DISPLAY_HOME_MS_RANGE)

    def internet_check_ms(self) -> int:
        """`operator_pad.internet_check_ms` (UC-016 BR-010); the default outside 5000 to 300000 or malformed."""
        return self._ranged_int("operator_pad.internet_check_ms", DEFAULT_INTERNET_CHECK_MS, INTERNET_CHECK_MS_RANGE)

    def internet_check_target(self) -> tuple[str, int] | None:
        """`operator_pad.internet_check_target` (UC-016 BR-010): (host, port), None when `off`.

        A failed read or a malformed value keeps the last good value (the default only before the
        first good read), so a flaky read never switches an `off` check back on.
        """
        key = "operator_pad.internet_check_target"
        try:
            raw = self._client.get_string_value(key, self._target_raw)
        except Exception as e:
            self._warn_once(key, f"Reading {key} failed, keeping {self._target_raw!r}: {e}")
            raw = self._target_raw
        try:
            target = parse_target(raw) if isinstance(raw, str) else parse_target(self._target_raw)
        except ValueError:
            self._warn_once(key, f"Malformed {key}={raw!r}, keeping {self._target_raw!r}")
            return parse_target(self._target_raw)
        if isinstance(raw, str):
            self._target_raw = raw
        return target

    def _ranged_int(self, key: str, default: int, valid: tuple[int, int]) -> int:
        """An int flag read at every use; a bad value warns once per value, not at every read."""
        low, high = valid
        try:
            value = self._client.get_integer_value(key, default)
        except Exception as e:
            self._warn_once(key, f"Reading {key} failed, using {default}: {e}")
            return default
        if isinstance(value, bool) or not isinstance(value, int) or not low <= value <= high:
            self._warn_once(key, f"Malformed or out of range {key}={value!r} (valid {low} to {high}), using {default}")
            return default
        self._warned.pop(key, None)
        return value

    def _warn_once(self, key: str, message: str) -> None:
        if self._warned.get(key) != message:
            self._warned[key] = message
            logger.warning(message)

    def device(self) -> tuple[str, str] | None:
        raw = self._str("operator_pad.device", DEFAULT_DEVICE)
        parsed = parse_device(raw)
        if parsed is None and raw:
            logger.warning(f"Malformed operator_pad.device={raw!r}: no device matches")
        return parsed

    def keymap(self) -> dict[int, str]:
        raw = self._str("operator_pad.keymap", DEFAULT_KEYMAP)
        parsed = parse_keymap(raw)
        if parsed is None:
            logger.warning(f"Malformed operator_pad.keymap={raw!r}, using the default keymap")
            parsed = parse_keymap(DEFAULT_KEYMAP)
            assert parsed is not None
        return parsed

    def timings(self) -> Timings:
        """The timings in force; the defaults when a hold time is not above the tap limit (BR-007)."""
        knob = self.knob_events_per_step()
        timings = Timings(
            knob_events_per_step=knob,
            tap_ms=self._int("operator_pad.tap_ms", DEFAULT_TAP_MS),
            hold_ms={
                K3: self._int("operator_pad.hold_agent_ms", DEFAULT_HOLD_AGENT_MS),
                K4: self._int("operator_pad.hold_end_ms", DEFAULT_HOLD_END_MS),
                K8: self._int("operator_pad.hold_lock_ms", DEFAULT_HOLD_LOCK_MS),
            },
        )
        if any(hold <= timings.tap_ms for hold in timings.hold_ms.values()):
            logger.warning(
                f"operator_pad timings invalid (tap_ms={timings.tap_ms}, hold={timings.hold_ms}): "
                "every hold must be longer than the tap limit; using the default timings"
            )
            return Timings(knob_events_per_step=knob)
        return timings
