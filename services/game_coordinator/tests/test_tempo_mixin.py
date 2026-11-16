"""TempoMixin (#2633): the music tempo / intensity driver moved out of BaseGameMode.

Guards the refactor seam, not the tempo behaviour (the UC-031 suites cover that):
- every moved method lives only on TempoMixin and BaseGameMode inherits it;
- the moved code looks up ``metrics``, ``tracer`` and ``read_float_flag`` on
  ``games.base`` at call time, so the many tests that patch them there still reach it.
"""

import sys
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

test_dir = Path(__file__).parent
project_root = test_dir.parent.parent.parent
sys.path.insert(0, str(project_root))
sys.path.insert(0, str(test_dir))

from conftest import MockControllerManagerService, async_noop

from services.game_coordinator.games import base as base_module
from services.game_coordinator.games.base import BaseGameMode
from services.game_coordinator.games.ffa import FFAGame
from services.game_coordinator.games.tempo import TempoMixin

MOVED = (
    "_init_tempo_state",
    "_emit_threshold_metrics",
    "_emit_music_tempo_metric",
    "_start_game_music",
    "_next_change_in",
    "_stop_game_music",
    "_get_music_change_time",
    "_decide_next_change_delay",
    "_read_ramp_seconds",
    "_start_intensity_ramp",
    "_push_audio",
    "_apply_ramp_tempo",
    "_adopt_linear_tempo_ramp",
    "_hold_intensity_now",
    "_check_music_speed",
    "_log_tick_failure",
    "_music_loop",
    "music_speed",
    "tempo_intensity",
)


def make_game() -> FFAGame:
    return FFAGame(
        controller_manager_client=MockControllerManagerService(num_controllers=2),
        event_publisher=async_noop,
        audio_client=AsyncMock(),
        game_id="test_tempo_mixin",
    )


@pytest.mark.uc("UC-031", br=["BR-005"])
class TestTempoMixin:
    def test_moved_names_live_only_on_the_mixin(self):
        assert TempoMixin in FFAGame.__mro__
        for name in MOVED:
            assert name in vars(TempoMixin), name
            assert name not in vars(BaseGameMode), name
            assert getattr(BaseGameMode, name) is getattr(TempoMixin, name), name

    def test_init_sets_the_tempo_state(self):
        game = make_game()
        assert game.tempo_ramp is None
        assert game.tempo_override is None
        assert game.music_loop_task is None
        assert game.speed_up is True
        assert game.tempo_intensity == 0.0

    def test_tempo_metric_goes_through_the_base_module(self):
        game = make_game()
        with patch.object(base_module, "metrics") as m:
            game._emit_music_tempo_metric()
        m.music_tempo.labels.assert_called_once_with(game_kind=game.game_kind, game_id="test_tempo_mixin")
        m.music_tempo.labels.return_value.set.assert_called_once_with(game.music_speed)

    def test_ramp_seconds_flag_goes_through_the_base_module(self):
        game = make_game()
        with patch.object(base_module, "read_float_flag", return_value=2.0) as flag:
            assert game._read_ramp_seconds(rising=True) == 2.0
        assert flag.call_args.args[:2] == ("game", "tempo.ramp_up_seconds")

    @pytest.mark.asyncio
    async def test_tempo_span_goes_through_the_base_module(self):
        game = make_game()
        with (
            patch.object(base_module, "tracer") as tracer,
            patch.object(base_module, "read_float_flag", return_value=1.0),
        ):
            await game._start_intensity_ramp(1.0, scheduled=False)
        tracer.start_as_current_span.assert_called_once_with("music_tempo_change")
