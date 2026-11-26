"""Unit tests for the per-spec evaluator on its own (intervention_evaluation.py).

The evaluator is driven with a scripted reader and recording dispatch/revert
callbacks: no flag client, no manager, no handler.
"""

import pytest

from services.game_coordinator.intervention_evaluation import _UNKNOWN, SpecEvaluator
from services.game_coordinator.intervention_sessions import SessionState, SessionView
from services.game_coordinator.interventions import INTERVENTION_SPECS
from services.game_coordinator.tests.conftest import FakeInterventionGame

SPECS = {s.flag_key: s for s in INTERVENTION_SPECS}


class ScriptedReads:
    """The evaluator's reads, answered from dicts the test sets."""

    def __init__(self):
        self.state: dict[str, object] = {}
        self.events: dict[str, object] = {}
        self.grants: dict[tuple[str, str], object] = {}
        self.levels: dict[tuple[str, str], object] = {}
        self.level_ids: dict[tuple[str, str], str] = {}

    def _read_flag(self, spec, _session):
        return self.state.get(spec.flag_key, spec.none_value)

    def _read_event_list(self, spec, _session):
        return self.events.get(spec.flag_key)

    def _read_grant(self, spec, _game_id, serial):
        return self.grants.get((spec.flag_key, serial), (None, spec.none_value, None))

    def read_levels(self, spec, _game_id, serials, ids=None):
        reads = {}
        for serial in serials:
            reads[serial] = self.levels.get((spec.flag_key, serial), spec.none_value)
            if ids is not None and (spec.flag_key, serial) in self.level_ids:
                ids[serial] = self.level_ids[(spec.flag_key, serial)]
        return reads


class Harness:
    def __init__(self, game=None, game_id="g1"):
        self.state = SessionState()
        self.reads = ScriptedReads()
        self.lagging = False
        self.dispatched: list[tuple] = []
        self.reverted: list[tuple] = []
        self.session = SessionView(game_id=game_id, game=game, publish=self._publish)
        self.state.begin(game_id)
        self.evaluator = SpecEvaluator(
            self.state,
            self.reads,
            allow_list_lags=lambda _spec: self.lagging,
            dispatch=self._dispatch,
            revert=self._revert,
        )

    async def _publish(self, _event, _data):
        return None

    async def _dispatch(self, spec, value, payload, target, session, *, intervention_id=None, intervention_ids=None):
        self.dispatched.append((spec.flag_key, value, payload, target, intervention_id, intervention_ids))

    async def _revert(self, spec, value, session, *, intervention_ids=None):
        self.reverted.append((spec.flag_key, value, intervention_ids))

    async def evaluate(self, flag_key):
        await self.evaluator.evaluate(SPECS[flag_key], self.session)


def _event_list(epoch, *events):
    """``(epoch, seq, [(seq, payload, decision_id)])`` as the reader returns it."""
    return epoch, max((e[0] for e in events), default=0), list(events)


# --------------------------------------------------------------------------- #
# State flags
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-040", flow="A22")
async def test_a_state_flag_dispatches_once_per_change_and_reverts_only_when_it_was_active():
    h = Harness()
    await h.evaluate("volume_override")  # neutral from an unknown baseline: nothing
    h.reads.state["volume_override"] = 0.4
    await h.evaluate("volume_override")
    await h.evaluate("volume_override")  # unchanged: nothing more
    assert h.dispatched == [("volume_override", 0.4, "", None, None, None)]

    h.reads.state["volume_override"] = SPECS["volume_override"].none_value
    await h.evaluate("volume_override")
    assert h.reverted == [("volume_override", SPECS["volume_override"].none_value, None)]


@pytest.mark.asyncio
@pytest.mark.uc("UC-053", flow="A9", tc="TC-019")
async def test_a_deferred_read_moves_no_baseline():
    h = Harness()
    h.reads.state["volume_override"] = 0.4
    h.lagging = True
    await h.evaluate("volume_override")
    assert h.dispatched == []
    assert "volume_override" not in h.state.existing_state_bucket("g1")

    h.lagging = False
    await h.evaluate("volume_override")
    assert [d[1] for d in h.dispatched] == [0.4]


@pytest.mark.asyncio
async def test_a_retired_session_is_never_fired_into():
    h = Harness()
    h.state.forget("g1")
    h.reads.state["volume_override"] = 0.4
    h.reads.events["end_game"] = _event_list("e1", (1, {}, None))
    await h.evaluate("volume_override")
    await h.evaluate("end_game")
    assert h.dispatched == []
    assert h.state.existing_state_bucket("g1") is None


# --------------------------------------------------------------------------- #
# One-shot event lists (FR-137)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-053", flow="A4", br=["BR-006"])
async def test_each_event_applies_once_and_a_new_epoch_starts_over():
    h = Harness()
    h.reads.events["eliminate_player"] = _event_list("e1", (1, {"serial": "p1"}, "id-1"))
    await h.evaluate("eliminate_player")
    await h.evaluate("eliminate_player")  # same seq: ignored
    h.reads.events["eliminate_player"] = _event_list("e2", (1, {"serial": "p2"}, None))
    await h.evaluate("eliminate_player")
    assert h.dispatched == [
        ("eliminate_player", "p1", "p1", "p1", "id-1", None),
        ("eliminate_player", "p2", "p2", "p2", None, None),
    ]
    assert h.state.existing_event_bucket("g1")["eliminate_player"] == ("e2", 1)


@pytest.mark.asyncio
@pytest.mark.uc("UC-053", flow="A4", br=["BR-006"])
async def test_a_failed_or_baseline_read_applies_nothing_and_an_empty_payload_is_skipped():
    h = Harness()
    h.reads.events["eliminate_player"] = None  # failed read
    await h.evaluate("eliminate_player")
    h.reads.events["eliminate_player"] = _event_list("")  # baseline value
    await h.evaluate("eliminate_player")
    h.reads.events["eliminate_player"] = _event_list("e1", (1, {"serial": ""}, None))  # payload required
    await h.evaluate("eliminate_player")
    assert h.dispatched == []
    assert h.state.existing_event_bucket("g1")["eliminate_player"] == ("e1", 1)


@pytest.mark.asyncio
@pytest.mark.uc("UC-053", flow="A4", br=["BR-006"])
async def test_events_that_fell_out_of_the_list_are_logged_and_the_rest_apply_in_order(caplog):
    h = Harness()
    h.reads.events["audio_cue"] = _event_list("e1", (5, {"sound": "b"}, None), (4, {"sound": "a"}, None))
    with caplog.at_level("WARNING", logger="services.game_coordinator.interventions"):
        await h.evaluate("audio_cue")
    assert [d[1] for d in h.dispatched] == ["a", "b"]
    assert "events 1..3 fell out of the event list" in caplog.text


# --------------------------------------------------------------------------- #
# Per-player grants and levels
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-040", flow="A19", br=["BR-012"])
async def test_a_grant_fires_once_per_new_variant_and_a_failed_read_moves_nothing():
    h = Harness(FakeInterventionGame(serials=("p1", "p2")))
    h.reads.grants[("shield_seconds", "p1")] = ("r1_1", 5.0, "id-1")
    await h.evaluate("shield_seconds")
    await h.evaluate("shield_seconds")  # same (variant, value): not granted again
    h.reads.grants[("shield_seconds", "p1")] = _UNKNOWN
    await h.evaluate("shield_seconds")
    h.reads.grants[("shield_seconds", "p1")] = ("r1_2", 5.0, None)  # a fresh write of the same value
    await h.evaluate("shield_seconds")
    assert h.dispatched == [
        ("shield_seconds", 5.0, "", "p1", "id-1", None),
        ("shield_seconds", 5.0, "", "p1", None, None),
    ]


@pytest.mark.asyncio
@pytest.mark.uc("UC-040", flow="A24", br=["BR-017"])
async def test_levels_dispatch_only_changed_players_and_revert_those_back_at_neutral():
    h = Harness(FakeInterventionGame(serials=("p1", "p2")))
    h.reads.levels[("player_sensitivity_factor", "p1")] = 1.5
    h.reads.level_ids[("player_sensitivity_factor", "p1")] = "id-1"
    await h.evaluate("player_sensitivity_factor")
    assert h.dispatched == [("player_sensitivity_factor", {"p1": 1.5}, "", None, None, {"p1": "id-1"})]

    h.reads.levels[("player_sensitivity_factor", "p1")] = 1.0
    h.reads.levels[("player_sensitivity_factor", "p2")] = _UNKNOWN  # never moves p2's baseline
    await h.evaluate("player_sensitivity_factor")
    assert h.reverted == [("player_sensitivity_factor", {"p1": 1.0}, {"p1": "id-1"})]
    # p2 read neutral in the first pass; its failed read leaves that baseline as it was.
    assert h.state.existing_state_bucket("g1")["player_sensitivity_factor"] == {"p1": 1.0, "p2": 1.0}


@pytest.mark.asyncio
@pytest.mark.uc("UC-040", flow="A24", br=["BR-017"])
async def test_levels_without_a_game_reset_the_baseline():
    h = Harness(game=None)
    h.state.existing_state_bucket("g1")["player_sensitivity_factor"] = {"p1": 1.5}
    await h.evaluate("player_sensitivity_factor")
    assert h.state.existing_state_bucket("g1")["player_sensitivity_factor"] is None
    assert h.dispatched == h.reverted == []
