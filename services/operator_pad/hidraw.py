"""The pad's raw HID interface and the display frame protocol (UC-016 BR-002).

The display node is the hidraw node of the device `operator_pad.device` matches whose report
descriptor declares usage page 0xFF60 (the Vial / VIA raw HID interface), found again through
sysfs at every rescan, never a fixed ``/dev/hidrawN``. In the container the host's ``/dev`` is
mounted read-only at ``/run/host-dev`` (services.md §3.1), so the node is looked up there too.

Every call here blocks (a USB interrupt write can take seconds), so the caller runs it on its own
worker thread, never on the asyncio loop (FR-240).
"""

from __future__ import annotations

import errno
import os
import re
import select
import time
from pathlib import Path
from typing import Protocol

from services.operator_pad.device import DENIED_USB_IDS
from services.operator_pad.screens import MEDIUM, Screen

CMD = 0x50  # not a VIA or Vial command id
OP_HELLO, OP_LINE, OP_SHOW, OP_BAR, OP_WHEEL = 0x00, 0x01, 0x02, 0x03, 0x04
ERROR = 0xFF
PROTOCOL_VERSION = 1  # the lowest version the link accepts; later ones keep the v1 ops
NO_MARKER = 0xFF
WHEEL_NOW = 0x01
BAR_LABEL_BYTES = 26
REPORT_SIZE = 32
READ_SIZE = 64
SYSFS_HIDRAW = Path("/sys/class/hidraw")
HOST_DEV = Path("/run/host-dev")
DEV_DIRS = (Path("/dev"), HOST_DEV)
RAW_USAGE_PAGE = bytes((0x06, 0x60, 0xFF))  # Usage Page (0xFF60), the long item of the raw HID interface


class ProtocolError(Exception):
    """The firmware answered with an error, or not with protocol version 1."""


class FeatureRefusedError(ProtocolError):
    """The firmware refused a bar or wheel op (`FF`): the caller drops to a lower layout, the link stays."""

    def __init__(self, op: int) -> None:
        super().__init__(f"op {op:#04x} refused")
        self.op = op


def report(op: int, *payload: int) -> bytes:
    data = bytes((CMD, op, *payload))
    return data.ljust(REPORT_SIZE, b"\x00")


def encode(text: str) -> bytes:
    """One byte per character: the glyph bytes up to 0x87 pass, anything else is ``?``."""
    return bytes(ord(c) if 0x20 <= ord(c) <= 0x87 else 0x3F for c in text)


def line_report(index: int, text: str, attrs: int) -> bytes:
    """A line op; a medium line (attribute bit 3) addresses medium row (index - 1) // 2 (BR-002)."""
    attrs = int(attrs)
    if attrs & MEDIUM:
        index = (index - 1) // 2
    return report(OP_LINE, index, attrs, *encode(text))


def bar_report(row: int, rungs: int, filled: int, marker: int | None, labels) -> bytes:
    text = encode("|".join(labels))[:BAR_LABEL_BYTES]
    return report(OP_BAR, row, rungs, filled, NO_MARKER if marker is None else marker, *text)


def wheel_report(slot: int, text: str, now: bool) -> bytes:
    return report(OP_WHEEL, slot, WHEEL_NOW if now else 0, *encode(text[:21].ljust(21)))


def show_report(seq: int) -> bytes:
    return report(OP_SHOW, seq & 0xFF)


class Handle(Protocol):
    def drain(self) -> None: ...
    def transact(self, data: bytes, timeout: float) -> bytes: ...
    def close(self) -> None: ...


class Backend(Protocol):
    def open(self, path: str) -> Handle: ...


def _node_number(node: Path) -> int:
    digits = node.name.removeprefix("hidraw")
    return int(digits) if digits.isdigit() else 1 << 30


_USB_DEVICE = re.compile(r"^\d+-[\d.]+$")  # a USB device in sysfs (`1-1.2`); its interfaces are `1-1.2:1.0`


def usb_device(sysfs_device: Path) -> str | None:
    """The USB device a hidraw or input node's sysfs ``device`` link sits under, or None (UC-016 BR-011)."""
    try:
        resolved = sysfs_device.resolve(strict=True)
    except OSError:
        return None
    for part in (resolved, *resolved.parents):
        if _USB_DEVICE.match(part.name):
            return str(part)
    return None


def _matches(node: Path, hid_id: str) -> bool:
    try:
        uevent = (node / "device" / "uevent").read_text()
        descriptor = (node / "device" / "report_descriptor").read_bytes()
    except OSError:
        return False
    ids = [line.split("=", 1)[1].strip().upper() for line in uevent.splitlines() if line.startswith("HID_ID=")]
    return bool(ids) and ids[0].endswith(hid_id) and RAW_USAGE_PAGE in descriptor


def find_display_node(
    vendor: str, product: str, sysfs_root: Path = SYSFS_HIDRAW, dev_dirs: tuple[Path, ...] = DEV_DIRS
) -> str | None:
    """The raw HID node of vendor:product (usage page 0xFF60), or None. Denied ids match nothing."""
    if (vendor.lower(), product.lower()) in DENIED_USB_IDS:
        return None
    hid_id = f":{int(vendor, 16):08X}:{int(product, 16):08X}"
    try:
        nodes = sorted(sysfs_root.iterdir(), key=_node_number)
    except OSError:
        return None
    for node in nodes:
        if not _matches(node, hid_id):
            continue
        for directory in dev_dirs:
            path = directory / node.name
            if path.exists():
                return str(path)
    return None


class HidrawHandle:
    """One open hidraw node; blocking calls, for the display's worker thread only."""

    def __init__(self, fd: int) -> None:
        self._fd = fd

    @classmethod
    def open(cls, path: str) -> HidrawHandle:
        return cls(os.open(path, os.O_RDWR | os.O_NONBLOCK | os.O_CLOEXEC))

    def drain(self) -> None:
        """Drop the input reports already queued (replies of an earlier opener, Vial's)."""
        while True:
            try:
                if not os.read(self._fd, READ_SIZE):
                    return
            except BlockingIOError:
                return

    def transact(self, data: bytes, timeout: float) -> bytes:
        """Write one report (report id 0) and wait for its reply; other reports are skipped.

        Every opener of the node gets every input report, so a reply whose command or op is not
        ours is dropped and the wait goes on until ``timeout``. Any OSError means the link is down.
        """
        os.write(self._fd, b"\x00" + data)
        deadline = time.monotonic() + timeout
        poller = select.poll()
        poller.register(self._fd, select.POLLIN)
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(f"no reply to op {data[1]:#04x}")
            events = poller.poll(remaining * 1000)
            if not events:
                continue  # the deadline check above ends the wait
            for _, event in events:
                if event & (select.POLLERR | select.POLLHUP | select.POLLNVAL):
                    raise OSError(errno.EIO, "hidraw node gone")
            try:
                reply = os.read(self._fd, READ_SIZE)
            except BlockingIOError:
                continue
            if len(reply) >= 3 and reply[0] == CMD and reply[1] == data[1]:
                return reply

    def close(self) -> None:
        os.close(self._fd)


class HidrawBackend:
    def open(self, path: str) -> Handle:
        return HidrawHandle.open(path)


def hello(handle: Handle, timeout: float) -> int:
    """The firmware's protocol version."""
    return handle.transact(report(OP_HELLO), timeout)[2]


def _check(handle: Handle, data: bytes, timeout: float) -> None:
    if handle.transact(data, timeout)[2] == ERROR:
        raise ProtocolError(f"op {data[1]:#04x} refused")


def _check_feature(handle: Handle, data: bytes, timeout: float) -> None:
    if handle.transact(data, timeout)[2] == ERROR:
        raise FeatureRefusedError(data[1])


def send_frame(handle: Handle, screen: Screen, seq: int, timeout: float) -> None:
    """The 8 lines, the bar or wheel, then a show, in one call: the firmware draws only on show (BR-002).

    The bar and the wheel go after the lines: writing a line they cover removes them.
    """
    for index, (text, attrs) in enumerate(screen.lines):
        _check(handle, line_report(index, text, attrs), timeout)
    if screen.bar is not None:
        _check_feature(handle, bar_report(*screen.bar), timeout)
    if screen.wheel is not None:
        for slot, (text, now) in enumerate(screen.wheel):
            _check_feature(handle, wheel_report(slot, text, now), timeout)
    _check(handle, show_report(seq), timeout)


def send_show(handle: Handle, seq: int, timeout: float) -> None:
    """A show alone: the heartbeat that keeps the firmware off its `NO HOST` screen."""
    _check(handle, show_report(seq), timeout)
