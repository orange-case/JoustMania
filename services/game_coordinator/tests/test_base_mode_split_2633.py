"""Focused tests for the pieces split out of BaseGameMode (#2633).

The split is a pure refactor: these pin the contracts the extracted helpers and
mixins must keep (the MRO, the patch points on ``games.base``, the per-frame trigger
helpers, the rubberband maths and the game loop's per-run state).
"""

import sys
from pathlib import Path
from unittest.mock import patch

import pytest

sys.path.insert(0, str(Path(__file__).parent.parent.parent.parent))
sys.path.insert(0, str(Path(__file__).parent))

from conftest import MockControllerManagerService, async_noop

from services.game_coordinator.games import base as base_module
from services.game_coordinator.games.base import SHIELD_CHARGE_RESEND_MARGIN_SECONDS, BaseGameMode, Player
from services.game_coordinator.games.elimination import EliminationMixin
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.games.game_loop import GameLoopMixin, _LoopState
from services.game_coordinator.games.gameplay_stream import GameplayStreamMixin
from services.game_coordinator.games.motion import MotionMixin
from services.game_coordinator.games.outcome import GameOutcomeMixin
from services.game_coordinator.games.player_telemetry import PlayerTelemetryMixin
from services.game_coordinator.games.rubberband import RubberbandMixin
from services.game_coordinator.games.shields import ShieldMixin
from services.game_coordinator.games.tempo import TempoMixin

MIXINS = (
    ShieldMixin,
    TempoMixin,
    GameLoopMixin,
    MotionMixin,
    GameplayStreamMixin,
    EliminationMixin,
    GameOutcomeMixin,
    RubberbandMixin,
    PlayerTelemetryMixin,
)


def _game() -> FFAGame:
    game = FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=2),
        event_publisher=async_noop,
        audio_client=None,
        game_id="test_split_2633",
    )
    for serial in ("p1", "p2"):
        game.players[serial] = Player(serial=serial, alive=True, color=(255, 0, 0))
    return game


@pytest.mark.uc("UC-030", flow="main")
class TestMixinComposition:
    def test_every_mixin_is_a_base_of_base_game_mode(self):
        assert all(m in BaseGameMode.__mro__ for m in MIXINS)

    def test_no_member_is_defined_by_two_mixins(self):
        """A shared name would let the MRO pick a different function than before the split."""
        seen: dict[str, str] = {}
        clashes = []
        for mixin in MIXINS:
            for name in vars(mixin):
                if name.startswith("__"):
                    continue
                if name in seen:
                    clashes.append(f"{name}: {seen[name]} and {mixin.__name__}")
                seen[name] = mixin.__name__
        assert clashes == []

    def test_base_game_mode_overrides_no_mixin_member(self):
        own = {n for n in vars(BaseGameMode) if not n.startswith("__")}
        mixed = {n for m in MIXINS for n in vars(m) if not n.startswith("__")}
        assert own & mixed == set()

    def test_init_groups_state_by_concern(self):
        game = _game()
        assert game._kills_in_progress == set()  # EliminationMixin._init_elimination_state
        assert game._latched_outcome is None  # GameOutcomeMixin._init_outcome_state
        assert game.gameplay_stream is None  # GameplayStreamMixin._init_stream_state
        assert game._shield_hold is None  # ShieldMixin._init_shield_state
        assert game._players_span is None  # PlayerTelemetryMixin._init_span_state


@pytest.mark.uc("UC-030", flow="main")
class TestPatchPointsStayOnBase:
    def test_health_window_reads_the_base_module_clock(self):
        player = Player(serial="p1", alive=True, color=(0, 0, 0), total_poll_drops=10)
        with patch.object(base_module.time, "monotonic", side_effect=[100.0, 103.0]):
            PlayerTelemetryMixin._update_poll_drop_rate(player)
            player.total_poll_drops = 40
            PlayerTelemetryMixin._update_poll_drop_rate(player)
        assert player._health_drop_rate == pytest.approx(10.0)

    def test_gameplay_spans_use_the_base_module_tracer(self):
        game = _game()
        with patch.object(base_module, "tracer", autospec=True) as tracer:
            game._open_gameplay_spans(tracer.start_span.return_value)
        names = [call.args[0] for call in tracer.start_span.call_args_list]
        assert names[:2] == ["game.players", "game.cycle"]


@pytest.mark.uc("UC-030", flow="A6", br=["BR-005"])
class TestTriggerHelpers:
    def test_charge_edge_only_on_an_armed_press(self):
        player = Player(serial="p1", alive=True, color=(0, 0, 0))
        player.trigger_armed = True
        assert MotionMixin._read_charge_trigger(player, True) is True
        assert MotionMixin._read_charge_trigger(player, True) is False  # held: no new edge
        assert MotionMixin._read_charge_trigger(player, False) is False
        assert player.trigger_armed is True
        assert MotionMixin._read_charge_trigger(player, True) is True

    def test_charge_due_at_expiry_and_resend(self):
        player = Player(serial="p1", alive=True, color=(0, 0, 0))
        assert not MotionMixin._shield_charge_due(player, 10.0)  # no charge held
        player.shield_charge_seconds = 3.0
        player.shield_charge_expires_at = 20.0
        player.shield_charge_layer_until = 30.0
        assert not MotionMixin._shield_charge_due(player, 10.0)
        assert MotionMixin._shield_charge_due(player, 20.0)  # expiry
        player.shield_charge_expires_at = 40.0
        resend_at = player.shield_charge_layer_until - SHIELD_CHARGE_RESEND_MARGIN_SECONDS
        assert MotionMixin._shield_charge_due(player, resend_at)  # re-send before the layer ends


@pytest.mark.uc("UC-030", flow="main", steps=[5])
class TestRubberbandHelpers:
    def test_window_per_strength(self):
        game = _game()
        assert game._rubberband_window("gentle", None) == (
            game.RUBBERBAND_MAX_BOOST_GENTLE,
            game.RUBBERBAND_DEFAULT_SECONDS,
        )
        assert game._rubberband_window("strong", 999.0) == (
            game.RUBBERBAND_MAX_BOOST_STRONG,
            game.RUBBERBAND_MAX_SECONDS,
        )
        assert game._rubberband_window("wild", 5.0) is None
        assert game._rubberband_window("gentle", 0.0) is None

    def test_target_boost_never_passes_the_leader(self):
        game = _game()
        laggard = Player(serial="p1", alive=True, color=(0, 0, 0))
        laggard.smoothed_accel = 1.0
        # leader headroom 0.2 above the laggard's: the no-invert cap wins over max_boost.
        boost = game._rubberband_target_boost(laggard, 0.0, 1.0, 0.2, 0.2, 0.4)
        assert boost == pytest.approx((0.2 + 1.0) / 1.0)
        assert boost * 1.0 - laggard.smoothed_accel <= 0.2 + 1e-9


@pytest.mark.uc("UC-030", flow="main")
def test_loop_state_frame_times_are_per_run():
    first = _LoopState(
        loop_start_time=0.0, last_iteration_time=0.0, target_frame_time_ms=16.7, last_alive_serials=set()
    )
    second = _LoopState(
        loop_start_time=0.0, last_iteration_time=0.0, target_frame_time_ms=16.7, last_alive_serials=set()
    )
    first.recent_frame_times.append(1.0)
    assert second.recent_frame_times == []
    assert (first.max_reconnect_attempts, first.reconnect_attempt, first.frame_failed) == (3, 0, False)
