"""Pytest fixtures for the stowaway tests: OTEL off globally; each test builds local providers."""

import pytest
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib.otel_logging import disable_logging_for_tests
from lib.otel_metrics import disable_metrics_for_tests
from lib.telemetry import disable_telemetry_for_tests

disable_telemetry_for_tests()
disable_metrics_for_tests()
disable_logging_for_tests()


@pytest.fixture(autouse=True)
def sdk_enabled(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # the providers in these tests are local


@pytest.fixture
def spans():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    exporter.tracer = provider.get_tracer("test")
    return exporter


@pytest.fixture
def metric_reader():
    reader = InMemoryMetricReader()
    reader.meter = MeterProvider(metric_readers=[reader]).get_meter("test")
    return reader
