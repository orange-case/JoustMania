"""Unit tests for the per-session intervention state on its own (#2634, intervention_sessions.py)."""

import asyncio

import pytest

from services.game_coordinator.intervention_sessions import _PRIMARY_SENTINEL_GAME_ID, SessionState, SessionView


async def _publish(_event, _data):
    return None


def _view(game_id, loop=None):
    return SessionView(game_id=game_id, game=None, publish=_publish, loop=loop)


def test_begin_creates_both_buckets_and_the_existing_accessors_never_create():
    state = SessionState()
    assert state.existing_state_bucket("g1") is None
    assert state.existing_event_bucket("g1") is None
    state.begin("g1")
    assert state.existing_state_bucket("g1") == {}
    assert state.existing_event_bucket("g1") == {}


@pytest.mark.uc("UC-053", flow="A4", br=["BR-006"])
def test_forget_drops_every_bucket_of_the_session():
    state = SessionState()
    state.begin("g1")
    state.dispatcher_for(_view("g1"), timeout_s=1.0)
    state.forget("g1")
    assert state.existing_state_bucket("g1") is None
    assert state.existing_event_bucket("g1") is None
    assert "g1" not in state.dispatchers


def test_prune_keeps_live_sessions_and_the_primary_sentinel_buckets():
    state = SessionState()
    for game_id in ("live", "gone", _PRIMARY_SENTINEL_GAME_ID):
        state.begin(game_id)
        state.dispatcher_for(_view(game_id), timeout_s=1.0)
    state.prune({"live"})
    assert set(state.last_state_value) == {"live", _PRIMARY_SENTINEL_GAME_ID}
    assert set(state.last_event) == {"live", _PRIMARY_SENTINEL_GAME_ID}
    # A dispatcher is bound to a game's loop: only live sessions keep theirs.
    assert set(state.dispatchers) == {"live"}


@pytest.mark.uc("UC-040", flow="A26", br=["BR-018"])
def test_a_session_keeps_its_dispatcher_until_its_loop_changes():
    state = SessionState()
    first_loop, second_loop = asyncio.new_event_loop(), asyncio.new_event_loop()
    try:
        first = state.dispatcher_for(_view("g1", first_loop), timeout_s=1.0)
        assert state.dispatcher_for(_view("g1", first_loop), timeout_s=1.0) is first
        second = state.dispatcher_for(_view("g1", second_loop), timeout_s=1.0)
        assert second is not first
        assert second.loop is second_loop
        assert state.dispatchers["g1"] is second
    finally:
        first_loop.close()
        second_loop.close()
