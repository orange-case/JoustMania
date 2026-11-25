"""TEMPORARY: the `objectives` GOAL row in `lib/setting_ladders.py` until the menu's PR brings it.

The menu owns `lib/setting_ladders.py`; until its D17 change is merged into this branch, this patches
the shared tables in this process so the pad's GOAL row is `objectives` (the flag's declared order,
wrapping, status field `agent_objectives`). A no-op once the module knows `objectives`; delete then.
"""

from __future__ import annotations

from lib import setting_ladders as _sl

OBJECTIVES = "objectives"
OBJECTIVES_LADDER = ["endurance_focused", "balanced_focused", "accelerate_focused", "chaos_focused", "mixed"]


def apply() -> None:
    if OBJECTIVES in _sl.AGENT_ROW_LADDERS:
        return
    old = getattr(_sl, "GOAL_OPTION", None)
    _sl.OBJECTIVES_OPTION = OBJECTIVES
    _sl.OBJECTIVES_LADDER = OBJECTIVES_LADDER
    rows = {OBJECTIVES: OBJECTIVES_LADDER}  # the GOAL row first, where the goal profile was
    rows.update((k, v) for k, v in _sl.AGENT_ROW_LADDERS.items() if k != old)
    _sl.AGENT_ROW_LADDERS.clear()
    _sl.AGENT_ROW_LADDERS.update(rows)
    _sl.AGENT_ROW_OPTIONS = frozenset(rows)
    _sl.FLAG_LADDERS.pop(old, None)
    _sl.FLAG_LADDERS[OBJECTIVES] = OBJECTIVES_LADDER
    _sl.WRAPPING = (_sl.WRAPPING - {old}) | {OBJECTIVES}
    _sl.PREVIEW_OPTIONS = (_sl.PREVIEW_OPTIONS - {old}) | {OBJECTIVES}
    _sl.AGENT_ROW_STATUS_FIELDS.pop(old, None)
    _sl.AGENT_ROW_STATUS_FIELDS[OBJECTIVES] = "agent_objectives"


apply()
