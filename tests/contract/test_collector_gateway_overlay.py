"""The opt-in gateway export is an overlay that only adds (#2571, UC-035 A7, BR-015, FR-086, NFR-029).

Static checks over services/otel-collector/config.yaml merged with
config.gateway.yaml, and over docker-compose.yml with docker-compose.gateway.yml.
The collector merges several --config files key by key: maps are merged, lists
and scalars of the later file replace the earlier ones (``_merge`` below). So the
overlay must only add map keys. The collector binary itself validates the merge
in CI (scripts/ci/check-collector-config.sh with both files).
"""

from __future__ import annotations

import copy
import re

import infra_config as ic
import pytest

OVERLAY_FILE = ic.REPO_ROOT / "services" / "otel-collector" / "config.gateway.yaml"
COMPOSE_GATEWAY_FILE = ic.REPO_ROOT / "docker-compose.gateway.yml"
BUILDER_FILE = ic.REPO_ROOT / "images" / "otel-collector" / "builder-config.yaml"

# `otlp_http` is the component type of the OTLP/HTTP exporter in the pinned collector
# (v0.164); `otlphttp` is its deprecated alias and logs a warning at start.
EXPORTER = "otlp_http/gateway"
# gateway pipeline -> its in-case twin
GATEWAY_PIPELINES = {
    "traces/gateway": "traces",
    "metrics/gateway": "metrics",
    "logs/gateway": "logs",
}
# The overlay's one processor: drops the records meant for the agent only (#2722).
AGENT_ONLY = "filter/gateway_agent_only"
HOST_VAR = "ORANGECASE_GATEWAY_HOST"
TOKEN_VAR = "ORANGECASE_GATEWAY_TOKEN"
MIB = 1024 * 1024


def _merge(base: dict, overlay: dict) -> dict:
    """What the collector's config resolver does with two --config files:
    maps merge recursively, every other value of the later file replaces."""
    out = copy.deepcopy(base)
    for key, value in overlay.items():
        if isinstance(value, dict) and isinstance(out.get(key), dict):
            out[key] = _merge(out[key], value)
        else:
            out[key] = copy.deepcopy(value)
    return out


def _leaf_paths(node, prefix=()):
    """Every path in ``node`` that ends in a non-map value (a list counts as one value)."""
    if isinstance(node, dict) and node:
        for key, value in node.items():
            yield from _leaf_paths(value, (*prefix, key))
    else:
        yield prefix


def _get(node, path):
    for key in path:
        if not isinstance(node, dict) or key not in node:
            return None
        node = node[key]
    return node


def _seconds(value: str) -> float:
    match = re.fullmatch(r"(\d+)(ms|s|m)", value)
    assert match, f"unparseable duration {value!r}"
    return int(match.group(1)) * {"ms": 0.001, "s": 1, "m": 60}[match.group(2)]


@pytest.fixture(scope="module")
def base():
    return ic.load_yaml(ic.COLLECTOR_FILE)


@pytest.fixture(scope="module")
def overlay():
    return ic.load_yaml(OVERLAY_FILE)


@pytest.fixture(scope="module")
def merged(base, overlay):
    return _merge(base, overlay)


@pytest.fixture(scope="module")
def gateway(merged):
    return merged["exporters"][EXPORTER]


def test_merge_helper_replaces_lists_and_merges_maps():
    """The helper follows the collector's rule, so a restated list would show up as a change."""
    base = {"a": {"keep": 1, "list": [1, 2]}, "b": [1]}
    assert _merge(base, {"a": {"list": [3], "new": 2}}) == {
        "a": {"keep": 1, "list": [3], "new": 2},
        "b": [1],
    }
    assert base == {"a": {"keep": 1, "list": [1, 2]}, "b": [1]}


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_overlay_adds_one_exporter_and_three_pipelines_and_nothing_else(overlay):
    """BR-015: the overlay holds exactly the gateway exporter, the three gateway pipelines and
    the filter of agent-only records (UC-100 BR-012)."""
    assert set(overlay) == {"processors", "exporters", "service"}
    assert set(overlay["processors"]) == {AGENT_ONLY}
    assert set(overlay["exporters"]) == {EXPORTER}
    assert set(overlay["service"]) == {"pipelines"}
    assert set(overlay["service"]["pipelines"]) == set(GATEWAY_PIPELINES)


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_overlay_overwrites_no_value_of_the_base_config(base, overlay):
    """BR-015: no path of the overlay exists in config.yaml, so the merge changes no base value
    and restates no base list (a restated list would replace the base list)."""
    clashes = [
        "::".join(path) for path in _leaf_paths(overlay) if _get(base, path) is not None
    ]
    assert not clashes, f"the overlay overwrites base config values: {clashes}"


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_in_case_configuration_is_unchanged_by_the_merge(base, merged):
    """A7 step 3: every receiver, processor, connector, exporter, extension and pipeline of
    config.yaml is the same after the merge."""
    for section in ("receivers", "connectors", "extensions"):
        assert merged[section] == base[section], section
    for name, cfg in base["processors"].items():
        assert merged["processors"][name] == cfg, name
    assert set(merged["processors"]) == set(base["processors"]) | {AGENT_ONLY}
    for name, cfg in base["exporters"].items():
        assert merged["exporters"][name] == cfg, name
    for name, pipeline in base["service"]["pipelines"].items():
        assert merged["service"]["pipelines"][name] == pipeline, name
    assert merged["service"]["extensions"] == base["service"]["extensions"]
    assert set(merged["service"]) == set(base["service"])
    assert set(merged["exporters"]) == set(base["exporters"]) | {EXPORTER}
    assert set(merged["service"]["pipelines"]) == set(
        base["service"]["pipelines"]
    ) | set(GATEWAY_PIPELINES)


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
@pytest.mark.parametrize("pipeline", sorted(GATEWAY_PIPELINES))
def test_every_component_of_a_gateway_pipeline_is_defined(merged, pipeline):
    """The merged config defines each receiver, processor and exporter a gateway pipeline names."""
    spec = merged["service"]["pipelines"][pipeline]
    for kind in ("receivers", "processors", "exporters"):
        assert spec[kind], f"{pipeline} has no {kind}"
        missing = [name for name in spec[kind] if name not in merged[kind]]
        assert not missing, f"{pipeline} references undefined {kind}: {missing}"


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_overlay_needs_no_component_missing_from_the_collector_image(overlay):
    """The pinned build has the OTLP/HTTP exporter; the overlay adds no extension
    (file_storage and bearertokenauth are not in the image, #2578)."""
    builder = BUILDER_FILE.read_text()
    assert "exporter/otlphttpexporter" in builder
    assert EXPORTER.split("/")[0] == "otlp_http"
    assert "extensions" not in overlay
    assert "extensions" not in overlay["service"]
    queue = overlay["exporters"][EXPORTER]["sending_queue"]
    assert "storage" not in queue, "a persistent queue needs file_storage (#2578)"
    assert "auth" not in overlay["exporters"][EXPORTER], (
        "an auth extension is not in the image; the token goes in the header"
    )


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
@pytest.mark.parametrize("pipeline", sorted(GATEWAY_PIPELINES))
def test_only_otlp_received_telemetry_goes_to_the_gateway(merged, pipeline):
    """A7 step 2: a gateway pipeline reads the `otlp` receiver only (what the services push):
    no host, container or scraped metrics and no connector output."""
    spec = merged["service"]["pipelines"][pipeline]
    assert spec["receivers"] == ["otlp"]
    assert spec["exporters"] == [EXPORTER]


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_no_other_pipeline_exports_to_the_gateway(merged):
    """Host, container, scraped and span metrics stay in the case: only the three gateway pipelines name the exporter."""
    users = {
        name
        for name, spec in merged["service"]["pipelines"].items()
        if EXPORTER in spec["exporters"]
    }
    assert users == set(GATEWAY_PIPELINES)


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
@pytest.mark.parametrize(("pipeline", "twin"), sorted(GATEWAY_PIPELINES.items()))
def test_gateway_pipeline_reuses_the_processors_of_its_in_case_twin(
    merged, pipeline, twin
):
    """The gateway gets the same data as the in-case backend: same processors, in the same
    order, memory limiter first and a batch processor of its own between it and the exporter.
    `logs/gateway` adds only the agent-only filter, before its batch (UC-100 BR-012)."""
    pipelines = merged["service"]["pipelines"]
    processors = pipelines[pipeline]["processors"]
    if pipeline == "logs/gateway":
        assert processors.index(AGENT_ONLY) < processors.index("batch")
        processors = [p for p in processors if p != AGENT_ONLY]
    assert AGENT_ONLY not in processors
    assert processors == pipelines[twin]["processors"]
    assert processors[0] == "memory_limiter"
    assert [p for p in processors if p.split("/")[0] == "batch"], (
        "without a batch processor the gateway exporter's errors reach the receiver"
    )


# The processors the gateway pipelines may use: none drops, rewrites or reshapes a record
# beyond the health-check spans and the resource attributes the in-case twin also gets, and
# the records meant for the agent only (UC-100 BR-012), which are never a backend's moment.
GATEWAY_PROCESSOR_KINDS = {
    "memory_limiter",
    "filter/grpc_health",
    AGENT_ONLY,
    "resource",
}


def _is_batch(name: str) -> bool:
    return name == "batch" or name.startswith("batch/")


@pytest.mark.uc("UC-100", flow="main", steps=[5])
@pytest.mark.parametrize("pipeline", ["traces/gateway", "logs/gateway"])
def test_every_moment_record_path_reaches_the_gateway_pipelines(merged, pipeline):
    """UC-100 step 5: a moment sent as a span, a span event or a log event is in the stream the
    gateway pipeline exports. The span and its span event travel in the OTLP trace data of
    `traces/gateway`, the log event in `logs/gateway`; neither pipeline drops, rewrites or
    narrows records beyond what its in-case twin does (health-check spans only; on
    `logs/gateway` also the records marked for the agent only, BR-012), and the
    moment-only filter of the agent (`filter/agent_moments`) is on `logs/agent` alone."""
    processors = merged["service"]["pipelines"][pipeline]["processors"]
    assert "filter/agent_moments" not in processors, (
        "the moment filter would cut every other log record from the gateway copy"
    )
    unknown = [
        p for p in processors if p not in GATEWAY_PROCESSOR_KINDS and not _is_batch(p)
    ]
    assert not unknown, (
        f"{pipeline} runs a processor that may drop or reshape moments: {unknown}"
    )
    if pipeline == "logs/gateway":
        assert [p for p in processors if p.startswith("filter")] == [AGENT_ONLY], (
            "every unmarked log record, so every moment log event, reaches the gateway"
        )


@pytest.mark.uc("UC-100", flow="main", steps=[5])
def test_only_the_agent_pipeline_filters_moments_and_the_gateway_filter_drops_health_checks_only(
    merged,
):
    """The moment filter exists once, on `logs/agent`; the one filter of `traces/gateway`
    drops gRPC health-check spans by `rpc.service` and nothing named like a moment."""
    pipelines = merged["service"]["pipelines"]
    users = {
        name
        for name, spec in pipelines.items()
        if "filter/agent_moments" in spec["processors"]
    }
    assert users == {"logs/agent"}
    filters = [
        p for p in pipelines["traces/gateway"]["processors"] if p.startswith("filter")
    ]
    assert filters == ["filter/grpc_health"]
    conditions = merged["processors"]["filter/grpc_health"]["trace_conditions"]
    assert conditions
    for condition in conditions:
        assert "rpc.service" in condition
        for moment_word in (
            "death_warning",
            "player_death",
            "game.",
            "moment",
            "span.name",
        ):
            assert moment_word not in condition, condition


@pytest.mark.uc("UC-101", flow="main", steps=[6])
def test_the_agent_decision_and_the_intervention_moment_reach_the_gateway(merged):
    """UC-101 step 6: the agent's `agent.decision` (its trace) and the coordinator's
    intervention moment (a span, a span event or a log event in the game's trace) both reach
    the case collector over OTLP, and the gateway pipelines take every OTLP record of their
    signal: the same `otlp` receiver as the in-case twin, on `logs/gateway` only the filter of
    records marked for the agent only (UC-100 BR-012), and on `traces/gateway` only the
    health-check filter, which names neither the agent's spans nor the intervention moment."""
    services = ic.compose_services()
    for service in ("agent", "game-coordinator"):
        env = services[service]["environment"]
        endpoint = next(e for e in env if e.startswith("OTEL_EXPORTER_OTLP_ENDPOINT="))
        assert "otel-collector:4317" in endpoint, (service, endpoint)
    pipelines = merged["service"]["pipelines"]
    for name in ("traces/gateway", "logs/gateway"):
        assert pipelines[name]["receivers"] == ["otlp"]
        assert pipelines[name]["exporters"] == [EXPORTER]
    assert [
        p for p in pipelines["logs/gateway"]["processors"] if p.startswith("filter")
    ] == [AGENT_ONLY]
    attributes = {"moment.id": "g/P1/3", "intervention.type": "grant_shield"}
    conditions = merged["processors"][AGENT_ONLY]["log_conditions"]
    assert ic.drops_log(conditions, "intervention", attributes, {}) is False
    for condition in merged["processors"]["filter/grpc_health"]["trace_conditions"]:
        for word in ("agent", "decision", "intervention"):
            assert word not in condition, condition


_AGENT = {"orangecase.audience": "agent"}


@pytest.mark.uc("UC-100", br=["BR-012"])
@pytest.mark.uc("UC-041", flow="A8", steps=[1])
@pytest.mark.parametrize(
    ("event_name", "attributes", "resource", "dropped"),
    [
        ("game.start", {"game.id": "g", **_AGENT}, {}, True),
        ("game.end", {"game.id": "g", **_AGENT}, {}, True),
        ("player_death", {"moment.id": "g/P1/2", **_AGENT}, {}, True),
        ("death_warning", {"moment.id": "g/P1/1", **_AGENT}, {}, True),
        ("", _AGENT, {}, True),
        ("player_death", {"moment.id": "g/P1/2"}, {}, False),
        ("death_warning", {"moment.id": "g/P1/1"}, {}, False),
        ("game.start", {"game.id": "g"}, {}, False),
        ("", {}, {}, False),
        ("", {"orangecase.audience": "gateway"}, {}, False),
        ("", {}, _AGENT, False),
        (
            "player_death",
            {"moment.id": "g/P1/2"},
            {"orangecase.synthetic": True},
            False,
        ),
    ],
    ids=[
        "marked-start-dropped",
        "marked-end-dropped",
        "marked-death-copy-dropped",
        "marked-warning-copy-dropped",
        "marked-ordinary-dropped",
        "moment-death-kept",
        "moment-warning-kept",
        "unmarked-start-kept",
        "ordinary-kept",
        "other-audience-kept",
        "marker-on-resource-only-kept",
        "synthetic-moment-kept",
    ],
)
def test_logs_gateway_drops_exactly_the_records_marked_for_the_agent(
    merged, event_name, attributes, resource, dropped
):
    """UC-100 BR-012, UC-041 A8 step 1: a record with `orangecase.audience` = `agent` never
    reaches `logs/gateway`; every unmarked record, moment log events and ordinary logs alike,
    still does. The decision is on the record's own attributes (a filter never sees the span),
    so a marker elsewhere changes nothing."""
    assert AGENT_ONLY in merged["service"]["pipelines"]["logs/gateway"]["processors"]
    conditions = merged["processors"][AGENT_ONLY]["log_conditions"]
    assert ic.drops_log(conditions, event_name, attributes, resource) is dropped


@pytest.mark.uc("UC-100", br=["BR-012"])
def test_only_logs_gateway_drops_the_records_marked_for_the_agent(merged):
    """BR-012: the agent-only filter is on `logs/gateway` alone, so `logs/agent` and the
    in-case `logs` pipeline still take the marked records."""
    users = {
        name
        for name, spec in merged["service"]["pipelines"].items()
        if AGENT_ONLY in spec["processors"]
    }
    assert users == {"logs/gateway"}
    assert merged["processors"][AGENT_ONLY]["error_mode"] == "ignore"


@pytest.mark.nfr("NFR-029")
@pytest.mark.uc("UC-100", flow="A3", steps=[1])
@pytest.mark.uc("UC-101", flow="A5", steps=[1])
@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_an_unreachable_gateway_cannot_touch_the_in_case_pipelines_or_the_agent(
    base, merged
):
    """UC-100 A3 step 1 (NFR-029): the gateway exporter and its three pipelines are separate from
    everything that feeds Jaeger, VictoriaMetrics and the agent. A gateway pipeline names no
    in-case exporter and no in-case pipeline names the gateway exporter, so an unreachable
    gateway (its own retries and its own bounded, dropping queue) holds back no batch the
    in-case exporters or the agent path send, and the agent still gets the traces, metrics and
    moment log events through the same pipelines as without the gateway."""
    pipelines = merged["service"]["pipelines"]
    in_case_exporters = set(base["exporters"])
    for name in GATEWAY_PIPELINES:
        assert pipelines[name]["exporters"] == [EXPORTER]
        assert not set(pipelines[name]["exporters"]) & in_case_exporters
    for name, spec in base["service"]["pipelines"].items():
        assert EXPORTER not in pipelines[name]["exporters"], name
        assert pipelines[name] == spec, f"{name} changed by the gateway overlay"
    agent_pipelines = {
        name
        for name, spec in base["service"]["pipelines"].items()
        if "otlp/agent" in spec["exporters"]
    }
    assert {"traces", "metrics", "logs/agent"} <= agent_pipelines
    queue = merged["exporters"][EXPORTER]["sending_queue"]
    assert queue["enabled"] is True
    assert not queue.get("block_on_overflow"), (
        "a full gateway queue must drop, never wait"
    )
    assert merged["exporters"]["otlp/agent"] == base["exporters"]["otlp/agent"]


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_endpoint_and_token_come_from_the_environment_only(gateway):
    """C-016: the config names no host and no token, only the two environment variables."""
    assert gateway["endpoint"] == f"https://${{env:{HOST_VAR}}}"
    assert gateway["headers"] == {"Authorization": f"Bearer ${{env:{TOKEN_VAR}}}"}
    for key in (
        "traces_endpoint",
        "metrics_endpoint",
        "logs_endpoint",
        "profiles_endpoint",
    ):
        assert key not in gateway, (
            f"{key} would bypass {HOST_VAR} and the fixed https scheme"
        )
    tls = gateway.get("tls") or {}
    assert not tls.get("insecure") and not tls.get("insecure_skip_verify"), (
        "the gateway export verifies the server certificate"
    )
    text = OVERLAY_FILE.read_text() + COMPOSE_GATEWAY_FILE.read_text()
    hosts = set(re.findall(r"https?://([^\s/\"'<>}]+)", text))
    assert hosts <= {"otlp.example.org", f"${{env:{HOST_VAR}"}, (
        f"real host names in the overlay: {hosts}"
    )


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_gateway_export_only_ever_dials_https(gateway, overlay):
    """C-016, FR-086: the scheme is a literal `https://` in the config and only the host comes
    from the environment, so no value of the variable makes the exporter dial plain http. (A
    redirect from the gateway is followed with the token; the gateway must not send one.)"""
    endpoint = gateway["endpoint"]
    assert endpoint.startswith("https://"), (
        "the scheme must not come from the environment"
    )
    assert endpoint.count("${") == 1 and endpoint.endswith(f"${{env:{HOST_VAR}}}")
    assert "http://" not in OVERLAY_FILE.read_text()
    assert set(overlay["exporters"]) == {EXPORTER}, (
        "a second exporter could carry the token"
    )


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_gateway_export_is_gzip_compressed(gateway):
    """UC-035 A7 step 2, telemetry.md: the copy goes over the venue uplink gzip-compressed
    (gzip is also the exporter's default in v0.164.0, so an absent key is gzip)."""
    assert gateway.get("compression", "gzip") == "gzip"


@pytest.mark.nfr("NFR-029")
@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_gateway_retry_is_bounded_to_five_minutes(gateway):
    """NFR-029: a batch is retried for at most 300 s and one attempt takes at most 10 s."""
    retry = gateway["retry_on_failure"]
    assert retry["enabled"] is True
    assert _seconds(retry["max_elapsed_time"]) == 300, "0 would retry forever"
    assert _seconds(retry["max_interval"]) <= 30
    assert _seconds(gateway["timeout"]) == 10


@pytest.mark.nfr("NFR-029")
@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_gateway_queue_is_bounded_in_bytes_and_never_blocks(base, gateway):
    """NFR-029: each signal's queue holds at most 16 MiB of serialized data (48 MiB for the
    three signals), measured in bytes so its memory is bounded, and a full queue drops."""
    queue = gateway["sending_queue"]
    assert queue["enabled"] is True, (
        "without a queue a slow gateway holds the batch processor"
    )
    assert queue["sizer"] == "bytes", "a queue counted in batches has no memory bound"
    assert queue["queue_size"] == 16 * MIB
    assert not queue.get("block_on_overflow"), "a full queue must drop, not block"
    assert not queue.get("wait_for_result"), (
        "the pipeline must not wait for the gateway"
    )
    limiter = base["processors"]["memory_limiter"]
    limit = int(re.fullmatch(r"\$\{env:\w+:-(\d+)\}", limiter["limit_mib"]).group(1))
    spike = int(
        re.fullmatch(r"\$\{env:\w+:-(\d+)\}", limiter["spike_limit_mib"]).group(1)
    )
    # A full queue costs about four times its serialized size in collector memory (measured,
    # UC-035 BR-015), so the heap of three full queues must stay below half the soft limit.
    heap_mib = 4 * len(GATEWAY_PIPELINES) * queue["queue_size"] / MIB
    assert heap_mib <= (limit - spike) / 2, (
        "full gateway queues must stay well below the memory limiter's soft limit (NFR-021)"
    )


# --------------------------------------------------------------------------- #
# Compose
# --------------------------------------------------------------------------- #
@pytest.fixture(scope="module")
def compose_overlay():
    return ic.load_yaml(COMPOSE_GATEWAY_FILE)


def _env_map(entries: list[str]) -> dict[str, str]:
    return dict(entry.split("=", 1) for entry in entries)


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_default_stack_loads_one_config_and_knows_no_gateway(base):
    """A7 step 1 (FR-086): without docker-compose.gateway.yml the collector loads config.yaml
    only, gets neither variable, and every exporter endpoint is a service of the compose network."""
    collector = ic.compose_services()["otel-collector"]
    assert collector["command"] == ["--config=/etc/otel-collector-config.yaml"]
    assert not [e for e in collector["environment"] if "GATEWAY" in e]
    assert not [v for v in collector["volumes"] if "gateway" in v]
    services = set(ic.compose_services())
    for name, cfg in base["exporters"].items():
        endpoint = (cfg or {}).get("endpoint")
        if endpoint is None:
            assert name == "debug", f"{name} has no endpoint"
            continue
        host = re.sub(r"^[a-z]+://", "", endpoint).split("/")[0].split(":")[0]
        assert host in services, f"{name} exports outside the compose network: {host}"
    assert "ORANGECASE_GATEWAY" not in ic.COLLECTOR_FILE.read_text()
    assert not [
        name
        for name in base["exporters"]
        if name.split("/")[0] in ("otlp_http", "otlphttp")
    ]


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_compose_overlay_changes_the_collector_only(compose_overlay):
    """The compose overlay touches one service and three of its keys."""
    assert set(compose_overlay) == {"services"}
    assert set(compose_overlay["services"]) == {"otel-collector"}
    assert set(compose_overlay["services"]["otel-collector"]) == {
        "command",
        "environment",
        "volumes",
    }


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_compose_overlay_loads_the_base_config_first_then_the_overlay(compose_overlay):
    """The overlay's command (compose replaces the list) is the base command plus the second
    --config, and the file that flag names is mounted read-only from the repo."""
    collector = compose_overlay["services"]["otel-collector"]
    base_command = ic.compose_services()["otel-collector"]["command"]
    target = "/etc/otel-collector-config.gateway.yaml"
    assert collector["command"] == [*base_command, f"--config={target}"]
    assert collector["volumes"] == [
        f"./{OVERLAY_FILE.relative_to(ic.REPO_ROOT).as_posix()}:{target}:ro"
    ]


@pytest.mark.uc("UC-035", flow="A7", br=["BR-015"])
def test_compose_overlay_requires_both_variables_and_holds_no_value(compose_overlay):
    """C-016: both variables are passed through from the environment with compose's
    required-variable form, so a missing one stops compose before the collector starts."""
    env = _env_map(compose_overlay["services"]["otel-collector"]["environment"])
    assert set(env) == {HOST_VAR, TOKEN_VAR}
    for name, value in env.items():
        assert re.fullmatch(rf"\$\{{{name}:\?[^}}]+\}}", value), (
            f"{name} must be ${{{name}:?...}}, got {value!r}"
        )
