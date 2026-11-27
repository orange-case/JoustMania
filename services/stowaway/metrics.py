"""stowaway's unmarked metrics: facts about the case, not synthetic load (telemetry.md §3.5)."""

import threading

from lib.otel_metrics import Gauge

# FR-246, UC-103 BR-003: padding spans of a match trace stowaway exported, set when the padding ends.
trace_spans_delivered = Gauge(
    "stowaway_trace_spans_delivered",
    "Spans of the match trace stowaway exported successfully (set when its padding ends)",
    ["game_id"],
)
# The dashboard reads recent rounds; older games' series are removed.
TRACE_SPANS_DELIVERED_KEEP = 20
_games: list[str] = []
_lock = threading.Lock()


def set_trace_spans_delivered(game_id: str, delivered: int) -> None:
    with _lock:
        trace_spans_delivered.labels(game_id=game_id).set(delivered)
        if game_id not in _games:
            _games.append(game_id)
        while len(_games) > TRACE_SPANS_DELIVERED_KEEP:
            trace_spans_delivered.remove(_games.pop(0))
