"""The pad's own voices with a screen (UC-016 BR-005, A1): silent while the display link is up.

The real CuePlayer and Previewer over a recording audio stub; the link state is a switch.
"""

import pytest

from lib.setting_ladders import MODE_LADDER
from services.operator_pad.cues import (
    CANCEL_CUE,
    FAILURE_CUE,
    LIMIT_CUES,
    LOCK_VOICES,
    NO_CONNECTION_CUE,
    REFUSAL_CUE,
    UNKNOWN_KEY_CUE,
    CuePlayer,
)
from services.operator_pad.tests.test_preview import NUMBER, TEAMS, Rig


def rig(link_up: bool) -> Rig:
    made = Rig()
    made.cues.screen_up = lambda: link_up
    return made


async def own_cues(cues: CuePlayer) -> None:
    await cues.refusal()
    await cues.limit(True)
    await cues.cancel()
    await cues.lock(True)
    await cues.announce(REFUSAL_CUE, CANCEL_CUE)  # any two clips: the announcement is what is gated


@pytest.mark.uc("UC-016", flow="main", br=["BR-005"])
async def test_the_pads_own_cues_are_silent_while_the_link_is_up():
    up = rig(True)
    await own_cues(up.cues)
    await up.time.advance(2.0)
    assert up.files() == []


@pytest.mark.uc("UC-016", flow="A1", br=["BR-005"])
async def test_the_pads_own_cues_play_with_the_link_down():
    down = rig(False)
    await own_cues(down.cues)
    await down.time.advance(2.0)
    for sound in (REFUSAL_CUE, LIMIT_CUES[True], CANCEL_CUE, LOCK_VOICES[True]):
        assert sound.value in down.files(), sound


@pytest.mark.uc("UC-016", flow="main", br=["BR-005"])
@pytest.mark.parametrize("link_up", [True, False])
async def test_the_failure_beeps_play_either_way_and_the_words_only_without_a_screen(link_up):
    each = rig(link_up)
    await each.cues.failure()
    await each.time.advance(2.0)
    await each.cues.drain()
    assert each.files().count(FAILURE_CUE.value) == 2, "the failure beeps warn without a look at the screen"
    assert (NO_CONNECTION_CUE.value in each.files()) is not link_up


@pytest.mark.uc("UC-016", flow="main", br=["BR-005"])
@pytest.mark.parametrize("link_up", [True, False])
async def test_the_unknown_key_sound_stays_either_way(link_up):
    each = rig(link_up)
    await each.cues.unknown_key()
    assert each.files() == [UNKNOWN_KEY_CUE.value]


@pytest.mark.uc("UC-016", flow="main", br=["BR-005"], steps=[4])
async def test_the_preview_speaks_nothing_while_the_link_is_up():
    up = rig(True)
    for net in range(1, 3):
        up.step(net)
        await up.time.advance(0.6)
    assert up.files() == []


@pytest.mark.uc("UC-016", flow="A1", br=["BR-005"], steps=[4])
async def test_the_preview_speaks_with_the_link_down():
    down = rig(False)
    down.step(1)
    await down.time.advance(1.0)
    assert down.files() == [TEAMS, NUMBER[3]]


@pytest.mark.uc("UC-016", flow="main", br=["BR-005"], steps=[4])
async def test_the_preview_led_pulse_still_happens_while_the_link_is_up():
    up = rig(True)
    up.step_mode(1)
    await up.time.advance(0.6)
    assert up.shown() == [MODE_LADDER[1]]
    assert up.files() == []


@pytest.mark.uc("UC-016", flow="main", br=["BR-005"])
async def test_a_screen_check_that_raises_counts_as_no_screen():
    def broken() -> bool:
        raise RuntimeError("display bug")

    each = rig(False)
    each.cues.screen_up = broken
    await each.cues.refusal()
    assert each.files() == [REFUSAL_CUE.value]
