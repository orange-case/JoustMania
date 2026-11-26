"""UC-101 (#2665, coordinator slice): an applied agent intervention is the moment ``intervention``.

The intervention manager reads the intervention id the flag service serves with a value
(``decisionId.<variant>`` metadata of the resolved variant, or an event entry's
``decision_id``; UC-101 BR-003) in the resolution it already makes, and runs the handler
in an intervention scope carrying it. The game method that records the lever's own
effect records the ``intervention`` moment on the player, in the game's shape, with
``intervention.type``, ``intervention.id`` and the moment identity (BR-001, BR-002,
BR-004), and a ground-truth entry that the game root gets at a normal end (FR-222,
BR-006). A blocked, refused, failed or unchanged intervention records none (BR-005), and
a missing or unreadable id never changes the intervention (NFR-061).

Real SDK tracer and logger providers with in-memory exporters (not the global ones).
"""

import asyncio
import os
import sys
import threading
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import pytest
import yaml
from openfeature.flag_evaluation import FlagEvaluationDetails
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

test_dir = Path(__file__).parent
project_root = test_dir.parent.parent.parent
sys.path.insert(0, str(project_root))
sys.path.insert(0, str(test_dir))

from conftest import FakeInterventionGame, MockControllerManagerService, async_noop  # noqa: E402
from event_list_helpers import object_details  # noqa: E402
from test_group_shield import _make as _group_game  # noqa: E402
from test_group_shield import clock, limits, policy, set_mode  # noqa: E402, F401 - fixtures
from test_respawn_elimination_order import _nonstop  # noqa: E402

from lib import moments, otel_logging  # noqa: E402
from lib.types import GameEvent  # noqa: E402
from services.game_coordinator.difficulty_handlers import register_difficulty_handlers  # noqa: E402
from services.game_coordinator.games import base  # noqa: E402
from services.game_coordinator.games.base import Player  # noqa: E402
from services.game_coordinator.games.ffa import FFAGame  # noqa: E402
from services.game_coordinator.games.ground_truth import GroundTruth  # noqa: E402
from services.game_coordinator.games.intervention_moment import (  # noqa: E402
    InterventionScope,
    decision_id_of,
    run_with_intervention_scope,
    take_intervention,
)
from services.game_coordinator.interventions import (  # noqa: E402
    INTERVENTION_SPECS,
    InterventionManager,
    SessionView,
)
from services.game_coordinator.lifecycle_handlers import register_lifecycle_handlers  # noqa: E402

MOMENT = "intervention"
GT_EVENT = "challenge.intervention_ground_truth"
GAME_ID = "game_uc101"
ALL_TYPES = ",".join(sorted(spec.type_id for spec in INTERVENTION_SPECS))
SPECS = {spec.flag_key: spec for spec in INTERVENTION_SPECS}


# --------------------------------------------------------------------------- #
# Fakes and helpers
# --------------------------------------------------------------------------- #
class _Sdk:
    def __init__(self):
        self.spans = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(self.spans))
        self.tracer = provider.get_tracer("test_intervention_moment")
        self.logs = InMemoryLogRecordExporter()
        self.log_provider = LoggerProvider()
        self.log_provider.add_log_record_processor(SimpleLogRecordProcessor(self.logs))

    def player_span(self, serial):
        return self.tracer.start_span("player_lifecycle", attributes={"player.serial": serial})

    def moment_logs(self):
        return [r.log_record for r in self.logs.get_finished_logs() if r.log_record.event_name == MOMENT]

    def moment_spans(self):
        return [s for s in self.spans.get_finished_spans() if s.name == f"game.{MOMENT}"]


@pytest.fixture
def sdk(monkeypatch):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    s = _Sdk()
    monkeypatch.setattr(otel_logging, "_logger_provider", s.log_provider)
    with patch.object(base, "tracer", s.tracer):
        yield s


def _events(span, name=MOMENT):
    return [dict(e.attributes) for e in span.events if e.name == name]


def _names(span):
    return [e.name for e in span.events]


class _Details(FlagEvaluationDetails):
    """Details whose ``flag_metadata`` read raises (an unreadable id, NFR-061)."""

    @property
    def flag_metadata(self):  # type: ignore[override]
        raise RuntimeError("metadata unreadable")

    @flag_metadata.setter
    def flag_metadata(self, _value):
        pass


class IdFlagClient:
    """Interventions domain serving per-serial values with their variant and intervention id.

    ``values[flag] = {serial: (variant, value, decision_id)}`` like a per-player rung of
    the flag service: the id is served as ``decisionId.<variant>`` metadata
    (``metadata`` adds other keys, as the flag service's other rungs and stamps).
    One-shot flags hold an event list, whose entries may carry ``decision_id``.
    """

    def __init__(self):
        self.values: dict = {}
        self.metadata: dict = {"flagSetId": "interventions"}
        self.unreadable_metadata = False

    def _details(self, key, default, ctx):
        entry = self.values.get(key, {}).get(getattr(ctx, "targeting_key", None))
        if entry is None:
            return FlagEvaluationDetails(flag_key=key, value=default, variant="neutral", flag_metadata=self.metadata)
        variant, value, decision_id = entry
        metadata = dict(self.metadata)
        if decision_id is not None:
            metadata[f"decisionId.{variant}"] = decision_id
        cls = _Details if self.unreadable_metadata else FlagEvaluationDetails
        return cls(flag_key=key, value=value, variant=variant, flag_metadata=metadata)

    def get_float_details(self, key, default, ctx=None):
        return self._details(key, default, ctx)

    def get_string_details(self, key, default, ctx=None):
        return self._details(key, default, ctx)

    def get_object_details(self, key, default, ctx=None):  # noqa: ARG002
        return object_details(key, self.values.get(key))

    def get_float_value(self, key, default, ctx=None):
        return self._details(key, default, ctx).value

    def get_string_value(self, key, default, ctx=None):
        return self._details(key, default, ctx).value

    def get_integer_value(self, key, default, ctx=None):  # noqa: ARG002
        return default

    def get_object_value(self, key, default, ctx=None):  # noqa: ARG002
        return default


class AgentStub:
    def __init__(self, allowed=ALL_TYPES):
        self.allowed = allowed

    def get_integer_value(self, key, default, _ctx=None):
        return {"policy.coordinator_backstop_per_minute": 1000, "policy.battery_threshold": 0}.get(key, default)

    def get_float_value(self, _key, default, _ctx=None):
        return default

    def get_string_value(self, key, default, _ctx=None):
        if key == "enabled":
            return "act"
        return self.allowed if key == "interventions_allowed" else default

    def get_object_value(self, _key, default, _ctx=None):
        return default


class RecordingStream:
    def __init__(self):
        self.writes = []

    async def write(self, message):
        self.writes.append(message)


def _ffa(sdk, count=3, shape="span_event"):
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=count),
        event_publisher=async_noop,
        audio_client=None,
        game_id=GAME_ID,
    )
    game.gameplay_stream = RecordingStream()
    game._play_sound = async_noop
    game.running = True
    game.moment_shape = shape
    for i in range(count):
        serial = f"p{i + 1}"
        game.players[serial] = Player(serial=serial, alive=True, color=(255, 0, 0))
        game.players[serial].span = sdk.player_span(serial)
    return game


def _manager(game, allowed=ALL_TYPES):
    events = []

    async def publisher(event_type, data):
        events.append((event_type, data))

    session = SessionView(game_id=GAME_ID, game=game, publish=publisher, is_real=False)
    mgr = InterventionManager(event_publisher=publisher, get_sessions=lambda: [session])
    mgr._interventions_client = IdFlagClient()
    mgr._agent_client = AgentStub(allowed)
    mgr._rate_limiter.set_budget(1000.0)
    register_lifecycle_handlers(mgr)
    register_difficulty_handlers(mgr)
    return mgr, events, session


async def _evaluate(mgr):
    with patch("services.game_coordinator.metrics.interventions_total"):
        await mgr.evaluate_all()


def _published(events, type_id):
    return [d for et, d in events if et == GameEvent.AGENT_INTERVENTION and d["type"] == type_id]


def _gt(game):
    """The FR-222 ground-truth attributes the game would record at a normal end."""
    names = {s: p.name or "" for s, p in game.players.items()}
    return [
        game._ground_truth.intervention_attributes(e, names.get(e.serial, ""))
        for e in game._ground_truth._interventions
    ]


# --------------------------------------------------------------------------- #
# The id is read in the resolution and lands on the moment (main 3-5)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.nfr("NFR-061")
@pytest.mark.uc(
    "UC-101",
    flow="main",
    steps=[4, 5],
    br=["BR-001", "BR-002", "BR-003", "BR-004", "BR-005"],
    cond=["Post-S-2"],
)
async def test_shield_grant_records_the_moment_with_the_served_id(sdk):
    game = _ffa(sdk)
    game.players["p1"].name = "Blue Phoenix"
    mgr, events, _ = _manager(game)
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_3", 5.0, "a1b2c3d4e5f60718")}

    await _evaluate(mgr)

    span = game.players["p1"].span
    (moment,) = _events(span)
    assert moment == {
        "intervention.type": "grant_shield",
        "intervention.id": "a1b2c3d4e5f60718",
        "moment.id": f"{GAME_ID}/p1/1",
        "player.serial": "p1",
        "player.name": "Blue Phoenix",
        "game.id": GAME_ID,
        "game.kind": game.game_kind,
    }
    # In addition to the lever's own event, right after it (BR-001).
    names = _names(span)
    assert names.index(MOMENT) == names.index("shield_granted") + 1
    assert all(_events(game.players[s].span) == [] for s in ("p2", "p3"))
    # BR-008: the outcome record carries no id.
    (outcome,) = _published(events, "grant_shield")
    assert outcome["blocked"] == "false"
    assert "a1b2c3d4e5f60718" not in str(outcome)
    # The same grant served again is no new intervention (BR-005): no second moment.
    await _evaluate(mgr)
    assert len(_events(span)) == 1
    # NFR-061: the id never takes part in change detection, so another id on the same
    # value is no new grant either.
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_3", 5.0, "ffffffffffffffff")}
    await _evaluate(mgr)
    assert len(_events(span)) == 1
    assert len(_published(events, "grant_shield")) == 1


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A3", br=["BR-003", "BR-004"])
async def test_only_the_resolved_variants_id_counts(sdk):
    """The served value is r1_3; an id under another variant (an older value's) is not its id."""
    game = _ffa(sdk)
    mgr, _, _ = _manager(game)
    client = mgr._interventions_client
    client.metadata = {"flagSetId": "interventions", "decisionId.r1_2": "0000000000000002"}
    client.values["shield_seconds"] = {"p1": ("r1_3", 5.0, None)}

    await _evaluate(mgr)

    (moment,) = _events(game.players["p1"].span)
    assert moment["intervention.type"] == "grant_shield"
    assert "intervention.id" not in moment
    (entry,) = _gt(game)
    assert "intervention.id" not in entry


@pytest.mark.parametrize(
    "variant, metadata, expected",
    [
        ("r1_3", {"decisionId.r1_3": "abc"}, "abc"),
        ("r1_3", {"decisionId.r1_2": "abc"}, None),
        ("r1_3", {"decisionId.r1_3": ""}, None),
        ("r1_3", {"decisionId.r1_3": 17}, None),
        ("", {"decisionId.": "abc"}, None),
        (None, {"decisionId.None": "abc"}, None),
        ("r1_3", {}, None),
    ],
)
@pytest.mark.uc("UC-101", flow="A3", br=["BR-003", "BR-004"])
def test_decision_id_of_reads_only_the_resolved_variant(variant, metadata, expected):
    details = FlagEvaluationDetails(flag_key="shield_seconds", value=5.0, variant=variant, flag_metadata=metadata)
    assert decision_id_of(details) == expected


@pytest.mark.asyncio
@pytest.mark.nfr("NFR-061")
@pytest.mark.uc("UC-101", flow="A3", br=["BR-003"], cond=["Post-F-1"])
async def test_an_unreadable_id_never_changes_the_intervention(sdk):
    game = _ffa(sdk)
    mgr, events, _ = _manager(game)
    client = mgr._interventions_client
    client.unreadable_metadata = True
    client.values["shield_seconds"] = {"p1": ("r1_3", 5.0, "abc")}

    await _evaluate(mgr)

    assert game.players["p1"].grace_until > 0  # the shield is applied
    (outcome,) = _published(events, "grant_shield")
    assert outcome["blocked"] == "false"
    (moment,) = _events(game.players["p1"].span)
    assert "intervention.id" not in moment


@pytest.mark.asyncio
@pytest.mark.nfr("NFR-061")
@pytest.mark.uc("UC-101", br=["BR-005"], cond=["Post-F-1"])
async def test_a_failing_moment_never_turns_an_applied_lever_into_handler_error(sdk):
    game = _ffa(sdk)
    mgr, events, _ = _manager(game)
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_3", 5.0, "abc")}

    with patch.object(base, "record_moment", side_effect=RuntimeError("exporter gone")):
        await _evaluate(mgr)

    assert game.players["p1"].grace_until > 0
    (outcome,) = _published(events, "grant_shield")
    assert outcome["blocked"] == "false"
    # No moment was sent, so the ground truth names none (BR-009).
    (entry,) = _gt(game)
    assert "moment.id" not in entry


# --------------------------------------------------------------------------- #
# Event lists: eliminate and revive (BR-001, BR-006)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[5, 7], br=["BR-001", "BR-004", "BR-006"])
async def test_eliminate_event_records_the_moment_at_the_death_time(sdk):
    game = _ffa(sdk)
    span = game.players["p2"].span  # FFA ends the player span with the death
    mgr, _, _ = _manager(game)
    mgr._interventions_client.values["eliminate_player"] = {
        "epoch": "e1",
        "seq": 1,
        "events": [{"seq": 1, "payload": {"serial": "p2"}, "decision_id": "dead0000beef0001"}],
    }

    await _evaluate(mgr)

    assert game.players["p2"].alive is False
    names = _names(span)
    assert names.index(MOMENT) == names.index("player_death") + 1
    death = next(e for e in span.events if e.name == "player_death")
    moment = next(e for e in span.events if e.name == MOMENT)
    assert moment.attributes["intervention.type"] == "eliminate_player"
    assert moment.attributes["intervention.id"] == "dead0000beef0001"
    assert moment.timestamp == death.timestamp  # one clock read (BR-006)
    (entry,) = _gt(game)
    assert entry["challenge.applied_at_ns"] == entry["challenge.first_death_at_ns"] == death.timestamp
    assert entry["moment.id"] == moment.attributes["moment.id"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[5], br=["BR-001"])
async def test_revive_records_the_moment_on_the_native_respawn_path(sdk):
    game = await _nonstop(2)
    dead, other = list(game.players)
    for serial, player in game.players.items():
        player.span = sdk.player_span(serial)
    game.players[dead].alive = False
    mgr, events, _ = _manager(game)
    mgr._interventions_client.values["revive_player"] = {
        "epoch": "e1",
        "seq": 1,
        "events": [{"seq": 1, "payload": {"serial": dead}, "decision_id": "feed0000000000aa"}],
    }

    await _evaluate(mgr)

    assert game.players[dead].alive is True
    (outcome,) = _published(events, "revive_player")
    assert outcome["blocked"] == "false"
    moments = [m for s in game.players.values() for m in _events(s.span)]
    assert [(m["player.serial"], m["intervention.type"], m["intervention.id"]) for m in moments] == [
        (dead, "revive_player", "feed0000000000aa")
    ]


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[5], br=["BR-001"])
async def test_revive_records_the_moment_on_the_generic_path(sdk):
    game = _ffa(sdk, count=2)
    game.players["p1"].alive = False
    scope = InterventionScope("revive_player", serials={"p1"}, default_id="id-generic")

    assert await run_with_intervention_scope(scope, game.revive_player, "p1") is True

    span = game.players["p1"].span
    names = _names(span)
    assert names.index(MOMENT) == names.index("player_revived") + 1
    (moment,) = _events(span)
    assert moment["intervention.id"] == "id-generic"


# --------------------------------------------------------------------------- #
# Group shield: one moment per shielded player, one id (A4)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.parametrize("mode", ["instant", "trigger", "hold"])
@pytest.mark.uc("UC-101", flow="A4", br=["BR-001", "BR-004"])
@pytest.mark.usefixtures("clock", "limits", "policy")
async def test_group_shield_records_one_moment_per_player_with_one_id(sdk, mode):
    game = _group_game()
    for serial, player in game.players.items():
        player.span = sdk.player_span(serial)
    set_mode(game, mode)
    scope = InterventionScope("grant_group_shield", serials=None, default_id="group0000000000a1")

    assert await run_with_intervention_scope(scope, game.grant_group_shield, ["p1", "p2", "p3"], 5.0) is True

    moments = [m for p in game.players.values() for m in _events(p.span)]
    assert len(moments) == 3
    assert {m["intervention.id"] for m in moments} == {"group0000000000a1"}
    assert {m["intervention.type"] for m in moments} == {"grant_group_shield"}
    assert len({m["moment.id"] for m in moments}) == 3
    assert len(_gt(game)) == 3


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
@pytest.mark.usefixtures("clock", "limits", "policy")
async def test_a_refused_group_shield_records_nothing(sdk):
    from services.game_coordinator.intervention_errors import InterventionNotApplied

    game = _group_game()
    for serial, player in game.players.items():
        player.span = sdk.player_span(serial)
    set_mode(game, "instant")
    game.players["p3"].alive = False
    scope = InterventionScope("grant_group_shield", serials=None, default_id="g")

    with pytest.raises(InterventionNotApplied):
        await run_with_intervention_scope(scope, game.grant_group_shield, ["p1", "p2", "p3"], 5.0)

    assert all(_events(p.span) == [] for p in game.players.values())
    assert _gt(game) == []


# --------------------------------------------------------------------------- #
# Levels: per player id, a changed factor only, a return to 1.0 included
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[5], br=["BR-001", "BR-004", "BR-005"])
@pytest.mark.uc("UC-101", flow="A3")
async def test_levels_record_each_changed_player_with_its_own_id(sdk):
    game = _ffa(sdk)
    mgr, _, _ = _manager(game)
    client = mgr._interventions_client
    client.values["player_sensitivity_factor"] = {
        "p1": ("r1_1", 1.5, "1111111111111111"),
        "p2": ("r1_2", 0.8, "2222222222222222"),
    }

    await _evaluate(mgr)

    assert [m["intervention.id"] for m in _events(game.players["p1"].span)] == ["1111111111111111"]
    assert [m["intervention.id"] for m in _events(game.players["p2"].span)] == ["2222222222222222"]
    assert {m["intervention.type"] for p in ("p1", "p2") for m in _events(game.players[p].span)} == {
        "adjust_player_sensitivity"
    }
    assert _events(game.players["p3"].span) == []

    # p1 back to 1.0 by a write with an id: one moment with that id (BR-001, a return to 1.0 included).
    # p2 cleared (the neutral variant, no id): one moment without id (A3).
    client.values["player_sensitivity_factor"] = {"p1": ("r1_3", 1.0, "3333333333333333")}
    await _evaluate(mgr)
    p1 = _events(game.players["p1"].span)
    p2 = _events(game.players["p2"].span)
    assert [m.get("intervention.id") for m in p1] == ["1111111111111111", "3333333333333333"]
    assert [m.get("intervention.id") for m in p2] == ["2222222222222222", None]
    assert game.players["p1"].sensitivity_factor == 1.0


LEVELS = [
    ("player_sensitivity_factor", "sensitivity_factor"),
    ("player_handicap_factor", "handicap_factor"),
]


@pytest.mark.asyncio
@pytest.mark.parametrize("flag, attr", LEVELS)
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
async def test_a_level_that_changes_no_factor_records_nothing(sdk, flag, attr):
    """A new write whose clamped factor equals the current one changed nothing (A2)."""
    game = _ffa(sdk)
    setattr(game.players["p1"], attr, 2.0)
    mgr, _, _ = _manager(game)
    client = mgr._interventions_client
    client.values[flag] = {"p1": ("r1_1", 3.0, "ffff")}  # clamps to 2.0
    await _evaluate(mgr)
    client.values[flag] = {"p1": ("r1_2", 2.5, "eeee")}  # a new write, clamps to 2.0 again
    await _evaluate(mgr)

    assert getattr(game.players["p1"], attr) == 2.0
    assert _events(game.players["p1"].span) == []
    assert _gt(game) == []


@pytest.mark.asyncio
@pytest.mark.parametrize("flag, attr", LEVELS)
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
@pytest.mark.uc("UC-101", flow="A3")
async def test_a_revert_of_a_player_already_at_neutral_records_nothing(sdk, flag, attr):
    """A blocked apply still moves the level baseline, so the later clear reverts a player at 1.0 (A2)."""
    game = _ffa(sdk)
    mgr, events, _ = _manager(game, allowed="adjust_volume")  # the chain blocks the apply
    client = mgr._interventions_client
    client.values[flag] = {"p1": ("r1_1", 1.5, "blocked-apply")}
    await _evaluate(mgr)
    assert getattr(game.players["p1"], attr) == 1.0  # nothing was applied
    assert _events(game.players["p1"].span) == []

    client.values[flag] = {}  # cleared: the neutral variant, a revert of p1 (no chain)
    await _evaluate(mgr)

    assert getattr(game.players["p1"], attr) == 1.0
    assert _events(game.players["p1"].span) == []
    assert _gt(game) == []


# --------------------------------------------------------------------------- #
# Soft penalty and toughness boost (BR-001, BR-005)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[5], br=["BR-001", "BR-005"])
async def test_soft_penalty_warn_and_tighten_each_record_one_moment(sdk):
    game = _ffa(sdk)
    mgr, _, _ = _manager(game)
    client = mgr._interventions_client
    client.values["soft_penalty_action"] = {"p1": ("r1_1", "warn", "w1"), "p2": ("r1_2", "tighten:5:0.6", "t1")}

    await _evaluate(mgr)

    p1 = game.players["p1"].span
    assert [m["intervention.id"] for m in _events(p1)] == ["w1"]
    assert _names(p1).index(MOMENT) > _names(p1).index("death_warning")
    p2 = game.players["p2"].span
    assert [m["intervention.id"] for m in _events(p2)] == ["t1"]
    assert _names(p2).index(MOMENT) == _names(p2).index("soft_penalty_tighten") + 1

    # A re-tighten that neither lengthens the window nor strengthens the factor changed nothing.
    client.values["soft_penalty_action"] = {"p2": ("r1_3", "tighten:1:0.9", "t2")}
    await _evaluate(mgr)
    assert [m["intervention.id"] for m in _events(p2)] == ["t1"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A2", br=["BR-001", "BR-005"])
async def test_toughness_boost_records_only_an_arming_that_changed_something(sdk):
    game = _ffa(sdk)
    mgr, _, _ = _manager(game)
    client = mgr._interventions_client
    client.values["toughness_boost_seconds"] = {"p1": ("r1_1", "10:2.0", "b1")}
    await _evaluate(mgr)
    span = game.players["p1"].span
    assert [m["intervention.id"] for m in _events(span)] == ["b1"]
    assert _names(span).index(MOMENT) == _names(span).index("toughness_boost_armed") + 1

    client.values["toughness_boost_seconds"] = {"p1": ("r1_2", "1:1.5", "b2")}  # shorter and weaker: kept
    await _evaluate(mgr)
    assert [m["intervention.id"] for m in _events(span)] == ["b1"]
    assert _names(span).count("toughness_boost_armed") == 2  # the lever event itself is unchanged


# --------------------------------------------------------------------------- #
# Nothing applied, nothing recorded (A2, BR-005)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
async def test_a_blocked_intervention_records_no_moment(sdk):
    game = _ffa(sdk)
    mgr, events, _ = _manager(game, allowed="adjust_volume")
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_1", 5.0, "x")}

    await _evaluate(mgr)

    assert _published(events, "grant_shield")[0]["blocked"] == "true"
    assert _events(game.players["p1"].span) == []
    assert _gt(game) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
async def test_a_refused_or_kept_shield_records_no_moment(sdk):
    game = _ffa(sdk)
    mgr, events, _ = _manager(game)
    game.players["p1"].alive = False  # grant_shield returns False: not applied
    game.players["p2"].grace_until = 1e12  # a longer protection runs: kept, nothing changed
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_1", 5.0, "x"), "p2": ("r1_2", 5.0, "y")}

    await _evaluate(mgr)

    assert _events(game.players["p1"].span) == []
    assert _events(game.players["p2"].span) == []
    assert _gt(game) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
async def test_a_handler_error_before_the_effect_records_none(sdk):
    game = _ffa(sdk)
    mgr, events, _ = _manager(game)

    async def failing(_ctx):
        raise RuntimeError("before the effect")

    mgr.register_handler("shield_seconds", failing)
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_1", 5.0, "x")}
    await _evaluate(mgr)

    assert _published(events, "grant_shield")[0]["block_reason"] == "handler_error"
    assert _events(game.players["p1"].span) == []


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A2", br=["BR-005"])
@pytest.mark.uc("UC-053", flow="A9")
async def test_a_deferred_grant_records_only_when_the_re_evaluation_applies_it(sdk):
    game = _ffa(sdk)
    mgr, _, _ = _manager(game)
    client = mgr._interventions_client
    client.values["shield_seconds"] = {"p1": ("r1_1", 5.0, "first")}

    with patch.object(InterventionManager, "_allow_list_lags", return_value=True):
        await _evaluate(mgr)
    assert _events(game.players["p1"].span) == []

    client.values["shield_seconds"] = {"p1": ("r1_1", 5.0, "served-now")}
    await _evaluate(mgr)
    assert [m["intervention.id"] for m in _events(game.players["p1"].span)] == ["served-now"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", br=["BR-001"])
async def test_game_wide_and_cosmetic_levers_record_no_moment(sdk):
    game = _ffa(sdk)
    mgr, _, session = _manager(game)
    game.players["p1"].handicap_factor = 0.6  # a laggard the rubberband boosts
    for flag, value in (("auto_rubberband", "strong"), ("global_sensitivity_override", 3)):
        with patch("services.game_coordinator.metrics.interventions_total"):
            await mgr._enforce_and_dispatch(SPECS[flag], value, "", None, session, intervention_id="x")
    assert all(_events(p.span) == [] for p in game.players.values())
    assert _gt(game) == []


# --------------------------------------------------------------------------- #
# The scope: one tag per player, named players only, closed after the handler
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.uc("UC-101", br=["BR-004", "BR-005"])
async def test_the_scope_gives_one_tag_per_named_player_while_the_handler_runs():
    scope = InterventionScope("grant_shield", serials={"p1"}, default_id="id1")
    seen = {}

    async def handler():
        seen["p2"] = take_intervention("p2")
        seen["other_type"] = take_intervention("p1", ("eliminate_player",))
        seen["first"] = take_intervention("p1", ("grant_shield",))
        seen["second"] = take_intervention("p1")
        return asyncio.create_task(asyncio.sleep(0))

    task = await run_with_intervention_scope(scope, handler)
    await task
    assert seen["p2"] is None and seen["other_type"] is None and seen["second"] is None
    assert (seen["first"].type, seen["first"].id) == ("grant_shield", "id1")
    assert take_intervention("p1") is None  # outside the scope
    later = InterventionScope("grant_shield", serials=None)
    await run_with_intervention_scope(later, async_noop)
    assert later.take("p9") is None  # closed: a task the handler spawned records nothing


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", br=["BR-004", "BR-005"])
async def test_a_targeted_dispatch_tags_only_its_target(sdk):
    """A handler that records for a second player (a cascade) tags nobody but the target (defence in depth)."""
    game = _ffa(sdk)
    mgr, _, session = _manager(game)

    async def touches_two_players(_ctx):
        game.record_intervention("p1")
        game.record_intervention("p2")

    mgr.register_handler("shield_seconds", touches_two_players)
    with patch("services.game_coordinator.metrics.interventions_total"):
        await mgr._enforce_and_dispatch(SPECS["shield_seconds"], 5.0, "", "p1", session, intervention_id="t1")

    assert [m["intervention.id"] for m in _events(game.players["p1"].span)] == ["t1"]
    assert _events(game.players["p2"].span) == []
    assert [e.serial for e in game._ground_truth._interventions] == ["p1"]


@pytest.fixture
def game_loop_thread():
    """A game loop in its own thread, like GameSession's."""
    loop = asyncio.new_event_loop()
    thread = threading.Thread(target=loop.run_forever, daemon=True)
    thread.start()
    yield loop
    loop.call_soon_threadsafe(loop.stop)
    thread.join(timeout=3.0)
    loop.close()


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="main", steps=[5], br=["BR-001", "BR-004"])
async def test_a_late_effect_still_records_the_moment_with_its_id_when_it_lands(sdk, game_loop_thread):
    """The dispatcher gives up (game_loop_late) but the handler keeps running on the game loop.

    The scope belongs to the handler call, not to the dispatcher's wait: the lever that
    lands after the timeout is still tagged (BR-001: where the lever's own effect is recorded).
    """
    game = _ffa(sdk)
    mgr, events, session = _manager(game)
    late_session = replace(session, loop=game_loop_thread)
    mgr._get_sessions = lambda: [late_session]
    mgr._dispatch_timeout_s = 0.1
    mgr._interventions_client.values["shield_seconds"] = {"p1": ("r1_3", 5.0, "1a7e0000000000aa")}
    real = mgr._handlers["shield_seconds"]
    landed = threading.Event()

    async def lands_late(ctx):
        await asyncio.sleep(0.5)  # well past the dispatch timeout
        try:
            return await real(ctx)
        finally:
            landed.set()

    mgr.register_handler("shield_seconds", lands_late)

    await _evaluate(mgr)

    (outcome,) = _published(events, "grant_shield")
    assert outcome["block_reason"] == "game_loop_late"
    assert outcome["blocked"] == "false"  # outcome unknown, the effect may still apply
    assert not landed.is_set() and _events(game.players["p1"].span) == []  # not landed yet: no moment yet
    assert await asyncio.to_thread(landed.wait, 3.0)
    assert game.players["p1"].grace_until > 0  # the lever landed
    (moment,) = _events(game.players["p1"].span)
    assert moment["intervention.type"] == "grant_shield"
    assert moment["intervention.id"] == "1a7e0000000000aa"
    (entry,) = _gt(game)
    assert entry["moment.id"] == moment["moment.id"]


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", br=["BR-001"])
async def test_the_fake_game_and_every_lever_dispatch_open_a_scope():
    """The handlers' difficulty path calls game.record_intervention (Protocol member, no getattr)."""
    game = FakeInterventionGame(serials=("p1", "p2"))
    mgr, _, _ = _manager(game)
    session = SessionView(game_id=GAME_ID, game=game, publish=mgr._publish)
    with patch("services.game_coordinator.metrics.interventions_total"):
        await mgr._enforce_and_dispatch(
            SPECS["player_handicap_factor"], {"p1": 1.4}, "", None, session, intervention_ids={"p1": "h1"}
        )
    assert [(s, t.type, t.id) for s, t in game.interventions] == [("p1", "set_player_handicap", "h1")]


# --------------------------------------------------------------------------- #
# Shapes (main 1-2, BR-002, A1)
# --------------------------------------------------------------------------- #
@pytest.mark.asyncio
@pytest.mark.parametrize("shape", ["span", "log_event"])
@pytest.mark.uc("UC-101", flow="main", steps=[5], br=["BR-002"])
@pytest.mark.uc("UC-101", flow="A1")
async def test_the_moment_follows_the_game_shape(sdk, shape):
    game = _ffa(sdk, shape=shape)
    scope = InterventionScope("grant_shield", serials={"p1"}, default_id="s1")
    await run_with_intervention_scope(scope, game.grant_shield, "p1", 5.0)
    span = game.players["p1"].span

    if shape == "span":
        (moment_span,) = sdk.moment_spans()
        assert moment_span.parent.span_id == span.get_span_context().span_id
        assert moment_span.start_time == moment_span.end_time
        assert moment_span.attributes["intervention.id"] == "s1"
        assert _events(span) == []
        moment_id = moment_span.attributes["moment.id"]
    else:
        (record,) = sdk.moment_logs()
        assert record.attributes["intervention.type"] == "grant_shield"
        moment_id = record.attributes["moment.id"]
        assert _events(span) == []
    (entry,) = _gt(game)
    assert entry["moment.id"] == moment_id


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", flow="A1", br=["BR-002"])
async def test_a_log_shape_without_log_channel_falls_back_to_the_span_event(sdk, monkeypatch):
    monkeypatch.setattr(otel_logging, "_logger_provider", None)
    monkeypatch.setattr(moments, "_logger_cache", None)
    game = _ffa(sdk, shape="log_event")
    await run_with_intervention_scope(InterventionScope("grant_shield", serials={"p1"}), game.grant_shield, "p1", 5.0)
    (event,) = _events(game.players["p1"].span)
    assert event["intervention.type"] == "grant_shield"


@pytest.mark.asyncio
@pytest.mark.uc("UC-101", br=["BR-001"])
async def test_shadow_games_record_the_moment_too(sdk):
    game = _ffa(sdk)
    game.game_kind = "shadow"
    await run_with_intervention_scope(InterventionScope("grant_shield", serials={"p1"}), game.grant_shield, "p1", 5.0)
    (event,) = _events(game.players["p1"].span)
    assert event["game.kind"] == "shadow"


# --------------------------------------------------------------------------- #
# Ground truth (main 7, BR-006, A7)
# --------------------------------------------------------------------------- #
@pytest.mark.uc("UC-101", flow="main", steps=[7], br=["BR-006", "BR-007"])
def test_ground_truth_names_the_first_death_at_or_after_the_apply_time():
    truth = GroundTruth()
    with patch("services.game_coordinator.games.ground_truth.time_ns", side_effect=[100, 300, 500]):
        truth.record_death("p1")  # 100: before the intervention, ignored
        shield = truth.record_intervention("p1", "grant_shield", "id1")  # 300
        truth.record_death("p1")  # 500
    shield.moment_id = "g/p1/2"
    survivor = truth.record_intervention("p2", "toughness_boost", None, at_ns=400)

    assert truth.intervention_attributes(shield, "Blue") == {
        "intervention.type": "grant_shield",
        "intervention.id": "id1",
        "player.serial": "p1",
        "player.name": "Blue",
        "moment.id": "g/p1/2",
        "challenge.applied_at_ns": 300,
        "challenge.first_death_at_ns": 500,
    }
    assert truth.intervention_attributes(survivor) == {
        "intervention.type": "toughness_boost",
        "player.serial": "p2",
        "challenge.applied_at_ns": 400,
    }


@pytest.mark.uc("UC-101", flow="main", steps=[7], br=["BR-006"])
@pytest.mark.uc("UC-101", flow="A7")
def test_the_game_root_gets_the_entries_at_a_normal_end_only(sdk):
    game = _ffa(sdk)
    game._ground_truth.record_intervention("p1", "grant_shield", "id1")
    root = sdk.tracer.start_span("game")
    game._game_root_span = root
    game._record_ground_truth()
    (entry,) = _events(root, GT_EVENT)
    assert entry["intervention.id"] == "id1"
    assert _names(root).index(GT_EVENT) > max(i for i, n in enumerate(_names(root)) if n == "challenge.ground_truth")

    forced = _ffa(sdk)
    forced._ground_truth.record_intervention("p1", "grant_shield", "id1")
    forced_root = sdk.tracer.start_span("game")
    forced._game_root_span = forced_root
    with patch.object(type(forced), "_game_outcome_attributes", return_value={"game.outcome": "force_ended"}):
        forced._record_ground_truth()
    assert _events(forced_root, GT_EVENT) == []


# --------------------------------------------------------------------------- #
# Span event limit (telemetry.md §3.2)
# --------------------------------------------------------------------------- #
@pytest.mark.uc("UC-101", flow="main", steps=[7], br=["BR-006"])
def test_the_coordinator_provider_honours_the_span_event_limit(monkeypatch):
    """OTEL_SPAN_EVENT_COUNT_LIMIT=1024 keeps the FR-219 and the FR-222 events of a long game.

    The SDK drops the OLDEST events past the limit, so at the default 128 the FR-222
    entries (recorded last) push out the FR-219 ones.
    """
    from lib import telemetry

    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    monkeypatch.setattr(telemetry, "OTLPSpanExporter", lambda **_kw: InMemorySpanExporter())

    def build(limit):
        if limit is None:
            monkeypatch.delenv("OTEL_SPAN_EVENT_COUNT_LIMIT", raising=False)
        else:
            monkeypatch.setenv("OTEL_SPAN_EVENT_COUNT_LIMIT", limit)
        built = {}
        monkeypatch.setattr(telemetry.trace, "set_tracer_provider", lambda p: built.setdefault("provider", p))
        monkeypatch.setattr(telemetry, "_get_bsp_schedule_delay_ms", lambda _name: 5000)
        telemetry._do_init()
        provider = built["provider"]
        exporter = InMemorySpanExporter()
        provider.add_span_processor(SimpleSpanProcessor(exporter))
        truth = GroundTruth()
        for i in range(100):
            truth.record_death(f"s{i}")
        for i in range(200):
            truth.record_intervention(f"s{i % 100}", "grant_shield", f"id{i}")
        root = provider.get_tracer("t").start_span("game")
        truth.record_on(root, {f"s{i}": "" for i in range(100)})
        root.end()
        events = exporter.get_finished_spans()[0].events
        provider.shutdown()
        return [e.name for e in events]

    raised = build("1024")
    assert raised.count("challenge.ground_truth") == 100 and raised.count(GT_EVENT) == 200
    default = build(None)
    assert len(default) == 128 and "challenge.ground_truth" not in default  # oldest dropped first


@pytest.mark.uc("UC-101", br=["BR-006"])
def test_compose_raises_the_span_event_limit_of_the_coordinator_only():
    def env(path):
        services = yaml.safe_load((project_root / path).read_text())["services"]
        out = {}
        for name, svc in services.items():
            raw = svc.get("environment") or {}
            if isinstance(raw, list):
                raw = dict(item.split("=", 1) for item in raw)
            out[name] = {k: str(v) for k, v in dict(raw).items()}
        return out

    base_env = env("docker-compose.yml")
    assert base_env["game-coordinator"]["OTEL_SPAN_EVENT_COUNT_LIMIT"] == "1024"
    assert all("OTEL_SPAN_EVENT_COUNT_LIMIT" not in v for n, v in base_env.items() if n != "game-coordinator")
    # The CI file replaces the coordinator's environment (!override): it must set it again.
    ci = (project_root / "docker-compose.ci.yml").read_text()
    ci_block = ci.split("\n  game-coordinator:", 1)[1].split("\n  # ", 1)[0]
    assert 'OTEL_SPAN_EVENT_COUNT_LIMIT: "1024"' in ci_block
    assert os.environ.get("OTEL_SPAN_EVENT_COUNT_LIMIT") in (None, "1024")
