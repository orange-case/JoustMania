"""TC-022: the flag service's Flagsmith link on start and restart (UC-054 A3, UC-055 A5, #2591, #2592).

The flag service is built from HEAD and linked to the stateful fake hosted Flagsmith
(``tests/service/harness/fakes/flagsmith.py``, ``Composition(fake_flagsmith=True)``):
FLAGSMITH_LINK=on, the fake's URLs and dummy keys. Every other composition keeps the link
off, as every compose file does. No preseed here: the first start creates the features
(FR-214). The fake keeps the link's own pushes as API-key values (audit rows without an
author) and a person's changes as a user's; since #2621 a restart reads no authors.

- start: the link resolves the environment and creates every registry flag that is not a
  feature yet (FR-214, BR-002, never an ``initial_value``);
- offline start: an unreachable Flagsmith never stops the flag service (BR-007);
- restart, Flagsmith wins (BR-006, FR-215): every value Flagsmith holds comes back,
  whoever wrote it: the link's own push, a person's value, an admin setting an API key set
  (persisted), a kill switch a person loosened (no restart tightening, BR-005); game values
  wait for their game's registration; stale game identities go after the grace (FR-211); a
  controller of a live game keeps its overrides and a controller without one is released (its
  identity kept, #2624); unsent local changes are lost (FR-212);
- start refused: identity overrides off in local evaluation keeps the link offline.

Unit detail (ping first, kill switches, game identities, events, pacing) is in
``services/flag-service/flagsmith/start_test.go`` and ``in_start_test.go``.
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
BACKEND = ("agent", "decision.backend")  # an admin setting (persist: yes)
TICK = ("agent", "experiment_tick_seconds")
MAX_GAMES = ("agent", "experiment_max_games")  # kill switch: lower is tighter, baseline 50
PACING = ("interventions", "pacing_profile")
HANDICAP = ("interventions", "player_handicap_factor")  # game+serial scope: a controller identity (#2624)


@pytest.fixture(autouse=True)
def settled_link(stack, flag_service):
    fake = stack.fake_flagsmith
    fl.wait_link(flag_service, FIRST_START_SECONDS, link="online")
    yield fake
    flag_service.cleanup()
    fl.settle(flag_service, fake)


def _restart(stack, flag_service, **want) -> float:
    """Restart the flag service; returns the monotonic time taken BEFORE the restart.

    Every effect of the new process (its Start, the stale-identity grace that starts at the end
    of Start, its pushes) is after this point, whatever the readiness probe's timing."""
    before = time.monotonic()
    stack.flag_plane.restart_flag_service()
    if want:
        fl.wait_link(flag_service, RESTART_SECONDS, **want)
    return before


def _registry_keys() -> set[str]:
    from tests.service.harness.fakes.flagsmith import registry_features
    from tests.service.harness.flag_plane import FLAG_SERVICE_SRC

    return {key for _, key in registry_features(FLAG_SERVICE_SRC / "registry.yaml")}


@pytest.mark.uc("UC-054", flow="A3", br=["BR-002"], tc="TC-022")
def test_start_creates_missing_features(stack, flag_service):
    fake = stack.fake_flagsmith
    link = fl.wait_link(flag_service, FIRST_START_SECONDS, link="online")
    assert link["environment"] == "service-tier"
    assert "restart_rule" not in link, link  # no restart rule since #2621
    created = fake.created_features()
    assert len(created) == len(set(created)), "a feature was created twice"
    assert set(created) == _registry_keys(), "every registry flag (and nothing else) becomes a feature"
    for req in fake.feature_creations():
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
def test_restart_brings_back_own_overlay(stack, flag_service, settled_link):
    fake = settled_link
    # Step 1: a local in-memory overlay is pushed: Flagsmith holds it with an API key's audit row.
    t0 = time.monotonic()
    fl.put_when_synced(flag_service, *CAPTURE, variant="off")
    fl.wait_push(fake, CAPTURE[1], lambda r: fl.patch_value(r) == (True, "off"), after=t0)
    assert fake.audit_rows[0] == {"log": f"Flag state updated for feature: {CAPTURE[1]}", "author": None}
    fl.wait_pulls(flag_service, 1)
    # Step 2: the flag service restarts (the in-memory overlay is gone locally).
    restarted = _restart(stack, flag_service, link="online")
    # Step 3: Flagsmith wins: the link's own value comes back from Flagsmith and is not cleared there.
    fl.wait_overlay(flag_service, *CAPTURE, timeout=RESTART_SECONDS, variant="off", writer="flagsmith")
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(CAPTURE[1]) == (True, "off")
    assert not [r for r in fake.pushes(CAPTURE[1]) if r["t"] >= restarted], "the link's own value was cleared"
    assert not [r for r in fake.requests("GET", "/audit/") if r["t"] >= restarted], "a restart read the audit log"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"], tc="TC-022")
def test_restart_applies_api_key_admin_setting(stack, flag_service, settled_link):
    fake = settled_link
    assert not flag_service.get(*BACKEND).get("admin_value"), "no persisted admin value before the test"
    # Step 1: while the flag service cannot see it, an API key (Terraform, the Flagsmith MCP
    # server) sets an admin setting (persist: yes) in Flagsmith; the flag service restarts.
    fake.down = True
    try:
        fl.wait_link(flag_service, fl.PULL_TIMEOUT, link="offline")
        fake.set_value(BACKEND[1], "claude")  # author None
        _restart(stack, flag_service, link="offline")
    finally:
        fake.down = False
    restarted = time.monotonic()
    # Step 2: back online, Flagsmith wins: the value is applied and persisted like the menu admin's,
    # not replaced in Flagsmith by the local value.
    admin = fl.wait_for(
        lambda: flag_service.get(*BACKEND).get("admin_value"), RESTART_SECONDS + 60, "admin value from Flagsmith"
    )
    assert admin["variant"] == "claude" and admin["writer"] == "flagsmith", admin
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(BACKEND[1]) == (True, "claude")
    assert not [r for r in fake.pushes(BACKEND[1]) if r["t"] >= restarted], "the admin setting was replaced"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-006"], tc="TC-022")
def test_restart_keeps_persons_value(stack, flag_service, settled_link):
    fake = settled_link
    # Step 1: a person's value is in force.
    fake.person_sets(CAPTURE[1], "off")
    fl.wait_overlay(flag_service, *CAPTURE, variant="off", writer="flagsmith")
    # Step 2: the flag service restarts (the in-memory overlay is gone).
    restarted = _restart(stack, flag_service, link="online")
    # Step 3: the person's value comes back (a start defers it while flagd has no sync stream yet).
    fl.wait_overlay(flag_service, *CAPTURE, timeout=RESTART_SECONDS, variant="off", writer="flagsmith")
    assert not [r for r in fake.pushes(CAPTURE[1]) if r["t"] >= restarted], "a person's value was cleared"


@pytest.mark.uc("UC-054", flow="A3", br=["BR-005"], tc="TC-022")
def test_restart_takes_flagsmith_kill_switch(stack, flag_service, settled_link):
    fake = settled_link
    baseline = flag_service.get(*MAX_GAMES)["variant_values"][flag_service.get(*MAX_GAMES)["baseline_default"]]
    # Step 1: a kill switch is loosened above the baseline in Flagsmith (a person, then an API key
    # such as the agent gates switched on by Terraform); it is in force.
    fake.person_sets(MAX_GAMES[1], baseline + 30)
    fl.wait_overlay(flag_service, *MAX_GAMES, value=baseline + 30, writer="flagsmith")
    fake.set_value(MAX_GAMES[1], baseline + 40)  # author None: an API key
    fl.wait_overlay(flag_service, *MAX_GAMES, value=baseline + 40, writer="flagsmith")
    fl.wait_pulls(flag_service, 1)
    # Step 2: restart: the local value is the (tighter) baseline and no last state is known.
    restarted = _restart(stack, flag_service, link="online")
    # Step 3: Flagsmith wins (#2621): the looser Flagsmith value comes back; the tighter baseline is
    # neither kept nor pushed over it (no restart tightening; live conflicts still tighten, BR-005).
    fl.wait_overlay(flag_service, *MAX_GAMES, timeout=RESTART_SECONDS, value=baseline + 40, writer="flagsmith")
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(MAX_GAMES[1]) == (True, baseline + 40)
    assert not [r for r in fake.pushes(MAX_GAMES[1]) if r["t"] >= restarted], "a restart pushed the tighter value"


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
        # Step 3: the games register: Flagsmith wins, both values are applied, whoever set them.
        fl.register_game(flag_service, person_game, ["MOCK0011"])
        fl.register_game(flag_service, own_game, ["MOCK0012"])
        got = fl.wait_for(
            lambda: (flag_service.get(*PACING).get("games") or {}).get(person_game), fl.PULL_TIMEOUT, "person's value"
        )
        assert got["value"] == "calm" and got["writer"] == "flagsmith", got
        got = fl.wait_for(
            lambda: (flag_service.get(*PACING).get("games") or {}).get(own_game), fl.PULL_TIMEOUT, "own value"
        )
        assert got["value"] == "frantic" and got["writer"] == "flagsmith", got
        assert fake.override(f"game:{own_game}", PACING[1]) is not None, "the link's own override was removed"
    finally:
        fl.end_game(flag_service, person_game)
        fl.end_game(flag_service, own_game)


@pytest.mark.uc("UC-054", flow="A3", tc="TC-022")
def test_restart_removes_stale_identities_after_grace(stack, flag_service, settled_link):
    fake = settled_link
    stale, live = fl.new_game_id(), fl.new_game_id()
    fake.add_identity(f"game:{stale}")
    fake.add_identity(f"game:{live}")
    # Controller identities (#2624): MOCK0013 plays in the live real game, and a person set its
    # handicap there before the restart; MOCK0014's overrides belong to the stale game.
    fake.add_identity("controller:MOCK0013", {"game_id": live})
    fake.person_overrides("controller:MOCK0013", HANDICAP[1], "1.5")
    fake.add_identity("controller:MOCK0014", {"game_id": stale})
    fake.person_overrides("controller:MOCK0014", HANDICAP[1], "1.75")
    try:
        # Step 1: restart; the live game registers again, the stale one never does.
        restarted = _restart(stack, flag_service)
        fl.wait_for(lambda: fl.register_game(flag_service, live, ["MOCK0013"], kind="real") or True, 30, "register")
        # The live game's controller keeps its override across the restart and it applies.
        player = fl.wait_for(
            lambda: (flag_service.get(*HANDICAP).get("serials") or {}).get(live, {}).get("MOCK0013"),
            fl.PULL_TIMEOUT,
            "the live game's controller override applied after the restart",
        )
        assert float(player["value"]) == 1.5, player
        # Step 2: the stale identity is deleted after the 45 s grace (bound 75 s), never before.
        fl.wait_for(lambda: f"game:{stale}" not in fake.identities(), 75, "the stale identity deleted")
        deletes = [r["t"] - restarted for r in fake.requests("DELETE", "/edge-identities/") if r["t"] >= restarted]
        # The grace starts at the end of Start, after ``restarted`` (taken before the restart).
        assert deletes and 45 <= min(deletes) <= 75, deletes
        # Step 3: the registered game's identity stays.
        assert f"game:{live}" in fake.identities()
        # Step 4: a controller without a live game keeps its identity, loses its overrides and traits.
        fl.wait_for(
            lambda: fake.override("controller:MOCK0014", HANDICAP[1]) is None, 30, "the stale controller released"
        )
        # The traits are cleared after the overrides.
        fl.wait_for(lambda: fake.identities()["controller:MOCK0014"]["traits"] == {}, 30, "its traits cleared")
        assert fake.override("controller:MOCK0013", HANDICAP[1]) is not None
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
    fl.wait_link(flag_service, RESTART_SECONDS + 60, link="online")
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
