"""``make dry-run`` starts the demo stack as FR-090 says (UC-070 Pre-1).

FR-090: one command starts the latest images with mock controllers and the agent;
``scripts/agent-dryrun-enable.sh on`` then sets the dynamic declarer's explicit candidate and the
fail-closed ``enabled`` / ``experiments_enabled`` flags (no seed env, no seed flag). Static checks over the Makefile recipe, the compose layers and the
flagd CI baseline; ``make -n`` prints the recipe without running docker.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
from pathlib import Path

import infra_config as ic
import pytest
import yaml

REPO_ROOT = ic.REPO_ROOT
DRY_RUN_FILE = REPO_ROOT / "docker-compose.dry-run.yml"
CI_FLAGD_DIR = REPO_ROOT / "services" / "flagd" / "ci"


class _ComposeLoader(yaml.SafeLoader):
    """SafeLoader that reads compose merge tags (``!reset``, ``!override``) as plain values."""


def _construct_tagged(loader: yaml.SafeLoader, suffix: str, node: yaml.Node):
    if isinstance(node, yaml.SequenceNode):
        return loader.construct_sequence(node, deep=True)
    if isinstance(node, yaml.MappingNode):
        return loader.construct_mapping(node, deep=True)
    return loader.construct_scalar(node)


_ComposeLoader.add_multi_constructor("!", _construct_tagged)


def _load_layer(path: Path) -> dict:
    return yaml.load(path.read_text(), Loader=_ComposeLoader)


def _dry_run_recipe(env_image_tag: str | None = None) -> str:
    env = {k: v for k, v in os.environ.items() if k not in ("IMAGE_TAG", "MAKEFLAGS")}
    if env_image_tag is not None:
        env["IMAGE_TAG"] = env_image_tag
    result = subprocess.run(
        ["make", "-n", "--no-print-directory", "-C", str(REPO_ROOT), "dry-run"],
        capture_output=True,
        text=True,
        env=env,
        check=True,
    )
    return result.stdout


@pytest.mark.uc("UC-070", cond=["Pre-1"])
def test_dry_run_pins_latest_images_not_the_env_release_pin():
    command = _dry_run_recipe().splitlines()[0]
    assert command.startswith("IMAGE_TAG=latest docker compose")
    assert "0.7.0" not in command
    # An exported IMAGE_TAG still wins, so a rehearsal can pin a tag on purpose.
    assert _dry_run_recipe("main").lstrip().startswith("IMAGE_TAG=main docker compose")


@pytest.mark.uc("UC-070", cond=["Pre-1"])
def test_dry_run_layers_the_ci_and_dry_run_compose_files_with_agent_profile():
    recipe = _dry_run_recipe()
    # Only the `docker compose` command line; later recipe lines are echo banners.
    compose_cmd = " ".join(
        line.rstrip("\\").strip()
        for line in recipe.replace("\\\n", " ").splitlines()
        if "docker compose" in line and not line.lstrip().startswith("echo")
    )
    files = re.findall(r"-f (docker-compose[\w.-]*\.yml)", compose_cmd)
    assert files == [
        "docker-compose.yml",
        "docker-compose.override.yml",
        "docker-compose.ci.yml",
        "docker-compose.dry-run.yml",
    ]
    assert "--profile agent" in compose_cmd and " up -d" in compose_cmd


def test_dry_run_layer_puts_the_agent_in_the_profile_make_dry_run_activates():
    """FR-090: the agent only starts if its service sits in the activated profile."""
    activated = re.findall(r"--profile (\w+)", _dry_run_recipe())
    assert "agent" in activated
    profiles = _load_layer(DRY_RUN_FILE)["services"]["agent"].get("profiles", [])
    assert "agent" in profiles


@pytest.mark.uc("UC-070", cond=["Pre-1"])
def test_every_image_follows_image_tag_and_no_layer_hard_pins_a_tag():
    for name, service in ic.compose_services().items():
        image = service.get("image", "")
        if "joustmania/" in image:
            # #1780: the collector may be overridden by its content-hash tag in CI.
            image = image.replace(
                "${OTEL_COLLECTOR_TAG:-${IMAGE_TAG:-latest}}", "${IMAGE_TAG:-latest}"
            )
            assert image.endswith(":${IMAGE_TAG:-latest}"), f"{name}: {image}"
    for layer in ("docker-compose.override.yml", "docker-compose.ci.yml"):
        services = _load_layer(REPO_ROOT / layer).get("services", {})
        for name, service in services.items():
            assert "joustmania/" not in service.get("image", ""), f"{layer}:{name}"
    assert "image" not in str(_load_layer(DRY_RUN_FILE)["services"].get("agent", {}))


@pytest.mark.uc("UC-070", cond=["Pre-1"])
def test_dry_run_uses_mock_controllers_from_the_ci_flag_baseline():
    ci = _load_layer(REPO_ROOT / "docker-compose.ci.yml")["services"]
    assert "./services/flagd/ci:/etc/flagd:ro" in ci["flag-service"]["volumes"]
    backend = json.loads((CI_FLAGD_DIR / "controller.json").read_text())["flags"][
        "backend"
    ]
    assert backend["variants"][backend["defaultVariant"]] == "mock"


@pytest.mark.uc("UC-043", flow="A16")
def test_dry_run_names_the_experiment_through_a_flag_not_the_env():
    """The experiment is a flag value; no compose layer sets a seed or gate env."""
    entries = _load_layer(DRY_RUN_FILE)["services"]["agent"].get("environment") or []
    base_env = ic.compose_services()["agent"]["environment"]
    for env in (entries, base_env):
        names = {str(entry).split("=", 1)[0] for entry in env}
        removed = {
            "AGENT_EXPERIMENTS_ENABLED",
            "AGENT_EXPERIMENT_TICK_SECONDS",
            "AGENT_EXPERIMENT_DYNAMIC_ENABLED",
            *(
                f"AGENT_EXPERIMENT_SEED_{n}"
                for n in ("FLAG", "VALUE", "OBJECTIVE", "TARGET_N", "HYPOTHESIS")
            ),
        }
        assert not names & removed, (
            f"a compose layer still sets {sorted(names & removed)}"
        )
    # The agent reads no seed env: no Go string literal names one (comments may).
    literal = re.compile(r'"AGENT_EXPERIMENT_SEED_\w*"')
    readers = [
        path.name
        for path in (REPO_ROOT / "services" / "agent").glob("*.go")
        if not path.name.endswith("_test.go") and literal.search(path.read_text())
    ]
    assert not readers, f"the agent still names a seed env var in {readers}"


@pytest.mark.uc("UC-043", flow="A16", br=["BR-012"])
def test_dryrun_enable_sets_a_declared_candidate_before_the_gates():
    """FR-090: `agent-dryrun-enable.sh on` names the candidate, then opens the gates."""
    script = (REPO_ROOT / "scripts" / "agent-dryrun-enable.sh").read_text()
    variant = re.search(r'^CANDIDATE_VARIANT="(\w+)"', script, re.MULTILINE)
    assert variant, "agent-dryrun-enable.sh names no CANDIDATE_VARIANT"
    candidates = json.loads((CI_FLAGD_DIR / "agent.json").read_text())["flags"][
        "experiment_dynamic_candidates"
    ]
    value = candidates["variants"].get(variant.group(1))
    assert value, (
        f"{variant.group(1)} is not a variant of experiment_dynamic_candidates"
    )
    key, _, game_variant = value.partition("=")
    game = json.loads((CI_FLAGD_DIR / "game.json").read_text())["flags"]
    assert game_variant in game[key]["variants"], f"{value} names no declared variant"
    on = script[script.index("  on)") : script.index("  off)")]
    for gate in (
        "set agent experiment_dynamic_enabled on",
        "set agent enabled act",
        "set agent experiments_enabled on",
    ):
        assert gate in on, f"on must run `{gate}`"
        assert on.index("experiment_dynamic_candidates") < on.index(gate), (
            f"on must write the candidate before `{gate}`"
        )
    off = script[script.index("  off)") : script.index("  status)")]
    assert "experiment_dynamic_candidates" in off, "off must clear the candidate"
    assert "experiment_dynamic_enabled" in off, "off must clear the declarer gate"
