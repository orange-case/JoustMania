"""UC-043 A17 on the service tier (T2): a hold experiment smoke test (#2096 PR-8, second half).

AGENT composition: the real agent (built at HEAD), game coordinator, controller manager
(mock backend) and menu from source, a fresh flag plane and the OtlpSink. It is a smoke
test, not a statistical one:

- The agent's dynamic declarer declares ``shield.activation`` -> ``hold`` from the explicit
  candidate ``experiment_dynamic_candidates`` = ``shield.activation`` (UC-043 A17, BR-012) on the
  one archetype that presses the trigger:
  ``burst_holder`` (UC-046 BR-018, #2212), selected through the declared
  ``experiment.mock_archetypes`` variant ``quick_cascade`` rewritten to that name (the
  overlay takes declared variants only). ``verdict_min_pairs`` = 1 so one archetype
  passes the selection check (UC-046 A13).
- Both arms run as shadow games; the experimental arm latches ``hold``, the control arm
  the baseline ``instant``. ``player_shield_energy`` is not published in shadow games, so
  the observable is the coordinator's ``game_shield_activation_mode`` gauge
  (telemetry.md §6) per game, plus what flagd serves to each arm's context.
- A ``promote`` verdict needs two pairs (a paired SD) and only one archetype has a trigger
  policy, so the agent's own promotion cannot be reached on a stack in bounded time. The
  second test covers the stack half of A17 step 3 instead: while a real game is
  registered, the ``real`` write of a hold sub-parameter is accepted (dry run). The
  agent's side is ``services/agent/promote/hold_key_uc043_test.go``.
"""

import re
import uuid
from dataclasses import replace

import pytest
from openfeature.evaluation_context import EvaluationContext

from tests.integration.flag_service import FlagService
from tests.integration.flag_service import bind as bind_flag_service
from tests.integration.helpers import flagd_probe_client, poll_until
from tests.service.harness import AGENT

pytestmark = pytest.mark.tier("service")

HOLD_FLAG = "shield.activation"
HOLD_VALUE = "hold"
BASELINE_VALUE = "instant"  # shield.activation default (flags.md §4)
TRIGGER_ARCHETYPE = "burst_holder"  # the one archetype with a trigger_policy (UC-046 BR-018)

ARCHETYPES_VARIANT = "quick_cascade"
SERVICE_COMPOSITION = replace(
    AGENT,
    flag_variant_values={
        ("agent", "experiment.mock_archetypes", ARCHETYPES_VARIANT): TRIGGER_ARCHETYPE,
    },
)

# Agent overlays (flags.md section 13.2): one pair, bounded games.
TUNING = {
    "verdict_min_pairs": 1,
    "experiment_tick_seconds": 3,
    "experiment.max_game_seconds": 60,
}
SERVICE_COMPOSITION = replace(
    SERVICE_COMPOSITION,
    agent_flags={
        "experiment.mock_archetypes": {"variant": ARCHETYPES_VARIANT},
        "experiment_dynamic_candidates": {"value": HOLD_FLAG},  # before the gates
        "experiment_dynamic_objective": {"variant": "endurance"},
        "experiment_dynamic_enabled": {"variant": "on"},
        "enabled": {"variant": "act"},
        "experiments_enabled": {"variant": "on"},
        "decision.backend": {"variant": "rules"},
        **{key: {"value": value} for key, value in TUNING.items()},
    },
)

COORDINATOR = "game-coordinator-service"
MODE_GAUGE = "game_shield_activation_mode"
# The second experiment tick comes within 5 s of start (first-tick cap, #1907).
DECLARE_TIMEOUT = 90.0
SPAWN_TIMEOUT = 150.0
GAUGE_TIMEOUT = 45.0
# Reasons the hold gate discards with (UC-043 A17 step 2); any of them fails the smoke test.
HOLD_DISCARDS = ("pattern_trigger_policy_missing", "hold_already_default", "hold_not_real_default")


def _agent_log(stack) -> str:
    return stack.agent.log_path.read_text(errors="replace")


def _lines(stack, needle: str) -> list[str]:
    return [line for line in _agent_log(stack).splitlines() if needle in line]


def _field(line: str, key: str) -> str:
    match = re.search(rf'\b{re.escape(key)}=("[^"]*"|\S+)', line)
    assert match, f"no {key}= in {line!r}"
    return match.group(1).strip('"')


def _hold_discard(stack) -> str | None:
    log = _agent_log(stack)
    return next((reason for reason in HOLD_DISCARDS if reason in log), None)


def _spawned(stack, experiment_id: str) -> dict[str, str]:
    """arm -> game id of the experiment's shadow games spawned so far (first per arm)."""
    arms: dict[str, str] = {}
    for line in _lines(stack, "experiment: shadow game spawned"):
        if experiment_id in line:
            arms.setdefault(_field(line, "arm"), _field(line, "game_id"))
    return arms


@pytest.fixture(scope="module")
def hold_stack(service_stack):
    client = FlagService.from_compose(service_stack)
    bind_flag_service(client)
    try:
        yield service_stack, client
    finally:
        bind_flag_service(None)
        client.cleanup()


@pytest.mark.uc("UC-043", flow="A17", steps=[1, 2])
@pytest.mark.uc("UC-046", br=["BR-018"])
def test_hold_experiment_runs_both_arms_and_the_experimental_arm_latches_hold(hold_stack):
    """A17 steps 1-2: the declarer declares shield.activation -> hold on burst_holder, the pin
    passes the hold gate, both arms run, and only the experimental arm latches hold."""
    stack, _client = hold_stack

    # Step 1: declared on shield.activation, started (no hold-gate discard at the pin).
    assert poll_until(
        lambda: any(_field(line, "flag_key") == HOLD_FLAG for line in _lines(stack, "experiment.declared")),
        timeout=DECLARE_TIMEOUT,
        interval=1.0,
    ), _agent_log(stack)[-4000:]
    declared = next(line for line in _lines(stack, "experiment.declared") if _field(line, "flag_key") == HOLD_FLAG)
    experiment_id = _field(declared, "experiment_id")
    assert poll_until(
        lambda: any(experiment_id in line for line in _lines(stack, "experiment.started")) or _hold_discard(stack),
        timeout=DECLARE_TIMEOUT,
        interval=1.0,
    ), _agent_log(stack)[-4000:]
    assert _hold_discard(stack) is None, f"Step 2: hold gate discarded the experiment: {_agent_log(stack)[-4000:]}"

    # Step 2: flagd serves hold to the experimental arm only (the arm rung, game_kind != real).
    with flagd_probe_client(stack, "game") as probe:

        def served(arm: str) -> str:
            ctx = EvaluationContext(attributes={"game_kind": "shadow", "experiment_id": experiment_id, "arm": arm})
            return probe.get_string_value(HOLD_FLAG, "", ctx)

        assert poll_until(lambda: served("experimental") == HOLD_VALUE, timeout=DECLARE_TIMEOUT), served("experimental")
        assert served("control") == BASELINE_VALUE, served("control")
        real = probe.get_string_value(HOLD_FLAG, "", EvaluationContext(attributes={"game_kind": "real"}))
        assert real == BASELINE_VALUE, f"the experiment leaked into real games: {real}"

    # Both arms run as shadow games (one pair on the one archetype).
    assert poll_until(
        lambda: set(_spawned(stack, experiment_id)) >= {"experimental", "control"},
        timeout=SPAWN_TIMEOUT,
        interval=1.0,
    ), f"not both arms spawned: {_spawned(stack, experiment_id)}\n{_agent_log(stack)[-4000:]}"
    games = _spawned(stack, experiment_id)

    # The latched mode per game (game_shield_activation_mode = 1 on the latched mode's series).
    def latched(arm: str) -> str:
        point = stack.otlp_sink.wait_metric(
            MODE_GAUGE,
            {"game_id": games[arm], "experiment_id": experiment_id, "arm": arm, "game_kind": "shadow"},
            service=COORDINATOR,
            predicate=lambda p: p.value == 1,
            timeout=GAUGE_TIMEOUT,
        )
        return point.attributes["mode"]

    assert latched("experimental") == HOLD_VALUE, f"experimental game {games['experimental']} did not run in hold"
    assert latched("control") == BASELINE_VALUE, f"control game {games['control']} did not run the baseline"


@pytest.mark.uc("UC-043", flow="A17", steps=[3])
def test_real_write_of_a_hold_key_lands_while_a_real_game_is_registered(hold_stack):
    """A17 step 3 (stack half): a hold sub-parameter's ``real`` write is accepted while a real
    game is registered, because the game latched its values at its start (UC-048 BR-011);
    ``shield.activation`` has no ``real`` scope. Dry run: nothing is written on the shared stack."""
    _, client = hold_stack
    game_id = f"uc043-a17-real-{uuid.uuid4().hex[:8]}"
    path = "/game/shield.energy_budget_seconds"
    write = {"value": 20, "scope": {"game_kind": "real"}, "writer": "uc043-a17-promotion"}

    # A real game (another coordinator instance, so the stack's reconcile never drops it).
    status, body = client.request(
        "PUT", f"/games/{game_id}", {"game_kind": "real", "coordinator_instance": "uc043-a17-test", "serials": []}
    )
    assert status == 200, (status, body)
    try:
        status, body = client.request("PUT", f"{path}?dry_run=true", write)
        assert status // 100 == 2, (status, body)
    finally:
        client.request("DELETE", f"/games/{game_id}")
