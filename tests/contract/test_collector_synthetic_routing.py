"""Synthetic telemetry routing in the case collector (#2577, UC-041 BR-011, telemetry.md §1.1).

Telemetry marked ``orangecase.synthetic`` (a bool ``true`` or the string ``"true"``, on the
resource or on the span / data point / log record) goes to the gateway always, to the agent
never, and to Jaeger / VictoriaMetrics only with ``ORANGECASE_SYNTHETIC_IN_CASE=true``.
Unmarked telemetry is routed as before.

Static checks over config.yaml (and its merge with config.gateway.yaml), a small evaluator for
the filter conditions, and, when docker is available, the same filters run in the compose
collector image against fixture OTLP data.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.request
import uuid

import infra_config as ic
import pytest
from test_collector_gateway_overlay import GATEWAY_PIPELINES, OVERLAY_FILE, _merge

AGENT_FILTER = "filter/synthetic"
IN_CASE_FILTER = "filter/synthetic_in_case"
SWITCH = "ORANGECASE_SYNTHETIC_IN_CASE"
AGENT_EXPORTER = "otlp_http/agent"
IN_CASE_EXPORTERS = {"otlp/jaeger", "prometheus_remote_write", "span_metrics"}

pytestmark = pytest.mark.uc("UC-041", br=["BR-011"])


@pytest.fixture(scope="module")
def collector():
    return ic.load_yaml(ic.COLLECTOR_FILE)


@pytest.fixture(scope="module")
def merged(collector):
    return _merge(collector, ic.load_yaml(OVERLAY_FILE))


def _otlp_pipelines(config: dict) -> dict:
    return {
        name: spec
        for name, spec in config["service"]["pipelines"].items()
        if "otlp" in spec["receivers"]
    }


# --------------------------------------------------------------------------- #
# Condition evaluator: exactly the OTTL forms the synthetic filters use.
# --------------------------------------------------------------------------- #
_MISSING = object()
_ENV_RE = re.compile(r"\$\{env:(\w+):-([^}]*)\}")


def _substitute_env(condition: str, env: dict[str, str]) -> str:
    """What the collector's confmap does with an embedded ``${env:VAR:-default}``."""
    return _ENV_RE.sub(lambda m: env.get(m.group(1), m.group(2)), condition)


def _drops(conditions: list[str], item: dict[str, dict], env: dict[str, str]) -> bool:
    """Whether any condition drops ``item`` ({"resource": {...}, "span": {...}, ...})."""

    def operand(text: str) -> object:
        text = text.strip()
        m = re.fullmatch(r'(resource|span|datapoint|log)\.attributes\["([^"]+)"\]', text)
        if m:
            return item.get(m.group(1), {}).get(m.group(2), _MISSING)
        if text in ("true", "false"):
            return text == "true"
        m = re.fullmatch(r'"([^"]*)"', text)
        assert m, f"unsupported OTTL operand {text!r}"
        return m.group(1)

    def term(text: str) -> bool:
        m = re.fullmatch(r"(.+?) (==|!=) (.+)", text.strip())
        assert m, f"unsupported OTTL term {text!r}"
        left, right = operand(m.group(1)), operand(m.group(3))
        # OTTL compares type-strictly: a bool never equals a string.
        equal = type(left) is type(right) and left == right
        return equal if m.group(2) == "==" else not equal

    return any(
        any(all(term(t) for t in alt.split(" and ")) for alt in cond.split(" or "))
        for cond in (_substitute_env(c, env) for c in conditions)
    )


MARKERS = [True, "true"]
NOT_MARKERS = [False, "false", "True", "TRUE", 1, "yes"]


def _conditions(config: dict, name: str, signal: str) -> list[str]:
    return config["processors"][name][f"{signal}_conditions"]


# --------------------------------------------------------------------------- #
# Pipeline wiring
# --------------------------------------------------------------------------- #
def test_every_pipeline_to_the_agent_drops_synthetic_telemetry(collector):
    """The agent exporter (otlp_http/agent) never receives synthetic telemetry: each pipeline
    that feeds it runs the agent's synthetic filter (logs: filter/agent_events), and none of
    them depends on the in-case switch."""
    to_agent = {
        name: spec
        for name, spec in collector["service"]["pipelines"].items()
        if AGENT_EXPORTER in spec["exporters"]
    }
    assert set(to_agent) == {"traces/agent", "metrics/agent", "logs/agent"}
    for name, spec in to_agent.items():
        assert spec["exporters"] == [AGENT_EXPORTER], name
        expected = "filter/agent_events" if name == "logs/agent" else AGENT_FILTER
        assert expected in spec["processors"], name
        assert IN_CASE_FILTER not in spec["processors"], name


def test_every_otlp_pipeline_to_jaeger_or_victoriametrics_runs_the_in_case_filter(
    collector,
):
    """Jaeger and VictoriaMetrics (also through the span metrics) take synthetic telemetry
    only behind the in-case switch."""
    feeding = {
        name: spec
        for name, spec in _otlp_pipelines(collector).items()
        if IN_CASE_EXPORTERS & set(spec["exporters"])
    }
    assert set(feeding) == {"traces", "metrics", "traces/span_metrics"}
    for name, spec in feeding.items():
        assert IN_CASE_FILTER in spec["processors"], name
        assert AGENT_FILTER not in spec["processors"], name


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
@pytest.mark.parametrize("pipeline", sorted(GATEWAY_PIPELINES))
def test_gateway_pipelines_always_carry_synthetic_telemetry(merged, pipeline):
    """The gateway gets synthetic and real data through the same processors: no synthetic
    filter on a gateway pipeline."""
    processors = merged["service"]["pipelines"][pipeline]["processors"]
    assert AGENT_FILTER not in processors
    assert IN_CASE_FILTER not in processors
    assert "filter/agent_events" not in processors


@pytest.mark.parametrize("signal", ["traces", "metrics"])
def test_unmarked_telemetry_is_routed_as_before_the_split(collector, signal):
    """The agent twin runs the same processors as the storage pipeline, only the synthetic
    filter differs, so unmarked data reaches the agent and the backends unchanged; together
    they export to the same exporters the single pipeline did before #2577."""
    pipelines = collector["service"]["pipelines"]
    storage, agent = pipelines[signal], pipelines[f"{signal}/agent"]
    assert storage["receivers"] == agent["receivers"] == ["otlp"]
    assert [p for p in storage["processors"] if p != IN_CASE_FILTER] == [
        p for p in agent["processors"] if p != AGENT_FILTER
    ]
    before = {
        "traces": ["otlp/jaeger", AGENT_EXPORTER, "debug"],
        "metrics": ["prometheus_remote_write", AGENT_EXPORTER],
    }[signal]
    assert sorted(storage["exporters"] + agent["exporters"]) == sorted(before)


@pytest.mark.parametrize("pipeline", ["traces", "traces/agent", "metrics", "metrics/agent"])
def test_synthetic_filter_runs_before_the_batch(collector, pipeline):
    """Filter first: a batch never holds data the pipeline then drops."""
    processors = collector["service"]["pipelines"][pipeline]["processors"]
    synthetic = next(p for p in processors if p in (AGENT_FILTER, IN_CASE_FILTER))
    batch = next(p for p in processors if p.split("/")[0] == "batch")
    assert processors[0] == "memory_limiter"
    assert processors.index(synthetic) < processors.index(batch)


# --------------------------------------------------------------------------- #
# Filter conditions
# --------------------------------------------------------------------------- #
_TRACE_PLACES = ["resource", "span"]
_METRIC_PLACES = ["resource", "datapoint"]


@pytest.mark.parametrize(
    ("signal", "place"),
    [("trace", p) for p in _TRACE_PLACES] + [("metric", p) for p in _METRIC_PLACES],
)
@pytest.mark.parametrize("marker", MARKERS, ids=repr)
def test_agent_filter_drops_every_marked_item(collector, signal, place, marker):
    item = {place: {"orangecase.synthetic": marker}}
    assert _drops(_conditions(collector, AGENT_FILTER, signal), item, {}) is True
    # The agent route ignores the in-case switch.
    assert _drops(_conditions(collector, AGENT_FILTER, signal), item, {SWITCH: "true"})


@pytest.mark.parametrize(
    ("signal", "place"),
    [("trace", p) for p in _TRACE_PLACES] + [("metric", p) for p in _METRIC_PLACES],
)
@pytest.mark.parametrize("value", NOT_MARKERS, ids=repr)
@pytest.mark.parametrize("name", [AGENT_FILTER, IN_CASE_FILTER])
def test_no_filter_drops_unmarked_items(collector, name, signal, place, value):
    """Real telemetry passes: no marker, or a spelling other than true / "true"."""
    conditions = _conditions(collector, name, signal)
    assert _drops(conditions, {}, {}) is False
    assert _drops(conditions, {place: {"service.name": "game-coordinator-service"}}, {}) is False
    assert _drops(conditions, {place: {"orangecase.synthetic": value}}, {}) is False


@pytest.mark.parametrize(
    ("signal", "place"),
    [("trace", p) for p in _TRACE_PLACES] + [("metric", p) for p in _METRIC_PLACES],
)
@pytest.mark.parametrize("marker", MARKERS, ids=repr)
@pytest.mark.parametrize(
    ("env", "dropped"),
    [({}, True), ({SWITCH: "false"}, True), ({SWITCH: "true"}, False)],
    ids=["unset", "false", "true"],
)
def test_in_case_filter_follows_the_switch(collector, signal, place, marker, env, dropped):
    """Default off: Jaeger and VictoriaMetrics hold no synthetic data unless the switch is true."""
    item = {place: {"orangecase.synthetic": marker}}
    assert _drops(_conditions(collector, IN_CASE_FILTER, signal), item, env) is dropped


def test_in_case_switch_defaults_off_in_every_condition(collector):
    for signal in ("trace", "metric"):
        for condition in _conditions(collector, IN_CASE_FILTER, signal):
            assert condition.startswith(f'"${{env:{SWITCH}:-false}}" != "true" and '), condition


@pytest.mark.parametrize("marker", MARKERS, ids=repr)
@pytest.mark.parametrize("place", ["resource", "log"])
def test_agent_log_events_drop_synthetic_on_resource_and_record(collector, marker, place):
    """logs/agent: a synthetic game.start never reaches the agent, marked on its resource or
    on the record itself."""
    conditions = collector["processors"]["filter/agent_events"]["log_conditions"]
    attributes = {"orangecase.audience": "agent", "game.id": "g"}
    resource: dict[str, object] = {}
    (attributes if place == "log" else resource)["orangecase.synthetic"] = marker
    assert ic.drops_log(conditions, "game.start", attributes, resource) is True
    del (attributes if place == "log" else resource)["orangecase.synthetic"]
    assert ic.drops_log(conditions, "game.start", attributes, resource) is False


def test_compose_passes_the_in_case_switch_default_off():
    env = ic.compose_services()["otel-collector"]["environment"]
    assert f"{SWITCH}=${{{SWITCH}:-false}}" in env


# --------------------------------------------------------------------------- #
# The filters in the real collector (compose image), with fixture OTLP data.
# --------------------------------------------------------------------------- #
_IMAGE_VAR_RE = re.compile(r"\$\{(\w+):-((?:[^{}]|\$\{[^}]*\})*)\}")


def _collector_image() -> str:
    image = ic.compose_image("otel-collector")
    while _IMAGE_VAR_RE.search(image):
        image = _IMAGE_VAR_RE.sub(
            lambda m: os.environ.get(m.group(1)) or m.group(2), image
        )
    return image


def _attrs(values: dict[str, object]) -> list[dict]:
    def value(v: object) -> dict:
        return {"boolValue": v} if isinstance(v, bool) else {"stringValue": str(v)}

    return [{"key": k, "value": value(v)} for k, v in values.items()]


_SPANS = {
    "REAL": ({}, {}),
    "REAL_CAPITAL": ({"orangecase.synthetic": "True"}, {}),
    "REAL_FALSE": ({"orangecase.synthetic": False}, {}),
    "SYN_RESOURCE_BOOL": ({"orangecase.synthetic": True}, {}),
    "SYN_RESOURCE_STR": ({"orangecase.synthetic": "true"}, {}),
    "SYN_SPAN_BOOL": ({}, {"orangecase.synthetic": True}),
    "SYN_SPAN_STR": ({}, {"orangecase.synthetic": "true"}),
}
_METRICS = {
    "REAL_M": ({}, {}),
    "SYN_M_RESOURCE": ({"orangecase.synthetic": True}, {}),
    "SYN_M_DATAPOINT": ({}, {"orangecase.synthetic": "true"}),
}


def _traces_body() -> dict:
    return {
        "resourceSpans": [
            {
                "resource": {"attributes": _attrs({"service.name": "fixture", **res})},
                "scopeSpans": [
                    {
                        "spans": [
                            {
                                "traceId": "0af7651916cd43dd8448eb211c80319c",
                                "spanId": f"{i + 1:016x}",
                                "name": name,
                                "kind": 5,
                                "startTimeUnixNano": "1",
                                "endTimeUnixNano": "2",
                                "attributes": _attrs(span),
                            }
                        ]
                    }
                ],
            }
            for i, (name, (res, span)) in enumerate(_SPANS.items())
        ]
    }


def _metrics_body() -> dict:
    return {
        "resourceMetrics": [
            {
                "resource": {"attributes": _attrs({"service.name": "fixture", **res})},
                "scopeMetrics": [
                    {
                        "metrics": [
                            {
                                "name": name,
                                "gauge": {
                                    "dataPoints": [
                                        {
                                            "asDouble": 1,
                                            "timeUnixNano": "1",
                                            "attributes": _attrs(dp),
                                        }
                                    ]
                                },
                            }
                        ]
                    }
                ],
            }
            for name, (res, dp) in _METRICS.items()
        ]
    }


def _routing_config(collector: dict) -> dict:
    """The collector's own synthetic filters, one debug sink per destination."""
    sinks = {"agent": AGENT_FILTER, "incase": IN_CASE_FILTER, "gateway": None}
    pipelines = {}
    for sink, processor in sinks.items():
        for signal in ("traces", "metrics"):
            pipelines[f"{signal}/{sink}"] = {
                "receivers": ["otlp"],
                "processors": [processor] if processor else [],
                "exporters": [f"debug/{sink}"],
            }
    return {
        "receivers": {"otlp": {"protocols": {"http": {"endpoint": "0.0.0.0:4318"}}}},
        "processors": {
            AGENT_FILTER: collector["processors"][AGENT_FILTER],
            IN_CASE_FILTER: collector["processors"][IN_CASE_FILTER],
        },
        "exporters": {f"debug/{s}": {"verbosity": "detailed"} for s in sinks},
        "service": {"pipelines": pipelines},
    }


_SINK_RE = re.compile(r'"otelcol\.component\.id": "debug/(\w+)"')
_NAME_RE = re.compile(r"(?:^|\s)(?:Name\s*:|-> Name:)\s*([A-Z_]+)\b")


def _received(logs: str) -> dict[str, set[str]]:
    """Span and metric names per debug sink, from the debug exporter's detailed output."""
    out: dict[str, set[str]] = {"agent": set(), "incase": set(), "gateway": set()}
    sink = None
    for line in logs.splitlines():
        m = _SINK_RE.search(line)
        if m:
            sink = m.group(1)
            continue
        m = _NAME_RE.search(line)
        if m and sink:
            out[sink].add(m.group(1))
    return out


def _post(port: int, signal: str, body: dict) -> None:
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}/v1/{signal}",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        assert resp.status == 200


def _run_collector(tmp_path, collector: dict, switch: str | None) -> dict[str, set[str]]:
    cfg = tmp_path / "routing.json"
    cfg.write_text(json.dumps(_routing_config(collector)))
    name = f"uc041-synthetic-{uuid.uuid4().hex[:8]}"
    env = ["-e", f"{SWITCH}={switch}"] if switch is not None else []
    subprocess.run(
        [
            "docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::4318", *env,
            "-v", f"{cfg}:/etc/routing.json:ro",
            _collector_image(), "--config=/etc/routing.json",
        ],
        capture_output=True, text=True, check=True, timeout=300,
    )  # fmt: skip
    try:
        port = int(
            subprocess.run(
                ["docker", "port", name, "4318/tcp"],
                capture_output=True, text=True, check=True, timeout=30,
            ).stdout.split(":")[-1].split()[0]
        )  # fmt: skip
        deadline = time.monotonic() + 30
        while True:
            try:
                _post(port, "traces", _traces_body())
                break
            except (urllib.error.URLError, ConnectionError, OSError):
                if time.monotonic() > deadline:
                    raise
                time.sleep(0.5)
        _post(port, "metrics", _metrics_body())
        expected = {"REAL", "REAL_M"}
        deadline = time.monotonic() + 15
        while True:
            logs = subprocess.run(
                ["docker", "logs", name], capture_output=True, text=True, timeout=30
            )
            got = _received(logs.stdout + logs.stderr)
            if all(expected <= names for names in got.values()):
                return got
            if time.monotonic() > deadline:
                raise AssertionError(f"collector exported too little: {got}")
            time.sleep(0.5)
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True, check=False)


_REAL = {n for n in [*_SPANS, *_METRICS] if n.startswith("REAL")}
_ALL = set(_SPANS) | set(_METRICS)


@pytest.mark.parametrize(
    ("switch", "in_case"),
    [(None, _REAL), ("true", _ALL)],
    ids=["switch-unset", "switch-true"],
)
def test_collector_routes_fixture_synthetic_telemetry(tmp_path, collector, switch, in_case):
    """In the compose collector image: the agent sink gets only the unmarked spans and metrics,
    the gateway sink everything, the in-case sink everything only with the switch true."""
    if not ic.docker_available():
        pytest.skip("docker not available")
    got = _run_collector(tmp_path, collector, switch)
    assert got["agent"] == _REAL
    assert got["gateway"] == _ALL
    assert got["incase"] == in_case
