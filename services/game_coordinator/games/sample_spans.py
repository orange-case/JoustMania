"""One span per processed motion sample, "How deep can you go?" (UC-103 BR-001, FR-224; telemetry.md §3.2)."""

from __future__ import annotations

from opentelemetry import trace

PER_SAMPLE_SPANS_ATTR = "telemetry.per_sample_spans"
SPAN_SAMPLE = "game.player.sample"
CHALLENGE_SPAN_ROUNDS = "span-rounds"


def read_per_sample_spans() -> bool:
    """Read the flag once per game; an unreadable flag is off."""
    from lib.feature_flags import read_bool_flag  # lazy: lib.feature_flags pulls in the metrics libs

    return read_bool_flag("observability", "telemetry.per_sample_spans", False)


def counts_spans(per_sample_spans: bool) -> bool:
    """Whether the game is a span round (UC-103 BR-003): per-sample spans on, or stowaway padding it.

    Ordinary games take no delivered count, so their end never waits on a span flush.
    """
    if per_sample_spans:
        return True
    from lib.feature_flags import read_string_flag

    return read_string_flag("observability", "stowaway.challenge", "none") == CHALLENGE_SPAN_ROUNDS


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
