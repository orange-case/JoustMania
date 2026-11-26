"""Unit tests for the enforcement chain on its own (intervention_policy.py)."""

import pytest

from services.game_coordinator.intervention_policy import (
    BLOCK_LOW_BATTERY,
    BLOCK_MODE_UNSUPPORTED,
    BLOCK_NOT_ALLOWED,
    BLOCK_RATE_LIMITED,
    EnforcementChain,
    _game_mode_name,
    _RateLimiter,
)
from services.game_coordinator.interventions import INTERVENTION_SPECS
from services.game_coordinator.tests.conftest import FakeInterventionGame

UC040_BR004 = pytest.mark.uc("UC-040", br=["BR-004"])

ELIMINATE = next(s for s in INTERVENTION_SPECS if s.flag_key == "eliminate_player")
REVIVE = next(s for s in INTERVENTION_SPECS if s.flag_key == "revive_player")


class Policy:
    """Mutable policy inputs; the chain reads them at check time."""

    def __init__(self, allowed=("eliminate_player", "revive_player"), threshold=20.0, battery=None):
        self.allowed = set(allowed)
        self.threshold = threshold
        self.battery = dict(battery or {})

    def chain(self, limiter: _RateLimiter, mode_deny=None) -> EnforcementChain:
        return EnforcementChain(
            limiter,
            allowed_types=lambda: self.allowed,
            battery_pct=self.battery.get,
            battery_threshold=lambda: self.threshold,
            mode_deny=mode_deny or {},
        )


def _limiter(budget=10.0):
    return _RateLimiter(budget=budget, time_fn=lambda: 0.0)


@UC040_BR004
def test_an_allowed_intervention_passes_and_reserves_its_weight():
    limiter = _limiter()
    reason, token = Policy().chain(limiter).check(ELIMINATE, "p1", FakeInterventionGame())
    assert reason is None
    assert token is not None
    assert limiter.current_weight() == ELIMINATE.weight


@UC040_BR004
def test_a_type_outside_the_allow_list_is_blocked_without_spending_budget():
    limiter = _limiter()
    reason, token = Policy(allowed=()).chain(limiter).check(ELIMINATE, "p1", FakeInterventionGame())
    assert (reason, token) == (BLOCK_NOT_ALLOWED, None)
    assert limiter.current_weight() == 0.0


@UC040_BR004
def test_the_allow_list_is_read_at_check_time():
    policy = Policy(allowed=())
    chain = policy.chain(_limiter())
    assert chain.check(ELIMINATE, "p1", None)[0] == BLOCK_NOT_ALLOWED
    policy.allowed.add(ELIMINATE.type_id)
    assert chain.check(ELIMINATE, "p1", None)[0] is None


@UC040_BR004
def test_the_mode_matrix_blocks_before_the_battery_guard_and_the_backstop():
    limiter = _limiter()
    chain = Policy(battery={"p1": 5.0}).chain(limiter, mode_deny={"FFA": frozenset({REVIVE.type_id})})
    assert chain.check(REVIVE, "p1", FakeInterventionGame("FFA")) == (BLOCK_MODE_UNSUPPORTED, None)
    assert limiter.current_weight() == 0.0


@UC040_BR004
def test_a_low_battery_target_is_blocked_and_an_unknown_battery_is_not():
    limiter = _limiter()
    chain = Policy(battery={"low": 10.0}).chain(limiter)
    assert chain.check(ELIMINATE, "low", None) == (BLOCK_LOW_BATTERY, None)
    assert chain.check(ELIMINATE, "unknown", None)[0] is None
    assert limiter.current_weight() == ELIMINATE.weight


@UC040_BR004
def test_an_exhausted_backstop_blocks_as_rate_limited():
    limiter = _limiter(budget=ELIMINATE.weight)
    chain = Policy().chain(limiter)
    assert chain.check(ELIMINATE, "p1", None)[0] is None
    assert chain.check(ELIMINATE, "p1", None) == (BLOCK_RATE_LIMITED, None)


@pytest.mark.uc("UC-040", flow="A31")
def test_the_returned_reservation_refunds_exactly_its_own_weight():
    limiter = _limiter()
    chain = Policy().chain(limiter)
    _, first = chain.check(ELIMINATE, "p1", None)
    chain.check(ELIMINATE, "p2", None)
    limiter.refund(first)
    assert limiter.current_weight() == ELIMINATE.weight


def test_game_mode_name_fails_open():
    class Raising(FakeInterventionGame):
        def get_game_name(self):
            raise RuntimeError("boom")

    assert _game_mode_name(None) == ""
    assert _game_mode_name(Raising()) == ""
    assert _game_mode_name(FakeInterventionGame("Zombie")) == "Zombie"
