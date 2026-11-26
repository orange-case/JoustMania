"""UC-041 BR-008 (#2786): a kill right before the end still reaches the agent.

``game_player_elimination_order`` is the agent's only death channel, so the game's
teardown leaves the series in place and the session's delayed retire removes it with
the other per-game series, ``RETIRE_REMOVAL_EXPORT_INTERVALS`` export intervals later.
"""

from unittest.mock import create_autospec

import pytest

from proto import game_coordinator_pb2
from services.game_coordinator import game_session, metrics
from services.game_coordinator.event_bus import EventBus
from services.game_coordinator.game_session import GameSession

GAME_ID = "game_elim01"


@pytest.fixture
def live_gauge(monkeypatch):
    """Make the real elimination-order gauge record series (metrics are off in tests)."""
    gauge = metrics.player_elimination_order
    monkeypatch.setattr(gauge, "_instrument_initialized", True)
    monkeypatch.setattr(gauge, "_values", {})
    monkeypatch.setattr(gauge, "_metrics", {})
    return gauge


def _series(gauge) -> list[dict]:
    return [dict(key) for key in gauge._values if dict(key).get("game_id") == GAME_ID]


def _session() -> GameSession:
    return GameSession(
        game_id=GAME_ID,
        game_name="FFA",
        players=[game_coordinator_pb2.Player(serial="p1"), game_coordinator_pb2.Player(serial="p2")],
        game_config=game_coordinator_pb2.StartGameConfig(sensitivity=2),
        event_bus=create_autospec(EventBus, instance=True),
        game_kind="real",
    )


class _HeldTimer:
    """Stands in for ``threading.Timer``: the delayed removal runs only when the test fires it."""

    started: list["_HeldTimer"] = []

    def __init__(self, interval, function):
        self.interval, self.function = interval, function
        self.daemon, self.name = False, ""

    def start(self):
        _HeldTimer.started.append(self)


@pytest.mark.uc("UC-041", br=["BR-008", "BR-009"])
def test_teardown_keeps_the_last_elimination_order(live_gauge):
    """The game's own end-of-run clear must not drop a kill set just before it."""
    metrics.player_elimination_order.labels(serial="p1", game_id=GAME_ID).set(1)

    metrics.clear_session_player_analytics(["p1", "p2"], GAME_ID, game_kind="real")

    assert _series(live_gauge) == [{"serial": "p1", "game_id": GAME_ID}]


@pytest.mark.uc("UC-041", br=["BR-008"])
def test_retire_removes_the_elimination_order_after_the_delay(live_gauge, monkeypatch):
    monkeypatch.setattr(game_session, "get_export_interval_ms", lambda: 100)
    monkeypatch.setattr(game_session.threading, "Timer", _HeldTimer)
    _HeldTimer.started.clear()
    session = _session()
    metrics.player_elimination_order.labels(serial="p2", game_id=GAME_ID).set(1)

    session.clear_metrics()

    (timer,) = _HeldTimer.started
    assert timer.interval == pytest.approx(game_session.RETIRE_REMOVAL_EXPORT_INTERVALS * 0.1)
    assert _series(live_gauge) == [{"serial": "p2", "game_id": GAME_ID}], "exported until the delay ends"
    timer.function()
    assert _series(live_gauge) == []
