"""In-process fakes of the service tier's external dependencies."""

from .claude import FakeClaude
from .jev import FakeJev
from .otlp import LogRecord, MetricPoint, MetricRecord, OtlpSink, SpanEventRecord, SpanRecord
from .tcp_proxy import TcpProxy

__all__ = [
    "FakeClaude",
    "FakeJev",
    "LogRecord",
    "MetricPoint",
    "MetricRecord",
    "OtlpSink",
    "SpanEventRecord",
    "SpanRecord",
    "TcpProxy",
]
