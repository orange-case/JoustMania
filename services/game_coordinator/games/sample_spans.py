"""One span per processed motion sample, "How deep can you go?" (UC-103 BR-001, FR-224; telemetry.md §3.2)."""

from __future__ import annotations

from opentelemetry import trace

PER_SAMPLE_SPANS_FLAG = "telemetry.per_sample_spans"
PER_SAMPLE_SPANS_ATTR = "telemetry.per_sample_spans"
SPAN_SAMPLE = "game.player.sample"


def read_per_sample_spans() -> bool:
    """Read the flag once per game; an unreadable flag is off."""
    from lib.feature_flags import read_bool_flag  # lazy: lib.feature_flags pulls in the metrics libs

    return read_bool_flag("observability", PER_SAMPLE_SPANS_FLAG, False)


def record_sample_span(
    tracer: trace.Tracer,
    player_span: trace.Span,
    serial: str,
    game_id: str,
    accel_mag: float,
    smoothed: float,
    threshold: float,
) -> None:
    tracer.start_span(
        SPAN_SAMPLE,
        context=trace.set_span_in_context(player_span),
        attributes={
            "player.serial": serial,
            "game.id": game_id,
            "accel_magnitude": accel_mag,
            "smoothed_accel": smoothed,
            "threshold": threshold,
        },
    ).end()
