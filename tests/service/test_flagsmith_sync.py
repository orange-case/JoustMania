"""TC-021: the operator steers the stack from Flagsmith, and the stack's own changes reach
Flagsmith (UC-054, UC-055, #2592).

The flag service built from HEAD runs its Flagsmith link against the stateful fake hosted
Flagsmith (``tests/service/harness/fakes/flagsmith.py``), which already holds every registry
flag as a feature (``fake_flagsmith_preseed``). The fake changes its environment document the
way Flagsmith does: a person's change in the UI (``person_sets`` / ``person_overrides``,
audit rows with a user), the link's own pushes (``PATCH __future__``, identity overrides,
audit rows of an API key). The link polls every 5 s (FLAGSMITH_POLL_SECONDS) with a 1 s
timeout (FLAGSMITH_TIMEOUT_SECONDS), so a "slow" Flagsmith is a timeout in this tier.

Not here: spans and metrics (the flag service has no OTLP endpoint in this tier; the Go
tests in services/flag-service/flagsmith are the evidence), the live SaaS semantics (edge
document identity overrides, audit authorship timing, request limits: a Known gap, #2592 D1).

Unit detail (merge table, guards, pacing, refusals) is in services/flag-service/flagsmith/*_test.go.
"""

from __future__ import annotations

import json
import time
import uuid

import pytest

from tests.integration.helpers import flagd_probe_client
from tests.service import flagsmith_flow as fl
from tests.service.harness import Composition

pytestmark = pytest.mark.tier("service")

SERVICE_COMPOSITION = Composition(
    ("controller-manager", "game-coordinator"), fake_flagsmith=True, fake_flagsmith_preseed=True
)

CAPTURE = ("agent", "decision.capture_content")  # variants on/off, stack only
TICK = ("agent", "experiment_tick_seconds")  # integer 1..1000, stack only
MAX_GAMES = ("agent", "experiment_max_games")  # kill switch (flags.md §13.6): lower is tighter
BACKEND = ("agent", "decision.backend")  # persist: true (an admin setting)
PACING = ("interventions", "pacing_profile")  # game scope
HANDICAP = ("interventions", "player_handicap_factor")  # game+serial scope
AUDIO_CUE = ("interventions", "audio_cue")  # event flag, game scope
SENSITIVITY = ("game", "sensitivity")  # also experiment+arm: read-only from Flagsmith
PUSH_RATE = 5  # FLAGSMITH_PUSH_RATE default (requests per second, UC-055 BR-001)


@pytest.fixture(autouse=True)
def settled_link(stack, flag_service):
    fake = stack.fake_flagsmith
    fl.wait_link(flag_service, fl.RESTART_SECONDS + 60, link="online")
    yield fake
    flag_service.cleanup()
    fl.settle(flag_service, fake)


def _hold_next_push(fake, feature: str, seconds: int = 15) -> None:
    """One 429 with Retry-After: the next local value of ``feature`` waits ``seconds`` in the
    out side while the test changes Flagsmith (plan #2592 §8.1, §8.2)."""
    fake.refuse(feature, 429, retry_after=seconds, times=1)


def _establish(flag_service, fake, flag_set: str, flag: str, value) -> None:
    """R: the local value is pushed AND a pull saw it (the value guard is gone)."""
    t0 = time.monotonic()
    flag_service.put(flag_set, flag, value=value)
    push = fl.wait_push(fake, flag, lambda r: fl.patch_value(r)[0], after=t0)
    fl.wait_pulls(flag_service, 1, after=time.time())
    assert fake.env_value(flag)[1] == value, (push, fake.env_value(flag))


# --------------------------------------------------------------------------- UC-054 main
@pytest.mark.uc("UC-054", flow="main", steps=[1, 2, 3, 4, 5, 6, 7], br=["BR-001", "BR-009"], tc="TC-021")
def test_person_stack_value_applied_as_flagsmith_write(stack, flag_service, settled_link):
    fake = settled_link
    t0 = time.monotonic()
    # Step 1: a person turns content capture off in Flagsmith.
    fake.person_sets(CAPTURE[1], "off")
    # Step 2: within a poll the flag service writes it as the flagsmith writer.
    got = fl.wait_overlay(flag_service, *CAPTURE, timeout=fl.POLL_SECONDS + 10, variant="off")
    assert got["writer"] == "flagsmith", got
    # Step 3: flagd serves it to every reader.
    with flagd_probe_client(stack, CAPTURE[0]) as client:  # the in-process resolver every service reads
        fl.wait_for(
            lambda: (lambda d: d.value is False or None)(client.get_boolean_details(CAPTURE[1], True, None)),
            15,
            "flagd serving capture_content=off",
        )
    # Step 4: in sync, and the applied value is not echoed back to Flagsmith (BR-004).
    fl.wait_pulls(flag_service, 1)
    state = fl.link(flag_service)
    assert state["out_of_sync"] == 0 and state["pending"] == 0, state
    assert [r for r in fake.pushes(CAPTURE[1]) if r["t"] >= t0] == [], "an applied Flagsmith value was pushed back"


@pytest.mark.uc("UC-054", flow="main", steps=[2, 3], br=["BR-003", "BR-009"], tc="TC-021")
def test_person_game_and_player_override_applied(flag_service, settled_link):
    fake = settled_link
    game, other = fl.new_game_id(), fl.new_game_id()
    try:
        fl.register_game(flag_service, game, ["MOCK0001"])
        fl.register_game(flag_service, other, ["MOCK0002"])
        fl.wait_for(lambda: f"game:{game}:MOCK0001" in fake.identities(), fl.PULL_TIMEOUT, "player identity listed")
        # Step 1: a person sets a game value and a player value in Flagsmith.
        fake.person_overrides(f"game:{game}", PACING[1], "calm")
        fake.person_overrides(f"game:{game}:MOCK0001", HANDICAP[1], "1.5")
        # Step 2: each lands in its own scope, written by flagsmith.
        pacing = fl.wait_for(
            lambda: (flag_service.get(*PACING).get("games") or {}).get(game), fl.PULL_TIMEOUT, "game value"
        )
        assert pacing["value"] == "calm" and pacing["writer"] == "flagsmith", pacing
        player = fl.wait_for(
            lambda: (flag_service.get(*HANDICAP).get("serials") or {}).get(game, {}).get("MOCK0001"),
            fl.PULL_TIMEOUT,
            "player value",
        )
        assert float(player["value"]) == 1.5 and player["writer"] == "flagsmith", player
        # Step 3: no other game and no game-wide handicap picked it up.
        assert other not in (flag_service.get(*PACING).get("games") or {})
        assert game not in (flag_service.get(*HANDICAP).get("games") or {})
        assert other not in (flag_service.get(*HANDICAP).get("serials") or {})
    finally:
        fl.end_game(flag_service, game)
        fl.end_game(flag_service, other)


@pytest.mark.uc("UC-054", flow="main", steps=[5], br=["BR-007"], tc="TC-022")
def test_person_admin_setting_persisted_and_survives_offline_restart(stack, flag_service, settled_link):
    fake = settled_link
    # Step 1: a person changes an admin setting (persist: yes) in Flagsmith.
    fake.person_sets(BACKEND[1], "claude")
    # Step 2: the flag service applies and persists it (admin value, C-030 state file).
    admin = fl.wait_for(lambda: flag_service.get(*BACKEND).get("admin_value"), fl.PULL_TIMEOUT, "admin value")
    assert admin["variant"] == "claude" and admin["writer"] == "flagsmith", admin
    # Step 3: Flagsmith is unreachable and the flag service restarts.
    fake.down = True
    try:
        stack.flag_plane.restart_flag_service()
        state = fl.wait_link(flag_service, fl.RESTART_SECONDS, link="offline")
        assert state["offline_reason"] == "http_503", state
        # Step 4: the persisted setting is served from the state file, offline.
        admin = flag_service.get(*BACKEND).get("admin_value") or {}
        assert admin.get("variant") == "claude", admin
    finally:
        fake.down = False
    fl.wait_link(flag_service, fl.RESTART_SECONDS + 60, link="online")


@pytest.mark.uc("UC-054", flow="main", steps=[2], tc="TC-021")
def test_person_event_override_appends_once(flag_service, settled_link):
    fake = settled_link
    game = fl.new_game_id()
    try:
        fl.register_game(flag_service, game, ["MOCK0003"])
        fl.wait_for(lambda: f"game:{game}" in fake.identities(), fl.PULL_TIMEOUT, "game identity listed")
        # Step 1: a person sets one audio cue payload on the game in Flagsmith (architecture note §4.3).
        fake.person_overrides(f"game:{game}", AUDIO_CUE[1], json.dumps({"sound": "bell"}))

        def events() -> list:
            return ((flag_service.get(*AUDIO_CUE).get("events") or {}).get(game) or {}).get("events") or []

        # Step 2: the flag service appends it once ...
        fl.wait_for(lambda: len(events()) >= 1, fl.PULL_TIMEOUT, "the cue appended")
        # Step 3: ... and never again over the following polls (the pushed list is the remote value).
        fl.wait_pulls(flag_service, 3)
        assert len(events()) == 1 and "bell" in json.dumps(events()), events()
    finally:
        fl.end_game(flag_service, game)


# ------------------------------------------------------------------ UC-054 BR-004 / FR-209
@pytest.mark.uc("UC-054", flow="main", br=["BR-004"], tc="TC-021")
@pytest.mark.uc("UC-055", flow="main", steps=[5], tc="TC-021")
def test_own_push_never_comes_back(flag_service, settled_link):
    """The document still holding the link's own last push is not a Flagsmith change
    (remote := pushed value): a newer local value waiting to be sent stays (plan #2592 §8.1)."""
    fake = settled_link
    # Step 1: local value A is pushed.
    t0 = time.monotonic()
    flag_service.put(*TICK, value=12)
    fl.wait_push(fake, TICK[1], lambda r: fl.patch_value(r) == (True, "12"), after=t0)
    # Step 2: local value B, its push held 15 s by a 429; the document still shows A.
    _hold_next_push(fake, TICK[1], seconds=15)
    flag_service.put(*TICK, value=13)
    # Step 3: two polls read A back: it is the link's own push, B stays and is not overwritten.
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(TICK[1]) == (True, 12), "the held push went out early; the test proves nothing"
    got = fl.overlay(flag_service, *TICK)
    assert got.get("value") == 13 and got.get("writer") != "flagsmith", got
    # Step 4: B goes out after the wait and the document converges to it.
    fl.wait_for(lambda: fake.env_value(TICK[1]) == (True, 13), 30, "B in Flagsmith")
    fl.wait_pulls(flag_service, 2)
    got = fl.overlay(flag_service, *TICK)
    assert got.get("value") == 13 and got.get("writer") != "flagsmith", got


# ------------------------------------------------------------------ UC-054 A1 + UC-055 A3
@pytest.mark.uc("UC-054", flow="A1", tc="TC-021")
@pytest.mark.uc("UC-055", flow="A3", tc="TC-021")
def test_offline_keeps_local_writes_and_drains_on_reconnect(flag_service, settled_link):
    fake = settled_link
    # Step 1: Flagsmith goes down: the link goes offline.
    fake.down = True
    try:
        state = fl.wait_link(flag_service, fl.PULL_TIMEOUT, link="offline")
        assert state["offline_reason"] == "http_503", state
        # Step 2: local writes, a kill switch included, are answered at once.
        flag_service.put(*TICK, value=14)
        flag_service.put(*MAX_GAMES, value=20)
        state = fl.link(flag_service)
        assert state["pending"] >= 1, state
        assert fl.overlay(flag_service, *MAX_GAMES).get("value") == 20
    finally:
        # Step 3: Flagsmith is back.
        fake.down = False
    fl.wait_link(flag_service, fl.RESTART_SECONDS + 60, link="online")
    # Step 4: the queue drains; Flagsmith holds the latest local values.
    fl.wait_for(lambda: fl.link(flag_service).get("pending") == 0, fl.PULL_TIMEOUT, "pending 0")
    assert fake.env_value(TICK[1]) == (True, 14)
    assert fake.env_value(MAX_GAMES[1]) == (True, 20)


# ------------------------------------------------------------------ UC-054 A2 (FR-210)
CONFLICT_ROWS = [
    # id, flag, R (established), L (local, push held), D (person), want local value, want writer, want remote
    ("local-change-pushed", TICK, 15, 16, None, 16, "local", 16),
    ("both-same-adopted", TICK, 15, 17, 17, 17, "local", 17),
    ("both-changed-flagsmith-wins", TICK, 15, 18, 19, 19, "flagsmith", 19),
    ("kill-switch-local-tighter-kept", MAX_GAMES, 40, 20, 30, 20, "local", 20),
    ("kill-switch-flagsmith-tighter-applied", MAX_GAMES, 40, 20, 10, 10, "flagsmith", 10),
]


@pytest.mark.uc("UC-054", flow="A2", tc="TC-021")
@pytest.mark.parametrize(
    ("flag", "r", "l_val", "d", "want", "writer", "remote"),
    [pytest.param(*row[1:], id=row[0]) for row in CONFLICT_ROWS],
)
def test_conflict_table(flag_service, settled_link, flag, r, l_val, d, want, writer, remote):
    """The 3-way merge (architecture note §6): R the last state both sides agreed on, L the
    local value, D the document. Built online: the local push is held by a one-shot 429 while
    the person changes Flagsmith (plan #2592 §8.2)."""
    fake = settled_link
    # Step 1: R is established on both sides.
    _establish(flag_service, fake, *flag, r)
    # Step 2: a local change whose push is held, and (maybe) a person's change in Flagsmith.
    _hold_next_push(fake, flag[1], seconds=15)
    held_until = time.monotonic() + 15
    flag_service.put(*flag, value=l_val)
    if d is not None:
        fake.person_sets(flag[1], d)
    # Step 3: the next polls merge R, L, D.
    fl.wait_pulls(flag_service, 2)
    got = fl.overlay(flag_service, *flag)
    assert got.get("value") == want, got
    assert (got.get("writer") == "flagsmith") == (writer == "flagsmith"), got
    # Step 4: after the held push both sides agree.
    fl.wait_for(
        lambda: time.monotonic() > held_until and fl.link(flag_service)["pending"] == 0, 40, "the held push done"
    )
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(flag[1]) == (True, remote), fake.pushes(flag[1])
    assert fl.overlay(flag_service, *flag).get("value") == want
    assert fl.link(flag_service)["out_of_sync"] == 0


@pytest.mark.uc("UC-054", flow="A2", tc="TC-021")
def test_conflict_after_reconnect_flagsmith_wins(flag_service, settled_link):
    """The one truly offline row: both sides changed while the link was down; on reconnect the
    reconcile runs before the queue drains, and Flagsmith wins (not a kill switch)."""
    fake = settled_link
    _establish(flag_service, fake, *TICK, 21)
    fake.down = True
    try:
        fl.wait_link(flag_service, fl.PULL_TIMEOUT, link="offline")
        flag_service.put(*TICK, value=22)
        fake.person_sets(TICK[1], 23)
    finally:
        fake.down = False
    fl.wait_link(flag_service, fl.RESTART_SECONDS + 60, link="online")
    fl.wait_overlay(flag_service, *TICK, value=23, writer="flagsmith")
    fl.wait_pulls(flag_service, 2)
    assert fake.env_value(TICK[1]) == (True, 23), "the offline local value overwrote Flagsmith's"


# ------------------------------------------------------------------ UC-054 A4 / FR-213
@pytest.mark.uc("UC-054", flow="A4", br=["BR-001", "BR-003"], tc="TC-021")
def test_refused_change_marks_feature_unhealthy(flag_service, settled_link):
    fake = settled_link

    def posts(feature: str, status: str) -> list[dict]:
        return [p for p in fake.health_posts() if p["feature"] == feature and p["status"] == status]

    # Step 1: a person sets a value outside the flag's bounds.
    fake.person_sets(TICK[1], 5000)
    # Step 2: refused: the local value stays, the feature turns unhealthy (one post).
    fl.wait_for(lambda: posts(TICK[1], "unhealthy"), fl.PULL_TIMEOUT, "an unhealthy post")
    assert fl.overlay(flag_service, *TICK) == {}
    assert fl.link(flag_service)["refused"] >= 1
    text = posts(TICK[1], "unhealthy")[0]["reason"]["text_blocks"][0]["text"]
    assert "out_of_bounds" in text, text
    # Step 3: no repeat post while the refused value stays.
    fl.wait_pulls(flag_service, 2)
    assert len(posts(TICK[1], "unhealthy")) == 1, fake.health_posts()
    # Step 4: a valid value is applied and the feature turns healthy.
    fake.person_sets(TICK[1], 24)
    fl.wait_overlay(flag_service, *TICK, value=24, writer="flagsmith")
    fl.wait_for(lambda: posts(TICK[1], "healthy"), fl.PULL_TIMEOUT, "a healthy post")
    # Step 5: an experiment arm is read-only from Flagsmith (BR-003).
    fake.person_overrides("experiment:x:a", SENSITIVITY[1], "fast")
    post = fl.wait_for(lambda: posts(SENSITIVITY[1], "unhealthy"), fl.PULL_TIMEOUT, "a read-only post")[0]
    assert "flagsmith_scope_read_only" in post["reason"]["text_blocks"][0]["text"], post


# ------------------------------------------------------------------ UC-054 A5
@pytest.mark.uc("UC-054", flow="A5", tc="TC-021")
def test_change_waits_for_flagd(stack, flag_service, settled_link):
    fake = settled_link
    plane = stack.flag_plane
    # Step 1: flagd is down (the flag service has no sync stream).
    plane.stop_flagd()
    try:
        # Step 2: a person's change is not applied while flagd is down ...
        fake.person_sets(CAPTURE[1], "off")
        fl.wait_pulls(flag_service, 2)
        assert fl.overlay(flag_service, *CAPTURE) == {}
    finally:
        # Step 3: flagd is back.
        plane.start_flagd()
    # Step 4: ... and is applied once it is (deferral backoff + a poll).
    fl.wait_overlay(flag_service, *CAPTURE, timeout=70, variant="off", writer="flagsmith")


# --------------------------------------------------------------------------- UC-055 main
@pytest.mark.uc("UC-055", flow="main", steps=[1, 2, 3, 4, 6], br=["BR-001", "BR-002"], tc="TC-021")
def test_local_write_reaches_flagsmith_merged(flag_service, settled_link):
    fake = settled_link
    history = f"Flag state updated for feature: {TICK[1]}"
    rows_before = sum(r["log"] == history for r in fake.audit_rows)
    t0 = time.monotonic()
    # Step 1: 15 local writes in a row are each answered at once.
    for value in range(101, 116):
        start = time.monotonic()
        flag_service.put(*TICK, value=value)
        assert time.monotonic() - start < 0.5
    writing = time.monotonic() - t0
    # Step 2: Flagsmith ends with the last value, merged: at most FLAGSMITH_PUSH_RATE (5/s) requests
    # while the writes ran, plus the first and the merged last one (+1 for timer jitter).
    fl.wait_for(lambda: fake.env_value(TICK[1]) == (True, 115), fl.PULL_TIMEOUT, "the last value in Flagsmith")
    sent = [r for r in fake.pushes(TICK[1]) if r["t"] >= t0]
    bound = min(14, int(writing * PUSH_RATE) + 3)
    assert len(sent) <= bound, f"{len(sent)} requests for 15 writes in {writing:.2f}s (bound {bound}): not merged"
    # UC-055 step 6: each accepted send is a change in the flag's history (an API key's audit row,
    # no author), the newest one first.
    accepted = sum(r["status"] == 200 for r in sent)
    rows = [r for r in fake.audit_rows if r["log"] == history]
    assert accepted >= 1 and len(rows) - rows_before == accepted, (
        f"{len(rows) - rows_before} history rows for {accepted} accepted sends"
    )
    assert fake.audit_rows[0] == {"log": history, "author": None}, fake.audit_rows[:3]
    # Step 3: a local clear turns the feature off with the baseline value.
    default = flag_service.get(*TICK)["baseline_default"]
    t1 = time.monotonic()
    flag_service.delete(*TICK)
    fl.wait_push(fake, TICK[1], lambda r: not fl.patch_value(r)[0], after=t1)
    assert fake.env_value(TICK[1]) == (False, default)


@pytest.mark.uc("UC-055", flow="A1", tc="TC-021")
def test_game_start_lists_identities(flag_service, settled_link):
    fake = settled_link
    game = fl.new_game_id()
    try:
        # Step 1: a game starts with two players.
        fl.register_game(flag_service, game, ["MOCK0004", "MOCK0005"])
        # Step 2: the game and each player are identities in Flagsmith; the game carries its traits.
        want = {f"game:{game}", f"game:{game}:MOCK0004", f"game:{game}:MOCK0005"}
        fl.wait_for(lambda: want <= set(fake.identities()), fl.PULL_TIMEOUT, f"identities {want}")
        fl.wait_for(
            lambda: fake.identities()[f"game:{game}"]["traits"]
            == {"game_kind": "shadow", "coordinator_instance": fl.COORDINATOR_INSTANCE},
            fl.PULL_TIMEOUT,
            "game traits",
        )
        assert fake.identities()[f"game:{game}:MOCK0004"]["traits"] == {}
    finally:
        fl.end_game(flag_service, game)


@pytest.mark.uc("UC-055", flow="A2", br=["BR-003"], tc="TC-021")
def test_game_end_removes_identities_and_values(flag_service, settled_link):
    fake = settled_link
    game = fl.new_game_id()
    ended = False
    try:
        fl.register_game(flag_service, game, ["MOCK0006"])
        # Step 1: a game value is set locally and reaches Flagsmith as an identity override.
        flag_service.put(*PACING, variant="frantic", scope={"game_id": game})
        fl.wait_for(lambda: fake.override(f"game:{game}", PACING[1]), fl.PULL_TIMEOUT, "the game override")
        # Step 2: the game ends: its identities and their overrides are removed.
        fl.end_game(flag_service, game)
        ended = True
        fl.wait_for(
            lambda: not any(k.startswith(f"game:{game}") for k in fake.identities()),
            fl.PULL_TIMEOUT,
            "the game's identities removed",
        )
        assert fake.override(f"game:{game}", PACING[1]) is None
    finally:
        if not ended:
            fl.end_game(flag_service, game)


@pytest.mark.uc("UC-055", flow="A4", tc="TC-021")
def test_flagsmith_refusal_marks_out_of_sync(flag_service, settled_link):
    fake = settled_link
    # Step 1: Flagsmith refuses the change (a change request is required).
    fake.refuse(TICK[1], 409)
    flag_service.put(*TICK, value=31)
    # Step 2: the local value stays in force; the key is refused and out of sync.
    fl.wait_for(lambda: fl.link(flag_service).get("refused", 0) >= 1, fl.PULL_TIMEOUT, "refused")
    assert fl.overlay(flag_service, *TICK).get("value") == 31
    assert fl.link(flag_service)["out_of_sync"] >= 1
    # Step 3: the refused value is not sent again over the next polls (#2604).
    sent = len(fake.pushes(TICK[1]))
    fl.wait_pulls(flag_service, 3)
    assert len(fake.pushes(TICK[1])) == sent, fake.pushes(TICK[1])
    # Step 4: Flagsmith accepts again; the next local change lands and the key is in sync.
    fake.clear_refusals()
    flag_service.put(*TICK, value=32)
    fl.wait_for(lambda: fake.env_value(TICK[1]) == (True, 32), fl.PULL_TIMEOUT, "32 in Flagsmith")
    fl.wait_for(
        lambda: (lambda s: s["out_of_sync"] == 0 and s["refused"] == 0)(fl.link(flag_service)),
        fl.PULL_TIMEOUT,
        "in sync",
    )
    # Step 5: a 429 is retried after its Retry-After and lands.
    fake.refuse(TICK[1], 429, retry_after=1, times=1)
    t0 = time.monotonic()
    flag_service.put(*TICK, value=33)
    fl.wait_for(lambda: fake.env_value(TICK[1]) == (True, 33), fl.PULL_TIMEOUT, "33 in Flagsmith after a 429")
    statuses = [r["status"] for r in fake.pushes(TICK[1]) if r["t"] >= t0]
    assert statuses[:2] == [429, 200], statuses


# --------------------------------------------------------------------------- NFRs
@pytest.mark.nfr("NFR-057")
@pytest.mark.uc("UC-055", flow="main", br=["BR-001"], tc="TC-021")
def test_local_write_latency_with_flagsmith_down_and_slow(stack, flag_service, settled_link, capsys):
    """A local write never waits for Flagsmith (NFR-057): the NFR-050 propagation probe with the
    link down, then with Flagsmith slower than the link's 1 s timeout (a timeout in this tier),
    plus the write's own answer time, against the test_flag_propagation.py CI bounds (p95 0.5 s,
    max 1.5 s)."""
    from tests.service.test_flag_propagation import (
        MAX_BOUND_SECONDS,
        P95_BOUND_SECONDS,
        _percentile,
        format_distribution,
        measure_write_propagation,
    )

    fake = settled_link
    reports = []
    for label, setup, reason in (
        ("down", {"down": True}, "http_503"),
        ("slow", {"delay": 3.0}, "timeout"),
    ):
        for name, value in setup.items():
            setattr(fake, name, value)
        try:
            # Sample only once the link has noticed: offline (down: http_503, slow: the 1 s timeout).
            state = fl.wait_link(flag_service, fl.PULL_TIMEOUT, link="offline")
            # The slow phase must exercise the timeout path, not another offline reason.
            assert state.get("offline_reason") == reason, f"{label}: {state}"
            latencies = measure_write_propagation(stack, flag_service, samples=10)
            # The write's own answer: the flag service never waits for Flagsmith before answering.
            answers = []
            for _ in range(10):
                start = time.monotonic()
                flag_service.put("observability", "flagservice.propagation_probe", value=uuid.uuid4().hex)
                answers.append(time.monotonic() - start)
            latencies["write_answer"] = answers
        finally:
            fake.down, fake.delay = False, 0.0
        report = format_distribution(latencies)
        reports.append(f"[flagsmith {label}]\n{report}")
        for reader, values in latencies.items():
            assert None not in values, f"{label}/{reader}: a write was never served\n{report}"
            seen = sorted(values)
            assert _percentile(seen, 0.95) < P95_BOUND_SECONDS, f"{label}/{reader}: p95\n{report}"
            assert seen[-1] < MAX_BOUND_SECONDS, f"{label}/{reader}: max\n{report}"
        fl.wait_link(flag_service, fl.RESTART_SECONDS + 60, link="online")
    with capsys.disabled():
        print("\n" + "\n".join(reports))


@pytest.mark.nfr("NFR-058")
@pytest.mark.uc("UC-054", flow="main", steps=[2], tc="TC-021")
def test_change_delay_within_poll(flag_service, settled_link):
    """A Flagsmith change is in force within 30 s (NFR-058, polling only: no realtime stream in
    this tier, #2592 D2; NFR-058 stays Implemented until measured live)."""
    fake = settled_link
    start = time.monotonic()
    fake.person_sets(CAPTURE[1], "off")
    fl.wait_overlay(flag_service, *CAPTURE, timeout=30, variant="off", writer="flagsmith")
    assert time.monotonic() - start <= 30
