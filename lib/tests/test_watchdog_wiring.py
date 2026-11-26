"""Every long-running Python service starts the loop watchdog and stops it at shutdown (UC-060 A8, NFR-080)."""

import ast
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]

ENTRY_POINTS = {
    "controller-manager": "services/controller_manager/server.py",
    "game-coordinator": "services/game_coordinator/server.py",
    "menu": "services/menu/server.py",
    "audio": "services/audio/server.py",
    "operator-pad": "services/operator_pad/server.py",
    "pairing-daemon": "services/pairing_daemon/psmove_pairing/daemon.py",
}


def _start_calls(tree: ast.AST) -> list[tuple[str, str]]:
    """(service name, assigned variable) of every ``x = start_loop_watchdog("name", ...)``."""
    found = []
    for node in ast.walk(tree):
        if (
            isinstance(node, ast.Assign)
            and isinstance(node.value, ast.Call)
            and isinstance(node.value.func, ast.Name)
            and node.value.func.id == "start_loop_watchdog"
            and node.value.args
            and isinstance(node.value.args[0], ast.Constant)
            and isinstance(node.targets[0], ast.Name)
        ):
            found.append((node.value.args[0].value, node.targets[0].id))
    return found


def _stops(tree: ast.AST, var: str) -> bool:
    return any(
        isinstance(node, ast.Call)
        and isinstance(node.func, ast.Attribute)
        and node.func.attr == "stop"
        and isinstance(node.func.value, ast.Name)
        and node.func.value.id == var
        for node in ast.walk(tree)
    )


@pytest.mark.uc("UC-060", flow="A8")
@pytest.mark.nfr("NFR-080")
@pytest.mark.parametrize(("service", "path"), sorted(ENTRY_POINTS.items()))
def test_entry_point_starts_and_stops_the_loop_watchdog(service, path):
    tree = ast.parse((REPO_ROOT / path).read_text(encoding="utf-8"))

    calls = _start_calls(tree)

    assert [name for name, _ in calls] == [service], f"{path} must call start_loop_watchdog({service!r}) once"
    assert _stops(tree, calls[0][1]), f"{path} must stop the watchdog when shutdown starts"
