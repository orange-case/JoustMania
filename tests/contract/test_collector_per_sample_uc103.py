"""The agent never gets the per-sample spans of "How deep can you go?" (UC-103 BR-005, telemetry.md §1).

Static checks over services/otel-collector/config.yaml and its merge with the gateway overlay.
"""

from __future__ import annotations

import infra_config as ic
import pytest
from test_collector_gateway_overlay import OVERLAY_FILE, _merge

PER_SAMPLE_FILTER = "filter/agent_per_sample"

pytestmark = pytest.mark.uc("UC-103", br=["BR-005"])


@pytest.fixture(scope="module")
def collector():
    return ic.load_yaml(ic.COLLECTOR_FILE)


def test_the_filter_drops_exactly_the_per_sample_spans(collector):
    spec = collector["processors"][PER_SAMPLE_FILTER]
    assert spec["trace_conditions"] == ['span.name == "game.player.sample"']
    assert set(spec) == {"error_mode", "trace_conditions"}


def test_only_the_agent_traces_pipeline_drops_them(collector):
    merged = _merge(collector, ic.load_yaml(OVERLAY_FILE))
    users = {
        name
        for name, spec in merged["service"]["pipelines"].items()
        if PER_SAMPLE_FILTER in spec["processors"]
    }
    assert users == {"traces/agent"}


def test_the_drop_runs_before_the_agent_batch(collector):
    processors = collector["service"]["pipelines"]["traces/agent"]["processors"]
    assert processors.index(PER_SAMPLE_FILTER) < processors.index("batch/traces")
