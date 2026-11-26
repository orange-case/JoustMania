"""UC-060 A8 on the service tier (T2): a wedged Python service exits with code 70 and comes back.

The real game coordinator from source runs with a 3 s watchdog timeout and the
SIGUSR2 stall hook on. The test blocks its event loop with SIGUSR2; the watchdog
thread must exit the process with code 70 and log why. Compose's
`restart: unless-stopped` is Docker's part; the test plays it by starting the
service again on the same port and checks it is SERVING.
"""

import dataclasses
import os
import signal

import pytest

from tests.service.harness import GAME
from tests.service.harness.processes import grpc_health_ready

pytestmark = [pytest.mark.tier("service"), pytest.mark.nfr("NFR-080")]

TIMEOUT_SECONDS = 3
# Timeout + check interval + the flush bound, with room for a slow CI runner.
EXIT_WITHIN_SECONDS = 20

SERVICE_COMPOSITION = dataclasses.replace(
    GAME,
    env={"game-coordinator": {"WATCHDOG_TIMEOUT_S": str(TIMEOUT_SECONDS), "WATCHDOG_STALL_ON_SIGUSR2": "1"}},
)


@pytest.mark.uc("UC-060", flow="A8", steps=[1, 2])
def test_wedged_coordinator_exits_70_and_serves_again_after_restart(fresh_stack):
    coordinator = fresh_stack.services["game-coordinator"]
    proc = coordinator.proc
    assert proc is not None and proc.poll() is None

    os.kill(proc.pid, signal.SIGUSR2)

    assert proc.wait(timeout=EXIT_WITHIN_SECONDS) == 70, coordinator.log_tail()
    log = coordinator.log_path.read_text(errors="replace")
    assert "watchdog: game-coordinator event loop stalled" in log
    assert "exiting with code 70" in log

    coordinator.start()  # what the restart policy does

    assert grpc_health_ready(coordinator.grpc_port)()
