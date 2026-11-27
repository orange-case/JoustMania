"""OOM order under host memory pressure (NFR-081, services.md §2, stage-host.md SH-18, #2914).

Every game service must outlive every observability and agent container: the
kernel's OOM killer adds `oom_score_adj` to each process's share of RAM, so the
game tier is negative and the expendable tier positive. The game and
observability lists come from lib-joustmania.sh, the lists the Pi start uses.
"""

from __future__ import annotations

import re

import infra_config as ic
import pytest
import yaml

GAME_ADJ = -500
EXPENDABLE_ADJ = 500
# Not in JOUSTMANIA_OBS_SERVICES (the start does not phase it), but nothing in
# the game needs it either.
EXTRA_EXPENDABLE = {"dashboard", "stowaway"}
COMPOSE_FILES = ["docker-compose.yml", "docker-compose.lite.yml"]
OVERLAYS = sorted(
    p.name
    for p in ic.REPO_ROOT.glob("docker-compose.*.yml")
    if p.name != "docker-compose.lite.yml"
)


def _bash_array(name: str) -> set[str]:
    text = (ic.REPO_ROOT / "scripts" / "setup" / "lib-joustmania.sh").read_text()
    m = re.search(rf"^{name}=\(([^)]*)\)", text, re.MULTILINE)
    assert m, f"{name} not found in lib-joustmania.sh"
    return set(m.group(1).split())


# Not a game service for the start, but the operator's way out of a stuck game.
EXTRA_PROTECTED = {"operator-pad"}
GAME = _bash_array("JOUSTMANIA_GAME_SERVICES") | EXTRA_PROTECTED
EXPENDABLE = _bash_array("JOUSTMANIA_OBS_SERVICES") | EXTRA_EXPENDABLE


class _ComposeLoader(yaml.SafeLoader):
    """SafeLoader that reads compose merge tags (!override, !reset) as plain values."""


def _construct_tagged(loader: yaml.SafeLoader, tag_suffix: str, node: yaml.Node):
    if isinstance(node, yaml.MappingNode):
        return loader.construct_mapping(node, deep=True)
    if isinstance(node, yaml.SequenceNode):
        return loader.construct_sequence(node, deep=True)
    return loader.construct_scalar(node)


_ComposeLoader.add_multi_constructor("!", _construct_tagged)


def _services(name: str) -> dict:
    data = yaml.load((ic.REPO_ROOT / name).read_text(), Loader=_ComposeLoader)
    return (data or {}).get("services") or {}


def _adj(svc: dict) -> int:
    return int((svc or {}).get("oom_score_adj", 0))


@pytest.mark.uc("UC-060", flow="main", steps=[4])  # stage-host.md SH-18
@pytest.mark.nfr("NFR-081")
@pytest.mark.parametrize("compose", COMPOSE_FILES)
def test_game_services_outlive_expendable_ones(compose):
    services = _services(compose)
    game = GAME & services.keys()
    expendable = EXPENDABLE & services.keys()
    assert game, f"{compose}: no game service found"
    wrong_game = {s: _adj(services[s]) for s in game if _adj(services[s]) != GAME_ADJ}
    assert not wrong_game, f"{compose}: game services need oom_score_adj {GAME_ADJ}: {wrong_game}"
    wrong_exp = {
        s: _adj(services[s]) for s in expendable if _adj(services[s]) != EXPENDABLE_ADJ
    }
    assert not wrong_exp, (
        f"{compose}: observability/agent/dashboard need oom_score_adj {EXPENDABLE_ADJ}: {wrong_exp}"
    )
    # Neutral services sit between the tiers, so they never outrank either.
    for name, svc in services.items():
        if name not in game | expendable:
            assert GAME_ADJ < _adj(svc) < EXPENDABLE_ADJ, (
                f"{compose}: {name} has oom_score_adj {_adj(svc)} outside the neutral tier"
            )


@pytest.mark.uc("UC-060", flow="main", steps=[4])  # stage-host.md SH-18
@pytest.mark.nfr("NFR-081")
def test_full_stack_has_every_listed_service():
    services = _services("docker-compose.yml")
    assert GAME | EXPENDABLE <= services.keys(), (
        f"lists name services the base file lacks: {(GAME | EXPENDABLE) - services.keys()}"
    )


@pytest.mark.uc("UC-060", flow="main", steps=[4])  # stage-host.md SH-18
@pytest.mark.nfr("NFR-081")
@pytest.mark.parametrize("overlay", OVERLAYS)
def test_overlays_leave_the_oom_order_to_the_base_file(overlay):
    touched = {n for n, s in _services(overlay).items() if "oom_score_adj" in (s or {})}
    assert not touched, f"{overlay} sets oom_score_adj for {sorted(touched)}"


@pytest.mark.uc("UC-060", flow="main", steps=[4])  # stage-host.md SH-18
@pytest.mark.nfr("NFR-081")
def test_every_protected_service_has_a_memory_limit():
    # A protected service without a ceiling could leak the host into OOM. The
    # lite file sets few limits at all and is not the Pi path.
    compose = "docker-compose.yml"
    services = _services(compose)
    unbounded = sorted(
        s
        for s in GAME & services.keys()
        if not (((services[s] or {}).get("deploy") or {}).get("resources") or {})
        .get("limits", {})
        .get("memory")
    )
    assert not unbounded, f"{compose}: game services without a memory limit: {unbounded}"
