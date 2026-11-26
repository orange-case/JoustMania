"""Shutdown start ends the client streams with UNAVAILABLE, before any other teardown (UC-030 BR-006)."""

import asyncio
import sys
import time
from pathlib import Path

import grpc
import grpc.aio
import pytest

test_dir = Path(__file__).parent
sys.path.insert(0, str(test_dir.parent.parent.parent))
sys.path.insert(0, str(test_dir))

from test_gameplay_stream_frame_error import (  # noqa: F401  (pytest fixtures)
    _AbortedError,
    _Context,
    _requests,
    stream_servicer,
    wire,
)
from test_identify_controller import servicer  # noqa: F401  (pytest fixture)

from proto import controller_manager_pb2
from services.controller_manager.servicer import SHUTDOWN_ABORT_DETAILS

# Well under the 1 s button-queue poll and the 5 s server grace: the end comes from the shutdown itself.
PROMPT_S = 0.5
# A missing shutdown check fails here (TimeoutError) instead of hanging the job.
TEST_BOUND_S = 5.0


async def _button_requests():
    yield controller_manager_pb2.ButtonEventStreamControl(config=controller_manager_pb2.ButtonEventStreamConfig())
    await asyncio.Event().wait()


async def _wait_for(predicate):
    async with asyncio.timeout(TEST_BOUND_S):
        while not predicate():  # noqa: ASYNC110  (a plain dict check)
            await asyncio.sleep(0.01)


@pytest.mark.asyncio
@pytest.mark.uc("UC-030", flow="main", br=["BR-006"])
async def test_shutdown_aborts_gameplay_stream_unavailable_without_another_frame(stream_servicer):  # noqa: F811
    """UC-030 BR-006: once shutdown starts, the stream builds no frame and aborts UNAVAILABLE."""
    built = []
    real_build = stream_servicer._build_gameplay_update

    def counting_build(*args, **kwargs):
        built.append(stream_servicer._shutting_down)
        return real_build(*args, **kwargs)

    stream_servicer._build_gameplay_update = counting_build
    context = _Context()

    with pytest.raises(_AbortedError):
        async with asyncio.timeout(TEST_BOUND_S):
            async for _ in stream_servicer.StreamGameplayData(_requests(), context):
                stream_servicer.begin_shutdown()

    assert built == [False]
    assert context.abort_args == (grpc.StatusCode.UNAVAILABLE, SHUTDOWN_ABORT_DETAILS)


@pytest.mark.asyncio
@pytest.mark.uc("UC-030", flow="main", br=["BR-006"])
async def test_wire_shutdown_ends_open_gameplay_call_unavailable_promptly(stream_servicer, wire):  # noqa: F811
    """UC-030 BR-006: the client's open call ends UNAVAILABLE within a frame or so of shutdown start."""
    call = wire.StreamGameplayData(_requests())
    await call.read()

    stream_servicer.begin_shutdown()
    started = time.monotonic()
    with pytest.raises(grpc.aio.AioRpcError) as excinfo:
        async with asyncio.timeout(TEST_BOUND_S):
            while True:
                await call.read()

    assert time.monotonic() - started < PROMPT_S
    assert excinfo.value.code() == grpc.StatusCode.UNAVAILABLE
    assert excinfo.value.details() == SHUTDOWN_ABORT_DETAILS


@pytest.mark.asyncio
@pytest.mark.uc("UC-030", flow="main", br=["BR-006"])
async def test_wire_gameplay_call_opened_during_shutdown_gets_no_frame(stream_servicer, wire):  # noqa: F811
    """UC-030 BR-006: a call opened after shutdown started ends UNAVAILABLE before its first frame."""
    stream_servicer.begin_shutdown()
    call = wire.StreamGameplayData(_requests())

    with pytest.raises(grpc.aio.AioRpcError) as excinfo:
        async with asyncio.timeout(TEST_BOUND_S):
            await call.read()

    assert excinfo.value.code() == grpc.StatusCode.UNAVAILABLE


@pytest.mark.asyncio
@pytest.mark.uc("UC-030", flow="main", br=["BR-006"])
async def test_wire_shutdown_ends_button_call_unavailable_at_once(stream_servicer, wire):  # noqa: F811
    """UC-030 BR-006: the menu's button stream ends the same way, without waiting out its queue poll."""
    call = wire.StreamButtonEvents(_button_requests())
    await _wait_for(lambda: stream_servicer.button_event_subscribers)

    stream_servicer.begin_shutdown()
    started = time.monotonic()
    with pytest.raises(grpc.aio.AioRpcError) as excinfo:
        async with asyncio.timeout(TEST_BOUND_S):
            async for _ in call:
                pass

    assert time.monotonic() - started < PROMPT_S
    assert excinfo.value.code() == grpc.StatusCode.UNAVAILABLE
    assert excinfo.value.details() == SHUTDOWN_ABORT_DETAILS


@pytest.mark.asyncio
@pytest.mark.uc("UC-030", flow="main", br=["BR-006"])
async def test_shutdown_begins_before_discovery_stops(servicer):  # noqa: F811
    """UC-030 BR-006: the streams are told to end before discovery stops, so no stale frame goes out."""
    seen = []
    servicer.discovery_loop.stop.side_effect = lambda: seen.append(servicer._shutting_down)

    await servicer.shutdown()

    assert seen == [True]
