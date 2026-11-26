"""UC-031 A7 / BR-009: the audio service is slow or down during a game.

- Effects never hold the game: ``_play_sound`` returns while the audio service
  hangs, so a kill still completes on the frame path; the game's end waits at
  most ``AUDIO_DRAIN_TIMEOUT_SECONDS`` for open sends.
- Every coordinator audio call has a deadline (``grpc_clients.DeadlineStub``): a
  hung RPC ends with ``DEADLINE_EXCEEDED``, counted in ``grpc_client_errors_total``.
- The music comes back: a refused glide, or the audio service answering again
  after an outage without the game's track, starts the music again at the game's
  volume and the tempo of the intensity now; a track that survived is kept.
"""

import asyncio
import time
from unittest.mock import MagicMock, patch

import grpc
import pytest
from grpc_health.v1 import health_pb2

from lib.grpc_utils import create_channel
from lib.types import Sound
from proto import audio_pb2, audio_pb2_grpc
from services.game_coordinator import game_clock
from services.game_coordinator.games import base as base_module
from services.game_coordinator.games.base import IntensityRamp, Player, tempo_from_intensity
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.grpc_clients import (
    AUDIO_DEADLINES,
    PROBE_DOWN,
    PROBE_OK,
    PROBE_RECONNECTED,
    AudioProbe,
    DeadlineStub,
)
from services.game_coordinator.tests.conftest import EventCollector, MockControllerManagerService
from services.game_coordinator.tests.test_death_detection_uc030 import LETHAL, STILL, _frame, _game

T0 = 10_000.0
GAME_VOLUME = 0.6


def _flag_default(*args, **_kwargs):
    """Stands in for ``read_float_flag``: every flag reads its default."""
    return args[2]


class FakeClock:
    def __init__(self, now: float = T0):
        self.now = now

    def __call__(self) -> float:
        return self.now


@pytest.fixture
def clock():
    fake = FakeClock()
    with patch.object(game_clock, "now", fake):
        yield fake


class HungAudio:
    """An audio service that never answers PlaySound."""

    def __init__(self):
        self.sent: list[str] = []
        self.release = asyncio.Event()

    async def PlaySound(self, request):  # noqa: N802 - gRPC stub name
        self.sent.append(request.file_path)
        await self.release.wait()
        return audio_pb2.PlaySoundResponse(success=True)


class RestartingAudio:
    """An audio service that can lose its track (a restart) and refuse glides for the old one."""

    def __init__(self):
        self.calls: list[tuple] = []
        self.track = "track-1"
        self.down = False
        self._next = 1

    def _check(self, name):
        if self.down:
            self.calls.append((name, "unavailable"))
            raise ConnectionError("audio service unavailable")

    async def SetVolume(self, request):  # noqa: N802 - gRPC stub name
        self._check("SetVolume")
        self.calls.append(("SetVolume", round(request.volume, 3)))
        return audio_pb2.SetVolumeResponse(success=True)

    async def PlayMusic(self, request):  # noqa: N802 - gRPC stub name
        self._check("PlayMusic")
        self.track = f"track-{self._next}"
        self._next += 1
        self.calls.append(("PlayMusic", round(request.tempo, 3)))
        return audio_pb2.PlayMusicResponse(track_id=self.track, success=True)

    async def ChangeTempo(self, request):  # noqa: N802 - gRPC stub name
        self._check("ChangeTempo")
        accepted = request.track_id == self.track
        self.calls.append(("ChangeTempo", request.track_id, accepted))
        return audio_pb2.ChangeTempoResponse(success=accepted, error="" if accepted else "Failed to change tempo")

    async def StopMusic(self, request):  # noqa: N802 - gRPC stub name
        return audio_pb2.StopMusicResponse(success=True)

    def restart(self):
        """The service came back without a track."""
        self.down = False
        self.track = None

    def of(self, name):
        return [c for c in self.calls if c[0] == name]


class ScriptedProbe:
    """Stands in for ``AudioProbe``: answers from a list, then ``PROBE_OK``."""

    def __init__(self, *answers):
        self.answers = list(answers)

    async def check(self):
        return self.answers.pop(0) if self.answers else PROBE_OK


async def started_game(audio, probe=None) -> FFAGame:
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=2),
        event_publisher=EventCollector().publish,
        audio_client=audio,
        game_id="test_uc031_a7",
    )
    game.audio_health_client = probe
    game.gameplay_span = None
    for serial in ("p1", "p2"):
        game.players[serial] = Player(serial=serial, alive=True, color=(255, 0, 0))
    with patch.object(base_module, "read_float_flag", _flag_default):
        await game._start_game_music(volume=GAME_VOLUME)
    game.change_time = T0 + 1_000.0  # no scheduled change during the test
    return game


async def tick(game, clock, seconds: float):
    """Run music-loop ticks (100 ms) for ``seconds``."""
    for _ in range(round(seconds / 0.1)):
        clock.now += 0.1
        await game._check_music_speed()


# --------------------------------------------------------------------------- #
# Step 1: effects never wait for the audio service
# --------------------------------------------------------------------------- #


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[1], br=["BR-009"])
async def test_play_sound_returns_while_the_audio_service_hangs():
    audio = HungAudio()
    game, _ = _game(2, audio=audio)

    await asyncio.wait_for(game._play_sound(Sound.SFX_BEEP_LOUD), timeout=0.5)

    assert audio.sent == [Sound.SFX_BEEP_LOUD.value]  # handed over before the caller went on
    assert len(game._audio_tasks) == 1
    audio.release.set()
    await game.drain_audio()
    assert not game._audio_tasks


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[1], br=["BR-009"])
@pytest.mark.uc("UC-030", flow="main", steps=[3, 4])
async def test_a_kill_completes_while_the_explosion_hangs():
    """The kill path sends the explosion and goes on: the death is recorded without an audio answer."""
    audio = HungAudio()
    game, _ = _game(3, audio=audio)

    async def kill():
        for _ in range(10):
            if await game._process_gameplay_update(_frame(("p0", LETHAL), ("p1", STILL), ("p2", STILL))):
                return

    await asyncio.wait_for(kill(), timeout=1.0)

    assert game.players["p0"].alive is False
    assert audio.sent == [Sound.SFX_EXPLOSION.value]
    await game.drain_audio(bound_seconds=0.05)  # the hung send is cancelled at the bound
    assert not game._audio_tasks


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[1], br=["BR-009"])
async def test_drain_audio_waits_at_most_its_bound():
    audio = HungAudio()
    game, _ = _game(2, audio=audio)
    await game._play_sound(Sound.VOX_CONGRATULATIONS)

    started = time.monotonic()
    await game.drain_audio(bound_seconds=0.1)

    assert time.monotonic() - started < 0.5
    assert not game._audio_tasks


# --------------------------------------------------------------------------- #
# BR-009: deadlines on the coordinator's audio client
# --------------------------------------------------------------------------- #


class _HangingAudioServicer(audio_pb2_grpc.AudioServiceServicer):
    async def PlaySound(self, request, context):  # noqa: N802 - gRPC servicer name
        await asyncio.sleep(5)
        return audio_pb2.PlaySoundResponse(success=True)


@pytest.mark.uc("UC-031", flow="A7", steps=[1], br=["BR-009"])
def test_deadline_stub_adds_the_method_default_and_keeps_a_caller_timeout():
    method = MagicMock(return_value="call")
    stub = DeadlineStub(MagicMock(PlaySound=method, Other=method), {"PlaySound": 1.0})

    assert stub.PlaySound("req") == "call"
    method.assert_called_with("req", timeout=1.0)
    stub.PlaySound("req", timeout=3.0)
    method.assert_called_with("req", timeout=3.0)
    stub.Other("req")
    method.assert_called_with("req")  # no default for a method without one
    assert AUDIO_DEADLINES == {
        "PlaySound": 1.0,
        "SetVolume": 1.0,
        "StopMusic": 1.0,
        "ChangeTempo": 0.5,
        "PlayMusic": 5.0,
        "StopSound": 2.0,
    }


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[1], br=["BR-009"], cond=["Post-F-1"])
async def test_a_hung_audio_rpc_ends_with_deadline_exceeded_and_is_counted():
    server = grpc.aio.server()
    audio_pb2_grpc.add_AudioServiceServicer_to_server(_HangingAudioServicer(), server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    channel = create_channel(f"127.0.0.1:{port}", enable_tracing=False)
    try:
        stub = DeadlineStub(audio_pb2_grpc.AudioServiceStub(channel), {"PlaySound": 0.2})
        with patch("lib.grpc_metrics.grpc_client_errors_total") as errors:
            started = time.monotonic()
            with pytest.raises(grpc.aio.AioRpcError) as raised:
                await stub.PlaySound(audio_pb2.PlaySoundRequest(file_path="x"))
        assert raised.value.code() == grpc.StatusCode.DEADLINE_EXCEEDED
        assert time.monotonic() - started < 2.0
        errors.labels.assert_called_once_with(service="AudioService", method="PlaySound", code="DEADLINE_EXCEEDED")
    finally:
        await channel.close()
        await server.stop(None)


# --------------------------------------------------------------------------- #
# Step 2: the music comes back
# --------------------------------------------------------------------------- #


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-009"])
async def test_a_refused_glide_starts_the_music_again_at_the_tempo_now(clock):
    """A restart quicker than the probe: the next glide is refused, so the music starts again."""
    audio = RestartingAudio()
    game = await started_game(audio)
    audio.restart()
    game._intensity = IntensityRamp(start=0.0, target=1.0, start_time=clock.now, duration=3.0)

    clock.now += 1.5
    await game._push_audio(1.0, 1.5)  # refused: the restarted service plays no track
    await tick(game, clock, 1.0)

    tempo_now = tempo_from_intensity(game._intensity.value_at(T0 + 1.5))
    assert audio.of("PlayMusic")[-1][1] == pytest.approx(tempo_now, abs=0.02)
    assert audio.of("SetVolume")[-1] == ("SetVolume", GAME_VOLUME)
    assert game.music_track_id == "track-2"
    # The rest of the running ramp went to the new track.
    assert ("ChangeTempo", "track-2", True) in audio.calls


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-009"])
async def test_refusals_outside_an_outage_restart_the_music_at_most_three_times(clock):
    audio = RestartingAudio()
    game = await started_game(audio)

    for _ in range(base_module.MUSIC_REFUSAL_RESTARTS_MAX + 2):
        audio.track = "someone-else"  # another player of the shared track
        await game._push_audio(1.0, 1.0)
        await tick(game, clock, 1.1)

    assert len(audio.of("PlayMusic")) == 1 + base_module.MUSIC_REFUSAL_RESTARTS_MAX
    assert game.music_track_id is None


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-009"])
@pytest.mark.uc("UC-031", flow="A6", steps=[3])
async def test_the_music_starts_again_when_the_audio_service_is_back_after_a_restart(clock):
    """Probe down, then reconnected: the old track is gone, so the music starts again; the game goes on."""
    audio = RestartingAudio()
    probe = ScriptedProbe(PROBE_DOWN, PROBE_DOWN, PROBE_RECONNECTED)
    game = await started_game(audio, probe)
    game.music_speed = 1.3  # the intensity reached the fast table before the restart
    audio.down = True

    await tick(game, clock, 2.1)  # two probes find the service down
    assert game._audio_down
    assert len(audio.of("PlayMusic")) == 1
    audio.restart()
    await tick(game, clock, 1.0)  # the reconnect

    assert ("ChangeTempo", "track-1", False) in audio.calls  # the check found the track gone
    assert audio.of("PlayMusic")[-1] == ("PlayMusic", 1.3)
    assert audio.of("SetVolume")[-1] == ("SetVolume", GAME_VOLUME)
    assert game.music_track_id == "track-2"
    assert not game._audio_down


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-009"])
async def test_a_track_that_survived_a_slow_spell_is_kept(clock):
    """A failed glide, then the service answers with the track still playing: no music restart."""
    audio = RestartingAudio()
    game = await started_game(audio, ScriptedProbe())
    audio.down = True
    await game._push_audio(1.0, 1.0)  # fails: the service is slow
    assert game._audio_down
    audio.down = False

    await tick(game, clock, 1.1)

    assert ("ChangeTempo", "track-1", True) in audio.calls
    assert len(audio.of("PlayMusic")) == 1
    assert game.music_track_id == "track-1"


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-009"])
async def test_a_failed_music_start_is_sent_again_once_the_service_answers(clock):
    audio = RestartingAudio()
    audio.down = True
    game = await started_game(audio, ScriptedProbe(PROBE_DOWN, PROBE_RECONNECTED))
    assert game.music_track_id is None
    audio.down = False

    await tick(game, clock, 2.1)

    assert audio.of("PlayMusic") == [("PlayMusic", 1.0)]
    assert game.music_track_id == "track-1"


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-006", "BR-009"])
async def test_a_shadow_game_never_probes_or_restarts_music(clock):
    audio = RestartingAudio()
    probe = ScriptedProbe(PROBE_RECONNECTED)
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=2),
        event_publisher=EventCollector().publish,
        audio_client=audio,
        game_id="test_uc031_a7_shadow",
    )
    game.game_kind = base_module.GAME_KIND_SHADOW
    game.audio_health_client = probe
    with patch.object(base_module, "read_float_flag", _flag_default):
        await game._start_game_music()
    game.change_time = T0 + 1_000.0

    await tick(game, clock, 2.0)

    assert audio.calls == []
    assert probe.answers == [PROBE_RECONNECTED]


# --------------------------------------------------------------------------- #
# The probe: no RPC while the connection is ready
# --------------------------------------------------------------------------- #


class FakeChannel:
    def __init__(self, state):
        self.state = state

    def get_state(self, try_to_connect=False):
        return self.state


class FakeHealth:
    def __init__(self):
        self.status = health_pb2.HealthCheckResponse.SERVING
        self.fail = False
        self.checks = 0

    async def Check(self, request):  # noqa: N802 - gRPC stub name
        self.checks += 1
        assert request.service == "audio.AudioService"
        if self.fail:
            raise ConnectionError("down")
        return health_pb2.HealthCheckResponse(status=self.status)


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="A7", steps=[2], br=["BR-009"])
async def test_audio_probe_sends_a_check_only_while_the_connection_is_not_ready():
    channel = FakeChannel(grpc.ChannelConnectivity.READY)
    health = FakeHealth()
    probe = AudioProbe(channel, health)

    assert await probe.check() == PROBE_OK
    assert health.checks == 0

    channel.state = grpc.ChannelConnectivity.IDLE  # the connection dropped: a restart
    health.fail = True
    assert await probe.check() == PROBE_DOWN
    channel.state = grpc.ChannelConnectivity.READY
    assert await probe.check() == PROBE_DOWN  # still lost until a check answers
    health.fail = False
    health.status = health_pb2.HealthCheckResponse.NOT_SERVING
    assert await probe.check() == PROBE_DOWN
    health.status = health_pb2.HealthCheckResponse.SERVING
    assert await probe.check() == PROBE_RECONNECTED
    assert await probe.check() == PROBE_OK
    assert health.checks == 4
