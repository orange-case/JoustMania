"""Menu voice lines share one audio group, so a new line replaces the previous one (UC-031, #2918)."""

import asyncio
from unittest.mock import MagicMock, patch

import pytest

from lib.types import Sound
from services.menu.utils.audio import MENU_VOICE_GROUP, AudioHelper


class _RecordingStub:
    requests: list = []

    def __init__(self, _channel):
        pass

    async def PlaySound(self, request, **_kwargs):  # noqa: N802 - gRPC stub name
        type(self).requests.append(request)
        return MagicMock(success=True)


@pytest.fixture
def audio():
    _RecordingStub.requests = []
    with patch("proto.audio_pb2_grpc.AudioServiceStub", _RecordingStub):
        yield AudioHelper(MagicMock())


async def _drain(helper):
    await asyncio.wait_for(asyncio.gather(*helper._sound_tasks), timeout=1.0)


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="main")
async def test_mode_voice_lines_are_sent_in_the_menu_voice_group(audio):
    await audio.play_game_mode_voice("JoustFFA")
    await audio.play_voice(Sound.MENU_VOX_LABEL_AGENT)
    await _drain(audio)

    assert [r.group for r in _RecordingStub.requests] == [MENU_VOICE_GROUP, MENU_VOICE_GROUP]
    assert MENU_VOICE_GROUP == "menu_voice"


@pytest.mark.asyncio
@pytest.mark.uc("UC-031", flow="main")
async def test_sound_effects_stay_ungrouped(audio):
    await audio.play_sound(Sound.SFX_BEEP)
    await _drain(audio)

    assert [r.group for r in _RecordingStub.requests] == [""]
