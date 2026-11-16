"""Telemetry vocabulary and environment contract (UC-035 BR-002, #2646).

`deployment.environment` is one variable per stack (`DEPLOYMENT_ENVIRONMENT`, default
`development`) that compose passes to every service and the collector. `game.kind` is
`real` / `shadow` on every producer; no consumer (Grafana, the dashboard SPA, the
operator scripts, the docs queries) filters on the retired `primary` value.
"""

from __future__ import annotations

import re

import infra_config as ic
import pytest

PASS_THROUGH = "DEPLOYMENT_ENVIRONMENT=${DEPLOYMENT_ENVIRONMENT:-development}"

# A game_kind selector or tag on the retired coordinator value.
PRIMARY_KIND = re.compile(
    r"""game[_.]kind(?:=~?\?"|%22%3A%22|"?\s*:\s*"|=)primary""",
)
# Where a consumer reads the kind: dashboards, SPA sources and tests, scripts, docs queries.
CONSUMER_GLOBS = [
    "services/grafana/dashboards/**/*.json",
    "services/dashboard/src/**/*.ts",
    "services/dashboard/src/index.html",
    "scripts/**/*.py",
    "scripts/**/*.sh",
    "docs/**/*.md",
]
# Rows that describe the retired value on purpose (history, migration notes).
ALLOWED = {"docs/specs/telemetry.md", "docs/glossary.md", "docs/entity_model.md"}


def _env_list(svc: dict) -> list[str]:
    env = svc.get("environment") or []
    if isinstance(env, dict):
        return [f"{k}={v}" for k, v in env.items()]
    return [str(e) for e in env]


@pytest.mark.uc("UC-035", br=["BR-002"])
def test_compose_passes_deployment_environment_to_every_telemetry_service():
    services = ic.load_yaml(ic.REPO_ROOT / "docker-compose.yml")["services"]
    with_otel = {
        name: svc
        for name, svc in services.items()
        if any(e.startswith("OTEL_RESOURCE_ATTRIBUTES=") for e in _env_list(svc))
    }
    assert "game-coordinator" in with_otel and "otel-collector" in with_otel
    missing = [n for n, s in with_otel.items() if PASS_THROUGH not in _env_list(s)]
    assert not missing, f"no {PASS_THROUGH} on: {missing}"


@pytest.mark.uc("UC-035", br=["BR-002"])
def test_collector_resource_processor_reads_deployment_environment():
    text = (ic.REPO_ROOT / "services/otel-collector/config.yaml").read_text()
    assert "${env:DEPLOYMENT_ENVIRONMENT:-development}" in text
    assert not re.search(r"key: deployment\.environment\s+value: development\b", text)


@pytest.mark.uc("UC-035", br=["BR-002"])
def test_env_example_documents_deployment_environment():
    text = (ic.REPO_ROOT / ".env.example").read_text()
    assert "DEPLOYMENT_ENVIRONMENT=production" in text
    assert "development" in text


@pytest.mark.uc("UC-035", br=["BR-002"])
def test_no_consumer_filters_on_the_retired_primary_game_kind():
    offenders = []
    for pattern in CONSUMER_GLOBS:
        for path in ic.REPO_ROOT.glob(pattern):
            rel = path.relative_to(ic.REPO_ROOT).as_posix()
            if "node_modules" in rel or rel in ALLOWED or not path.is_file():
                continue
            for n, line in enumerate(path.read_text(errors="ignore").splitlines(), 1):
                if PRIMARY_KIND.search(line):
                    offenders.append(f"{rel}:{n}")
    assert not offenders, f"game_kind=primary is retired (use real): {offenders}"


@pytest.mark.uc("UC-035", br=["BR-002"])
def test_grafana_game_kind_variable_defaults_to_real():
    import json

    dash = json.loads(
        (
            ic.REPO_ROOT / "services/grafana/dashboards/game/game-players.json"
        ).read_text()
    )
    var = next(v for v in dash["templating"]["list"] if v["name"] == "game_kind")
    assert var["current"]["value"] == "real"
