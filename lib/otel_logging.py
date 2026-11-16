"""
OTEL Log Export with Trace Correlation for JoustMania.

Attaches an OpenTelemetry LoggingHandler to Python's root logger so all
existing ``logging.getLogger()`` calls automatically flow through OTLP.
The OTEL handler injects trace_id and span_id from the active span context,
enabling log-trace correlation in the observability backend.

Usage:
    from lib.otel_logging import init_logging

    # Call once during service startup (after init_metrics)
    init_logging()
"""

from __future__ import annotations

import logging
import os
import threading

from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor

from lib.log_level import register_handler, resolve_log_level, start_runtime_log_level
from lib.otel_resource import get_otel_resource

_initialized = False
_init_lock = threading.Lock()
_test_mode = False
# The LoggerProvider init_logging built, for SDK log events that must not go through
# Python logging (lib/moments.py, #2658): None until init_logging ran, and in tests.
# Not registered globally, so ordinary logs keep flowing only through the handler.
_logger_provider: LoggerProvider | None = None

logger = logging.getLogger(__name__)


def disable_logging_for_tests() -> None:
    """
    Disable OTLP log export for unit tests.

    Call this once in conftest.py to prevent OTLP connection attempts
    during tests.

    Example:
        # conftest.py
        from lib.otel_logging import disable_logging_for_tests
        disable_logging_for_tests()
    """
    global _test_mode, _initialized
    _test_mode = True
    _initialized = True  # Prevent real initialization


def is_test_mode() -> bool:
    """Check if OTLP logging is in test mode (disabled for tests)."""
    return _test_mode


def get_logger_provider() -> LoggerProvider | None:
    """The process's OTLP ``LoggerProvider``, or ``None`` when OTLP logging is not initialised.

    For SDK log events emitted on a ``Logger`` directly (UC-100 moments, #2658), which
    the ``observability/log_level`` flag never filters. ``None`` in tests and in a
    process that never called ``init_logging``.
    """
    return _logger_provider


def init_logging() -> None:
    """
    Initialize OTLP log export with trace correlation.

    Sets up a ``LoggerProvider`` with ``OTLPLogExporter`` (gRPC) and
    ``BatchLogRecordProcessor``, then attaches a ``LoggingHandler`` to
    Python's root logger. The handler automatically injects trace_id and
    span_id from the active span context into each log record.

    Service identity is configured via environment variables set in
    docker-compose.yml (same vars as telemetry and metrics).

    Environment Variables:
        OTEL_EXPORTER_OTLP_ENDPOINT: OTLP collector endpoint (default: http://otel-collector:4317)
        OTEL_SERVICE_NAME: Service name (required in docker-compose.yml)
        OTEL_SERVICE_NAMESPACE: Service namespace (default: "joustmania")
        LOG_LEVEL: bootstrap-only (the startup level until the flag resolves; default: INFO); the
            ``observability/log_level`` flag wins and is followed at runtime (lib/log_level.py)
    """
    global _initialized, _logger_provider

    with _init_lock:
        if _initialized:
            logger.warning("OTLP logging already initialized, skipping")
            return

        otlp_endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4317")
        service_name = os.getenv("OTEL_SERVICE_NAME", "unknown-service")

        resource = get_otel_resource()

        exporter = OTLPLogExporter(endpoint=otlp_endpoint, insecure=True)
        provider = LoggerProvider(resource=resource)
        provider.add_log_record_processor(BatchLogRecordProcessor(exporter))

        # Attach OTEL handler to root logger so all existing loggers flow through OTLP
        # Flag first, then LOG_LEVEL, then INFO (#1788); the level follows the flag at runtime.
        log_level = resolve_log_level()
        log_level_str = logging.getLevelName(log_level)

        handler = LoggingHandler(level=log_level, logger_provider=provider)
        logging.getLogger().addHandler(handler)
        register_handler(handler)

        _logger_provider = provider
        _initialized = True
        start_runtime_log_level()

        logger.info(
            f"OTLP log export initialized: {service_name} -> {otlp_endpoint} "
            f"(level={log_level_str}, trace correlation enabled)"
        )
