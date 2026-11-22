"""Every span-metrics panel matches both decide span names during the transition (#2584).

The span_metrics connector only sees the agent's decide span (collector
`filter/agent_decide`), so every `traces_span_metrics_*` expression in a Grafana
dashboard is about that span. Since #2584 it is named `decision.SpanDecide`
("invoke_agent joustmania-agent"); series from before the rename carry
`span_name="agent.decide"`. A panel that matches only one name either goes empty
on a stack with older agent images or loses the history before the rename.
"""

from __future__ import annotations

import json
import re

import infra_config as ic
import pytest

DASHBOARDS = ic.REPO_ROOT / "services" / "grafana" / "dashboards"


def _exprs(node):
    if isinstance(node, dict):
        for key, value in node.items():
            if key == "expr" and isinstance(value, str):
                yield value
            else:
                yield from _exprs(value)
    elif isinstance(node, list):
        for value in node:
            yield from _exprs(value)


def _span_metrics_exprs() -> list[tuple[str, str]]:
    out = []
    for path in sorted(DASHBOARDS.rglob("*.json")):
        rel = path.relative_to(DASHBOARDS).as_posix()
        for expr in _exprs(json.loads(path.read_text(encoding="utf-8"))):
            if "traces_span_metrics_" in expr:
                out.append((rel, expr))
    return out


def both_names_selector() -> str:
    """The PromQL selector as written in the expression (regex dot escaped as `\\.`)."""
    old = ic.OLD_DECIDE_SPAN.replace(".", "\\\\.")
    new = ic.decide_span_name().replace(".", "\\\\.")
    return f'span_name=~"{new}|{old}"'


def test_span_metrics_panels_exist():
    files = {rel for rel, _ in _span_metrics_exprs()}
    assert {"agent/agent-operations.json", "talk/talk.json"} <= files, files


@pytest.mark.uc("UC-035", flow="main")
@pytest.mark.parametrize(("dashboard", "expr"), _span_metrics_exprs())
def test_span_metrics_expr_matches_both_decide_span_names(dashboard, expr):
    """UC-035: the decide panels read the new name and keep the series from before #2584."""
    selectors = re.findall(r'span_name\s*=~?\s*"[^"]*"', expr)
    assert selectors, f"{dashboard}: no span_name selector in {expr}"
    want = both_names_selector()
    for selector in selectors:
        assert selector == want, f"{dashboard}: {selector!r}, want {want!r} in {expr}"
    assert f'span_name="{ic.OLD_DECIDE_SPAN}"' not in expr, f"{dashboard}: old name only: {expr}"
