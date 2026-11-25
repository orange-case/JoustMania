"""TC-020 on the service tier (T2): host and stage change flags at runtime.

Moved from tests/integration/test_admin_flow.py (#1562). LOBBY composition on a
fresh flag plane: the host's write goes pad command -> menu -> flag service
(persisted admin value), the presenter's goes through the real stage scripts (``scripts/agent-backend.sh``,
``scripts/talk-stage.sh``) pointed at this stack's flag service, and flagd
serves both. The stage reset clears only the stage's overlays.

The test changes stack-wide state (a persisted admin value and the stage's
overlays), so it runs on ``fresh_stack``: a stack of its own.
"""

import json
import os
import subprocess

import pytest
from openfeature.evaluation_context import EvaluationContext

from tests.integration.flag_service import default as flag_service_default
from tests.integration.flag_service import variant_value
from tests.integration.helpers import (
    async_poll_until,
    flagd_probe_client,
    get_menu_client,
    pad_command,
)
from tests.service.harness import LOBBY
from tests.service.lobby_flow import start_lobby

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = LOBBY

GAME = "game"
REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
PERSIST_TIMEOUT_SECONDS = 5.0
SCRIPT_TIMEOUT_SECONDS = 60


def _default_variant(flag_set: str, flag_key: str) -> str:
    return flag_service_default().get(flag_set, flag_key)["effective_default"]


def _stage_baseline_backend() -> str:
    """The decision backend the stage reset re-applies (scripts/talk-baseline.json)."""
    with open(os.path.join(REPO_ROOT, "scripts", "talk-baseline.json")) as fh:
        return json.load(fh)["checked"]["decision.backend"]


def _persisted_admin_value(flag_set: str, flag_key: str) -> dict | None:
    return flag_service_default().get(flag_set, flag_key).get("admin_value")


@pytest.mark.asyncio
@pytest.mark.uc("UC-053", flow="A2", br=["BR-004"], tc="TC-020")
@pytest.mark.uc("UC-053", flow="A1", tc="TC-020")
@pytest.mark.uc("UC-015", flow="main", steps=[5], tc="TC-020")
@pytest.mark.uc("UC-044", flow="main", tc="TC-020")
async def test_host_and_stage_change_flags_at_runtime(fresh_stack, flag_service):
    """The host's admin setting and the presenter's stage overlay coexist; the
    stage reset clears only the stage's overlays (TC-020)."""
    stack = fresh_stack
    env = dict(os.environ, FLAG_SERVICE_URL=flag_service.base_url)

    def stage(*args: str) -> str:
        return subprocess.run(
            list(args),
            cwd=REPO_ROOT,
            env=env,
            check=True,
            capture_output=True,
            text=True,
            timeout=SCRIPT_TIMEOUT_SECONDS,
        ).stdout

    menu_client, menu_channel = await get_menu_client(stack)
    try:
        # Precondition: the menu runs with connected mock controllers (menu
        # restarted and mode selected before they are added; see start_lobby).
        await start_lobby(stack, count=4)
        baseline = _default_variant(GAME, "sensitivity")

        # Step 1: the host steps sensitivity with the pad (a persisted admin value).
        reply = await pad_command(menu_client, "step_setting", option="sensitivity", direction="up", steps=1)
        assert reply.success, f"sensitivity step refused: {reply.error}"
        assert await async_poll_until(
            lambda: _default_variant(GAME, "sensitivity") != baseline,
            timeout=PERSIST_TIMEOUT_SECONDS,
        )
        host_variant = _default_variant(GAME, "sensitivity")
        assert host_variant != baseline
        assert (_persisted_admin_value(GAME, "sensitivity") or {}).get("variant") == host_variant

        # Step 2: the presenter flips the decision backend with the stage script.
        stage("scripts/agent-backend.sh", "jev")
        view = flag_service.get("agent", "decision.backend")
        assert view["effective_default"] == "jev"
        # agent-backend.sh persists the backend (C-018, #1682): it is an admin value.
        assert (view.get("admin_value") or {}).get("writer") == "stage", view

        # Step 3: flagd serves both changes.
        with flagd_probe_client(stack, "agent") as agent:
            assert await async_poll_until(
                lambda: agent.get_string_value("decision.backend", "", EvaluationContext()) == "jev"
            ), "flagd did not serve decision.backend=jev"
        with flagd_probe_client(stack, GAME) as game:
            expected = variant_value(GAME, "sensitivity", host_variant)
            assert await async_poll_until(
                lambda: game.get_integer_value("sensitivity", -1, EvaluationContext()) == expected
            ), "flagd did not serve the host's sensitivity"

        # Step 4: the stage reset clears the stage's overlays and re-applies the
        # stage baseline; the host's setting is untouched.
        stage("scripts/talk-stage.sh", "reset")
        stage_backend = _stage_baseline_backend()
        backend = flag_service.get("agent", "decision.backend")
        assert backend["effective_default"] == stage_backend, backend
        assert _default_variant(GAME, "sensitivity") == host_variant
    finally:
        flag_service_default().request("DELETE", "/agent?writer=stage")
        flag_service_default().request("DELETE", "/agent/decision.backend?persist=true")
        await menu_channel.close()
