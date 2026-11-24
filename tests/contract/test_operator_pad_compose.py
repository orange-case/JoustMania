"""The operator pad deploys as an opt-in, unprivileged compose service (UC-015 Pre-1, FR-195, #2050 PR7).

Static checks over the compose files: ``docker-compose.yml`` starts ``operator-pad`` only
under its ``operator-pad`` profile, never privileged, with two host mounts (``/dev/input``
read-only, and for the display of UC-016 the host's ``/dev`` read-only and not recursive at
``/run/host-dev``), the evdev cgroup rule (char major 13, so a pad plugged in later works), the
host's input group through ``INPUT_GID`` and no ``/run/udev``. The hidraw rule (``HIDRAW_MAJOR``)
is only in ``docker-compose.operator-pad-hidraw.yml``, which ``joustmania-compose.sh`` adds with a
known major; real compose renders both ways without a duplicate rule. No other service
waits for it, so a stack without the profile behaves as before (FR-195). The lite stack has
no optional services and no pad. Hardware behaviour (grab, LEDs) needs a Pi (#2050 PR8).
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess

import infra_config as ic
import pytest

PAD = "operator-pad"
HIDRAW_OVERLAY = "docker-compose.operator-pad-hidraw.yml"
OVERLAYS = [
    "docker-compose.override.yml",
    "docker-compose.dev.yml",
    "docker-compose.ci.yml",
    "docker-compose.dry-run.yml",
]


def _services(name: str = "docker-compose.yml") -> dict:
    return ic.load_yaml(ic.REPO_ROOT / name)["services"]


def _pad() -> dict:
    return _services()[PAD]


@pytest.mark.uc("UC-015", cond=["Pre-1"])
def test_pad_starts_only_with_its_profile():
    assert _pad().get("profiles") == [PAD]
    for overlay in OVERLAYS:
        path = ic.REPO_ROOT / overlay
        # text match: the overlays use compose merge tags (!override, !reset) safe_load rejects
        if path.exists():
            redefined = re.search(rf"^  {PAD}:", path.read_text(), re.MULTILINE)
            assert not redefined, (
                f"{overlay} redefines {PAD}; a merged profile could start it by default"
            )


@pytest.mark.uc("UC-015", cond=["Pre-1"])
def test_pad_is_not_privileged():
    pad = _pad()
    assert pad.get("privileged", False) is False
    for key in (
        "user",
        "pid",
        "network_mode",
        "cap_add",
        "devices",
        "ipc",
        "security_opt",
    ):
        assert key not in pad, f"{PAD} must not set {key}"
    assert pad.get("restart") == "unless-stopped"


@pytest.mark.uc("UC-015", cond=["Pre-1"])
@pytest.mark.uc("UC-016", cond=["Pre-2"])
def test_pad_mounts_dev_input_and_host_dev_read_only_not_recursive():
    volumes = _pad().get("volumes")
    assert volumes == [
        "/dev/input:/dev/input:ro",
        # The devtmpfs only: no /dev/shm, /dev/pts or /dev/mqueue with it (Compose v2.24+).
        {
            "type": "bind",
            "source": "/dev",
            "target": "/run/host-dev",
            "read_only": True,
            "bind": {"recursive": "disabled"},
        },
    ], volumes
    assert not any("udev" in str(v) for v in volumes), "evdev needs no /run/udev"


@pytest.mark.uc("UC-015", cond=["Pre-1"])
@pytest.mark.uc("UC-016", cond=["Pre-2"])
def test_pad_cgroup_rules_evdev_in_the_base_hidraw_only_in_its_overlay():
    # No hidraw fallback in the base file: a wrong major grants other devices (UC-016 A1: link down).
    assert _pad().get("device_cgroup_rules") == ["c 13:* rw"]
    overlay = ic.load_yaml(ic.REPO_ROOT / HIDRAW_OVERLAY)["services"]
    assert overlay == {
        PAD: {"device_cgroup_rules": [overlay[PAD]["device_cgroup_rules"][0]]}
    }
    rule = overlay[PAD]["device_cgroup_rules"][0]
    assert re.fullmatch(r"c \$\{HIDRAW_MAJOR:\?[^}]*\}:\* rw", rule), rule


def _render_pad(files: list[str], hidraw_major: str | None) -> tuple[list[str], str]:
    """The pad's cgroup rules as real compose renders them, and compose's stderr."""
    env = {k: v for k, v in os.environ.items() if k != "HIDRAW_MAJOR"}
    if hidraw_major is not None:
        env["HIDRAW_MAJOR"] = hidraw_major
    args = ["docker", "compose", "--project-directory", str(ic.REPO_ROOT)]
    for name in files:
        args += ["-f", str(ic.REPO_ROOT / name)]
    done = subprocess.run(
        [*args, "--profile", PAD, "config", "--format", "json"],
        capture_output=True,
        text=True,
        check=False,
        cwd=ic.REPO_ROOT,
        env=env,
        timeout=60,
    )
    # `config -q` exits 0 on some errors (Compose v5): the exit status and stderr both count.
    assert done.returncode == 0, done.stderr
    assert "error" not in done.stderr.lower() and "equal" not in done.stderr.lower(), (
        done.stderr
    )
    rules = json.loads(done.stdout)["services"][PAD].get("device_cgroup_rules", [])
    return rules, done.stderr


def _compose_cli() -> bool:
    if not shutil.which("docker"):
        return False
    return (
        subprocess.run(
            ["docker", "compose", "version"], capture_output=True, check=False
        ).returncode
        == 0
    )


@pytest.mark.uc("UC-016", flow="A1", cond=["Pre-2"])
@pytest.mark.skipif(
    not _compose_cli(), reason="needs the docker compose CLI (no daemon)"
)
def test_compose_renders_unique_pad_cgroup_rules_with_and_without_the_hidraw_overlay():
    rules, _ = _render_pad(["docker-compose.yml"], None)
    assert rules == ["c 13:* rw"]
    rules, _ = _render_pad(["docker-compose.yml", HIDRAW_OVERLAY], "240")
    assert rules == ["c 13:* rw", "c 240:* rw"]
    assert len(set(rules)) == len(rules)


@pytest.mark.uc("UC-016", flow="A1", cond=["Pre-2"])
@pytest.mark.skipif(
    not _compose_cli(), reason="needs the docker compose CLI (no daemon)"
)
def test_hidraw_overlay_refuses_to_render_without_a_major():
    done = subprocess.run(
        [
            "docker",
            "compose",
            "--project-directory",
            str(ic.REPO_ROOT),
            "-f",
            str(ic.REPO_ROOT / "docker-compose.yml"),
            "-f",
            str(ic.REPO_ROOT / HIDRAW_OVERLAY),
            "--profile",
            PAD,
            "config",
            "-q",
        ],
        capture_output=True,
        text=True,
        check=False,
        cwd=ic.REPO_ROOT,
        env={k: v for k, v in os.environ.items() if k != "HIDRAW_MAJOR"},
        timeout=60,
    )
    assert done.returncode != 0 or "HIDRAW_MAJOR" in done.stderr, (
        "no fallback major (UC-016 A1)"
    )


@pytest.mark.uc("UC-016", cond=["Pre-2"])
def test_hidraw_major_is_documented_and_computed_by_the_pi_wrapper():
    wrapper = (ic.REPO_ROOT / "scripts/setup/joustmania-compose.sh").read_text()
    assert "/proc/devices" in wrapper and "export HIDRAW_MAJOR" in wrapper
    assert HIDRAW_OVERLAY in wrapper
    assert "HIDRAW_MAJOR" in (ic.REPO_ROOT / ".env.example").read_text()
    flags = (ic.REPO_ROOT / "docs/specs/flags.md").read_text()
    section = flags.split("### 15.2", 1)[1]
    assert "`HIDRAW_MAJOR`" in section, (
        "flags.md §15.2 names HIDRAW_MAJOR (host identity, C-035)"
    )
    rule = (ic.REPO_ROOT / "scripts/setup/99-joustmania-operator-pad.rules").read_text()
    assert (
        'ATTRS{idVendor}=="cb00", ATTRS{idProduct}=="1337"' in rule
        and 'GROUP="input"' in rule
    )


@pytest.mark.uc("UC-015", cond=["Pre-1"])
def test_pad_joins_the_host_input_group():
    group_add = _pad().get("group_add")
    assert isinstance(group_add, list) and len(group_add) == 1, group_add
    match = re.fullmatch(r"\$\{INPUT_GID:-(\d+)\}", str(group_add[0]))
    assert match, (
        f"group_add must be ${{INPUT_GID:-<gid>}} (host identity, C-035), got {group_add[0]!r}"
    )
    assert match.group(1) == "996", "the default is the stage Pi's input GID"


@pytest.mark.uc("UC-015", cond=["Pre-1"])
def test_pad_waits_only_for_flagd():
    depends = _pad().get("depends_on") or {}
    assert set(depends) == {"flagd"}, depends


@pytest.mark.uc("UC-015", cond=["Pre-1"])
@pytest.mark.parametrize("compose", ["docker-compose.yml", "docker-compose.lite.yml"])
def test_no_service_depends_on_the_pad(compose):
    """FR-195: removing the pad leaves every other service unchanged."""
    for name, svc in _services(compose).items():
        assert PAD not in (svc.get("depends_on") or {}), f"{name} depends on {PAD}"


def test_lite_stack_has_no_pad():
    assert PAD not in _services("docker-compose.lite.yml")
