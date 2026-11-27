"""UC-106 BR-001: everything stowaway exports carries orangecase.synthetic=true on its resource."""

import pytest
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from services.stowaway import synthetic


@pytest.mark.uc("UC-106", br=["BR-001"])
def test_the_resource_is_marked_with_the_bool(monkeypatch):
    monkeypatch.setenv("OTEL_SERVICE_NAME", "stowaway")
    resource = synthetic.synthetic_resource()
    assert resource.attributes["orangecase.synthetic"] is True
    assert resource.attributes["service.name"] == "stowaway"


@pytest.mark.uc("UC-106", br=["BR-001"])
def test_both_providers_export_with_the_marked_resource(monkeypatch):
    installed = {}
    monkeypatch.setattr(synthetic.trace, "set_tracer_provider", lambda p: installed.setdefault("traces", p))
    monkeypatch.setattr(synthetic.metrics, "set_meter_provider", lambda p: installed.setdefault("metrics", p))
    exporter = InMemorySpanExporter()
    tracer_provider, meter_provider = synthetic.init_providers(
        "http://collector:4317", SimpleSpanProcessor(exporter), InMemoryMetricReader()
    )
    assert installed == {"traces": tracer_provider, "metrics": meter_provider}
    tracer_provider.get_tracer("t").start_span("x").end()
    (span,) = exporter.get_finished_spans()
    assert span.resource.attributes["orangecase.synthetic"] is True
    assert meter_provider._sdk_config.resource.attributes["orangecase.synthetic"] is True
