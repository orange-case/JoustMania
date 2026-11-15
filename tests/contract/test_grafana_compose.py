"""Grafana start-up stays offline and inside its memory limit (#2600).

Grafana 13 installs and auto-updates a list of "preinstalled" catalog plugins in the
background at start-up. In CI the zipkin update pushed the container past its 256m
limit and it was OOM-killed (exit 137), so `docker compose up` failed on an unhealthy
grafana. The stack only uses the prometheus and jaeger data sources, which the full
image ships under data/plugins-bundled, so the installer is turned off: start-up then
neither downloads from grafana.com nor grows memory for plugins nobody uses.
"""

from __future__ import annotations

import infra_config as ic
import pytest


def _environment(service: dict) -> dict[str, str]:
    env = service.get("environment") or {}
    if isinstance(env, dict):
        return {k: str(v) for k, v in env.items()}
    # A pass-through entry ("- GF_FOO", no "=") takes its value from the host: unset here.
    return {
        key: value for key, sep, value in (item.partition("=") for item in env) if sep
    }


@pytest.mark.uc("UC-035", flow="main", steps=[6])
def test_grafana_disables_the_preinstalled_plugin_installer():
    env = _environment(ic.compose_services()["grafana"])
    assert env.get("GF_PLUGINS_PREINSTALL_DISABLED", "").lower() == "true", (
        "grafana must set GF_PLUGINS_PREINSTALL_DISABLED=true: the background "
        "plugin installer OOM-kills it at start-up (#2600)"
    )


def _megabytes(limit: str) -> float:
    units = {"k": 1 / 1024, "m": 1, "g": 1024}
    value = str(limit).strip().lower().rstrip("b")
    if value[-1] in units:
        return float(value[:-1]) * units[value[-1]]
    return int(value) / (1024 * 1024)


@pytest.mark.uc("UC-035", flow="main", steps=[6])
def test_grafana_memory_limit_covers_its_start_up_peak():
    limits = ic.compose_services()["grafana"]["deploy"]["resources"]["limits"]
    # Grafana 13.3 peaks at about 295 MB right after start with this config (#2605).
    assert _megabytes(limits["memory"]) >= 512, (
        f"grafana memory limit {limits['memory']} is below its start-up peak (#2605)"
    )
