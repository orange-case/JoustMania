"""The agent observe path: collector fan-out to the agent (UC-041 Pre-1, Post-F-1).

Static checks over services/otel-collector/config.yaml and docker-compose.yml.
The best-effort exporter settings (timeout, retries, queue) are covered by
test_collector_best_effort.py.
"""

from __future__ import annotations

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


# --- The agent's log events (#2722, FR-250, UC-100 BR-012, UC-041 A8, BR-009) ---

_AGENT = {"orangecase.audience": "agent"}


@pytest.mark.uc("UC-041", flow="A8", steps=[1], cond=["Pre-1"])
def test_only_the_logs_agent_pipeline_sends_logs_to_the_agent(collector):
    """UC-041 Pre-1, A8 step 1: the agent's log events reach it through their own
    pipeline (filter before batch); the logs pipeline keeps ending in the debug sink."""
    pipelines = collector["service"]["pipelines"]
    assert pipelines["logs/agent"] == {
        "receivers": ["otlp"],
        "processors": [
            "memory_limiter",
            "filter/agent_events",
            "batch/agent_events",
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
    assert collector["processors"]["batch/agent_events"]["timeout"] == "1s"


@pytest.mark.uc("UC-041", br=["BR-009"])
@pytest.mark.uc("UC-101", br=["BR-010"], cond=["Post-F-2"])
@pytest.mark.parametrize("marked", [False, True], ids=["unmarked", "marked"])
@pytest.mark.parametrize(
    "event_name", ["death_warning", "player_death", "intervention"]
)
def test_no_moment_log_event_reaches_the_agent(collector, event_name, marked):
    """UC-041 BR-009, C-001: the agent reads deaths from the elimination-order gauge, so no
    moment log event passes, in any shape and with or without the agent marker; the
    agent's inputs are those of a game without the challenge (UC-101 Post-F-2)."""
    attributes: dict[str, object] = {
        "moment.id": "game_2786/P1/3",
        "player.serial": "P1",
        "game.id": "game_2786",
        "game.kind": "real",
        **(_AGENT if marked else {}),
    }
    conditions = collector["processors"]["filter/agent_events"]["log_conditions"]
    assert ic.drops_log(conditions, event_name, attributes, {}) is True


@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.uc("UC-041", flow="A8", steps=[1])
@pytest.mark.parametrize(
    ("event_name", "attributes", "resource", "dropped"),
    [
        ("game.start", {"game.id": "g", **_AGENT}, {}, False),
        ("game.end", {"game.id": "g", **_AGENT}, {}, False),
        ("game.start", {"game.id": "g"}, {}, True),
        ("game.end", {"game.id": "g"}, {}, True),
        ("game.start", {"orangecase.audience": "gateway"}, {}, True),
        ("game.start", _AGENT, {"orangecase.synthetic": True}, True),
        ("game.end", _AGENT, {"orangecase.synthetic": "true"}, True),
        ("game.start", _AGENT, {"orangecase.synthetic": False}, False),
        ("", _AGENT, {}, True),
        ("", {}, {}, True),
        ("game.round", _AGENT, {}, True),
    ],
    ids=[
        "marked-start-kept",
        "marked-end-kept",
        "unmarked-start-dropped",
        "unmarked-end-dropped",
        "other-audience-dropped",
        "synthetic-bool-start-dropped",
        "synthetic-string-end-dropped",
        "not-synthetic-kept",
        "marked-ordinary-dropped",
        "ordinary-dropped",
        "marked-other-name-dropped",
    ],
)
def test_agent_events_filter_passes_lifecycle_records_only_when_marked(
    collector, event_name, attributes, resource, dropped
):
    """UC-041 A8 step 1, UC-100 BR-012, C-001: `game.start` and `game.end` reach the agent
    only with `orangecase.audience` = `agent`; the marker lets no other record through and
    synthetic data stays out (#2577)."""
    conditions = collector["processors"]["filter/agent_events"]["log_conditions"]
    assert ic.drops_log(conditions, event_name, attributes, resource) is dropped


@pytest.mark.uc("UC-041", flow="A8", steps=[5], br=["BR-004"])
@pytest.mark.parametrize(
    ("attributes", "resource", "dropped"),
    [
        ({"game.id": "g", "game.phase.span": "game_cycle", **_AGENT}, {}, False),
        ({"game.id": "g", "game.phase.span": "gameplay_phase"}, {}, True),
        (_AGENT, {"orangecase.synthetic": True}, True),
    ],
    ids=["marked-kept", "unmarked-dropped", "synthetic-dropped"],
)
def test_agent_events_filter_passes_phase_start_only_when_marked(
    collector, attributes, resource, dropped
):
    """UC-041 BR-004: `game.phase.start` carries the span ids to the agent, marked like `game.start`."""
    conditions = collector["processors"]["filter/agent_events"]["log_conditions"]
    assert ic.drops_log(conditions, "game.phase.start", attributes, resource) is dropped
