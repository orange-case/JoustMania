"""The flag service's Flagsmith link on the service tier (T2): start and restart (UC-054 A3, #2591).

The flag service is built from HEAD and linked to an in-process fake hosted Flagsmith
(``tests/service/harness/fakes/flagsmith.py``, ``Composition(fake_flagsmith=True)``):
FLAGSMITH_LINK=on, the fake's URLs and dummy keys. Every other composition keeps the link
off, as every compose file does.

- start: the link resolves the environment, creates every registry flag that is not a
  feature yet (FR-214, BR-002, never an ``initial_value``) and reports the restart rule;
- offline start: an unreachable Flagsmith never stops the flag service: ``/healthz`` is
  200, writes are accepted, ``/state`` shows the link offline with the reason (BR-007);
- degraded rule: values the audit log has no author for are a person's and the rule is
  ``degraded`` (BR-006, architecture note §5.1);
- the restart journeys that need the in and out sides (#2589, #2590) are strict xfails.

Unit detail (ping first, audit parsing, kill switches, game identities, pacing) is in
``services/flag-service/flagsmith/start_test.go``.
"""

import time

import pytest

from tests.integration.flag_service import FlagService, FlagServiceError
from tests.service.harness import Composition

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = Composition(("controller-manager", "game-coordinator"), fake_flagsmith=True)

# The first start creates ~130 features and their tags at FLAGSMITH_PUSH_RATE (5/s).
FIRST_START_SECONDS = 90.0
RESTART_SECONDS = 30.0


def _link(flag_service: FlagService) -> dict:
    return flag_service.state().get("flagsmith", {})


def _wait_link(flag_service: FlagService, timeout: float, **want) -> dict:
    deadline = time.monotonic() + timeout
    link: dict = {}
    while time.monotonic() < deadline:
        link = _link(flag_service)
        if all(link.get(k) == v for k, v in want.items()):
            return link
        time.sleep(0.25)
    raise AssertionError(f"flagsmith link {link}, want {want}")


def _registry_keys() -> set[str]:
    import yaml

    from tests.service.harness.flag_plane import FLAG_SERVICE_SRC

    doc = yaml.safe_load((FLAG_SERVICE_SRC / "registry.yaml").read_text())
    rows = doc["flags"] if isinstance(doc, dict) and "flags" in doc else doc
    return {row["key"] for row in rows}


@pytest.mark.uc("UC-054", flow="A3", br=["BR-002"])
def test_start_creates_missing_features(stack, flag_service):
    fake = stack.fake_flagsmith
    link = _wait_link(flag_service, FIRST_START_SECONDS, link="online")
    assert link["environment"] == "service-tier"
    assert link["restart_rule"] == "audit_log"  # an empty document has nothing to classify
    created = fake.created_features()
    assert len(created) == len(set(created)), "a feature was created twice"
    assert set(created) == _registry_keys(), "every registry flag (and nothing else) becomes a feature"
    for req in fake.requests("POST", "/features/"):
        body = req["body"]
        assert "initial_value" not in body, body
        assert body["default_enabled"] is False and body["type"] == "STANDARD"
        assert body.get("tags"), f"{body['name']} has no flag-set tag"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-007"])
def test_offline_start_serves_baseline(stack, flag_service):
    fake = stack.fake_flagsmith
    _wait_link(flag_service, FIRST_START_SECONDS, link="online")
    fake.down = True
    try:
        stack.flag_plane.restart_flag_service()
        link = _wait_link(flag_service, RESTART_SECONDS, link="offline")
        assert link["offline_reason"] == "http_503"
        status, body = flag_service.request("GET", "/healthz")
        assert status == 200, body
        # flagd reconnects on its own after the restart (503 no_sync_stream until then).
        deadline = time.monotonic() + RESTART_SECONDS
        while True:
            try:
                flag_service.put("agent", "decision.capture_content", variant="off")
                break
            except FlagServiceError:
                if time.monotonic() > deadline:
                    raise
                time.sleep(0.25)
        assert flag_service.get("agent", "decision.capture_content")["stack_overlay"]["variant"] == "off"
    finally:
        fake.down = False
    _wait_link(flag_service, RESTART_SECONDS + 60, link="online")


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"])
def test_degraded_rule_in_state(stack, flag_service):
    fake = stack.fake_flagsmith
    _wait_link(flag_service, FIRST_START_SECONDS, link="online")
    fake.set_document(
        [{"feature": {"id": 1, "name": "experiment_tick_seconds", "type": "STANDARD"},
          "enabled": True, "feature_segment": None, "feature_state_value": 10}]
    )  # fmt: skip
    fake.audit_rows = []  # the free plan: 200 with no rows
    try:
        stack.flag_plane.restart_flag_service()
        link = _wait_link(flag_service, RESTART_SECONDS, link="online")
        assert link["restart_rule"] == "degraded"
        assert "restart_rule_degraded" in stack.flag_plane.flag_service.log_tail()
    finally:
        fake.set_document([])


@pytest.mark.xfail(strict=True, reason="needs the out and in sides (#2589, #2590)")
@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"])
def test_restart_clears_own_overlay(stack, flag_service):
    fake = stack.fake_flagsmith
    _wait_link(flag_service, FIRST_START_SECONDS, link="online")
    # An in-memory overlay is pushed (out side), then the flag service restarts.
    flag_service.put("agent", "decision.capture_content", variant="off")
    deadline = time.monotonic() + RESTART_SECONDS
    while time.monotonic() < deadline and not fake.requests(path_contains="__future__"):
        time.sleep(0.25)
    assert fake.requests(path_contains="__future__"), "the overlay was not pushed"
    fake.audit_rows = [{"log": "Flag state updated for feature: decision.capture_content", "author": None}]
    stack.flag_plane.restart_flag_service()
    _wait_link(flag_service, RESTART_SECONDS, link="online")
    # The API-key value is cleared in Flagsmith, not applied locally.
    assert flag_service.get("agent", "decision.capture_content").get("stack_overlay") is None


@pytest.mark.xfail(strict=True, reason="needs the in side (#2590)")
@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"])
def test_restart_keeps_persons_value(stack, flag_service):
    fake = stack.fake_flagsmith
    _wait_link(flag_service, FIRST_START_SECONDS, link="online")
    fake.set_document(
        [{"feature": {"id": 1, "name": "decision.capture_content", "type": "STANDARD"},
          "enabled": True, "feature_segment": None, "feature_state_value": "off"}]
    )  # fmt: skip
    fake.audit_rows = [{"log": "Flag state updated for feature: decision.capture_content", "author": {"id": 1}}]
    try:
        stack.flag_plane.restart_flag_service()
        _wait_link(flag_service, RESTART_SECONDS, link="online", restart_rule="audit_log")
        overlay = flag_service.get("agent", "decision.capture_content").get("stack_overlay") or {}
        assert overlay.get("variant") == "off" and overlay.get("writer") == "flagsmith"
    finally:
        fake.set_document([])
