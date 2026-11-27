"""stowaway deploys opt-in, capped and off by its flags (UC-106 BR-001, BR-004, BR-005, #2576).

Static checks: the compose service starts only under its ``stowaway`` profile, has its CPU, core
and memory caps and is killed first, sends to the case's collector and nothing else; the flag
files serve the ``stowaway.*`` flags with the kill switch off; the players-mode archetype is the
agent.json variant of the same name.
"""

from __future__ import annotations

import ast
import json

import infra_config as ic
import pytest

SERVICE = "stowaway"


def _service() -> dict:
    return ic.load_yaml(ic.REPO_ROOT / "docker-compose.yml")["services"][SERVICE]


def _env(svc: dict) -> dict[str, str]:
    return dict(item.split("=", 1) for item in svc["environment"])


@pytest.mark.uc("UC-106", br=["BR-004"], cond=["Post-F-1"])
def test_stowaway_runs_only_under_its_profile_with_hard_caps():
    svc = _service()
    assert svc["profiles"] == [SERVICE]
    limits = svc["deploy"]["resources"]["limits"]
    assert float(limits["cpus"]) <= 1.0
    assert limits["memory"] == "256m"
    assert svc["cpuset"] == "${STOWAWAY_CPUSET:-3}"
    assert svc["oom_score_adj"] == 500
    assert not svc.get("privileged")
    assert "ports" not in svc


@pytest.mark.uc("UC-106", br=["BR-001"])
def test_stowaway_sends_only_to_the_case_collector():
    env = _env(_service())
    assert env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "http://otel-collector:4317"
    assert not [k for k in env if "GATEWAY" in k or "TOKEN" in k]


@pytest.mark.uc("UC-106", br=["BR-002"])
def test_no_other_service_and_the_lite_stack_do_not_depend_on_it():
    services = ic.load_yaml(ic.REPO_ROOT / "docker-compose.yml")["services"]
    assert not [
        name
        for name, svc in services.items()
        if SERVICE in (svc.get("depends_on") or {})
    ]
    assert (
        SERVICE
        not in ic.load_yaml(ic.REPO_ROOT / "docker-compose.lite.yml")["services"]
    )


@pytest.mark.uc("UC-106", br=["BR-003"], cond=["Post-F-1"])
@pytest.mark.parametrize(
    "flag_file",
    ["services/flagd/observability.json", "services/flagd/ci/observability.json"],
)
def test_the_stowaway_flags_default_to_off(flag_file):
    flags = json.loads((ic.REPO_ROOT / flag_file).read_text(encoding="utf-8"))["flags"]
    enabled = flags["stowaway.enabled"]
    assert enabled["variants"][enabled["defaultVariant"]] is False
    challenge = flags["stowaway.challenge"]
    assert challenge["variants"][challenge["defaultVariant"]] == "none"
    for key in ("stowaway.mode", "stowaway.intensity", "stowaway.span_target"):
        assert key in flags


def _module_constant(path: str, name: str):
    tree = ast.parse((ic.REPO_ROOT / path).read_text(encoding="utf-8"))
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
            getattr(t, "id", None) == name for t in node.targets
        ):
            return ast.literal_eval(node.value)
    raise AssertionError(f"{name} not found in {path}")


@pytest.mark.uc("UC-106", flow="A2", br=["BR-005"])
def test_the_players_archetype_is_the_agent_variant():
    name = _module_constant("services/stowaway/players.py", "ARCHETYPE")
    params = _module_constant("services/stowaway/players.py", "ARCHETYPE_PARAMS")
    agent = json.loads(
        (ic.REPO_ROOT / "services/flagd/agent.json").read_text(encoding="utf-8")
    )
    assert agent["flags"]["experiment.mock_archetype"]["variants"][name] == params
