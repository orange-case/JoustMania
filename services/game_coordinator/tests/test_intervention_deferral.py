"""Unit tests for the cross-set consistency gate on its own (intervention_deferral.py)."""

import asyncio
import logging

import pytest

from services.game_coordinator.intervention_deferral import DEFERRAL_WARN_AFTER_S, AllowListGate
from services.game_coordinator.interventions import INTERVENTION_SPECS

UC053_A9 = pytest.mark.uc("UC-053", flow="A9", tc="TC-019")
SPEC = INTERVENTION_SPECS[0]


class Probes:
    def __init__(self, held=None, allow=None, *, interventions=True, agent=True):
        self.held = held
        self.allow = allow
        self.has_interventions_client = interventions
        self.has_agent_client = agent
        self.allow_reads = 0

    def interventions_snapshot(self):
        if isinstance(self.held, Exception):
            raise self.held
        return self.held

    def allow_list_snapshot(self):
        self.allow_reads += 1
        return self.allow


class Clock:
    def __init__(self):
        self.now = 100.0

    def __call__(self):
        return self.now


def _gate(probes, clock=None, spawned=None, recheck_s=lambda: 5.0):
    return AllowListGate(
        probes,
        time_fn=clock or Clock(),
        spawn_evaluation=(spawned.append if spawned is not None else lambda _loop: None),
        recheck_s=recheck_s,
    )


@UC053_A9
def test_no_interventions_client_or_an_unstamped_set_is_not_gated():
    assert _gate(Probes(held=("e1", 3), interventions=False)).lags(SPEC) is False
    assert _gate(Probes(held=None, allow=None)).lags(SPEC) is False


@UC053_A9
def test_an_allow_list_at_least_as_new_passes_and_an_older_or_other_epoch_defers():
    assert _gate(Probes(held=("e1", 3), allow=("e1", 3))).lags(SPEC) is False
    assert _gate(Probes(held=("e1", 3), allow=("e1", 2))).lags(SPEC) is True
    assert _gate(Probes(held=("e1", 3), allow=("e0", 9))).lags(SPEC) is True
    assert _gate(Probes(held=("e1", 3), allow=None)).lags(SPEC) is True


@UC053_A9
def test_a_raising_probe_defers():
    assert _gate(Probes(held=RuntimeError("probe down"))).lags(SPEC) is True


@UC053_A9
def test_the_pass_cache_skips_the_allow_list_reread_until_the_next_pass():
    probes = Probes(held=("e1", 3), allow=("e1", 4))
    gate = _gate(probes)
    assert gate.lags(SPEC) is False
    assert gate.lags(SPEC) is False
    assert probes.allow_reads == 1
    gate.begin_pass()
    gate.lags(SPEC)
    assert probes.allow_reads == 2


@UC053_A9
def test_a_lasting_deferral_warns_once_per_interval(caplog):
    clock = Clock()
    gate = _gate(Probes(held=("e2", 1), allow=("e1", 9)), clock)
    with caplog.at_level(logging.DEBUG, logger="services.game_coordinator.interventions"):
        gate.lags(SPEC)
        clock.now += DEFERRAL_WARN_AFTER_S + 1
        gate.lags(SPEC)
        clock.now += 1
        gate.lags(SPEC)
    warnings = [r for r in caplog.records if r.levelno == logging.WARNING]
    assert len(warnings) == 1
    assert "('e2', 1)" in warnings[0].getMessage()


@pytest.mark.asyncio
@UC053_A9
async def test_a_deferral_schedules_one_recheck_and_ending_it_cancels_the_recheck():
    probes = Probes(held=("e2", 1), allow=("e1", 9))
    spawned = []
    gate = _gate(probes, spawned=spawned, recheck_s=lambda: 0)
    gate.lags(SPEC)
    handle = gate.recheck
    gate.lags(SPEC)
    assert gate.recheck is handle  # one pending re-check per episode
    await asyncio.sleep(0.01)
    assert spawned == [asyncio.get_running_loop()]
    assert gate.recheck is None

    gate.lags(SPEC)
    pending = gate.recheck
    gate.end()
    assert pending.cancelled()
    assert gate.since is None and gate.recheck is None


def test_without_a_running_loop_no_recheck_is_scheduled():
    gate = _gate(Probes(held=("e2", 1), allow=("e1", 9)))
    assert gate.lags(SPEC) is True
    assert gate.recheck is None
