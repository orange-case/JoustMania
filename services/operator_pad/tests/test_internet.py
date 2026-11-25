"""The internet check (UC-016 BR-010, FR-256): a TCP connect on its own thread, one at a time. Bounded waits."""

import asyncio
import contextlib
import socket
import threading
import time
from types import SimpleNamespace

import pytest

from services.operator_pad import internet
from services.operator_pad.display import DisplayModel
from services.operator_pad.internet import InternetCheck, tcp_check
from services.operator_pad.pages import NET_DOWN, NET_OFF, NET_OK, NET_UNKNOWN
from services.operator_pad.tests.display_fakes import pad_config


class Probe:
    def __init__(self, result=True, block: threading.Event | None = None) -> None:
        self.result = result
        self.block = block
        self.calls: list[tuple] = []

    def __call__(self, host, port, timeout):
        self.calls.append((host, port, timeout))
        if self.block is not None:
            self.block.wait(5.0)
        if isinstance(self.result, Exception):
            raise self.result
        return self.result


def make(probe, target=("google.com", 443), version=3, check_ms=30000):
    config = pad_config(target=target, check_ms=check_ms)
    model = DisplayModel(config)
    if version is not None:
        model.set_protocol(version)
    return model, InternetCheck(config, model, probe=probe)


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
@pytest.mark.parametrize(("result", "state"), [(True, NET_OK), (False, NET_DOWN), (OSError("refused"), NET_DOWN)])
async def test_a_check_sets_ok_or_down(result, state):
    probe = Probe(result)
    model, check = make(probe)
    assert model.internet == NET_UNKNOWN  # before the first check ends
    await asyncio.wait_for(check.check(), 2.0)
    assert model.internet == state
    assert probe.calls == [("google.com", 443, internet.CONNECT_TIMEOUT_SECONDS)]


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
async def test_target_off_runs_no_check():
    probe = Probe()
    model, check = make(probe, target=None)
    await asyncio.wait_for(check.check(), 2.0)
    assert model.internet == NET_OFF and probe.calls == []


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
async def test_a_hung_check_counts_as_down_never_blocks_and_the_next_one_is_skipped(monkeypatch):
    monkeypatch.setattr(internet, "CONNECT_TIMEOUT_SECONDS", 0.05)
    monkeypatch.setattr(internet, "WAIT_MARGIN_SECONDS", 0.05)
    block = threading.Event()
    probe = Probe(True, block)
    model, check = make(probe)
    started = time.monotonic()
    await asyncio.wait_for(check.check(), 2.0)
    assert time.monotonic() - started < 1.0 and model.internet == NET_DOWN
    await asyncio.wait_for(check.check(), 2.0)
    assert len(probe.calls) == 1  # one check at a time: the hung lookup still holds the thread
    block.set()
    await asyncio.sleep(0.05)
    await asyncio.wait_for(check.check(), 2.0)
    assert len(probe.calls) == 2 and model.internet == NET_OK


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
async def test_run_checks_only_with_pages_and_at_once_on_link_up():
    probe = Probe()
    model, check = make(probe, version=None, check_ms=60000)
    stop = asyncio.Event()
    task = asyncio.create_task(check.run(stop))
    try:
        await asyncio.sleep(0.05)
        assert probe.calls == []  # no link with protocol 2 or higher
        model.set_protocol(1)
        await asyncio.sleep(0.05)
        assert probe.calls == []
        model.set_protocol(2)  # link-up wakes the check: no 60 s wait
        for _ in range(100):
            if model.internet == NET_OK:
                break
            await asyncio.sleep(0.01)
        assert model.internet == NET_OK and len(probe.calls) == 1
    finally:
        stop.set()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
def test_tcp_check_connects_and_closes_at_once_with_nothing_sent():
    server = socket.socket()
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    port = server.getsockname()[1]
    try:
        assert tcp_check("127.0.0.1", port, 1.0)
        conn, _ = server.accept()
        conn.settimeout(1.0)
        assert conn.recv(16) == b""  # closed, nothing sent
        conn.close()
    finally:
        server.close()
    assert not tcp_check("127.0.0.1", port, 1.0)  # refused


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
def test_tcp_check_failed_lookup_is_down_and_ipv4_is_tried_first(monkeypatch):
    families = []

    def lookup(_host, _port, family=0, _kind=0):
        families.append(family)
        raise socket.gaierror("no such name")

    monkeypatch.setattr(internet.socket, "getaddrinfo", lookup)
    assert not tcp_check("example.test", 443, 1.0)
    assert families == [socket.AF_INET, 0]


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
def test_tcp_check_shares_the_budget_so_a_later_address_is_still_tried(monkeypatch):
    """Several A records, the first ones dead: the whole check stays within the budget the caller waits."""
    clock = [0.0]
    tried = []
    addresses = [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (f"192.0.2.{k}", 443)) for k in (1, 2, 3)]

    def connect(address, timeout):
        tried.append((address[0], timeout))
        if address[0] != "192.0.2.3":
            clock[0] += timeout  # a dead address uses its whole timeout
            raise TimeoutError
        return contextlib.nullcontext()

    monkeypatch.setattr(internet.socket, "getaddrinfo", lambda *_a, **_k: addresses)
    monkeypatch.setattr(internet.socket, "create_connection", connect)
    monkeypatch.setattr(internet, "time", SimpleNamespace(monotonic=lambda: clock[0]))
    assert tcp_check("example.test", 443, 3.0)
    assert [a for a, _ in tried] == ["192.0.2.1", "192.0.2.2", "192.0.2.3"]
    assert clock[0] <= 3.0 and tried[-1][1] > 0


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
def test_tcp_check_lookup_that_used_the_whole_budget_is_down_with_no_connect(monkeypatch):
    clock = [0.0]
    tried = []

    def lookup(*_a, **_k):
        clock[0] += 3.0
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("192.0.2.1", 443))]

    def connect(address, _timeout):
        tried.append(address)
        return contextlib.nullcontext()

    monkeypatch.setattr(internet.socket, "getaddrinfo", lookup)
    monkeypatch.setattr(internet.socket, "create_connection", connect)
    monkeypatch.setattr(internet, "time", SimpleNamespace(monotonic=lambda: clock[0]))
    assert not tcp_check("example.test", 443, 3.0)
    assert tried == []
