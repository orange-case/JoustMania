"""Loop watchdog (UC-060 A8, NFR-080): a stalled event loop exits the process with code 70."""

import asyncio
import signal
import threading
import time

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib import watchdog as wd

pytestmark = [pytest.mark.uc("UC-060", flow="A8"), pytest.mark.nfr("NFR-080")]

FAST = {"beat_interval_s": 0.02, "check_interval_s": 0.02}


class _Exit:
    def __init__(self):
        self.codes: list[int] = []
        self.called = threading.Event()

    def __call__(self, code: int) -> None:
        self.codes.append(code)
        self.called.set()


@pytest.fixture
def spans():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    return exporter, provider.get_tracer("test")


def _watchdog(timeout_s, exit_fn, tracer, **kw):
    opts = {**FAST, **kw}
    return wd.LoopWatchdog("test-svc", timeout_s, exit_fn=exit_fn, flush_fn=lambda: None, tracer=tracer, **opts)


async def test_stalled_loop_exits_with_code_70_and_records_the_trip_span(spans):
    exporter, tracer = spans
    exit_fn = _Exit()
    dog = _watchdog(0.2, exit_fn, tracer).start()
    await asyncio.sleep(0.05)

    time.sleep(0.6)  # noqa: ASYNC251  # blocks the loop: no beat

    assert exit_fn.called.wait(2)
    assert exit_fn.codes == [wd.EXIT_CODE] == [70]
    assert dog.tripped
    (span,) = exporter.get_finished_spans()
    assert span.name == "watchdog.trip"
    assert span.attributes["watchdog.timeout_seconds"] == 0.2
    assert span.attributes["watchdog.stalled_seconds"] >= 0.2
    dog.stop()


async def test_ticking_loop_never_trips(spans):
    _, tracer = spans
    exit_fn = _Exit()
    dog = _watchdog(0.2, exit_fn, tracer).start()

    for _ in range(40):  # 0.8 s of short awaits, four times the timeout
        await asyncio.sleep(0.02)

    dog.stop()
    assert exit_fn.codes == []
    assert not dog.tripped


async def test_a_stall_shorter_than_the_timeout_does_not_trip(spans):
    _, tracer = spans
    exit_fn = _Exit()
    dog = _watchdog(0.5, exit_fn, tracer).start()
    await asyncio.sleep(0.05)

    time.sleep(0.25)  # noqa: ASYNC251  # half the timeout
    await asyncio.sleep(0.2)

    assert exit_fn.codes == []
    time.sleep(0.7)  # noqa: ASYNC251  # past the timeout
    assert exit_fn.called.wait(2)
    assert exit_fn.codes == [70]
    dog.stop()


async def test_stop_before_a_stall_means_no_exit(spans):
    _, tracer = spans
    exit_fn = _Exit()
    dog = _watchdog(0.1, exit_fn, tracer).start()
    await asyncio.sleep(0.05)

    dog.stop()
    time.sleep(0.4)  # noqa: ASYNC251

    assert not exit_fn.called.wait(0.2)


async def test_timeout_zero_turns_the_watchdog_off(spans):
    _, tracer = spans
    exit_fn = _Exit()
    dog = _watchdog(0, exit_fn, tracer).start()

    time.sleep(0.2)  # noqa: ASYNC251

    assert not dog.enabled
    assert not exit_fn.called.wait(0.1)
    dog.stop()


async def test_a_hanging_flush_cannot_keep_the_process_alive(spans):
    _, tracer = spans
    exit_fn = _Exit()
    release = threading.Event()
    dog = wd.LoopWatchdog(
        "test-svc",
        0.1,
        exit_fn=exit_fn,
        flush_fn=lambda: release.wait(10),
        tracer=tracer,
        hard_exit_after_s=0.2,
        **FAST,
    ).start()
    await asyncio.sleep(0.05)

    time.sleep(0.3)  # noqa: ASYNC251

    try:
        assert exit_fn.called.wait(2)
        assert exit_fn.codes[0] == 70
    finally:
        release.set()
        dog.stop()


@pytest.mark.parametrize(
    ("raw", "expected"),
    [(None, 20.0), ("", 20.0), ("0", 0.0), ("45", 45.0), ("2.5", 2.5)],
)
def test_timeout_from_env(monkeypatch, raw, expected):
    if raw is None:
        monkeypatch.delenv(wd.TIMEOUT_ENV, raising=False)
    else:
        monkeypatch.setenv(wd.TIMEOUT_ENV, raw)
    assert wd.timeout_from_env() == expected


@pytest.mark.parametrize("raw", ["abc", "-1", "nan", "inf"])
def test_bad_timeout_fails_naming_the_variable(monkeypatch, raw):
    monkeypatch.setenv(wd.TIMEOUT_ENV, raw)
    with pytest.raises(ValueError, match=wd.TIMEOUT_ENV):
        wd.timeout_from_env()


async def test_stall_signal_is_installed_only_when_asked(monkeypatch, spans):
    _, tracer = spans
    loop = asyncio.get_running_loop()
    installed = []
    monkeypatch.setattr(loop, "add_signal_handler", lambda sig, _cb: installed.append(sig))
    monkeypatch.setenv(wd.TIMEOUT_ENV, "0")

    monkeypatch.delenv(wd.STALL_SIGNAL_ENV, raising=False)
    wd.start_loop_watchdog("test-svc", tracer=tracer).stop()
    assert installed == []

    monkeypatch.setenv(wd.STALL_SIGNAL_ENV, "1")
    wd.start_loop_watchdog("test-svc", tracer=tracer).stop()
    assert installed == [signal.SIGUSR2]
