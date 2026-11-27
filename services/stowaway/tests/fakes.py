"""Fakes for the game coordinator and the mock control API, and game-event builders."""

from __future__ import annotations

import asyncio

from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from proto import controller_manager_mock_pb2 as mock_pb2
from proto import game_coordinator_pb2 as gc_pb2
from services.stowaway.config import Settings


def event(event_type: str, game_id: str = "g1", parent_span=None, **data) -> gc_pb2.GameEvent:
    values = {k: str(v) for k, v in data.items()}
    if parent_span is not None:
        carrier: dict[str, str] = {}
        from opentelemetry import trace

        TraceContextTextMapPropagator().inject(carrier, context=trace.set_span_in_context(parent_span))
        values["_traceparent"] = carrier["traceparent"]
    return gc_pb2.GameEvent(event_type=event_type, data=values, game_id=game_id)


class FlagState:
    """A mutable flag reading for SettingsSource(read=...)."""

    def __init__(self, **values) -> None:
        self.settings = Settings(**values)
        self.reads = 0

    def __call__(self) -> Settings:
        self.reads += 1
        return self.settings

    def set(self, **values) -> None:
        from dataclasses import replace

        self.settings = replace(self.settings, **values)


class FakeCall:
    """A server stream: yields queued events until closed or cancelled."""

    def __init__(self) -> None:
        self.queue: asyncio.Queue = asyncio.Queue()
        self.cancelled = False

    def push(self, *events) -> None:
        for e in events:
            self.queue.put_nowait(e)

    def close(self) -> None:
        self.queue.put_nowait(None)

    def cancel(self) -> bool:
        self.cancelled = True
        self.queue.put_nowait(None)
        return True

    def __aiter__(self):
        return self

    async def __anext__(self):
        item = await self.queue.get()
        if item is None:
            if self.cancelled:
                raise RuntimeError("CANCELLED")
            raise StopAsyncIteration
        return item


class FakeCoordinator:
    def __init__(self, state=gc_pb2.IDLE) -> None:
        self.state = state
        self.calls: list[FakeCall] = []
        self.requests: list = []
        self.force_ended: list = []
        self.fail_state = False

    async def GetGameState(self, request, timeout=None):
        if self.fail_state:
            raise RuntimeError("unreachable")
        return gc_pb2.GetGameStateResponse(success=True, game_info=gc_pb2.GameInfo(state=self.state))

    def StreamGameEvents(self, request):
        call = FakeCall()
        self.requests.append(request)
        self.calls.append(call)
        return call

    async def ForceEndGame(self, request, timeout=None):
        self.force_ended.append(request)
        return gc_pb2.ForceEndGameResponse(success=True)


class FakeMock:
    def __init__(self, success: bool = True) -> None:
        self.success = success
        self.added: list = []
        self.removed: list[str] = []

    async def AddControllers(self, request, timeout=None):
        self.added.append(request)
        serials = [f"mock-{i}" for i in range(request.count)]
        return mock_pb2.AddControllersResponse(
            success=self.success, serials=serials, error="" if self.success else "no"
        )

    async def RemoveController(self, request, timeout=None):
        self.removed.append(request.serial)
        return mock_pb2.RemoveControllerResponse(success=True)
