"""
gRPC Client Manager - Manages connections to dependent services.

Centralizes gRPC client lifecycle:
- Environment-based configuration
- Async channel creation
- Graceful cleanup

Usage:
    from services.game_coordinator.grpc_clients import GrpcClientManager

    # Create manager
    clients = GrpcClientManager()

    # Initialize connections (call in async context)
    await clients.connect()

    # Access clients
    clients.controller_manager  # ControllerManagerServiceStub
    clients.audio              # AudioServiceStub

    # Cleanup
    await clients.close()
"""

import logging
import os
from collections.abc import Mapping
from typing import Any

logger = logging.getLogger(__name__)

# Default deadline per audio RPC (UC-031 BR-009). PlayMusic is long because the
# audio service loads the track before it answers.
AUDIO_RPC_TIMEOUT_SECONDS = 1.0
AUDIO_DEADLINES: Mapping[str, float] = {
    "PlaySound": AUDIO_RPC_TIMEOUT_SECONDS,
    "SetVolume": AUDIO_RPC_TIMEOUT_SECONDS,
    "StopMusic": AUDIO_RPC_TIMEOUT_SECONDS,
    "ChangeTempo": 0.5,
    "PlayMusic": 5.0,
    "StopSound": 2.0,
}
AUDIO_HEALTH_DEADLINES: Mapping[str, float] = {"Check": 0.5}


class DeadlineStub:
    """Forwards to a gRPC stub, adding a per-method default ``timeout`` when the caller sets none.

    A missed deadline ends the call with DEADLINE_EXCEEDED, which the channel's
    metrics interceptor counts in grpc_client_errors_total; a caller-side
    asyncio cancel would not be counted.
    """

    def __init__(self, stub: Any, deadlines: Mapping[str, float]):
        self._stub = stub
        self._deadlines = deadlines

    def __getattr__(self, name: str) -> Any:
        method = getattr(self._stub, name)
        deadline = self._deadlines.get(name)
        if deadline is None or not callable(method):
            return method

        def call(request, **kwargs):
            kwargs.setdefault("timeout", deadline)
            return method(request, **kwargs)

        return call


AUDIO_HEALTH_SERVICE = "audio.AudioService"
PROBE_OK = "ok"
PROBE_DOWN = "down"
PROBE_RECONNECTED = "reconnected"


class AudioProbe:
    """Tells the music loop whether the audio service is up and whether it reconnected (UC-031 A7).

    While the channel stays READY the probe sends nothing (no RPC, no span). Once
    the connection is not READY (the service restarted or is unreachable) it sends
    a health check, which also makes the channel reconnect; the first ``SERVING``
    answer after that returns ``PROBE_RECONNECTED``: the service may have lost
    the game's track.
    """

    def __init__(self, channel: Any, health_stub: Any):
        self._channel = channel
        self._health = health_stub
        self._lost = False

    async def check(self) -> str:
        import grpc
        from grpc_health.v1 import health_pb2

        if self._channel.get_state(try_to_connect=False) == grpc.ChannelConnectivity.READY and not self._lost:
            return PROBE_OK
        self._lost = True
        try:
            response = await self._health.Check(health_pb2.HealthCheckRequest(service=AUDIO_HEALTH_SERVICE))
        except Exception:
            return PROBE_DOWN
        if response.status != health_pb2.HealthCheckResponse.SERVING:
            return PROBE_DOWN
        self._lost = False
        return PROBE_RECONNECTED


class GrpcClientManager:
    """
    Manages gRPC client connections to dependent services.

    Handles:
    - ControllerManager service (controller state, effects)
    - Audio service (sound playback)
    """

    def __init__(self):
        """Initialize client manager with environment configuration."""
        # Service addresses from environment
        self._controller_manager_host = os.getenv("CONTROLLER_MANAGER_HOST", "controller-manager")
        self._controller_manager_port = os.getenv("CONTROLLER_MANAGER_PORT", "50052")
        self._audio_host = os.getenv("AUDIO_HOST", "audio")
        self._audio_port = os.getenv("AUDIO_PORT", "50056")

        # Channels and stubs (initialized on connect)
        self._controller_manager_channel = None
        self._controller_manager_stub = None
        self._audio_channel = None
        self._audio_stub = None
        self._audio_health_stub = None

    @property
    def controller_manager(self):
        """Get ControllerManager service stub."""
        return self._controller_manager_stub

    @property
    def audio(self):
        """Audio service stub; every call has a default deadline (``AUDIO_DEADLINES``)."""
        return self._audio_stub

    @property
    def audio_health(self) -> AudioProbe | None:
        """Probe of the audio channel; its health check has a 0.5 s default deadline."""
        return self._audio_health_stub

    @property
    def is_connected(self) -> bool:
        """Check if essential clients are connected."""
        return self._controller_manager_stub is not None

    async def connect(self):
        """
        Initialize async gRPC client connections.

        Creates channels and stubs for all dependent services.
        On failure, clients are set to None for graceful degradation.
        """
        from grpc_health.v1 import health_pb2_grpc

        from lib.grpc_utils import create_channel
        from proto import audio_pb2_grpc, controller_manager_pb2_grpc

        try:
            # ControllerManager client
            cm_address = f"{self._controller_manager_host}:{self._controller_manager_port}"
            self._controller_manager_channel = create_channel(cm_address)
            self._controller_manager_stub = controller_manager_pb2_grpc.ControllerManagerServiceStub(
                self._controller_manager_channel
            )
            logger.info(f"Connected to ControllerManager at {cm_address}")

            # Audio client
            audio_address = f"{self._audio_host}:{self._audio_port}"
            self._audio_channel = create_channel(audio_address)
            self._audio_stub = DeadlineStub(audio_pb2_grpc.AudioServiceStub(self._audio_channel), AUDIO_DEADLINES)
            self._audio_health_stub = AudioProbe(
                self._audio_channel,
                DeadlineStub(health_pb2_grpc.HealthStub(self._audio_channel), AUDIO_HEALTH_DEADLINES),
            )
            logger.info(f"Connected to Audio at {audio_address}")

        except Exception as e:
            logger.error(f"Failed to initialize gRPC clients: {e}")
            # Set to None for graceful degradation
            self._controller_manager_stub = None
            self._audio_stub = None
            self._audio_health_stub = None

    async def close(self):
        """
        Close all gRPC channels gracefully with timeout.

        Safe to call multiple times.
        Uses timeout to prevent hanging if service is unresponsive.
        """
        import asyncio

        close_timeout = 0.5  # seconds — channels on same Docker network close quickly

        if self._controller_manager_channel:
            try:
                await asyncio.wait_for(self._controller_manager_channel.close(), timeout=close_timeout)
            except TimeoutError:
                logger.warning("Timeout closing controller_manager channel")
            except Exception as e:
                logger.warning(f"Error closing controller_manager channel: {e}")
            self._controller_manager_channel = None
            self._controller_manager_stub = None

        if self._audio_channel:
            try:
                await asyncio.wait_for(self._audio_channel.close(), timeout=close_timeout)
            except TimeoutError:
                logger.warning("Timeout closing audio channel")
            except Exception as e:
                logger.warning(f"Error closing audio channel: {e}")
            self._audio_channel = None
            self._audio_stub = None
            self._audio_health_stub = None

        logger.info("Closed all gRPC channels")
