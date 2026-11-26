"""Menu status reads for the operator pad (services.md §3.1 `GetMenuStatus`, UC-015 A5).

The pad reads a fresh status at every key press and release; it never caches one across
gestures. A failed read is None: the caller sends nothing and never queues (UC-015 BR-010),
except the K5 hold, whose stop needs no read (BR-002).
"""

from __future__ import annotations

import logging

from proto import menu_pb2

logger = logging.getLogger(__name__)

STATUS_TIMEOUT_SECONDS = 0.5


# UC-015 BR-004 / BR-019 ladder; an empty variant is a flag the menu could not resolve.
# `probe` and `shadow_experimental` are off the ladder, so they read as unknown (BR-013, fail closed).
AGENT_LEVELS = frozenset({"none", "ambient", "standard", "full"})

# `agent_enabled_state` of a resolved kill switch and `agent_interventions_enabled_state` of a resolved
# write gate; `unknown`, and the empty value of a menu older than #2101 / #2093, are not (services.md §3.1).
ENABLED_STATES = frozenset({"on", "off"})


AGENT_ON, AGENT_WATCH, AGENT_OFF, AGENT_UNKNOWN = "on", "watch", "off", "unknown"


def agent_state(status: menu_pb2.GetMenuStatusResponse | None) -> str:
    """UC-016 BR-011: `off` (`enabled` off), `watch` (the write gate off), `on`, or `unknown` by BR-001 line 3."""
    if status is None or status.agent_enabled_state not in ENABLED_STATES:
        return AGENT_UNKNOWN
    if status.agent_enabled_state == "off":
        return AGENT_OFF
    if status.agent_interventions_enabled_state not in ENABLED_STATES or status.agent_level not in AGENT_LEVELS:
        return AGENT_UNKNOWN
    return AGENT_WATCH if status.agent_interventions_enabled_state == "off" else AGENT_ON


def pad_commands(status) -> frozenset[str]:
    """services.md §3.1 `pad_commands`: what this menu understands (empty for an older menu)."""
    try:
        return frozenset(status.pad_commands) if status is not None else frozenset()
    except AttributeError:
        return frozenset()


class MenuStatusReader:
    def __init__(self, menu_stub, timeout: float = STATUS_TIMEOUT_SECONDS, log_failures: bool = True) -> None:
        self._stub = menu_stub
        self._timeout = timeout
        # The display's poll logs only when the connection state changes (UC-016 A2), not every read.
        self._log_failures = log_failures

    async def fetch(self) -> menu_pb2.GetMenuStatusResponse | None:
        try:
            return await self._stub.GetMenuStatus(menu_pb2.GetMenuStatusRequest(), timeout=self._timeout)
        except Exception as e:  # grpc.aio.AioRpcError, timeouts, a closed channel
            if self._log_failures:
                logger.warning(f"GetMenuStatus failed: {e}")
            else:
                logger.debug(f"GetMenuStatus failed: {e}")
            return None
