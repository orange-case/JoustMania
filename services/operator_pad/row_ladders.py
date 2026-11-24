"""The ladders of the rows iteration 4 makes settable (UC-016 BR-007, BR-012; services.md §3.1). Pure.

Values are in each option's canonical format (services.md §3.1), the one the status fields, `from`,
`expect`, `setting_defaults` and `value` share: the rate an integer decimal, the brief its preset
name or `custom`, the others their variant names. None of these ladders restarts outside: a value
off the ladder has no rung, so an edit from it is refused, never moved to the first rung.
"""

from __future__ import annotations

from lib.setting_ladders import clamp_step, wrap_step

GOAL = "goal.profile"
RATE = "policy.max_interventions_per_minute"
GROUP = "policy.group_grant.enabled"
RETRO_BACKEND = "inference.backend"
EFFORT = "inference.effort"
BRIEF = "decision.operator_brief"

# `policy.max_interventions_per_minute` variants by value; the ladder holds the values.
RATE_VARIANTS: dict[str, int] = {"conservative": 1, "default": 2, "aggressive": 4, "lively": 8, "frenzy": 20}
CUSTOM_BRIEF = "custom"  # a brief text that equals no preset's value

LADDERS: dict[str, list[str]] = {
    GOAL: ["from_objectives", "long_running_game", "lively_finale", "balanced_contest", "quick_game"],
    RATE: [str(value) for value in sorted(RATE_VARIANTS.values())],
    GROUP: ["off", "on"],  # up is on, not the declared order `on`, `off`
    RETRO_BACKEND: ["stub", "claude"],
    EFFORT: ["low", "medium", "high", "xhigh", "max"],
    BRIEF: ["off", "unkillable", "protect_weakest"],
}
WRAPPING: frozenset[str] = frozenset({GOAL})
# UC-016 BR-014: a rung up gives the agent more autonomy (rate, group) or costs more (retro backend, effort).
RAISES_UP: frozenset[str] = frozenset({RATE, GROUP, RETRO_BACKEND, EFFORT})

# services.md §3.1 `GetMenuStatus` fields 22 to 27.
STATUS_FIELDS: dict[str, str] = {
    GOAL: "agent_goal_profile",
    RATE: "agent_max_interventions_per_minute",
    GROUP: "agent_group_grant_state",
    RETRO_BACKEND: "inference_backend",
    EFFORT: "inference_effort",
    BRIEF: "agent_operator_brief_variant",
}
OPTIONS = frozenset(LADDERS)


def status_value(option: str, status) -> str | None:
    """The value in force as the status reports it; None while unknown (empty, or a non-string zero default)."""
    value = getattr(status, STATUS_FIELDS[option], None) if status is not None else None
    return value if isinstance(value, str) and value else None


def step(option: str, now: str | None, steps: int) -> tuple[str | None, bool]:
    """(new, at_limit) from ``now``; (None, False) from a value off the ladder."""
    ladder = LADDERS[option]
    if now not in ladder:
        return None, False
    if option in WRAPPING:
        return wrap_step(ladder, now, steps), False
    return clamp_step(ladder, now, steps)


def above(option: str, new: str | None, now: str | None) -> bool | None:
    """Whether ``new`` is a higher rung than ``now``; None when either is off the ladder."""
    ladder = LADDERS[option]
    if new not in ladder or now not in ladder:
        return None
    return ladder.index(new) > ladder.index(now)
