"""The `operator` flag set readers (flags.md §10a, UC-015 Pre-2, BR-007)."""

from unittest.mock import create_autospec, patch

import pytest
from openfeature.client import OpenFeatureClient

from services.operator_pad import config as config_module
from services.operator_pad.config import DEFAULT_KEYMAP, PadConfig, Timings, parse_device, parse_keymap


@pytest.mark.uc("UC-015", cond=["Pre-2"])
def test_default_keymap_matches_the_verified_hardware_codes():
    assert parse_keymap(DEFAULT_KEYMAP) == {
        183: "K1",
        184: "K2",
        185: "KNOB_PRESS",
        186: "KNOB_TURN_A",
        187: "KNOB_TURN_B",
        188: "K3",
        189: "K4",
        190: "K5",
        191: "K6",
        192: "K7",
        193: "K8",
    }


@pytest.mark.uc("UC-015", cond=["Pre-2"])
@pytest.mark.parametrize("raw", ["", "K1=F13,K1=F14", "K1=F13,K2=F13", "K9=F13", "K1=G13", "K1"])
def test_malformed_keymaps_are_rejected(raw):
    assert parse_keymap(raw) is None


@pytest.mark.uc("UC-015", flow="A4")
@pytest.mark.parametrize(
    ("raw", "expected"), [("cb00:1337", ("cb00", "1337")), ("CB00:1337", ("cb00", "1337")), ("", None), ("cb00", None)]
)
def test_device_ids(raw, expected):
    assert parse_device(raw) == expected


def _client(values: dict):
    client = create_autospec(OpenFeatureClient, instance=True)
    client.get_integer_value.side_effect = lambda key, default: values.get(key, default)
    client.get_string_value.side_effect = lambda key, default: values.get(key, default)
    client.get_boolean_value.side_effect = lambda key, default: values.get(key, default)
    return client


@pytest.mark.uc("UC-015", flow="main", br=["BR-007"])
def test_timings_are_read_at_use_time_with_defaults_for_bad_values():
    values = {"operator_pad.tap_ms": 250, "operator_pad.hold_end_ms": -1}
    config = PadConfig(_client(values))
    timings = config.timings()
    assert timings.tap_ms == 250
    assert (timings.hold_agent_ms, timings.hold_end_ms, timings.hold_lock_ms, timings.hold_reset_ms) == (
        1000,
        1500,
        3000,
        1000,
    )
    values["operator_pad.tap_ms"] = 300
    assert config.timings().tap_ms == 300


@pytest.mark.uc("UC-015", cond=["Pre-2"])
def test_malformed_keymap_flag_falls_back_to_the_default():
    config = PadConfig(_client({"operator_pad.keymap": "nonsense"}))
    assert config.keymap() == parse_keymap(DEFAULT_KEYMAP)


@pytest.mark.uc("UC-015", flow="A4")
def test_unreadable_flags_keep_the_defaults():
    client = create_autospec(OpenFeatureClient, instance=True)
    client.get_integer_value.side_effect = RuntimeError("no provider")
    client.get_string_value.side_effect = RuntimeError("no provider")
    client.get_boolean_value.side_effect = RuntimeError("no provider")
    config = PadConfig(client)
    assert config.timings().tap_ms == 400
    assert config.device() == ("cb00", "1337")
    assert config.enabled() is True


@pytest.mark.uc("UC-015", flow="main", br=["BR-007"])
@pytest.mark.parametrize(
    "values",
    [
        {"operator_pad.hold_agent_ms": 300},
        {"operator_pad.hold_end_ms": 400},
        {"operator_pad.tap_ms": 2000},
    ],
    ids=["agent-hold-below-tap", "end-hold-equal-tap", "tap-above-holds"],
)
def test_a_hold_not_above_the_tap_limit_keeps_the_default_timings(values):
    with patch.object(config_module, "logger") as log:
        timings = PadConfig(_client(values)).timings()
    assert timings == Timings()
    log.warning.assert_called_once()


@pytest.mark.uc("UC-015", flow="main", br=["BR-005"], steps=[8])
@pytest.mark.parametrize("value", [1, 2, 4, 6, 12])
def test_knob_events_per_step_in_range_is_used(value):
    config = PadConfig(_client({"operator_pad.knob_events_per_step": value}))
    assert config.knob_events_per_step() == value
    assert config.timings().knob_events_per_step == value


@pytest.mark.uc("UC-015", flow="main", br=["BR-005"], steps=[8])
@pytest.mark.parametrize("value", [0, -3, 13, 100, True, "4", 2.5])
def test_knob_events_per_step_outside_1_to_12_or_malformed_falls_back_to_1_with_a_warning(value):
    with patch.object(config_module, "logger") as log:
        config = PadConfig(_client({"operator_pad.knob_events_per_step": value}))
        assert config.knob_events_per_step() == 1
    log.warning.assert_called_once()


@pytest.mark.uc("UC-015", flow="main", br=["BR-005"], steps=[8])
def test_knob_events_per_step_defaults_to_one_per_detent():
    """The rev 5 pad's detented knob sends one raw event per detent (#2742)."""
    assert PadConfig(_client({})).knob_events_per_step() == 1
    assert Timings().knob_events_per_step == 1


@pytest.mark.uc("UC-015", flow="main", br=["BR-005", "BR-007"])
def test_default_timings_after_a_bad_hold_keep_the_valid_knob_setting():
    config = PadConfig(_client({"operator_pad.hold_end_ms": 400, "operator_pad.knob_events_per_step": 2}))
    assert config.timings() == Timings(knob_events_per_step=2)


@pytest.mark.uc("UC-015", flow="main", br=["BR-017"], steps=[5])
@pytest.mark.parametrize("value", [0, 100, 150, 300, 1000])
def test_preview_debounce_in_range_is_used(value):
    assert PadConfig(_client({"operator_pad.preview_debounce_ms": value})).preview_debounce_ms() == value


@pytest.mark.uc("UC-015", flow="main", br=["BR-017"], steps=[5])
@pytest.mark.parametrize("value", [-1, 1001, True, "150", 1.5])
def test_preview_debounce_outside_0_to_1000_or_malformed_falls_back_to_150_with_a_warning(value):
    with patch.object(config_module, "logger") as log:
        assert PadConfig(_client({"operator_pad.preview_debounce_ms": value})).preview_debounce_ms() == 150
    log.warning.assert_called_once()


@pytest.mark.uc("UC-015", flow="main", br=["BR-017"], steps=[5])
def test_preview_flags_default_on_and_150_and_survive_an_unreadable_client():
    assert PadConfig(_client({})).preview_enabled() is True
    assert PadConfig(_client({})).preview_debounce_ms() == 150
    assert PadConfig(_client({"operator_pad.preview_enabled": False})).preview_enabled() is False
    client = create_autospec(OpenFeatureClient, instance=True)
    client.get_integer_value.side_effect = RuntimeError("no provider")
    client.get_boolean_value.side_effect = RuntimeError("no provider")
    assert PadConfig(client).preview_enabled() is True
    assert PadConfig(client).preview_debounce_ms() == 150


# --- UC-016: the display flags (flags.md §10a) ------------------------------------------------------


@pytest.mark.uc("UC-016", cond=["Pre-3"])
@pytest.mark.parametrize(
    ("method", "key", "value", "expected"),
    [
        ("display_poll_ms", "operator_pad.display_poll_ms", 250, 250),
        ("display_poll_ms", "operator_pad.display_poll_ms", 199, 500),
        ("display_poll_ms", "operator_pad.display_poll_ms", 5001, 500),
        ("display_poll_ms", "operator_pad.display_poll_ms", True, 500),
        ("display_poll_ms", "operator_pad.display_poll_ms", "fast", 500),
        ("display_result_ms", "operator_pad.display_result_ms", 3000, 3000),
        ("display_result_ms", "operator_pad.display_result_ms", 299, 1500),
        ("display_result_ms", "operator_pad.display_result_ms", 10001, 1500),
        ("display_autopage_ms", "operator_pad.display_autopage_ms", 0, 0),  # BR-009 `off`
        ("display_autopage_ms", "operator_pad.display_autopage_ms", 3000, 3000),
        ("display_autopage_ms", "operator_pad.display_autopage_ms", 999, 5000),
        ("display_autopage_ms", "operator_pad.display_autopage_ms", 60001, 5000),
        ("display_autopage_ms", "operator_pad.display_autopage_ms", False, 5000),
        ("display_home_ms", "operator_pad.display_home_ms", 30000, 30000),
        ("display_home_ms", "operator_pad.display_home_ms", 1999, 10000),
        ("display_home_ms", "operator_pad.display_home_ms", 0, 10000),
        ("internet_check_ms", "operator_pad.internet_check_ms", 60000, 60000),
        ("internet_check_ms", "operator_pad.internet_check_ms", 4999, 30000),
        ("internet_check_ms", "operator_pad.internet_check_ms", 300001, 30000),
    ],
)
def test_display_flags_defaults_ranges_malformed(method, key, value, expected):
    client = create_autospec(OpenFeatureClient, instance=True)
    client.get_integer_value.return_value = value
    assert getattr(PadConfig(client), method)() == expected
    assert client.get_integer_value.call_args.args[0] == key


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("google.com:443", ("google.com", 443)),
        ("1.1.1.1:443", ("1.1.1.1", 443)),
        ("[2606:4700::1111]:443", ("2606:4700::1111", 443)),
        ("", None),
        (" ", None),
    ],
)
def test_internet_target_parses_host_port_and_off(raw, expected):
    assert config_module.parse_target(raw) == expected
    assert PadConfig(_client({"operator_pad.internet_check_target": raw})).internet_check_target() == expected


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
@pytest.mark.parametrize(
    "raw",
    [
        "google.com",
        "google.com:0",
        "google.com:65536",
        ":443",
        "2606:4700::1111:443",
        "[]:443",
        "[x]:443",
        "a b:443",
        "host:44a",
        "[::1]",
    ],
)
def test_malformed_internet_target_keeps_the_last_good_value(raw):
    values = {"operator_pad.internet_check_target": ""}
    config = PadConfig(_client(values))
    assert config.internet_check_target() is None  # `off` read once
    values["operator_pad.internet_check_target"] = raw
    with pytest.raises(ValueError):
        config_module.parse_target(raw)
    assert config.internet_check_target() is None  # malformed: still off


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
def test_a_failed_target_read_keeps_off_and_the_default_only_before_the_first_good_read():
    client = create_autospec(OpenFeatureClient, instance=True)
    client.get_string_value.side_effect = RuntimeError("flagd down")
    assert PadConfig(client).internet_check_target() == ("google.com", 443)
    client = create_autospec(OpenFeatureClient, instance=True)
    client.get_string_value.side_effect = lambda _key, _default: ""
    config = PadConfig(client)
    assert config.internet_check_target() is None
    client.get_string_value.side_effect = RuntimeError("flagd down")
    assert config.internet_check_target() is None
    # The OpenFeature client answers the default it is given when it cannot resolve: the last good value.
    client.get_string_value.side_effect = lambda _key, default: default
    assert config.internet_check_target() is None


@pytest.mark.uc("UC-016", cond=["Pre-3"])
def test_a_bad_display_flag_warns_once_per_value_not_per_read():
    client = create_autospec(OpenFeatureClient, instance=True)
    config = PadConfig(client)
    with patch.object(config_module.logger, "warning") as warning:
        client.get_integer_value.return_value = 9
        for _ in range(5):
            config.display_poll_ms()
        client.get_integer_value.return_value = 8
        config.display_poll_ms()
    assert warning.call_count == 2


@pytest.mark.uc("UC-016", flow="A1", cond=["Pre-3"])
@pytest.mark.parametrize(
    ("value", "raises", "expected"), [(True, False, True), (False, False, False), (None, True, True)]
)
def test_display_enabled_default_on_and_unreadable_keeps_it_on(value, raises, expected):
    client = create_autospec(OpenFeatureClient, instance=True)
    if raises:
        client.get_boolean_value.side_effect = RuntimeError("flagd down")
    else:
        client.get_boolean_value.return_value = value
    assert PadConfig(client).display_enabled() is expected
