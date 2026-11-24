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
import select
import time
from pathlib import Path
from typing import Protocol

from services.operator_pad.device import DENIED_USB_IDS
from services.operator_pad.screens import Screen

CMD = 0x50  # not a VIA or Vial command id
OP_HELLO, OP_LINE, OP_SHOW = 0x00, 0x01, 0x02
ERROR = 0xFF
PROTOCOL_VERSION = 1  # the lowest version the link accepts; later ones keep the v1 ops
REPORT_SIZE = 32
READ_SIZE = 64
SYSFS_HIDRAW = Path("/sys/class/hidraw")
HOST_DEV = Path("/run/host-dev")
DEV_DIRS = (Path("/dev"), HOST_DEV)
RAW_USAGE_PAGE = bytes((0x06, 0x60, 0xFF))  # Usage Page (0xFF60), the long item of the raw HID interface


class ProtocolError(Exception):
    """The firmware answered with an error, or not with protocol version 1."""


def report(op: int, *payload: int) -> bytes:
    data = bytes((CMD, op, *payload))
    return data.ljust(REPORT_SIZE, b"\x00")


def line_report(index: int, text: str, inverted: bool) -> bytes:
    return report(OP_LINE, index, 1 if inverted else 0, *text.encode("ascii", "replace"))


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


def send_frame(handle: Handle, screen: Screen, seq: int, timeout: float) -> None:
    """The 8 lines and a show, in one call: the firmware draws only on show (BR-002)."""
    for index, (text, inverted) in enumerate(screen.lines):
        _check(handle, line_report(index, text, inverted), timeout)
    _check(handle, show_report(seq), timeout)


def send_show(handle: Handle, seq: int, timeout: float) -> None:
    """A show alone: the heartbeat that keeps the firmware off its `NO HOST` screen."""
    _check(handle, show_report(seq), timeout)
