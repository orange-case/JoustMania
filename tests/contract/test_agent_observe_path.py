"""The agent observe path: collector fan-out to the agent (UC-041 Pre-1, Post-F-1).

Static checks over services/otel-collector/config.yaml and docker-compose.yml.
The best-effort exporter settings (timeout, retries, queue) are covered by
test_collector_best_effort.py.
"""

from __future__ import annotations

import re

import infra_config as ic
import pytest


@pytest.fixture(scope="module")
def collector():
    return ic.load_yaml(ic.COLLECTOR_FILE)


@pytest.mark.uc("UC-041", flow="main", cond=["Pre-1"])
@pytest.mark.parametrize("pipeline", ["traces", "metrics"])
def test_collector_fans_out_traces_and_metrics_to_the_agent_on_4317(
    collector, pipeline
):
    """UC-041 Pre-1: both pipelines export to otlp/agent, which targets agent:4317."""
    assert collector["exporters"]["otlp/agent"]["endpoint"] == "agent:4317"
    pipe = collector["service"]["pipelines"][pipeline]
    assert "otlp/agent" in pipe["exporters"]
    assert "otlp" in pipe["receivers"]


@pytest.mark.uc("UC-041", flow="main", cond=["Pre-1"])
def test_agent_service_listens_on_the_port_the_collector_targets():
    """UC-041 Pre-1: the compose agent service listens on :4317."""
    agent = ic.compose_services()["agent"]
    assert "AGENT_LISTEN_ADDR=:4317" in agent["environment"]
    assert "4317" in [str(p) for p in agent["expose"]]


@pytest.mark.uc("UC-041", flow="main", cond=["Post-F-1"])
@pytest.mark.parametrize(
    ("pipeline", "storage"),
    [("traces", "otlp/jaeger"), ("metrics", "prometheus_remote_write")],
)
def test_storage_exporters_are_independent_of_the_agent_exporter(
    collector, pipeline, storage
):
    """UC-041 Post-F-1 (static proxy): the storage exporters do not depend on the agent.

    A static config check, not a runtime test with the agent down. The storage
    exporter is its own exporter in the same fan-out pipeline, targets its own
    backend, and is the only one of the two that keeps the collector's default
    retries; the agent exporter is the only one that is best-effort (short
    timeout, no retries, bounded queue), so a dead agent costs at most its own
    timeout and drops (see also test_collector_best_effort.py).
    """
    exporters = collector["exporters"]
    pipe_exporters = collector["service"]["pipelines"][pipeline]["exporters"]
    assert storage in pipe_exporters and "otlp/agent" in pipe_exporters
    assert storage != "otlp/agent"

    agent = exporters["otlp/agent"]
    store = exporters[storage]
    assert "agent" not in store["endpoint"]
    assert agent["timeout"] == "2s"
    assert agent["retry_on_failure"] == {"enabled": False}
    assert agent["sending_queue"]["enabled"] is True
    # The best-effort settings belong to the agent exporter only: the storage
    # exporter carries none of them, so it keeps the collector defaults.
    for key in ("timeout", "retry_on_failure", "sending_queue"):
        assert key not in store, f"{storage} must not copy the agent's {key}"


# --- Moment log events to the agent (#2658 slice 2, FR-218, UC-041 A7, BR-009) ---

_MISSING = object()


def _drops_log(
    conditions: list[str],
    event_name: str,
    attributes: dict[str, object],
    resource: dict[str, object],
) -> bool:
    """Evaluate the filter/agent_moments OTTL drop conditions for one log record.

    Supports exactly the terms the filter uses (`log.event_name != "x"`,
    `log.attributes["k"] == nil`, `resource.attributes["k"] == <bool|"str">`), joined
    by `and` / `or` (`and` binds tighter); any other term fails the test.
    """

    def value(path: str) -> object:
        if path == "log.event_name":
            return event_name
        m = re.fullmatch(r'(log|resource)\.attributes\["([^"]+)"\]', path)
        assert m, f"unsupported OTTL path {path!r}"
        source = attributes if m.group(1) == "log" else resource
        return source.get(m.group(2), _MISSING)

    def literal(text: str) -> object:
        if text == "nil":
            return _MISSING
        if text in ("true", "false"):
            return text == "true"
        m = re.fullmatch(r'"([^"]*)"', text)
        assert m, f"unsupported OTTL literal {text!r}"
        return m.group(1)

    def term(text: str) -> bool:
        m = re.fullmatch(r"(\S+) (==|!=) (.+)", text.strip())
        assert m, f"unsupported OTTL term {text!r}"
        left, op, right = value(m.group(1)), m.group(2), literal(m.group(3).strip())
        # OTTL: a bool never equals a string; type-strict comparison.
        equal = type(left) is type(right) and left == right
        return equal if op == "==" else not equal

    return any(
        any(all(term(t) for t in alt.split(" and ")) for alt in cond.split(" or "))
        for cond in conditions
    )


@pytest.mark.uc("UC-041", flow="A7", steps=[1], cond=["Pre-1"])
def test_only_the_logs_agent_pipeline_sends_logs_to_the_agent(collector):
    """UC-041 Pre-1, A7 step 1: moment log events reach the agent through their own
    pipeline (filter before batch); the logs pipeline keeps ending in the debug sink."""
    pipelines = collector["service"]["pipelines"]
    assert pipelines["logs/agent"] == {
        "receivers": ["otlp"],
        "processors": [
            "memory_limiter",
            "filter/agent_moments",
            "batch/moments",
            "resource",
        ],
        "exporters": ["otlp/agent"],
    }
    assert pipelines["logs"]["exporters"] == ["debug"]
    to_agent = [
        name
        for name, p in pipelines.items()
        if name.split("/")[0] == "logs" and "otlp/agent" in p["exporters"]
    ]
    assert to_agent == ["logs/agent"]
    assert collector["processors"]["batch/moments"]["timeout"] == "1s"


@pytest.mark.uc("UC-041", flow="A7", steps=[1])
@pytest.mark.parametrize(
    ("event_name", "attributes", "resource", "dropped"),
    [
        ("death_warning", {"moment.id": "g/P1/1"}, {}, False),
        ("player_death", {"moment.id": "g/P1/2"}, {}, False),
        ("", {}, {}, True),
        ("", {"moment.id": "g:P1:x:1"}, {}, True),
        ("intervention", {"moment.id": "g:intervention:1"}, {}, True),
        ("death_warning", {}, {}, True),
        ("player_death", {"moment.id": "g/P1/2"}, {"orangecase.synthetic": True}, True),
        (
            "player_death",
            {"moment.id": "g/P1/2"},
            {"orangecase.synthetic": "true"},
            True,
        ),
        (
            "player_death",
            {"moment.id": "g/P1/2"},
            {"orangecase.synthetic": False},
            False,
        ),
    ],
    ids=[
        "warning-kept",
        "death-kept",
        "ordinary-dropped",
        "ordinary-with-id-dropped",
        "intervention-dropped",
        "warning-without-id-dropped",
        "synthetic-bool-dropped",
        "synthetic-string-dropped",
        "not-synthetic-kept",
    ],
)
def test_agent_moments_filter_passes_only_moment_log_events(
    collector, event_name, attributes, resource, dropped
):
    """UC-041 A7 step 1, BR-009, C-001: the collector forwards only death_warning and
    player_death records that carry a moment id, and never synthetic data (#2577)."""
    conditions = collector["processors"]["filter/agent_moments"]["log_conditions"]
    assert _drops_log(conditions, event_name, attributes, resource) is dropped


@pytest.mark.uc("UC-101", br=["BR-010"], cond=["Post-F-2"])
@pytest.mark.parametrize(
    "intervention_id",
    ["0f1e2d3c4b5a6978", None],
    ids=["with-id", "without-id"],
)
def test_intervention_log_event_never_reaches_the_agent(collector, intervention_id):
    """UC-101 BR-010, Post-F-2: an intervention moment sent as a log event (shape `log_event`
    or `both`) carries a real moment id of the form `<game.id>/<serial>/<n>` and the moment's
    identity attributes, like a warning, and still never passes `filter/agent_moments`, so the
    agent's inputs are those of a game without the challenge. The span half (the
    `game.intervention` span is ignored) is the Go test `TestApplySpans_InterventionMomentSpanIsIgnored`."""
    attributes: dict[str, object] = {
        "moment.id": "game_2665/P1/3",
        "intervention.type": "grant_shield",
        "player.serial": "P1",
        "game.id": "game_2665",
        "game.kind": "real",
    }
    if intervention_id is not None:
        attributes["intervention.id"] = intervention_id
    conditions = collector["processors"]["filter/agent_moments"]["log_conditions"]
    assert _drops_log(conditions, "intervention", attributes, {}) is True
    # The same record named as a warning passes: the drop is by name, not by a missing id.
    assert _drops_log(conditions, "death_warning", attributes, {}) is False
