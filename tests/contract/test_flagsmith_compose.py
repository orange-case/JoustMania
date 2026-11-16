"""The Flagsmith link is off by default in every stack and never runs in CI or the dry run (#2588).

services.md §7.3 (Config), architecture note F10: ``FLAGSMITH_LINK`` defaults to ``off``
in the base and lite stacks (a ``${FLAGSMITH_LINK:-off}`` passthrough, so the stage
stack opts in through ``.env.local``), and the ci and dry-run overlays pin it to a
literal ``off`` with empty secrets, whatever the shell exports. The link only connects
out (UC-054 BR-008, C-037): the flag service gets no port for Flagsmith and the link
package opens no listener.
"""

from __future__ import annotations

import re

import env_inventory as inv
import pytest

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


@pytest.mark.uc("UC-054", cond=["Pre-2"])
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


@pytest.mark.uc("UC-054", cond=["Pre-2"])
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


# The flag service's own ports, both older than the link: the HTTP write/read API and
# the flagd sync stream (services.md §7).
FLAG_SERVICE_PORTS = {8070, 50064}
_LISTEN_RE = re.compile(
    r"\bnet\.Listen\w*\(|\bhttp\.ListenAndServe\w*\(|\bhttp\.Server\{|\bhttp\.Serve\("
)
LINK_PACKAGE = inv.REPO_ROOT / "services" / "flag-service" / "flagsmith"


@pytest.mark.uc("UC-054", br=["BR-008"])
def test_link_only_connects_out():
    """Flagsmith never connects to the stack (UC-054 BR-008, C-015, C-037): no compose file
    gives the flag service a port beyond its own API and flagd sync, a published port stays
    on loopback, and the link package opens no listener (the feature-health webhook is an
    outbound POST)."""
    files = sorted(inv.REPO_ROOT.glob("docker-compose*.yml"))
    assert files, "no compose files found"
    for path in files:
        svc = (inv._load_compose(path) or {}).get("flag-service") or {}
        expose = {int(p) for p in svc.get("expose") or []}
        assert expose <= FLAG_SERVICE_PORTS, (
            f"{path.name}: flag-service exposes {sorted(expose - FLAG_SERVICE_PORTS)}"
        )
        for port in svc.get("ports") or []:
            assert isinstance(port, str), f"{path.name}: long-form port {port!r}"
            assert port.startswith("127.0.0.1:"), (
                f"{path.name}: flag-service publishes {port} beyond loopback"
            )
            assert int(port.rsplit(":", 1)[-1]) in FLAG_SERVICE_PORTS, (
                f"{path.name}: flag-service publishes {port}"
            )
    sources = sorted(
        p for p in LINK_PACKAGE.glob("*.go") if not p.name.endswith("_test.go")
    )
    assert sources, f"no Go sources in {LINK_PACKAGE}"
    listeners = [
        p.name for p in sources if _LISTEN_RE.search(p.read_text(encoding="utf-8"))
    ]
    assert not listeners, f"the Flagsmith link opens a listener in {listeners}"
