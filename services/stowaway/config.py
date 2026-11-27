"""stowaway settings: the ``stowaway.*`` flags, read at use time (UC-106 BR-003, BR-004; flags.md §10)."""

from __future__ import annotations

import math
import time
from collections.abc import Callable
from dataclasses import dataclass, replace

DOMAIN = "observability"

FLAG_ENABLED = "stowaway.enabled"
FLAG_CHALLENGE = "stowaway.challenge"
FLAG_MODE = "stowaway.mode"
FLAG_INTENSITY = "stowaway.intensity"
FLAG_SPAN_TARGET = "stowaway.span_target"

CHALLENGE_NONE = "none"
CHALLENGES = ("none", "span-rounds", "swing-density", "cardinality-bomb", "hostile-wifi", "clock-skew")

MODE_TELEMETRY = "telemetry"
MODE_PLAYERS = "players"
MODE_BOTH = "both"
MODES = (MODE_TELEMETRY, MODE_PLAYERS, MODE_BOTH)

# Placeholder until the collector's CPU at maximum intensity is measured on the stage Pi (NFR-062).
INTENSITY_CEILING = 4.0
DEFAULT_INTENSITY = 1.0
DEFAULT_SPAN_TARGET = 10_000
MAX_SPAN_TARGET = 1_000_000

SETTINGS_MAX_AGE_S = 0.2


@dataclass(frozen=True)
class Settings:
    enabled: bool = False
    challenge: str = CHALLENGE_NONE
    mode: str = MODE_TELEMETRY
    intensity: float = DEFAULT_INTENSITY
    span_target: int = DEFAULT_SPAN_TARGET

    def telemetry_on(self) -> bool:
        return self.enabled and self.challenge != CHALLENGE_NONE and self.mode in (MODE_TELEMETRY, MODE_BOTH)

    def players_on(self) -> bool:
        return self.enabled and self.mode in (MODE_PLAYERS, MODE_BOTH)


@dataclass(frozen=True)
class Overrides:
    """Command-line values; ``None`` leaves the flag in charge."""

    challenge: str | None = None
    mode: str | None = None
    intensity: float | None = None
    span_target: int | None = None


def clamp_intensity(value: object) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        return DEFAULT_INTENSITY
    return min(max(float(value), 0.0), INTENSITY_CEILING)


def normalize_challenge(value: object) -> str:
    return value if isinstance(value, str) and value in CHALLENGES else CHALLENGE_NONE


def normalize_mode(value: object) -> str:
    return value if isinstance(value, str) and value in MODES else MODE_TELEMETRY


def normalize_span_target(value: object) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        return DEFAULT_SPAN_TARGET
    return min(value, MAX_SPAN_TARGET)


def apply_overrides(settings: Settings, overrides: Overrides) -> Settings:
    changes: dict[str, object] = {}
    if overrides.challenge is not None:
        changes["challenge"] = normalize_challenge(overrides.challenge)
    if overrides.mode is not None:
        changes["mode"] = normalize_mode(overrides.mode)
    if overrides.intensity is not None:
        changes["intensity"] = clamp_intensity(overrides.intensity)
    if overrides.span_target is not None:
        changes["span_target"] = normalize_span_target(overrides.span_target)
    return replace(settings, **changes) if changes else settings


def read_flagd() -> Settings:
    """One read of every ``stowaway.*`` flag. The kill switch fails closed: any failure reads off."""
    from lib.feature_flags import read_bool_flag, read_float_flag, read_int_flag, read_string_flag

    return Settings(
        enabled=read_bool_flag(DOMAIN, FLAG_ENABLED, False),
        challenge=normalize_challenge(read_string_flag(DOMAIN, FLAG_CHALLENGE, CHALLENGE_NONE)),
        mode=normalize_mode(read_string_flag(DOMAIN, FLAG_MODE, MODE_TELEMETRY)),
        intensity=clamp_intensity(read_float_flag(DOMAIN, FLAG_INTENSITY, DEFAULT_INTENSITY)),
        span_target=normalize_span_target(read_int_flag(DOMAIN, FLAG_SPAN_TARGET, DEFAULT_SPAN_TARGET)),
    )


class SettingsSource:
    """Settings at most ``max_age`` seconds old, command-line overrides applied.

    ``read`` is the flag read; with ``--no-flagd`` it is a constant enabled reading. A read that
    raises counts as the kill switch off.
    """

    def __init__(
        self,
        read: Callable[[], Settings] = read_flagd,
        overrides: Overrides | None = None,
        max_age: float = SETTINGS_MAX_AGE_S,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._read = read
        self._overrides = overrides or Overrides()
        self._max_age = max_age
        self._clock = clock
        self._cached: Settings | None = None
        self._at = 0.0

    def get(self) -> Settings:
        now = self._clock()
        if self._cached is None or now - self._at >= self._max_age:
            try:
                settings = self._read()
            except Exception:
                settings = Settings(enabled=False)
            self._cached = apply_overrides(settings, self._overrides)
            self._at = now
        return self._cached
