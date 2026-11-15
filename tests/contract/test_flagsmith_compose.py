"""The Flagsmith link is off by default in every stack and never runs in CI or the dry run (#2588).

services.md §7.3 (Config), architecture note F10: ``FLAGSMITH_LINK`` defaults to ``off``
in the base and lite stacks (a ``${FLAGSMITH_LINK:-off}`` passthrough, so the stage
stack opts in through ``.env.local``), and the ci and dry-run overlays pin it to a
literal ``off`` with empty secrets, whatever the shell exports.
"""

from __future__ import annotations

import env_inventory as inv
import pytest

# UC: UC-054 A1

FLAGSMITH_VARS = (
    "FLAGSMITH_LINK",
    "FLAGSMITH_API_URL",
    "FLAGSMITH_DOCUMENT_URL",
    "FLAGSMITH_SERVER_KEY",
    "FLAGSMITH_ADMIN_API_KEY",
    "FLAGSMITH_ENVIRONMENT_KEY",
    "FLAGSMITH_PROJECT_ID",
    "FLAGSMITH_POLL_SECONDS",
    "FLAGSMITH_REALTIME_URL",
    "FLAGSMITH_PUSH_RATE",
    "FLAGSMITH_HEALTH_WEBHOOK_URL",
    "FLAGSMITH_TIMEOUT_SECONDS",
)

PASSTHROUGH = {
    "base": (inv.COMPOSE_BASE, ()),
    "lite": (inv.COMPOSE_LITE, ()),
}
PINNED_OFF = {
    "ci": (inv.COMPOSE_BASE, (inv.COMPOSE_CI,)),
    "dry-run": (inv.COMPOSE_BASE, (inv.COMPOSE_DRY_RUN,)),
}


def _env(stack: tuple) -> dict[str, str | None]:
    base_file, overlays = stack
    env = inv.compose_environment("flag-service", overlays, base_file)
    assert env is not None, "flag-service is missing from the stack"
    return env


@pytest.mark.parametrize("name", PASSTHROUGH)
def test_link_defaults_off_and_passes_every_var_through(name):
    env = _env(PASSTHROUGH[name])
    assert env.get("FLAGSMITH_LINK") == "${FLAGSMITH_LINK:-off}", (
        f"{name}: FLAGSMITH_LINK must default to off and stay a host passthrough"
    )
    missing = [v for v in FLAGSMITH_VARS if v not in env]
    assert not missing, (
        f"{name}: the flag service reads {missing} but compose does not pass them"
    )


@pytest.mark.parametrize("name", PINNED_OFF)
def test_link_is_pinned_off_without_secrets(name):
    env = _env(PINNED_OFF[name])
    assert env.get("FLAGSMITH_LINK") == "off", f"{name}: FLAGSMITH_LINK must be off"
    for secret in (
        "FLAGSMITH_SERVER_KEY",
        "FLAGSMITH_ADMIN_API_KEY",
        "FLAGSMITH_HEALTH_WEBHOOK_URL",
    ):
        assert env.get(secret) == "", (
            f"{name}: {secret} must be empty (no developer key copied in)"
        )
