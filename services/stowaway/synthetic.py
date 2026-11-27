"""stowaway's own tracer and meter providers: everything they export is marked synthetic (UC-106 BR-001)."""

from __future__ import annotations

import logging
import os

from opentelemetry import metrics, trace
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import MetricReader, PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanProcessor

from lib.otel_resource import get_otel_resource
from lib.span_count import CountingSpanExporter

logger = logging.getLogger(__name__)

SYNTHETIC_ATTR = "orangecase.synthetic"
DEFAULT_ENDPOINT = "http://otel-collector:4317"

# The padding peaks at 2000 spans/s (500/s x the ceiling): a short flush and a deep queue keep the
# batch processor from dropping it.
SPAN_FLUSH_MS = 500
SPAN_QUEUE = 16384
SPAN_BATCH = 2048
METRIC_EXPORT_MS = 100


def synthetic_resource() -> Resource:
    return get_otel_resource().merge(Resource({SYNTHETIC_ATTR: True}))


def init_providers(
    endpoint: str | None = None,
    span_processor: SpanProcessor | None = None,
    metric_reader: MetricReader | None = None,
) -> tuple[TracerProvider, MeterProvider]:
    """Install the synthetic providers globally (tests pass an in-memory processor and reader)."""
    target = endpoint or os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", DEFAULT_ENDPOINT)
    resource = synthetic_resource()
    if span_processor is None:
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter

        span_processor = BatchSpanProcessor(
            CountingSpanExporter(OTLPSpanExporter(endpoint=target, insecure=not target.startswith("https://"))),
            max_queue_size=SPAN_QUEUE,
            max_export_batch_size=SPAN_BATCH,
            schedule_delay_millis=SPAN_FLUSH_MS,
        )
    if metric_reader is None:
        from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter

        metric_reader = PeriodicExportingMetricReader(
            OTLPMetricExporter(endpoint=target, insecure=not target.startswith("https://")),
            export_interval_millis=METRIC_EXPORT_MS,
        )
    tracer_provider = TracerProvider(resource=resource)
    tracer_provider.add_span_processor(span_processor)
    meter_provider = MeterProvider(resource=resource, metric_readers=[metric_reader])
    trace.set_tracer_provider(tracer_provider)
    metrics.set_meter_provider(meter_provider)
    logger.info("stowaway telemetry -> %s (marked %s=true)", target, SYNTHETIC_ATTR)
    return tracer_provider, meter_provider
