"""UC-106 main scenario: stowaway follows the primary game and runs the challenge per match."""

import asyncio

import pytest

from proto import game_coordinator_pb2 as gc_pb2
from services.stowaway.challenges import STOP_MATCH_END
from services.stowaway.cli import parse, settings_source
from services.stowaway.config import SettingsSource
from services.stowaway.runner import Stowaway
from services.stowaway.tests.fakes import FakeCoordinator, FlagState, event


class RecordingGenerator:
    def __init__(self) -> None:
        self.matches = []

    async def run(self, run):
        self.matches.append((run.name, run.match.game_id))
        while run.stop_reason() is None:
            await run.tick()
        return run.stop_reason()


async def _start(stowaway, gc, stop):
    task = asyncio.create_task(stowaway.follow_primary(stop))
    for _ in range(100):
        if gc.calls:
            return task, gc.calls[-1]
        await asyncio.sleep(0.01)
    raise AssertionError("no StreamGameEvents call")


@pytest.mark.uc("UC-106", flow="main", steps=[3, 4, 5], br=["BR-002"])
async def test_the_primary_match_runs_the_flagged_challenge(spans):
    gc = FakeCoordinator()
    generator = RecordingGenerator()
    flags = FlagState(enabled=True, challenge="span-rounds", mode="telemetry")
    stowaway = Stowaway(SettingsSource(read=flags, max_age=0.0), gc, {"span-rounds": generator})
    stop = asyncio.Event()
    task, call = await _start(stowaway, gc, stop)

    assert not gc.requests[0].HasField("start_config")  # a consumer of the primary game only
    parent = spans.tracer.start_span("root")
    call.push(event("game_started", "g1", parent_span=parent))
    await asyncio.sleep(0.05)
    assert generator.matches == [("span-rounds", "g1")]
    call.push(event("game_ended", "g1"))
    await asyncio.sleep(0.05)
    assert stowaway.primary.match is None
    stop.set()
    await asyncio.wait_for(task, 1.0)


@pytest.mark.uc("UC-106", flow="A4")
@pytest.mark.parametrize(
    "values",
    [
        {"enabled": True, "challenge": "none", "mode": "telemetry"},
        {"enabled": True, "challenge": "span-rounds", "mode": "players"},
        {"enabled": False, "challenge": "span-rounds", "mode": "telemetry"},
    ],
)
async def test_no_generator_without_challenge_telemetry_mode_or_kill_switch(spans, values):
    gc = FakeCoordinator()
    generator = RecordingGenerator()
    stowaway = Stowaway(SettingsSource(read=FlagState(**values), max_age=0.0), gc, {"span-rounds": generator})
    stop = asyncio.Event()
    task, call = await _start(stowaway, gc, stop)
    call.push(event("game_started", "g1", parent_span=spans.tracer.start_span("root")))
    await asyncio.sleep(0.05)
    stop.set()
    await asyncio.wait_for(task, 1.0)
    assert generator.matches == []


@pytest.mark.uc("UC-106", flow="main", steps=[3])
async def test_a_broken_stream_reconnects_and_ends_the_open_match(spans):
    gc = FakeCoordinator()
    ended = []

    class Gen(RecordingGenerator):
        async def run(self, run):
            reason = await super().run(run)
            ended.append(reason)
            return reason

    flags = FlagState(enabled=True, challenge="span-rounds")
    stowaway = Stowaway(SettingsSource(read=flags, max_age=0.0), gc, {"span-rounds": Gen()}, reconnect_s=0.01)
    stop = asyncio.Event()
    task, call = await _start(stowaway, gc, stop)
    call.push(event("game_started", "g1", parent_span=spans.tracer.start_span("root")))
    await asyncio.sleep(0.05)
    call.cancel()  # the stream fails
    await asyncio.sleep(0.1)
    assert ended == [STOP_MATCH_END]
    assert len(gc.calls) >= 2
    stop.set()
    await asyncio.wait_for(task, 1.0)


@pytest.mark.uc("UC-106", flow="A2", steps=[2], br=["BR-005"])
async def test_both_mode_attaches_the_challenge_to_the_pattern_game(spans):
    from services.stowaway.tests.fakes import FakeMock

    gc = FakeCoordinator(state=gc_pb2.IDLE)
    generator = RecordingGenerator()
    flags = FlagState(enabled=True, challenge="span-rounds", mode="both")
    stowaway = Stowaway(SettingsSource(read=flags, max_age=0.0), gc, {"span-rounds": generator}, mock=FakeMock())
    parent = spans.tracer.start_span("root")
    stowaway.pattern.feed(event("game_started", "p1", parent_span=parent))
    await asyncio.sleep(0.02)
    assert generator.matches == [("span-rounds", "p1")]
    await stowaway.pattern.close()


@pytest.mark.uc("UC-106", flow="A6", br=["BR-007"])
def test_no_flagd_needs_an_endpoint():
    with pytest.raises(SystemExit):
        parse(["run", "--no-flagd"])
    args = parse(["run", "--challenge", "swing-density", "--no-flagd", "--endpoint", "http://localhost:4317"])
    settings = settings_source(args).get()
    assert settings.enabled and settings.challenge == "swing-density"


@pytest.mark.uc("UC-106", flow="main", steps=[1], br=["BR-007"])
def test_the_command_line_overrides_the_flags():
    args = parse(["run", "--challenge", "span-rounds", "--mode", "both", "--intensity", "2", "--span-target", "500"])
    source = settings_source(args)
    source._read = FlagState(enabled=True)
    settings = source.get()
    assert (settings.challenge, settings.mode, settings.intensity, settings.span_target) == (
        "span-rounds",
        "both",
        2.0,
        500,
    )
    with pytest.raises(SystemExit):
        parse(["run", "--challenge", "bogus"])
