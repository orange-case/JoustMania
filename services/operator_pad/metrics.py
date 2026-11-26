"""OTEL push metrics of the operator pad."""

from lib.otel_metrics import Counter, Gauge

# System metrics
process_cpu_seconds_total = Counter("process_cpu_seconds_total", "Total user and system CPU time spent in seconds")

process_resident_memory_bytes = Gauge("process_resident_memory_bytes", "Resident memory size in bytes")

process_threads = Gauge("process_threads", "Number of active threads")
