"""Fakes for the display link tests (UC-016): a hidraw backend that records reports, a sysfs tree."""

from __future__ import annotations

import errno
import threading
from pathlib import Path
from unittest.mock import create_autospec

from services.operator_pad import hidraw
from services.operator_pad.config import PadConfig

FF60 = bytes((0x06, 0x60, 0xFF, 0x09, 0x61))  # Usage Page 0xFF60, Usage 0x61
KEYBOARD = bytes((0x05, 0x01, 0x09, 0x06))  # Generic Desktop, Keyboard


def make_node(sysfs: Path, dev: Path, name: str, hid_id: str, descriptor: bytes) -> None:
    device = sysfs / name / "device"
    device.mkdir(parents=True)
    (device / "uevent").write_text(f"DRIVER=hid-generic\nHID_ID={hid_id}\nHID_NAME=pad\n")
    (device / "report_descriptor").write_bytes(descriptor)
    dev.mkdir(parents=True, exist_ok=True)
    (dev / name).write_text("")


def pad_tree(tmp_path: Path, name: str = "hidraw2") -> tuple[Path, Path]:
    """sysfs with the pad's keyboard node, its raw HID node ``name`` and a PS Move's node."""
    sysfs, dev = tmp_path / "sys", tmp_path / "dev"
    make_node(sysfs, dev, "hidraw1", "0003:0000CB00:00001337", KEYBOARD)
    make_node(sysfs, dev, name, "0003:0000CB00:00001337", KEYBOARD + FF60)
    make_node(sysfs, dev, "hidraw0", "0005:0000054C:000003D5", FF60)
    return sysfs, dev


class FakeHandle:
    """Answers like the firmware; can fail, block, or answer an old protocol version."""

    def __init__(self, backend: FakeBackend, path: str) -> None:
        self.backend = backend
        self.path = path
        self.reports: list[bytes] = []
        self.closed_on: str | None = None

    def drain(self) -> None:
        pass

    def transact(self, data: bytes, timeout: float) -> bytes:
        backend = self.backend
        if not backend.block.wait(5.0):  # a write stuck in the kernel, until its USB timeout
            raise OSError(errno.ETIMEDOUT, "Connection timed out")
        if backend.fail_writes:
            raise OSError(errno.ENODEV, "No such device")
        self.reports.append(data)
        backend.all_reports.append(data)
        status = backend.version if data[1] == hidraw.OP_HELLO else 0
        if data[1] in backend.refuse:
            status = hidraw.ERROR  # a firmware without the op (BR-002: `50 <op> FF`)
        return bytes((hidraw.CMD, data[1], status)).ljust(32, b"\x00")

    def close(self) -> None:
        self.closed_on = threading.current_thread().name
        self.backend.closed.append(self)


class FakeBackend:
    def __init__(self, version: int = 1, refuse: tuple[int, ...] = ()) -> None:
        self.version = version
        self.refuse = set(refuse)
        self.fail_writes = False
        self.block = threading.Event()
        self.block.set()
        self.open_block = threading.Event()  # cleared: an open hangs (a busy USB bus), bounded
        self.open_block.set()
        self.opening = 0
        self.handles: list[FakeHandle] = []
        self.closed: list[FakeHandle] = []
        self.all_reports: list[bytes] = []

    def open(self, path: str) -> FakeHandle:
        self.opening += 1
        self.open_block.wait(5.0)
        handle = FakeHandle(self, path)
        self.handles.append(handle)
        return handle

    def shows(self) -> int:
        return sum(1 for r in self.all_reports if r[1] == hidraw.OP_SHOW)

    def lines(self) -> list[bytes]:
        return [r for r in self.all_reports if r[1] == hidraw.OP_LINE]

    def last_frame(self) -> list[str]:
        """The text of the last 8 line reports sent (glyph bytes as latin-1 characters)."""
        return [r[4:25].decode("latin-1").rstrip() for r in self.lines()[-8:]]

    def ops(self) -> list[int]:
        return [r[1] for r in self.all_reports]


def pad_config(
    enabled: bool = True,
    display: bool = True,
    poll_ms: int = 200,
    result_ms: int = 300,
    autopage_ms: int = 5000,
    home_ms: int = 10000,
    target: tuple[str, int] | None = ("google.com", 443),
    check_ms: int = 30000,
):
    config = create_autospec(PadConfig, instance=True)
    config.enabled.return_value = enabled
    config.display_enabled.return_value = display
    config.display_poll_ms.return_value = poll_ms
    config.display_result_ms.return_value = result_ms
    config.device.return_value = ("cb00", "1337")
    config.preview_enabled.return_value = False
    config.display_autopage_ms.return_value = autopage_ms
    config.display_home_ms.return_value = home_ms
    config.internet_check_target.return_value = target
    config.internet_check_ms.return_value = check_ms
    return config
