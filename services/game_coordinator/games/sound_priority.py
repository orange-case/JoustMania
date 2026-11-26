"""Audio priorities of the coordinator's sound lines (audio.proto AudioPriority)."""

# Only CRITICAL steals a channel when all are busy: the countdown and the result
# lines must be heard over a full mix of effects (UC-031).
CRITICAL = 3
