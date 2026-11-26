"""FakeAudio: an in-process AudioService that records every call.

The real audio service needs ALSA and assets; the service tier replaces it with
this fake on an ephemeral port (``AUDIO_HOST``/``AUDIO_PORT`` of the callers). A
test reads ``calls`` (or ``calls_of("PlaySound")``) to assert what the
coordinator or menu asked for. Every RPC answers ``success=True``, except a
``ChangeTempo`` for a track that is not playing, as the real service does: each
``PlayMusic`` plays a new ``track-<n>``. It serves ``grpc.health.v1`` like the
real service, and ``restart()`` plays a container restart on the same port: the
new instance plays no track.
"""

from __future__ import annotations

import threading
from concurrent import futures
from dataclasses import dataclass, field

import grpc
from google.protobuf.json_format import MessageToDict
from grpc_health.v1 import health, health_pb2, health_pb2_grpc

from proto import audio_pb2, audio_pb2_grpc

HEALTH_SERVICE = "audio.AudioService"


@dataclass
class AudioCall:
    method: str
    request: dict = field(default_factory=dict)


class _Servicer(audio_pb2_grpc.AudioServiceServicer):
    def __init__(self, record):
        self._record = record
        self._lock = threading.Lock()
        self._next_track = 1
        self.track_id: str | None = None

    def PlaySound(self, request, context):
        self._record("PlaySound", request)
        return audio_pb2.PlaySoundResponse(success=True)

    def PlayMusic(self, request, context):
        self._record("PlayMusic", request)
        with self._lock:
            self.track_id = f"track-{self._next_track}"
            self._next_track += 1
            return audio_pb2.PlayMusicResponse(track_id=self.track_id, success=True)

    def StopMusic(self, request, context):
        self._record("StopMusic", request)
        with self._lock:
            self.track_id = None
        return audio_pb2.StopMusicResponse(success=True)

    def ChangeTempo(self, request, context):
        self._record("ChangeTempo", request)
        with self._lock:
            playing = not request.track_id or request.track_id == self.track_id
        return audio_pb2.ChangeTempoResponse(success=playing, error="" if playing else "Failed to change tempo")

    def SetVolume(self, request, context):
        self._record("SetVolume", request)
        return audio_pb2.SetVolumeResponse(success=True)


class FakeAudio:
    def __init__(self):
        self._lock = threading.Lock()
        self._calls: list[AudioCall] = []
        self._server: grpc.Server | None = None
        self._servicer: _Servicer | None = None
        self._instance = 0
        self.port: int | None = None

    def _record(self, method: str, request) -> None:
        with self._lock:
            self._calls.append(AudioCall(method, MessageToDict(request, preserving_proto_field_name=True)))

    @property
    def calls(self) -> list[AudioCall]:
        with self._lock:
            return list(self._calls)

    def calls_of(self, method: str) -> list[AudioCall]:
        return [c for c in self.calls if c.method == method]

    def clear(self) -> None:
        with self._lock:
            self._calls.clear()

    @property
    def is_up(self) -> bool:
        return self._server is not None

    @property
    def track_id(self) -> str | None:
        """The track the current instance plays (None after a restart until the next PlayMusic)."""
        return self._servicer.track_id if self._servicer is not None else None

    def start(self) -> int:
        self._instance += 1
        self._servicer = _Servicer(self._record)
        # Track ids stay unique across restarts, as the real service's do.
        self._servicer._next_track = 1 + 1000 * (self._instance - 1)
        self._server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
        audio_pb2_grpc.add_AudioServiceServicer_to_server(self._servicer, self._server)
        health_servicer = health.HealthServicer()
        health_pb2_grpc.add_HealthServicer_to_server(health_servicer, self._server)
        health_servicer.set(HEALTH_SERVICE, health_pb2.HealthCheckResponse.SERVING)
        health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)
        address = f"127.0.0.1:{self.port}" if self.port is not None else "127.0.0.1:0"
        self.port = self._server.add_insecure_port(address)
        self._server.start()
        return self.port

    def stop(self) -> None:
        if self._server is not None:
            self._server.stop(grace=1).wait()
            self._server = None

    def go_down(self) -> None:
        """The container stops: open connections drop, new ones are refused."""
        if self._server is not None:
            self._server.stop(grace=None).wait()
            self._server = None

    def come_back(self) -> int:
        """The restarted container serves on the same port, playing no track."""
        return self.start()
