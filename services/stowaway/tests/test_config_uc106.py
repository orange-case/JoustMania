"""UC-106 BR-003 / BR-004: the stowaway flags, read at use time, the kill switch failing closed."""

import pytest

from services.stowaway.config import (
    INTENSITY_CEILING,
    MAX_SPAN_TARGET,
    Overrides,
    Settings,
    SettingsSource,
    clamp_intensity,
)
from services.stowaway.tests.fakes import FlagState


@pytest.mark.uc("UC-106", flow="A1", br=["BR-003"])
def test_a_failing_read_is_the_kill_switch_off():
    def boom():
        raise RuntimeError("flagd down")

    assert SettingsSource(read=boom).get().enabled is False


@pytest.mark.uc("UC-106", cond=["Post-F-1"])
def test_defaults_emit_nothing():
    settings = Settings()
    assert not settings.enabled
    assert not settings.telemetry_on()
    assert not settings.players_on()


@pytest.mark.uc("UC-106", br=["BR-004"])
@pytest.mark.parametrize(
    ("value", "expected"),
    [(1.5, 1.5), (99.0, INTENSITY_CEILING), (-1.0, 0.0), (float("nan"), 1.0), (True, 1.0), ("2", 1.0)],
)
def test_intensity_is_clamped_to_the_ceiling(value, expected):
    assert clamp_intensity(value) == expected


@pytest.mark.uc("UC-106", br=["BR-007"])
def test_overrides_replace_flags_but_never_the_kill_switch():
    flags = FlagState(enabled=False, challenge="none")
    source = SettingsSource(
        read=flags, overrides=Overrides(challenge="span-rounds", mode="both", intensity=50.0, span_target=10**9)
    )
    settings = source.get()
    assert settings.challenge == "span-rounds"
    assert settings.mode == "both"
    assert settings.intensity == INTENSITY_CEILING
    assert settings.span_target == MAX_SPAN_TARGET
    assert settings.enabled is False


@pytest.mark.uc("UC-106", flow="main", steps=[2], br=["BR-003"])
def test_settings_are_at_most_max_age_old():
    now = [0.0]
    flags = FlagState(enabled=True)
    source = SettingsSource(read=flags, max_age=0.2, clock=lambda: now[0])
    assert source.get().enabled
    flags.set(enabled=False)
    now[0] = 0.1
    assert source.get().enabled  # cached
    now[0] = 0.2
    assert source.get().enabled is False
    assert flags.reads == 2


@pytest.mark.uc("UC-106", br=["BR-006"])
def test_unknown_flag_values_read_as_their_safe_default():
    flags = FlagState(enabled=True, challenge="bogus", mode="bogus")
    settings = SettingsSource(read=flags, overrides=Overrides(challenge="nope", mode="nope")).get()
    assert settings.challenge == "none"
    assert settings.mode == "telemetry"


@pytest.mark.uc("UC-106", br=["BR-003", "BR-004"])
def test_read_flagd_normalizes_every_flag(monkeypatch):
    import lib.feature_flags as ff
    from services.stowaway import config

    values = {
        config.FLAG_ENABLED: True,
        config.FLAG_CHALLENGE: "swing-density",
        config.FLAG_MODE: "players",
        config.FLAG_INTENSITY: 9.0,
        config.FLAG_SPAN_TARGET: 100_000,
    }
    seen = []

    def reader(domain, key, default, game_id=None):  # noqa: ARG001
        seen.append((domain, key, default))
        return values[key]

    for name in ("read_bool_flag", "read_string_flag", "read_float_flag", "read_int_flag"):
        monkeypatch.setattr(ff, name, reader)
    settings = config.read_flagd()
    assert settings == Settings(True, "swing-density", "players", INTENSITY_CEILING, 100_000)
    assert (config.DOMAIN, config.FLAG_ENABLED, False) in seen  # the kill switch's fallback is off
