"""rust-hid is the default Bluetooth backend in every stack that reads real controllers (UC-002 BR-003, #1371).

Static checks over the compose files: ``docker-compose.yml`` and ``docker-compose.lite.yml``
both start rust-hid without a profile and make the controller manager wait for it to be
healthy; no other HID backend service exists. Hardware behaviour needs a Pi.
"""

from __future__ import annotations

import infra_config as ic
import pytest

STACKS = {
    "full": ic.COMPOSE_FILE,
    "lite": ic.REPO_ROOT / "docker-compose.lite.yml",
}


def _services(stack: str) -> dict:
    return ic.load_yaml(STACKS[stack])["services"]


@pytest.mark.uc("UC-002", br=["BR-003"])
@pytest.mark.parametrize("stack", sorted(STACKS))
def test_stack_starts_rust_hid_by_default(stack):
    services = _services(stack)
    rust_hid = services["rust-hid"]
    assert not rust_hid.get("profiles"), (
        "a profile would keep rust-hid from starting by default"
    )
    depends = services["controller-manager"]["depends_on"]
    assert depends["rust-hid"]["condition"] == "service_healthy"
    assert depends["rust-hid"].get("required", True) is True


@pytest.mark.uc("UC-002", br=["BR-003"])
@pytest.mark.parametrize("stack", sorted(STACKS))
def test_rust_hid_is_the_only_hid_backend(stack):
    services = _services(stack)
    assert "python-hid" not in services
    assert "python-hid" not in services["controller-manager"]["depends_on"]
