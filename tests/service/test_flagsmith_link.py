"""TC-022: the flag service's Flagsmith link on start and restart (UC-054 A3, UC-055 A5, #2591, #2592).

The flag service is built from HEAD and linked to the stateful fake hosted Flagsmith
(``tests/service/harness/fakes/flagsmith.py``, ``Composition(fake_flagsmith=True)``):
FLAGSMITH_LINK=on, the fake's URLs and dummy keys. Every other composition keeps the link
off, as every compose file does. No preseed here: the first start creates the features
(FR-214). The fake keeps the link's own pushes as API-key values (audit rows without an
author) and a person's changes as a user's, so a restart reads the authors as it would
from Flagsmith.

- start: the link resolves the environment, creates every registry flag that is not a
  feature yet (FR-214, BR-002, never an ``initial_value``) and reports the restart rule;
- offline start: an unreachable Flagsmith never stops the flag service (BR-007);
- degraded rule: values the audit log has no author for are a person's (BR-006);
- restart: an API key's value is cleared in Flagsmith, a person's value comes back, a kill
  switch is never loosened (BR-005), game values wait for their game's registration, stale
  game identities go after the grace (FR-211), unsent local changes are lost (FR-212);
- start refused: identity overrides off in local evaluation keeps the link offline.

Unit detail (ping first, audit parsing, kill switches, game identities, pacing) is in
``services/flag-service/flagsmith/start_test.go``.
"""

from __future__ import annotations

import time

import pytest

from tests.service import flagsmith_flow as fl
from tests.service.harness import Composition

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = Composition(("controller-manager", "game-coordinator"), fake_flagsmith=True)

# The first start creates ~130 features and their tags at FLAGSMITH_PUSH_RATE (5/s).
FIRST_START_SECONDS = 90.0
RESTART_SECONDS = fl.RESTART_SECONDS

CAPTURE = ("agent", "decision.capture_content")
TICK = ("agent", "experiment_tick_seconds")
MAX_GAMES = ("agent", "experiment_max_games")  # kill switch: lower is tighter, baseline 50
PACING = ("interventions", "pacing_profile")


@pytest.fixture(autouse=True)
def settled_link(stack, flag_service):
    fake = stack.fake_flagsmith
    fl.wait_link(flag_service, FIRST_START_SECONDS, link="online")
    yield fake
    flag_service.cleanup()
    fl.settle(flag_service, fake)


def _restart(stack, flag_service, **want) -> float:
    """Restart the flag service; returns the monotonic time the restart returned."""
    stack.flag_plane.restart_flag_service()
    returned = time.monotonic()
    if want:
        fl.wait_link(flag_service, RESTART_SECONDS, **want)
    return returned


def _registry_keys() -> set[str]:
    from tests.service.harness.fakes.flagsmith import registry_features
    from tests.service.harness.flag_plane import FLAG_SERVICE_SRC

    return {key for _, key in registry_features(FLAG_SERVICE_SRC / "registry.yaml")}


@pytest.mark.uc("UC-054", flow="A3", br=["BR-002"], tc="TC-022")
def test_start_creates_missing_features(stack, flag_service):
    fake = stack.fake_flagsmith
    link = fl.wait_link(flag_service, FIRST_START_SECONDS, link="online")
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


@pytest.mark.uc("UC-054", flow="A3", br=["BR-007"], tc="TC-022")
def test_offline_start_serves_baseline(stack, flag_service, settled_link):
    fake = settled_link
    fake.down = True
    try:
        link = _restart(stack, flag_service, link="offline") and fl.link(flag_service)
        assert link["offline_reason"] == "http_503"
        status, body = flag_service.request("GET", "/healthz")
        assert status == 200, body
        # flagd reconnects on its own after the restart (503 no_sync_stream until then).
        fl.put_when_synced(flag_service, *CAPTURE, variant="off")
        assert fl.overlay(flag_service, *CAPTURE)["variant"] == "off"
    finally:
        fake.down = False
    fl.wait_link(flag_service, RESTART_SECONDS + 60, link="online")


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"], tc="TC-022")
def test_degraded_rule_in_state(stack, flag_service, settled_link):
    fake = settled_link
    fake.person_sets(TICK[1], 40)
    fake.audit_rows = []  # the free plan: 200 with no rows
    _restart(stack, flag_service, link="online", restart_rule="degraded")
    assert "restart_rule_degraded" in stack.flag_plane.flag_service.log_tail()
    # A value without an author is applied as a person's.
    fl.wait_overlay(flag_service, *TICK, timeout=RESTART_SECONDS, value=40, writer="flagsmith")


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"], tc="TC-022")
def test_restart_clears_own_overlay(stack, flag_service, settled_link):
    fake = settled_link
    # Step 1: a local overlay is pushed: Flagsmith holds it with an API key's audit row.
    t0 = time.monotonic()
    fl.put_when_synced(flag_service, *CAPTURE, variant="off")
    fl.wait_push(fake, CAPTURE[1], lambda r: fl.patch_value(r) == (True, "off"), after=t0)
    assert fake.audit_rows[0] == {"log": f"Flag state updated for feature: {CAPTURE[1]}", "author": None}
    fl.wait_pulls(flag_service, 1)
    # Step 2: the flag service restarts.
    restarted = _restart(stack, flag_service, link="online", restart_rule="audit_log")
    # Step 3: the link's own value is cleared in Flagsmith (enabled false), not applied locally.
    # The clear is queued by a pull round after the start round: nudge the sender (#2608).
    fl.wait_push(
        fake, CAPTURE[1], lambda r: not fl.patch_value(r)[0], after=restarted, timeout=RESTART_SECONDS,
        flag_service=flag_service,
    )  # fmt: skip
    assert fake.env_value(CAPTURE[1])[0] is False
    fl.wait_pulls(flag_service, 2)
    assert fl.overlay(flag_service, *CAPTURE) == {}, "the link's own value came back after a restart"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"], tc="TC-022")
def test_restart_keeps_persons_value(stack, flag_service, settled_link):
    fake = settled_link
    # Step 1: a person's value is in force.
    fake.person_sets(CAPTURE[1], "off")
    fl.wait_overlay(flag_service, *CAPTURE, variant="off", writer="flagsmith")
    # Step 2: the flag service restarts (the in-memory overlay is gone).
    restarted = _restart(stack, flag_service, link="online", restart_rule="audit_log")
    # Step 3: the person's value comes back (a start defers it while flagd has no sync stream yet).
    fl.wait_overlay(flag_service, *CAPTURE, timeout=RESTART_SECONDS, variant="off", writer="flagsmith")
    assert not [r for r in fake.pushes(CAPTURE[1]) if r["t"] >= restarted], "a person's value was cleared"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-005"], tc="TC-022")
def test_restart_never_loosens_kill_switch(stack, flag_service, settled_link):
    fake = settled_link
    baseline = flag_service.get(*MAX_GAMES)["variant_values"][flag_service.get(*MAX_GAMES)["baseline_default"]]
    # (a) Step 1: a person loosened the kill switch above the baseline; it is in force.
    fake.person_sets(MAX_GAMES[1], baseline + 30)
    fl.wait_overlay(flag_service, *MAX_GAMES, value=baseline + 30, writer="flagsmith")
    # Step 2: restart: with no last state the tighter local baseline stays and is pushed.
    restarted = _restart(stack, flag_service, link="online")
    # Queued by the start reconcile, or by a pull round if flagd was not synced yet (#2608: nudge).
    fl.wait_push(
        fake, MAX_GAMES[1], lambda r: not fl.patch_value(r)[0], after=restarted, timeout=RESTART_SECONDS,
        flag_service=flag_service,
    )  # fmt: skip
    fl.wait_pulls(flag_service, 2)
    assert fl.overlay(flag_service, *MAX_GAMES) == {}, "a restart loosened a kill switch"
    # (b) Step 3: an API key tightened it; the restart clear does not undo the tightening.
    fake.set_value(MAX_GAMES[1], baseline - 30)  # author None: an API key
    fl.wait_overlay(flag_service, *MAX_GAMES, value=baseline - 30, writer="flagsmith")
    fl.wait_pulls(flag_service, 1)
    restarted = _restart(stack, flag_service, link="online")
    fl.wait_overlay(flag_service, *MAX_GAMES, timeout=RESTART_SECONDS, value=baseline - 30, writer="flagsmith")
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(MAX_GAMES[1]) == (True, baseline - 30)
    assert not [r for r in fake.pushes(MAX_GAMES[1]) if r["t"] >= restarted], "the tightening was cleared"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"], tc="TC-022")
def test_restart_game_overrides_wait_for_registration(stack, flag_service, settled_link):
    fake = settled_link
    person_game, own_game = fl.new_game_id(), fl.new_game_id()
    # Step 1: Flagsmith holds a person's game value and one an API key (the link) set.
    fake.person_overrides(f"game:{person_game}", PACING[1], "calm")
    fake.set_override(f"game:{own_game}", PACING[1], "frantic")  # author None
    try:
        # Step 2: restart; no game is registered yet: nothing is applied.
        _restart(stack, flag_service, link="online")
        fl.wait_pulls(flag_service, 2)
        assert not (flag_service.get(*PACING).get("games") or {}), "a game value applied before registration"
        # Step 3: the games register: the person's value is applied, the link's own is removed.
        fl.register_game(flag_service, person_game, ["MOCK0011"])
        fl.register_game(flag_service, own_game, ["MOCK0012"])
        got = fl.wait_for(
            lambda: (flag_service.get(*PACING).get("games") or {}).get(person_game), fl.PULL_TIMEOUT, "person's value"
        )
        assert got["value"] == "calm" and got["writer"] == "flagsmith", got
        # The restart rule queues the removal in a pull round; nudge the sender (#2608).
        fl.wait_for(
            lambda: (fl.nudge(flag_service), fake.override(f"game:{own_game}", PACING[1]) is None)[1],
            fl.PULL_TIMEOUT,
            "own override removed",
            interval=2.0,
        )
        assert own_game not in (flag_service.get(*PACING).get("games") or {})
    finally:
        fl.end_game(flag_service, person_game)
        fl.end_game(flag_service, own_game)


@pytest.mark.uc("UC-054", flow="A3", tc="TC-022")
def test_restart_removes_stale_identities_after_grace(stack, flag_service, settled_link):
    fake = settled_link
    stale, live = fl.new_game_id(), fl.new_game_id()
    fake.add_identity(f"game:{stale}")
    fake.add_identity(f"game:{live}")
    try:
        # Step 1: restart; the live game registers again, the stale one never does.
        restarted = _restart(stack, flag_service)
        fl.wait_for(lambda: fl.register_game(flag_service, live, ["MOCK0013"]) or True, 30, "register")
        # Step 2: the stale identity is deleted after the 45 s grace (bound 75 s), never before.
        fl.wait_for(lambda: f"game:{stale}" not in fake.identities(), 75, "the stale identity deleted")
        deletes = [r for r in fake.requests("DELETE", "/edge-identities/") if r["t"] >= restarted]
        assert deletes and min(r["t"] for r in deletes) - restarted >= 45, [r["t"] - restarted for r in deletes]
        # Step 3: the registered game's identity stays.
        assert f"game:{live}" in fake.identities()
    finally:
        fl.end_game(flag_service, live)


@pytest.mark.uc("UC-055", flow="A5", tc="TC-022")
def test_restart_loses_unsent_changes(stack, flag_service, settled_link):
    fake = settled_link
    # Step 1: Flagsmith is down; a local change waits to be sent.
    fake.down = True
    try:
        fl.wait_link(flag_service, fl.PULL_TIMEOUT, link="offline")
        flag_service.put(*TICK, value=77)
        assert fl.link(flag_service)["pending"] >= 1
        # Step 2: the flag service restarts before Flagsmith is back.
        _restart(stack, flag_service, link="offline")
    finally:
        fake.down = False
    # Step 3: back online, the unsent change is gone: never sent, not in force.
    state = fl.wait_link(flag_service, RESTART_SECONDS + 60, link="online")
    assert state["restart_rule"], state
    fl.wait_pulls(flag_service, 2)
    assert not [r for r in fake.pushes(TICK[1]) if fl.patch_value(r) == (True, "77")], fake.pushes(TICK[1])
    assert fl.overlay(flag_service, *TICK) == {}
    assert fl.link(flag_service)["pending"] == 0


@pytest.mark.uc("UC-054", flow="A3", tc="TC-022")
def test_identity_overrides_disabled_keeps_link_offline(stack, flag_service, settled_link):
    """Last in the module: the link stays offline until the setting is back."""
    fake = settled_link
    fake.identity_overrides_in_local_eval = False
    try:
        state = _restart(stack, flag_service, link="offline") and fl.link(flag_service)
        assert state["offline_reason"] == "identity_overrides_disabled", state
        # Local writes still work.
        fl.put_when_synced(flag_service, *TICK, value=55)
        assert fl.overlay(flag_service, *TICK)["value"] == 55
    finally:
        fake.identity_overrides_in_local_eval = True
    fl.wait_link(flag_service, RESTART_SECONDS + 60, link="online")
