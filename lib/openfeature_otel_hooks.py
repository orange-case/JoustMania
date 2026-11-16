"""OpenFeature evaluation metrics with the standard OpenTelemetry hook names (#1427).

The span side is the official hook: ``openfeature-hooks-opentelemetry``'s
``TracingHook`` (registered in :func:`lib.feature_flags.init_flag_domain`) adds one
``feature_flag.evaluation`` span event per evaluation. That package (0.3.1, the
latest release) has no metrics hook yet; its metrics hook is an open upstream PR
(open-feature/python-sdk-contrib#344). Until it is released, :class:`MetricsHook`
records the metrics of the Go reference hook (go-sdk-contrib
``hooks/open-telemetry``, which the agent registers) with the same instrument
names and attributes, so one dashboard query covers every service:

* ``feature_flag.evaluation_requests_total`` (counter): ``feature_flag.key``,
  ``feature_flag.provider.name``; one per evaluation that reached the provider;
* ``feature_flag.evaluation_success_total`` (counter): the same plus
  ``feature_flag.result.variant`` and ``reason`` (the OpenFeature reason, e.g.
  ``STATIC``, ``TARGETING_MATCH``, ``DEFAULT``, ``ERROR``);
* ``feature_flag.evaluation_error_total`` (counter): ``feature_flag.key``,
  ``feature_flag.provider.name``; the Go hook's free-text ``error.message`` is not
  a label (the agent drops it with a view); the error reason is on
  ``flag_eval_errors_total`` (:mod:`lib.flag_eval_visibility`);
* ``feature_flag.evaluation_active_count`` (up-down counter): ``feature_flag.key``.

VictoriaMetrics names: ``feature_flag_evaluation_requests_total`` and so on, labels
``feature_flag_key``, ``feature_flag_provider_name``, ``feature_flag_result_variant``,
``reason`` (docs/specs/telemetry.md §6).

Cardinality: the targeting key and the evaluation context never become labels (they
stay on the span event), and a variant name the flag service generates for a written
value (``r<epoch>_<n>`` scoped, ``o<epoch>_<n>`` overlay; a fresh one per write) is
replaced by its class, ``scoped_value`` or ``overlay_value``.

The instruments come from the global MeterProvider, which :func:`lib.otel_metrics.init_metrics`
sets; created earlier, they are OpenTelemetry proxies that bind to it once it is set,
and in unit tests (no provider) they are no-ops.
"""

from __future__ import annotations

import re
import threading
import time
import weakref

from openfeature.contrib.hook.opentelemetry import TracingHook
from openfeature.flag_evaluation import FlagEvaluationDetails, Reason
from openfeature.hook import Hook, HookContext, HookHints
from opentelemetry import metrics, trace
from opentelemetry.trace import Span

METER_NAME = "openfeature.hooks.opentelemetry"

EVALUATION_ACTIVE = "feature_flag.evaluation_active_count"
EVALUATION_REQUESTS = "feature_flag.evaluation_requests_total"
EVALUATION_SUCCESS = "feature_flag.evaluation_success_total"
EVALUATION_ERRORS = "feature_flag.evaluation_error_total"

ATTR_KEY = "feature_flag.key"
ATTR_PROVIDER = "feature_flag.provider.name"
ATTR_VARIANT = "feature_flag.result.variant"
ATTR_REASON = "reason"

# services/flag-service/state/state.go nextVariant: "<r|o><epoch hex>_<n>".
_GENERATED_VARIANT = re.compile(r"^([ro])[0-9a-f]{1,16}_[0-9]+$")

_STARTED = "joustmania.metrics_hook.started"


def metric_variant(variant: str | None) -> str | None:
    """The variant label value: the declared name, or the class of a generated one."""
    if not variant:
        return variant
    match = _GENERATED_VARIANT.match(variant)
    if match is None:
        return variant
    return "overlay_value" if match.group(1) == "o" else "scoped_value"


def _base_attributes(hook_context: HookContext) -> dict[str, str]:
    attributes = {ATTR_KEY: hook_context.flag_key}
    if hook_context.provider_metadata is not None:
        attributes[ATTR_PROVIDER] = hook_context.provider_metadata.name
    return attributes


class MetricsHook(Hook):
    """Evaluation counters with the go-sdk-contrib OpenTelemetry metrics hook's names."""

    def __init__(self, meter_provider: metrics.MeterProvider | None = None) -> None:
        meter = (meter_provider or metrics.get_meter_provider()).get_meter(METER_NAME)
        self._active = meter.create_up_down_counter(EVALUATION_ACTIVE, description="active flag evaluations counter")
        self._requests = meter.create_counter(
            EVALUATION_REQUESTS, description="feature flag evaluation request counter"
        )
        self._success = meter.create_counter(EVALUATION_SUCCESS, description="feature flag evaluation success counter")
        self._errors = meter.create_counter(EVALUATION_ERRORS, description="feature flag evaluation error counter")

    # The hook methods keep the SDK's signatures; unused arguments are part of the interface.
    def before(self, hook_context: HookContext, hints: HookHints) -> None:  # noqa: ARG002
        self._active.add(1, {ATTR_KEY: hook_context.flag_key})
        self._requests.add(1, _base_attributes(hook_context))
        hook_context.hook_data[_STARTED] = time.monotonic()

    def after(self, hook_context: HookContext, details: FlagEvaluationDetails, hints: HookHints) -> None:  # noqa: ARG002
        attributes = _base_attributes(hook_context)
        variant = metric_variant(details.variant)
        if variant:
            attributes[ATTR_VARIANT] = variant
        if details.reason:
            attributes[ATTR_REASON] = str(details.reason)
        self._success.add(1, attributes)

    def error(self, hook_context: HookContext, exception: Exception, hints: HookHints) -> None:  # noqa: ARG002
        self._errors.add(1, _base_attributes(hook_context))

    def finally_after(
        self,
        hook_context: HookContext,
        details: FlagEvaluationDetails,  # noqa: ARG002
        hints: HookHints,  # noqa: ARG002
    ) -> None:
        # The SDK skips `before` when the provider is not ready but still runs `finally`;
        # only an evaluation that was counted active is counted down.
        if hook_context.hook_data.pop(_STARTED, None) is not None:
            self._active.add(-1, {ATTR_KEY: hook_context.flag_key})


# --- Span side (#2649) -------------------------------------------------------------------------

# Flags that only decide whether telemetry exists: a ``feature_flag.evaluation`` event for them is
# noise on every span (grpc_rpc_spans is read by every gRPC span to decide whether the span is
# created). Their metrics are unchanged. docs/specs/telemetry.md §6.
SPAN_EVENT_SKIP_FLAGS: frozenset[str] = frozenset({"grpc_rpc_spans"})

# Most (flag key, variant) pairs remembered per span; further new pairs on that span are dropped.
MAX_SEEN_PER_SPAN = 256


class DedupingTracingHook(TracingHook):
    """The official ``TracingHook`` with one event per (span, flag key, variant).

    A flag read on every RPC or tick no longer adds an event per evaluation; a changed variant
    (or an error after a success) is still recorded. The seen-set is held in a weak map keyed by
    the span, so it is freed with the span, and capped at :data:`MAX_SEEN_PER_SPAN` pairs, so a
    long-lived span (a game, a stream) stays bounded. Hot path: one weak-map get, one set lookup;
    the lock is taken only the first time a span is seen. A race between two threads on a new
    pair can at worst record it twice.
    """

    def __init__(self, exclude_exceptions: bool = False) -> None:
        super().__init__(exclude_exceptions=exclude_exceptions)
        self._seen: weakref.WeakKeyDictionary[Span, set[tuple[str, str | None, bool]]] = weakref.WeakKeyDictionary()
        self._lock = threading.Lock()

    def finally_after(self, hook_context: HookContext, details: FlagEvaluationDetails, hints: HookHints) -> None:
        if details.flag_key in SPAN_EVENT_SKIP_FLAGS:
            return
        span = trace.get_current_span()
        if not span.is_recording():
            return
        seen = self._seen.get(span)
        if seen is None:
            with self._lock:
                seen = self._seen.setdefault(span, set())
        pair = (details.flag_key, details.variant, details.reason == Reason.ERROR)
        if pair in seen or len(seen) >= MAX_SEEN_PER_SPAN:
            return
        seen.add(pair)
        super().finally_after(hook_context, details, hints)
