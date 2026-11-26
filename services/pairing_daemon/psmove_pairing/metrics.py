"""OTEL push metrics for PS Move pairing daemon."""

from lib.otel_metrics import Counter, Gauge

# Bluetooth monitoring metrics (host-level HCI layer)
# Note: These are distinct from controller_* metrics in controller-manager
# which measure the application layer (psmoveapi). These measure the raw
# Bluetooth HCI layer on the host.
pairing_bluetooth_device_rssi_dbm = Gauge(
    "pairing_bluetooth_device_rssi_dbm",
    "Bluetooth device signal strength in dBm (host HCI layer)",
    ["serial", "hci_adapter"],
)
pairing_bluetooth_device_connected = Gauge(
    "pairing_bluetooth_device_connected",
    "Bluetooth device connection status at HCI layer (1=connected, 0=disconnected)",
    ["serial", "hci_adapter"],
)
pairing_bluetooth_adapter_connections = Gauge(
    "pairing_bluetooth_adapter_connections",
    "Number of controllers per adapter",
    ["hci_adapter"],
)

# Adapter identity, HCI errors and link roles (UC-035 step 6, FR-122, #1431). Read-only
# observation from hciconfig -a / hcitool con / sysfs; nothing changes a role.
pairing_bluetooth_adapter_info = Gauge(
    "pairing_bluetooth_adapter_info",
    "Bluetooth adapter identity (always 1): HCI manufacturer, HCI version, bus, USB vendor:product, onboard",
    ["hci_adapter", "address", "manufacturer", "manufacturer_id", "hci_version", "bus", "usb_id", "onboard"],
)
pairing_bluetooth_adapter_hci_errors_total = Counter(
    "pairing_bluetooth_adapter_hci_errors_total",
    "Increases of the kernel's cumulative HCI error counters per adapter (direction rx/tx)",
    ["hci_adapter", "direction"],
)
pairing_bluetooth_link_role_info = Gauge(
    "pairing_bluetooth_link_role_info",
    "Local adapter role on a controller link (always 1; role central/peripheral/unknown)",
    ["serial", "hci_adapter", "role"],
)

# Process-level system metrics (standard Prometheus naming convention)
process_cpu_seconds_total = Counter(
    "process_cpu_seconds_total", "Total user and system CPU time spent in seconds", unit="s"
)
process_resident_memory_bytes = Gauge("process_resident_memory_bytes", "Resident memory size in bytes", unit="By")
process_threads = Gauge("process_threads", "Number of active threads")
