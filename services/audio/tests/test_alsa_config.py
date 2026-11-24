"""Tests for ALSA card auto-detection and configuration."""

from unittest.mock import mock_open, patch

from services.audio.alsa_config import (
    ASOUND_CONF_PATH,
    _auto_detect_card,
    _pick_best_card,
    _resolve_card_number,
    _write_asound_conf,
    configure_alsa_device,
    get_detected_card,
)


class TestResolveCardNumber:
    def test_explicit_card_number(self):
        assert _resolve_card_number("1") == 1

    def test_explicit_card_zero(self):
        assert _resolve_card_number("0") == 0

    @patch("services.audio.alsa_config._auto_detect_card", return_value=2)
    def test_auto_calls_detect(self, mock_detect):
        assert _resolve_card_number("auto") == 2
        mock_detect.assert_called_once()

    @patch("services.audio.alsa_config._auto_detect_card", return_value=0)
    def test_invalid_string_falls_back_to_auto(self, mock_detect):
        assert _resolve_card_number("bad") == 0
        mock_detect.assert_called_once()


class TestPickBestCard:
    @patch("alsaaudio.card_name", side_effect=[("vc4-hdmi-0",), ("vc4-hdmi-1",), ("USB Audio",)])
    def test_prefers_usb_over_hdmi(self, _mock_name):
        idx, name = _pick_best_card([0, 1, 2])
        assert idx == 2
        assert name == "USB Audio"

    @patch("alsaaudio.card_name", side_effect=[("vc4-hdmi-0",), ("USB Audio",), ("vc4-hdmi-1",)])
    def test_picks_first_usb_card(self, _mock_name):
        idx, name = _pick_best_card([0, 1, 2])
        assert idx == 1
        assert name == "USB Audio"

    @patch("alsaaudio.card_name", side_effect=[("vc4-hdmi-0",), ("Headphones",)])
    def test_falls_back_to_first_when_no_usb(self, _mock_name):
        idx, name = _pick_best_card([0, 1])
        assert idx == 0
        assert name == "vc4-hdmi-0"

    @patch("alsaaudio.card_name", return_value=("usb audio device",))
    def test_case_insensitive_usb_match(self, _mock_name):
        idx, name = _pick_best_card([3])
        assert idx == 3

    @patch("alsaaudio.card_name", side_effect=[Exception("bad card"), ("USB Audio",)])
    def test_unknown_name_on_error(self, _mock_name):
        idx, name = _pick_best_card([0, 1])
        assert idx == 1
        assert name == "USB Audio"

    @patch("alsaaudio.card_name", side_effect=[Exception("bad"), Exception("bad")])
    def test_all_unknown_falls_back_to_first(self, _mock_name):
        idx, name = _pick_best_card([5, 6])
        assert idx == 5
        assert name == "unknown"


class TestAutoDetectCard:
    @patch("services.audio.alsa_config.time.sleep")
    @patch("services.audio.alsa_config._pick_best_card", return_value=(0, "Headphones"))
    @patch("alsaaudio.card_indexes", return_value=[0, 1])
    def test_delegates_to_pick_best_card(self, mock_indexes, mock_pick, _mock_sleep):
        assert _auto_detect_card() == 0
        mock_pick.assert_called_once_with([0, 1])

    @patch("services.audio.alsa_config.time.sleep")
    @patch("services.audio.alsa_config._pick_best_card", return_value=(2, "USB Audio"))
    @patch("alsaaudio.card_indexes", return_value=[0, 1, 2])
    def test_returns_usb_card_from_pick(self, mock_indexes, mock_pick, _mock_sleep):
        assert _auto_detect_card() == 2

    @patch("services.audio.alsa_config.time.sleep")
    @patch("alsaaudio.card_indexes", return_value=[])
    def test_empty_cards_returns_zero_after_retries(self, mock_indexes, mock_sleep):
        assert _auto_detect_card() == 0
        # Should have retried _DETECT_MAX_RETRIES times
        assert mock_indexes.call_count == 3
        assert mock_sleep.call_count == 2  # sleeps between retries, not after last

    @patch("services.audio.alsa_config.time.sleep")
    @patch("alsaaudio.card_indexes", side_effect=Exception("no ALSA"))
    def test_exception_returns_zero_after_retries(self, mock_indexes, mock_sleep):
        assert _auto_detect_card() == 0
        assert mock_indexes.call_count == 3

    @patch("services.audio.alsa_config.time.sleep")
    @patch("services.audio.alsa_config._pick_best_card", return_value=(1, "USB Audio"))
    @patch("alsaaudio.card_indexes", side_effect=[[], [1]])
    def test_retry_succeeds_on_second_attempt(self, mock_indexes, mock_pick, mock_sleep):
        assert _auto_detect_card() == 1
        assert mock_indexes.call_count == 2
        mock_sleep.assert_called_once()

    @patch("services.audio.alsa_config.time.sleep")
    @patch("services.audio.alsa_config._pick_best_card", return_value=(0, "Headphones"))
    @patch("alsaaudio.card_indexes", side_effect=[Exception("not ready"), [0]])
    def test_retry_after_exception_succeeds(self, mock_indexes, mock_pick, mock_sleep):
        assert _auto_detect_card() == 0
        assert mock_indexes.call_count == 2


class TestWriteAsoundConf:
    @patch("builtins.open", mock_open())
    def test_writes_config_with_card_number(self):
        _write_asound_conf(2)
        handle = open(ASOUND_CONF_PATH, "w")  # noqa: SIM115
        written = handle.write.call_args[0][0]
        assert 'pcm "hw:2,0"' in written
        assert "card 2" in written

    @patch("builtins.open", side_effect=OSError("permission denied"))
    def test_handles_write_error(self, mock_file):
        # Should not raise
        _write_asound_conf(0)


class TestConfigureAlsaDevice:
    @patch("services.audio.alsa_config._write_asound_conf")
    @patch("services.audio.alsa_config._resolve_card_number", return_value=1)
    def test_calls_resolve_and_write(self, mock_resolve, mock_write):
        configure_alsa_device("auto")
        mock_resolve.assert_called_once_with("auto")
        mock_write.assert_called_once_with(1)

    @patch("services.audio.alsa_config._write_asound_conf")
    @patch("services.audio.alsa_config._resolve_card_number", return_value=3)
    def test_explicit_card_passed_through(self, mock_resolve, mock_write):
        configure_alsa_device("3")
        mock_resolve.assert_called_once_with("3")
        mock_write.assert_called_once_with(3)

    @patch("services.audio.alsa_config._write_asound_conf")
    @patch("services.audio.alsa_config._resolve_card_number", return_value=2)
    def test_stores_detected_card(self, mock_resolve, mock_write):
        configure_alsa_device("auto")
        assert get_detected_card() == 2


class TestGetDetectedCard:
    def test_default_is_zero(self):
        import services.audio.alsa_config as mod

        original = mod._detected_card
        try:
            mod._detected_card = 0
            assert get_detected_card() == 0
        finally:
            mod._detected_card = original

    @patch("services.audio.alsa_config._write_asound_conf")
    @patch("services.audio.alsa_config._resolve_card_number", return_value=5)
    def test_updated_by_configure(self, _mock_resolve, _mock_write):
        configure_alsa_device("5")
        assert get_detected_card() == 5


import asyncio
import multiprocessing
import os

import pytest

from services.audio import alsa_config
from services.audio.alsa_config import (
    CARD_CHANGE_EXIT_CODE,
    preferred_card,
    read_proc_cards,
    read_sysfs_cards,
    should_restart_for_card_change,
    watch_card_changes,
)

HDMI = (0, "[vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0")
USB = (2, "[Audio          ]: USB-Audio - USB Audio")


@pytest.mark.uc("UC-031", flow="A6", br=["BR-008"])
class TestCardChangeDecision:
    def test_usb_appeared_restarts(self):
        assert should_restart_for_card_change(0, [HDMI, USB], failed=False)

    def test_same_preferred_card_no_restart(self):
        assert not should_restart_for_card_change(2, [HDMI, USB], failed=False)

    def test_usb_removed_without_failure_no_restart(self):
        assert not should_restart_for_card_change(2, [HDMI], failed=False)

    def test_usb_removed_and_playback_failed_restarts(self):
        assert should_restart_for_card_change(2, [HDMI], failed=True)

    def test_no_cards_no_restart(self):
        assert not should_restart_for_card_change(0, [], failed=True)

    def test_preferred_card_rule(self):
        assert preferred_card([HDMI, USB]) == 2
        assert preferred_card([HDMI]) == 0

    def test_read_proc_cards(self, tmp_path):
        f = tmp_path / "cards"
        f.write_text(
            " 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0\n                      vc4-hdmi-0\n"
            " 2 [Audio          ]: USB-Audio - USB Audio\n                      C-Media USB Audio at usb-1\n"
        )
        assert [i for i, _ in read_proc_cards(str(f))] == [0, 2]
        assert read_proc_cards(str(tmp_path / "missing")) == []


def _run_watcher(cards, *, is_busy=lambda: False, failed=False, max_polls=6):
    """Run the watcher with a plain-function /proc reader (a Mock would record every call) and a hard bound."""
    calls = []

    def fake_cards():
        return cards

    async def go():
        await asyncio.wait_for(
            watch_card_changes(0, lambda: calls.append(1), interval_s=0, is_busy=is_busy, max_polls=max_polls),
            timeout=5,
        )

    with (
        patch.object(alsa_config, "read_cards", new=fake_cards),
        patch.object(alsa_config, "playback_failed", new=lambda: failed),
    ):
        asyncio.run(go())
    return calls


@pytest.mark.uc("UC-031", flow="A6", br=["BR-008"])
class TestWatcher:
    def test_logs_warn_once_and_calls_back(self, caplog):
        with caplog.at_level("WARNING"):
            calls = _run_watcher([HDMI, USB])
        assert calls == [1]
        assert sum("ALSA card change" in r.message for r in caplog.records) == 1
        assert CARD_CHANGE_EXIT_CODE == 75

    def test_no_change_ends_at_max_polls_without_callback(self):
        # The mutation "decision always True" is caught by test_logs_warn_once; the mirror (never True) ends bounded.
        assert _run_watcher([HDMI]) == []

    def test_deferred_while_busy(self):
        assert _run_watcher([HDMI, USB], is_busy=lambda: True) == []

    def test_busy_but_playback_failed_restarts(self):
        assert _run_watcher([HDMI, USB], is_busy=lambda: True, failed=True) == [1]

    def test_restarts_in_lobby_waits_in_game(self):
        in_lobby = {"v": False}
        assert _run_watcher([HDMI, USB], is_busy=lambda: not in_lobby["v"]) == []
        in_lobby["v"] = True
        assert _run_watcher([HDMI, USB], is_busy=lambda: not in_lobby["v"]) == [1]

    def test_restarts_once_idle(self):
        busy = iter([True, True, False])
        assert _run_watcher([HDMI, USB], is_busy=lambda: next(busy, False)) == [1]

    def test_flapping_card_resets_stability(self):
        seq = iter([[HDMI, USB], [HDMI]] * 3)
        calls = []

        async def go():
            await asyncio.wait_for(watch_card_changes(0, lambda: calls.append(1), interval_s=0, max_polls=6), timeout=5)

        with patch.object(alsa_config, "read_cards", new=lambda: next(seq)):
            asyncio.run(go())
        assert calls == []


def _fake_sysfs(root, cards):
    """Build /sys/class/sound-like entries: card<N>/id and a device link into a USB or platform path."""
    devices = root / "devices"
    sound = root / "class" / "sound"
    sound.mkdir(parents=True)
    for idx, card_id, usb in cards:
        dev = devices / ("usb1/1-1/1-1.3/1-1.3:1.0" if usb else f"platform/soc/hdmi{idx}")
        dev.mkdir(parents=True)
        card = sound / f"card{idx}"
        card.mkdir()
        (card / "id").write_text(card_id + "\n")
        os.symlink(dev, card / "device")
    (sound / "controlC0").mkdir()  # not a card entry
    (sound / "timer").mkdir()
    return sound


@pytest.mark.uc("UC-031", flow="A6", br=["BR-008"])
class TestSysfsCards:
    """#2715: Docker masks /proc/asound, so the watcher reads the cards from sysfs."""

    def test_read_sysfs_cards_marks_usb_by_bus(self, tmp_path):
        sound = _fake_sysfs(tmp_path, [(0, "vc4hdmi0", False), (2, "Audio", True)])
        cards = read_sysfs_cards(str(sound))
        assert [i for i, _ in cards] == [0, 2]
        assert preferred_card(cards) == 2
        assert read_sysfs_cards(str(tmp_path / "missing")) == []

    def test_watcher_finds_hotplugged_usb_card_with_proc_masked(self, tmp_path):
        sound = _fake_sysfs(tmp_path, [(0, "vc4hdmi0", False), (2, "Audio", True)])
        calls = []

        async def go():
            await asyncio.wait_for(watch_card_changes(0, lambda: calls.append(1), interval_s=0, max_polls=4), timeout=5)

        with (
            patch.object(alsa_config, "SYSFS_SOUND_PATH", str(sound)),
            patch.object(alsa_config, "PROC_CARDS_PATH", str(tmp_path / "masked")),
        ):
            asyncio.run(go())
        assert calls == [1]

    def test_proc_cards_fallback_when_sysfs_empty(self, tmp_path):
        proc = tmp_path / "cards"
        proc.write_text(" 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0\n")
        with (
            patch.object(alsa_config, "SYSFS_SOUND_PATH", str(tmp_path / "nosys")),
            patch.object(alsa_config, "PROC_CARDS_PATH", str(proc)),
        ):
            assert [i for i, _ in alsa_config.read_cards()] == [0]

    def test_start_rule_counts_usb_bus_card_as_usb(self, tmp_path):
        # Start-up and watcher must prefer the same card, or a USB card without "USB" in its name restarts forever.
        sound = _fake_sysfs(tmp_path, [(0, "vc4hdmi0", False), (2, "Speaker", True)])
        with (
            patch.object(alsa_config, "SYSFS_SOUND_PATH", str(sound)),
            patch("alsaaudio.card_name", side_effect=lambda i: ("vc4-hdmi-0", "") if i == 0 else ("Speaker", "")),
        ):
            assert _pick_best_card([0, 2])[0] == 2
            assert preferred_card(read_sysfs_cards(str(sound))) == 2


def _fail_in_music_process(flag):
    """Child-process stand-in for the music loop: adopts the inherited flag, then fails like _open_pcm."""
    alsa_config.adopt_playback_failed_flag(flag)
    alsa_config.note_playback_failure()


@pytest.fixture
def _clear_playback_failed():
    alsa_config.playback_failed_flag().value = 0
    yield
    alsa_config.playback_failed_flag().value = 0


@pytest.mark.uc("UC-031", flow="A6", br=["BR-008"])
@pytest.mark.usefixtures("_clear_playback_failed")
class TestPlaybackFailureAcrossProcesses:
    """#2715: music plays in a subprocess; its failure must reach the watcher in the parent."""

    def test_failure_in_child_process_is_seen_by_parent(self):
        p = multiprocessing.Process(target=_fail_in_music_process, args=(alsa_config.playback_failed_flag(),))
        p.start()
        p.join(timeout=30)
        assert p.exitcode == 0
        assert alsa_config.playback_failed()

    def test_no_failure_reads_false(self):
        assert not alsa_config.playback_failed()
