"""A challenge flag flipped mid-match shows on the match trace within one tick (FR-255, UC-035 BR-018)."""

from __future__ import annotations

import time

import pytest
from openfeature import api
from openfeature.provider.in_memory_provider import InMemoryFlag, InMemoryProvider
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib import challenge_flags
from lib.challenge_flags import EVENT_END, EVENT_START, match_scope
from lib.openfeature_otel_hooks import DedupingTracingHook
from proto import controller_manager_pb2
from services.game_coordinator import game_clock
from services.game_coordinator.games.base import Player
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.tests.conftest import EventCollector, MockControllerManagerService

DOMAIN = "challenge_uc035_test"
KEY = "stowaway.challenge"
STILL = controller_manager_pb2.Vector3(x=0.0, y=0.0, z=1.0)
FLIP_AT = 2  # the frame (0-based) delivered right after the flip


def _flag(variant: str) -> InMemoryFlag:
    return InMemoryFlag(variant, {"off": "off", "swing": "swing"})


def _frame():
    return controller_manager_pb2.GameplayDataUpdate(
        controllers=[controller_manager_pb2.GameplayData(serial=s, accel=STILL) for s in ("p0", "p1")],
        timestamp=int(time.time() * 1000),
    )


class FlippingStream:
    """Yields frames; flips the flag before frame FLIP_AT and notes the span events seen per fetch."""

    def __init__(self, provider: InMemoryProvider, span, frames: int = 5):
        self.provider = provider
        self.span = span
        self.remaining = frames
        self.fetched = 0
        self.swing_events_at_fetch: list[int] = []

    async def write(self, message):
        pass

    def __aiter__(self):
        return self

    async def __anext__(self):
        self.swing_events_at_fetch.append(
            sum(
                1
                for e in self.span.events
                if e.name == "feature_flag.evaluation" and e.attributes.get("feature_flag.result.variant") == "swing"
            )
        )
        if self.fetched == self.remaining:
            raise StopAsyncIteration
        if self.fetched == FLIP_AT:
            self.provider._flags[KEY] = _flag("swing")
        self.fetched += 1
        return _frame()


@pytest.fixture(autouse=True)
def sdk_enabled(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)  # conftest disables the SDK; these are local providers


@pytest.fixture
def watched(monkeypatch):
    provider = InMemoryProvider({KEY: _flag("off")})
    api.set_provider(provider, domain=DOMAIN)
    client = challenge_flags._client(DOMAIN)
    client.add_hooks([DedupingTracingHook()])
    monkeypatch.setattr(challenge_flags, "WATCHED_CHALLENGE_FLAGS", ((DOMAIN, KEY, "off"),))
    yield provider
    client.hooks.clear()
    challenge_flags._clients.pop(DOMAIN, None)


def _game() -> FFAGame:
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=2),
        event_publisher=EventCollector().publish,
        audio_client=None,
        game_id="test_uc035_challenge",
    )
    game.running = True
    game.start_time = game_clock.now()
    for serial in ("p0", "p1"):
        game.players[serial] = Player(serial=serial, team=0, alive=True, color=(255, 0, 0))
    return game


@pytest.mark.asyncio
@pytest.mark.uc("UC-035", br=["BR-018"])
async def test_flip_mid_match_shows_one_event_within_one_tick(watched):
    exporter = InMemorySpanExporter()
    tracer_provider = TracerProvider()
    tracer_provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracer = tracer_provider.get_tracer("t")
    game = _game()

    with (
        tracer.start_as_current_span("Joust Free-for-All") as root,
        match_scope(root),
        tracer.start_as_current_span("game.phase.gameplay") as gameplay,
    ):
        game.gameplay_stream = FlippingStream(watched, gameplay)
        await game._game_loop()

    spans = {s.name: s for s in exporter.get_finished_spans()}
    gameplay_span, root_span = spans["game.phase.gameplay"], spans["Joust Free-for-All"]
    assert gameplay_span.context.trace_id == root_span.context.trace_id

    variants = [
        e.attributes["feature_flag.result.variant"]
        for e in gameplay_span.events
        if e.name == "feature_flag.evaluation" and e.attributes["feature_flag.key"] == KEY
    ]
    assert variants == ["off", "swing"]  # five ticks, one event per change

    stream = game.gameplay_stream
    # The frame after the flip is processed before the next fetch: the event is there by then.
    assert stream.swing_events_at_fetch[FLIP_AT] == 0
    assert stream.swing_events_at_fetch[FLIP_AT + 1] == 1

    challenge = [
        (e.name, e.attributes["orangecase.challenge.name"], e.attributes.get("orangecase.challenge.end_reason"))
        for e in root_span.events
        if e.name in (EVENT_START, EVENT_END)
    ]
    assert challenge == [(EVENT_START, "swing", None), (EVENT_END, "swing", "match_end")]


@pytest.mark.asyncio
@pytest.mark.uc("UC-035", br=["BR-018"])
async def test_a_poll_failure_never_reaches_the_game_loop(monkeypatch):
    def boom(*_args):
        raise RuntimeError("provider exploded")

    monkeypatch.setattr(challenge_flags, "WATCHED_CHALLENGE_FLAGS", (("x", KEY, "off"),))
    monkeypatch.setattr(challenge_flags, "_client", boom)
    game = _game()
    tracer = TracerProvider().get_tracer("t")
    with tracer.start_as_current_span("root") as root, match_scope(root):
        game.gameplay_stream = FlippingStream(InMemoryProvider({}), root, frames=3)
        await game._game_loop()
    assert game.gameplay_stream.fetched == 3
