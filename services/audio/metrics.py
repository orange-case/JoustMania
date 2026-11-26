"""
OTEL push metrics for Audio Service.

Tracks system resources and audio playback.
"""

from lib.otel_metrics import Counter, Gauge

# System metrics
process_cpu_seconds_total = Counter(
    "process_cpu_seconds_total", "Total user and system CPU time spent in seconds", unit="s"
)

process_resident_memory_bytes = Gauge("process_resident_memory_bytes", "Resident memory size in bytes", unit="By")

process_threads = Gauge("process_threads", "Number of active threads")

# Audio metrics
audio_sounds_played_total = Counter(
    "audio_sounds_played_total",
    "Total sound effects played",
    ["result"],  # "success", "no_channel", "file_not_found", "error"
)
