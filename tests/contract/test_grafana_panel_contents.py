"""UC-035 step 6 and step 7: the named Grafana panels and the provisioned datasources (#1725, #2059).

Step 6 names what each folder shows; this pins every named item to a panel and
the series it reads, so removing or emptying one fails here. Step 7 finds a game's errors in
Jaeger, so the provisioned datasources are exactly Prometheus (VictoriaMetrics) and Jaeger.
"""

from __future__ import annotations

import json
import re

import infra_config as ic
import pytest

GRAFANA = ic.REPO_ROOT / "services" / "grafana"
DASHBOARDS = GRAFANA / "dashboards"
DATASOURCES = GRAFANA / "provisioning" / "datasources"


def _flatten(panels: list[dict], out: dict[str, list[str]]) -> dict[str, list[str]]:
    """Panel title -> its target expressions, rows flattened."""
    for p in panels:
        if p.get("type") == "row":
            _flatten(p.get("panels", []), out)
        else:
            out[p.get("title", "")] = [t.get("expr", "") for t in p.get("targets", [])]
    return out


def _panels(folder: str, uid: str) -> dict[str, list[str]]:
    for path in (DASHBOARDS / folder).glob("*.json"):
        dash = json.loads(path.read_text())
        if dash["uid"] == uid:
            return _flatten(dash["panels"], {})
    raise AssertionError(f"no dashboard {uid} in {folder}")


def _assert_panel_reads(
    panels: dict[str, list[str]], title: str, *patterns: str
) -> None:
    assert title in panels, f"panel {title!r} is missing"
    exprs = " ".join(panels[title])
    for pattern in patterns:
        assert re.search(pattern, exprs), (
            f"{title!r} does not read {pattern!r}: {exprs}"
        )


# (folder, dashboard uid, panel title, series the panel must read)
STEP6_PANELS = [
    # Game: per-player acceleration against the warning and death lines.
    (
        "game",
        "game-players",
        "Acceleration (10 Hz)",
        r"controller_accel_magnitude",
        r"game_effective_warning_threshold",
        r"game_effective_death_threshold",
    ),
    # Game: connection quality per controller (BR-005).
    (
        "game",
        "controller-overview",
        "Connection Quality",
        r"controller_connection_quality",
    ),
    (
        "game",
        "controller-overview",
        "p95 Report Gap (10 s)",
        r"controller_bluetooth_report_gap_p95_seconds",
    ),
    (
        "game",
        "controller-overview",
        "Longest Report Gap (10 s)",
        r"controller_bluetooth_report_gap_max_seconds",
    ),
    # Game: each adapter's identity, HCI error counters and link roles (FR-122).
    (
        "game",
        "controller-overview",
        "Adapter Identity",
        r"pairing_bluetooth_adapter_info",
    ),
    (
        "game",
        "controller-overview",
        "HCI Errors (5 min)",
        r"pairing_bluetooth_adapter_hci_errors_total",
    ),
    (
        "game",
        "controller-overview",
        "Link Roles per Adapter",
        r"pairing_bluetooth_link_role_info",
    ),
    # Agent: decisions by backend and outcome.
    (
        "agent",
        "agent-operations",
        "Backend x Outcome (time range)",
        r"traces_span_metrics_calls_total",
        r"decide_backend",
        r"decide_outcome",
        # The decide span since #2584; agent.decide kept for the series before the rename.
        r'span_name=~"invoke_agent joustmania-agent\|agent\\\\\.decide"',
    ),
    # Agent: remote-model latency and usage.
    (
        "agent",
        "agent-operations",
        "Model Call Latency p95 (by caller, model)",
        r"gen_ai_client_operation_duration_seconds_bucket",
    ),
    (
        "agent",
        "agent-operations",
        "Tokens / min (by caller, model, token type)",
        r"gen_ai_client_token_usage",
    ),
    # Agent: interventions and their measured effect.
    (
        "agent",
        "agent-operations",
        "Decision Timeline (real games)",
        r"agent_decisions_total",
        r'game_kind="real"',
    ),
    (
        "agent",
        "agent-operations",
        "Interventions Dispatched / min (by action)",
        r"agent_interventions_applied_total",
    ),
    (
        "agent",
        "agent-operations",
        "Effect Delta p50 (by signal)",
        r"agent_intervention_effect_delta_ratio_bucket",
    ),
    # Agent: shadow games.
    (
        "agent",
        "agent-operations",
        "Shadow Games Started / min",
        r"game_started_total",
        r"shadow",
    ),
    # Platform: infrastructure and services.
    (
        "platform",
        "joustmania-service-health",
        "CPU per service",
        r"process_cpu_seconds_total",
    ),
    (
        "platform",
        "joustmania-service-health",
        "Host CPU",
        r"system_cpu_time_seconds_total",
    ),
    # Platform: flags and the flag service.
    (
        "platform",
        "feature-flags",
        "Evaluations / s (by flag, top 15)",
        r"feature_flag_evaluation_requests_total",
    ),
    (
        "platform",
        "feature-flags",
        "Writes / min (by flag set, result and op)",
        r"flagservice_writes_total",
    ),
    # Platform: the Flagsmith link (telemetry.md §10.4, #2592).
    (
        "platform",
        "feature-flags",
        "Flagsmith link state",
        r"flagservice_flagsmith_link\b",
    ),
    (
        "platform",
        "feature-flags",
        "Flagsmith change delay p95 (NFR-058)",
        r"flagservice_flagsmith_change_delay_seconds_bucket",
    ),
]


@pytest.mark.uc("UC-035", flow="main", steps=[6])
@pytest.mark.parametrize(
    ("folder", "uid", "title", "patterns"),
    [(f, u, t, p) for f, u, t, *p in STEP6_PANELS],
    ids=[row[2] for row in STEP6_PANELS],
)
def test_step6_named_panel_reads_its_series(folder, uid, title, patterns):
    _assert_panel_reads(_panels(folder, uid), title, *patterns)


@pytest.mark.uc("UC-035", flow="main", steps=[6])
def test_step6_talk_dashboard_is_titled_for_the_talk():
    dash = json.loads((DASHBOARDS / "talk" / "talk.json").read_text())
    assert dash["title"] == "Talk: Two Standards"


@pytest.mark.uc("UC-035", flow="main", steps=[6])
def test_step6_talk_live_is_one_acceleration_panel_with_event_markers():
    dash = json.loads((DASHBOARDS / "talk" / "talk-live.json").read_text())
    assert dash["uid"] == "talk-live" and dash["title"] == "Talk: Live"
    assert [p["id"] for p in dash["panels"]] == [1], "one panel, linked as d-solo panelId=1"
    exprs = [t["expr"] for t in dash["panels"][0]["targets"]]
    assert "controller_accel_magnitude" in exprs[0]
    marks = {a["name"]: a["expr"] for a in dash["annotations"]["list"] if a.get("expr")}
    assert "game_player_deaths_total" in marks["Player out"]
    assert "game_interventions_total" in marks["Agent intervention"]


@pytest.mark.uc("UC-035", flow="main", steps=[7])
def test_step7_grafana_provisions_only_prometheus_and_jaeger_datasources():
    sources = [
        ds
        for path in sorted(DATASOURCES.glob("*.yml"))
        for ds in (ic.load_yaml(path).get("datasources") or [])
    ]
    assert sorted(ds["type"] for ds in sources) == ["jaeger", "prometheus"], sources
    # The dashboard's Experiments links open the agent's dossiers and agent.experiment in Jaeger.
    spa = (ic.REPO_ROOT / "services" / "dashboard" / "src" / "index.html").read_text()
    assert "/experiments?format=html" in spa
    assert "/jaeger/search?service=agent&operation=agent.experiment" in spa


FLAGSMITH_TELEMETRY = (
    ic.REPO_ROOT / "services" / "flag-service" / "flagsmith" / "telemetry.go"
)
FLAGSMITH_METRICS = {
    "flagservice_flagsmith_link",
    "flagservice_flagsmith_pulls_total",
    "flagservice_flagsmith_pushes_total",
    "flagservice_flagsmith_pending",
    "flagservice_flagsmith_out_of_sync",
    "flagservice_flagsmith_conflicts_total",
    "flagservice_flagsmith_change_delay_seconds",
    "flagservice_flagsmith_health_posts_total",
}


def _flagsmith_instrument_labels() -> dict[str, set[str]]:
    """Metric name -> the label names telemetry.go records on it."""
    src = FLAGSMITH_TELEMETRY.read_text()
    names = {
        var: name
        for var, name in re.findall(
            r"(?:m\.)?(\w+)(?:, err)?\s*:?=\s*meter\.\w+\(\s*\"([^\"]+)\"", src
        )
    }
    labels: dict[str, set[str]] = {name: set() for name in names.values()}
    for field, gauge, rest in re.findall(
        r"(?:m\.(\w+)\.(?:Add|Record)\(|o\.ObserveInt64\((\w+),)([^\n]*)", src
    ):
        var = field or gauge
        assert var in names, f"{var} records but is no instrument of telemetry.go"
        labels[names[var]].update(re.findall(r"attribute\.String\(\"(\w+)\"", rest))
    return labels


def _row_panels(folder: str, uid: str, row: str) -> dict[str, list[str]]:
    """Panel title -> target expressions of the panels in one dashboard row."""
    for path in (DASHBOARDS / folder).glob("*.json"):
        dash = json.loads(path.read_text())
        if dash["uid"] != uid:
            continue
        out: dict[str, list[str]] = {}
        inside = False
        for p in dash["panels"]:
            if p.get("type") == "row":
                if inside:
                    break
                inside = p.get("title") == row
                if inside:
                    _flatten(p.get("panels", []), out)
            elif inside:
                _flatten([p], out)
        assert inside or out, f"no row {row!r} in {uid}"
        return out
    raise AssertionError(f"no dashboard {uid} in {folder}")


@pytest.mark.uc("UC-054", cond=["Post-S-3"])
@pytest.mark.uc("UC-055", cond=["Post-S-3", "Post-F-3"])
def test_flagsmith_link_row_reads_every_link_metric():
    """The "Flagsmith link" row reads every link metric of telemetry.md §10.4, by the labels telemetry.go records."""
    labels = _flagsmith_instrument_labels()
    assert set(labels) == FLAGSMITH_METRICS, (
        "telemetry.go and telemetry.md §10.4 list different link metrics"
    )
    panels = _row_panels("platform", "feature-flags", "Flagsmith link")
    assert panels, "the Flagsmith link row has no panels"
    metric_re = re.compile(
        r"\b(" + "|".join(sorted(FLAGSMITH_METRICS, key=len, reverse=True)) + r")(_bucket)?\b"
    )
    read: set[str] = set()
    for title, exprs in panels.items():
        assert exprs, f"{title!r} has no query"
        for expr in exprs:
            found = metric_re.findall(expr)
            assert found, f"{title!r} reads no Flagsmith link metric: {expr}"
            allowed = set().union(*(labels[m] for m, _ in found))
            if any(bucket for _, bucket in found):
                allowed.add("le")
            used = set()
            for group in re.findall(r"\bby \(([^)]*)\)", expr):
                used.update(x.strip() for x in group.split(",") if x.strip())
            for selector in re.findall(r"\{([^}]*)\}", expr):
                used.update(re.findall(r"(\w+)\s*(?:=~|!~|!=|=)", selector))
            used.update(re.findall(r"label_replace\(vector\(0\),\s*\"(\w+)\"", expr))
            unknown = used - allowed
            assert not unknown, (
                f"{title!r} uses labels {sorted(unknown)} that telemetry.go never records: {expr}"
            )
            read.update(m for m, _ in found)
    missing = FLAGSMITH_METRICS - read
    assert not missing, f"the Flagsmith link row does not read {sorted(missing)}"
    # Refusals are pushes the Admin API rejected, by reason (no refused gauge).
    assert any(
        re.search(r"flagservice_flagsmith_pushes_total\{result=\"rejected\"\}", e)
        and re.search(r"\bby \([^)]*\breason\b", e)
        for exprs in panels.values()
        for e in exprs
    ), "no panel shows the refused pushes by reason"


@pytest.mark.uc("UC-035", flow="main", steps=[6], br=["BR-004"])
def test_step6_every_latency_panel_reads_a_histogram():
    """Every quantile panel reads a histogram's ``_bucket`` series, so it uses the declared buckets."""
    quantile_exprs = [
        (path.name, expr)
        for path in sorted(DASHBOARDS.rglob("*.json"))
        for exprs in _flatten(json.loads(path.read_text())["panels"], {}).values()
        for expr in exprs
        if "histogram_quantile" in expr
    ]
    assert quantile_exprs, "no latency panel found"
    for name, expr in quantile_exprs:
        assert re.search(r"_bucket\b", expr), (
            f"{name}: quantile without a histogram: {expr}"
        )


@pytest.mark.uc("UC-054", cond=["Post-S-3"])
def test_flagsmith_link_state_stat_is_an_instant_query():
    """The link-state stat shows only the current state: a range query would keep every state that was 1 in the range."""

    def walk(panels, found):
        for p in panels:
            if p.get("title") == "Flagsmith link state":
                found.append(p)
            walk(p.get("panels", []), found)

    for path in (DASHBOARDS / "platform").glob("*.json"):
        dash = json.loads(path.read_text())
        if dash["uid"] != "feature-flags":
            continue
        found = []
        walk(dash["panels"], found)
        assert len(found) == 1, "expected one 'Flagsmith link state' panel"
        assert found[0]["targets"] and all(t.get("instant") is True for t in found[0]["targets"])
        return
    raise AssertionError("no feature-flags dashboard")


# The dashboards that mark when the agent decided: (folder, uid).
DECISION_ANNOTATED = [("agent", "agent-operations"), ("game", "game-players")]


@pytest.mark.parametrize(("folder", "uid"), DECISION_ANNOTATED)
def test_agent_decisions_are_annotated(folder: str, uid: str) -> None:
    """Applied and blocked real-game decisions are on by default, shadow ones off."""
    dash = next(
        d
        for d in (json.loads(p.read_text()) for p in (DASHBOARDS / folder).glob("*.json"))
        if d["uid"] == uid
    )
    anns = {a["name"]: a for a in dash["annotations"]["list"]}
    expected = {
        "Agent decision: applied": True,
        "Agent decision: blocked": True,
        "Agent decision: shadow games": False,
    }
    for name, enabled in expected.items():
        assert name in anns, f"{uid}: annotation {name!r} is missing"
        a = anns[name]
        assert a["datasource"]["uid"] == "prometheus"
        assert "agent_decisions_total" in a["expr"]
        assert a["enable"] is enabled, f"{uid}: {name!r} enable should be {enabled}"
