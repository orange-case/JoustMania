"""The display link and the status poll (UC-016 main 1 to 3, A1, A2, BR-002, BR-003), with a fake hidraw.

Every wait is bounded (`asyncio.wait_for`), so a regression fails instead of hanging.
"""

import asyncio
import errno
import os
import socket
import threading
import time
from unittest.mock import create_autospec

import pytest

from proto import menu_pb2
from services.operator_pad import hidraw
from services.operator_pad.display import DisplayModel, PadDisplay
from services.operator_pad.screens import Screen
from services.operator_pad.state import MenuStatusReader
from services.operator_pad.tests.display_fakes import FF60, FakeBackend, make_node, pad_config, pad_tree

RESCAN = 0.05
HEARTBEAT = 0.05


def lobby(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {"selection": "JoustFFA", "ready_count": 1, "connected_count": 2}
    base.update(kw)
    return menu_pb2.GetMenuStatusResponse(**base)


class Link:
    def __init__(self, tmp_path, backend=None, config=None, statuses=None, heartbeat=HEARTBEAT):
        self.sysfs, self.dev = pad_tree(tmp_path)
        self.backend = backend or FakeBackend()
        self.config = config or pad_config()
        self.reader = create_autospec(MenuStatusReader, instance=True)
        self.reader.fetch.return_value = lobby()
        if statuses is not None:
            self.reader.fetch.side_effect = statuses
        self.model = DisplayModel(self.config, version="0.14.0")
        self.display = PadDisplay(
            self.config,
            self.model,
            self.reader,
            backend=self.backend,
            sysfs_root=self.sysfs,
            dev_dirs=(self.dev,),
            rescan_seconds=RESCAN,
            heartbeat_seconds=heartbeat,
            reply_timeout=0.2,
        )
        self.stop = asyncio.Event()
        self.task: asyncio.Task | None = None

    def start(self) -> "Link":
        self.task = asyncio.create_task(self.display.run(self.stop))
        return self

    async def until(self, condition, seconds: float = 2.0) -> None:
        """Wait for state the worker thread changes too (no event to wait on), bounded."""

        async def poll():
            while not condition():  # noqa: ASYNC110 - the fake hidraw runs on the display's worker thread
                await asyncio.sleep(0.005)

        await asyncio.wait_for(poll(), seconds)

    async def close(self) -> None:
        self.stop.set()
        self.backend.block.set()
        self.backend.open_block.set()
        await asyncio.wait_for(self.task, 3.0)


@pytest.fixture
async def link(tmp_path):
    made: list[Link] = []

    def make(**kw) -> Link:
        made.append(Link(tmp_path, **kw).start())
        return made[-1]

    yield make
    for each in made:
        await each.close()


@pytest.mark.uc("UC-016", flow="main", br=["BR-002", "BR-001"], steps=[1, 2])
async def test_hello_v1_brings_link_up_and_draws_status(link):
    pad = link()
    await pad.until(lambda: pad.model.link_up and "READY 1/2" in pad.backend.last_frame())
    (handle,) = pad.backend.handles
    assert handle.path.endswith("hidraw2")  # the 0xFF60 interface, not the keyboard or the PS Move
    assert handle.reports[0][:2] == bytes((hidraw.CMD, hidraw.OP_HELLO))
    assert pad.display.protocol_version == 1
    frame = pad.backend.last_frame()
    assert frame[0] == "JOUSTMANIA LOBBY" and frame[7] == "V0.14.0"


@pytest.mark.uc("UC-016", flow="main", br=["BR-002"], steps=[1])
async def test_a_later_protocol_version_brings_the_link_up_with_the_v1_screens(link):
    pad = link(backend=FakeBackend(version=2))
    await pad.until(lambda: pad.model.link_up and len(pad.backend.lines()) >= 8)
    assert pad.display.protocol_version == 2
    assert pad.backend.last_frame()[0].startswith("JOUSTMANIA")


@pytest.mark.uc("UC-016", flow="main", br=["BR-002"])
async def test_frame_is_eight_lines_then_one_show_and_unchanged_screen_sends_show_heartbeat(link):
    pad = link(statuses=None)
    await pad.until(lambda: pad.backend.shows() >= 4)
    reports = pad.backend.handles[0].reports
    first_show = next(i for i, r in enumerate(reports) if r[1] == hidraw.OP_SHOW)
    assert [r[1] for r in reports[1:first_show]] == [hidraw.OP_LINE] * 8
    assert reports[1][3] == 1 and all(r[3] == 0 for r in reports[2:first_show])  # only the title is inverted
    lines_before = len(pad.backend.lines())
    shows_before = pad.backend.shows()
    await pad.until(lambda: pad.backend.shows() >= shows_before + 3)
    assert len(pad.backend.lines()) - lines_before in (0, 8)  # at most the first poll's redraw; no line per beat


@pytest.mark.uc("UC-016", flow="main", br=["BR-003"], steps=[3])
async def test_status_change_redraws_without_key(link):
    pad = link()
    await pad.until(lambda: "READY 1/2" in pad.backend.last_frame())
    pad.reader.fetch.return_value = lobby(ready_count=2)
    await pad.until(lambda: "READY 2/2" in pad.backend.last_frame())


@pytest.mark.uc("UC-016", flow="A2", br=["BR-001", "BR-003"])
async def test_failed_read_keeps_values_and_shows_no_connection(link):
    pad = link()
    await pad.until(lambda: "READY 1/2" in pad.backend.last_frame())
    pad.reader.fetch.return_value = None
    await pad.until(lambda: pad.backend.last_frame()[7] == "NO CONNECTION")
    assert "READY 1/2" in pad.backend.last_frame()


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_old_firmware_link_down_retries_at_rescan_interval(link):
    pad = link(backend=FakeBackend(version=0))
    started = time.monotonic()
    await asyncio.sleep(RESCAN * 4.5)
    hellos = len(pad.backend.handles)
    elapsed = time.monotonic() - started
    assert not pad.model.link_up
    assert pad.backend.lines() == []
    assert 2 <= hellos <= elapsed / RESCAN + 1  # retried, never in a busy loop
    assert len(pad.backend.closed) >= hellos - 1


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_missing_node_link_down(link, tmp_path):
    pad = link()
    await pad.until(lambda: pad.model.link_up)
    await pad.close()
    empty = Link(tmp_path / "empty")
    empty.display._sysfs_root = tmp_path / "nothing"
    empty.start()
    await asyncio.sleep(RESCAN * 3)
    assert empty.backend.handles == [] and not empty.model.link_up
    await empty.close()


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_write_failure_drops_link_and_reattaches_new_node(link):
    pad = link()
    await pad.until(lambda: pad.model.link_up)
    pad.backend.fail_writes = True  # unplugged
    await pad.until(lambda: not pad.model.link_up)
    # The pad comes back as another hidraw node after a firmware reset.
    os.rename(pad.sysfs / "hidraw2", pad.sysfs / "hidraw7")
    os.rename(pad.dev / "hidraw2", pad.dev / "hidraw7")
    pad.backend.fail_writes = False
    await pad.until(lambda: pad.model.link_up and pad.backend.handles[-1].path.endswith("hidraw7"))
    assert pad.backend.handles[0] in pad.backend.closed


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"], cond=["Pre-3"])
async def test_display_disabled_closes_link_live(link):
    pad = link()
    await pad.until(lambda: pad.model.link_up)
    pad.config.display_enabled.return_value = False
    await pad.until(lambda: not pad.model.link_up and pad.backend.closed)
    opened = len(pad.backend.handles)
    await asyncio.sleep(RESCAN * 3)
    assert len(pad.backend.handles) == opened  # stays down while the flag is off


@pytest.mark.uc("UC-016", flow="A1", cond=["Pre-1"])
async def test_pad_disabled_keeps_the_display_link_down(link):
    pad = link(config=pad_config(enabled=False))
    await asyncio.sleep(RESCAN * 3)
    assert pad.backend.handles == [] and not pad.model.link_up


@pytest.mark.uc("UC-016", flow="main", br=["BR-003"], steps=[2])
async def test_poll_only_while_link_up_at_poll_interval(link):
    pad = link(backend=FakeBackend(version=0))
    await asyncio.sleep(0.5)
    pad.reader.fetch.assert_not_awaited()  # BR-003: no poll with the link down
    pad.backend.version = 1
    await pad.until(lambda: pad.model.link_up)
    start = pad.reader.fetch.await_count
    await asyncio.sleep(0.5)
    polls = pad.reader.fetch.await_count - start
    assert 1 <= polls <= 4  # every 200 ms (pad_config's display_poll_ms), not every loop


@pytest.mark.uc("UC-016", flow="A1", br=["BR-004"])
async def test_a_screen_that_raises_takes_the_link_down_and_it_comes_back(link):
    pad = link()
    await pad.until(lambda: pad.model.link_up)
    real = pad.model.current
    calls = {"n": 0}

    def broken(now):
        calls["n"] += 1
        if calls["n"] < 3:
            raise RuntimeError("bug in a screen")
        return real(now)

    pad.model.current = broken
    pad.model.dirty.set()
    await pad.until(lambda: not pad.model.link_up)  # the volume sound comes back (BR-004)
    await pad.until(lambda: pad.model.link_up)


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_hung_write_never_blocks_the_loop_and_close_runs_on_the_worker_thread(link):
    pad = link()
    await pad.until(lambda: pad.model.link_up)
    pad.backend.block.clear()  # the next write hangs in the "kernel"
    pad.model.dirty.set()
    started = time.monotonic()
    await asyncio.sleep(0.01)
    assert time.monotonic() - started < 0.2  # the loop keeps running
    await pad.until(lambda: not pad.model.link_up, seconds=4.0)
    assert len(pad.backend.handles) == 1  # no second link while the worker is stuck
    pad.backend.block.set()
    await pad.until(lambda: pad.backend.closed, seconds=4.0)
    assert pad.backend.closed[0].closed_on.startswith("pad-display")
    assert pad.backend.closed[0].closed_on != threading.current_thread().name


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
async def test_an_open_that_returns_after_its_timeout_is_closed_on_the_worker_thread(link):
    backend = FakeBackend()
    backend.open_block.clear()  # the open hangs past its deadline (reply 0.2 s + margin 0.5 s)
    pad = link(backend=backend)
    await pad.until(lambda: backend.opening == 1)
    await asyncio.sleep(1.0)  # the link gave up on it
    assert backend.handles == [] and not pad.model.link_up
    backend.open_block.set()
    await pad.until(lambda: bool(backend.handles) and backend.handles[0] in backend.closed, seconds=3.0)
    late = backend.handles[0]
    assert late.closed_on.startswith("pad-display") and late.reports == []
    await pad.until(lambda: pad.model.link_up)  # the next rescan opens a fresh handle


@pytest.mark.uc("UC-016", flow="main", br=["BR-001", "BR-002"])
async def test_a_change_made_while_the_screen_is_computed_is_drawn_without_the_heartbeat(link):
    """`dirty` is cleared before `current()`: a hook firing right after it still wakes the loop."""
    pad = link(config=pad_config(poll_ms=60_000), heartbeat=30.0)
    await pad.until(lambda: len(pad.backend.lines()) >= 8)
    real = pad.model.current
    fired = []

    def current_then_a_hook(now):
        screen = real(now)
        if not fired:
            fired.append(now)
            pad.model.note_status(lobby(ready_count=2), pad.model.read_token())
        return screen

    pad.model.current = current_then_a_hook
    pad.model.dirty.set()
    try:
        await pad.until(lambda: "READY 2/2" in pad.backend.last_frame(), seconds=1.5)
    finally:
        pad.stop.set()
        pad.model.dirty.set()  # the loop sleeps until the 30 s heartbeat otherwise


@pytest.mark.uc("UC-016", flow="main", br=["BR-002"])
def test_find_display_node_picks_ff60_interface_and_denies_ps_move(tmp_path):
    sysfs, dev = pad_tree(tmp_path)
    host_dev = tmp_path / "host-dev"
    make_node(sysfs, host_dev, "hidraw9", "0003:0000ABCD:00000001", FF60)
    assert hidraw.find_display_node("cb00", "1337", sysfs, (dev,)) == str(dev / "hidraw2")
    assert hidraw.find_display_node("054c", "03d5", sysfs, (dev,)) is None  # PS Move: never
    # In the container the node exists only under the host's /dev mount.
    assert hidraw.find_display_node("abcd", "0001", sysfs, (dev, host_dev)) == str(host_dev / "hidraw9")
    assert hidraw.find_display_node("cb00", "9999", sysfs, (dev,)) is None


@pytest.mark.uc("UC-016", flow="main", br=["BR-002"])
def test_hidraw_handle_skips_foreign_reports_and_reads_the_reply():
    """Bench check of our framing over a socketpair: report id 0 first, other openers' reports skipped."""
    ours, firmware = socket.socketpair(socket.AF_UNIX, socket.SOCK_SEQPACKET)
    handle = hidraw.HidrawHandle(os.dup(ours.fileno()))
    ours.close()

    def answer():
        request = firmware.recv(64)
        assert request[0] == 0 and len(request) == 33
        firmware.send(bytes((0x03, 0x00)).ljust(32, b"\x00"))  # a Vial reply to someone else
        firmware.send(bytes((hidraw.CMD, hidraw.OP_SHOW, 0)).ljust(32, b"\x00"))  # another op
        firmware.send(bytes((hidraw.CMD, hidraw.OP_HELLO, 1)).ljust(32, b"\x00"))

    thread = threading.Thread(target=answer)
    thread.start()
    try:
        assert hidraw.hello(handle, 1.0) == 1
    finally:
        thread.join(2)
        handle.close()
        firmware.close()


@pytest.mark.uc("UC-016", flow="A1", br=["BR-002"])
def test_hidraw_handle_times_out_without_a_reply_and_an_error_reply_raises():
    ours, firmware = socket.socketpair(socket.AF_UNIX, socket.SOCK_SEQPACKET)
    handle = hidraw.HidrawHandle(os.dup(ours.fileno()))
    ours.close()
    try:
        with pytest.raises(TimeoutError):
            handle.transact(hidraw.report(hidraw.OP_HELLO), 0.05)
        firmware.send(bytes((hidraw.CMD, hidraw.OP_SHOW, hidraw.ERROR)).ljust(32, b"\x00"))
        with pytest.raises(hidraw.ProtocolError):
            hidraw.send_show(handle, 1, 0.5)
        firmware.close()
        with pytest.raises(OSError) as gone:
            handle.transact(hidraw.report(hidraw.OP_HELLO), 0.5)
        assert gone.value.errno in (errno.EIO, errno.EPIPE, errno.ECONNRESET)
    finally:
        handle.close()


def test_screen_equality_drives_redraws():
    assert Screen.of(["A"]) == Screen.of(["A"])
    assert Screen.of(["A"]) != Screen.of(["B"])
