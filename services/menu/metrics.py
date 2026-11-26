"""
OTEL push metrics for Menu Service.

Tracks system resources and button dispatch timing.
"""

from lib.otel_metrics import Counter, Gauge, Histogram

# System metrics
process_cpu_seconds_total = Counter("process_cpu_seconds_total", "Total user and system CPU time spent in seconds")

process_resident_memory_bytes = Gauge("process_resident_memory_bytes", "Resident memory size in bytes")

process_threads = Gauge("process_threads", "Number of active threads")

# #2380: how long the menu takes to handle one button-stream event, handler failures included.
button_dispatch_seconds = Histogram(
    "menu_button_dispatch_seconds",
    "Time the menu takes to dispatch one button-stream event",
    ["event_type"],  # connect / disconnect / button
    buckets=[0.001, 0.005, 0.010, 0.025, 0.050, 0.100, 0.250, 0.500, 1.0, 2.5],
)
