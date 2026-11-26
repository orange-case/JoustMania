"""
Unit tests for the bluetooth-health window accumulator and span emission (#732 M3).

Drives synthetic poll sequences (fresh / None / stale-replay patterns) through the
DiscoveryLoop window accumulator with an injected wall clock, then asserts the
computed gap / dropped-ratio / movement-hz signals, the controller.bluetooth_health
span attributes, and the per-serial bluetooth_controller_sample events.

A stale replay is detected by object identity: an adapter that returns the *same*
dict object again is counted as a poll attempt but NOT a fresh movement update.
"""

import sys
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

test_dir = Path(__file__).parent
service_dir = test_dir.parent
project_root = service_dir.parent.parent
sys.path.insert(0, str(project_root))

from services.controller_manager.tests.backend_doubles import (
    autospec_backend,
    autospec_button_detector,
    autospec_feedback_manager,
    autospec_monitoring,
    autospec_publisher,
    autospec_rescan_timer,
)


class FakeSpan:
    """Records attributes and events set on a span for assertions."""

    def __init__(self):
        self.attributes: dict = {}
        self.events: list[tuple[str, dict]] = []

    def set_attribute(self, key, value):
        self.attributes[key] = value

    def add_event(self, name, attrs=None):
        self.events.append((name, dict(attrs or {})))

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakeTracer:
    """Captures spans created via start_as_current_span by name."""

    def __init__(self):
        self.spans: dict[str, FakeSpan] = {}

    def start_as_current_span(self, name):
        span = FakeSpan()
        self.spans[name] = span
        return span


def _make_loop(backend=None):
    from services.controller_manager.discovery_loop import DiscoveryLoop

    return DiscoveryLoop(
        backend=backend or autospec_backend(),
        tracked_controllers={},
        controller_states={},
        button_detector=autospec_button_detector(),
        feedback_manager=autospec_feedback_manager(),
        monitoring=autospec_monitoring(),
        rescan_timer=autospec_rescan_timer(),
        paired_serials=[],
        base_colors={},
        event_publisher=autospec_publisher(),
    )


def _feed(loop, serial, sequence, t0=0.0, dt=0.01):
    """Feed a sequence of poll outcomes into the window accumulator.

    Each element is either a dict (a frame — fresh if a new object, stale if the
    same object as the previous element) or None (a drop). ``dt`` is the spacing
    between consecutive polls in seconds.
    """
    t = t0
    for item in sequence:
        loop._record(serial).hw_polls += 1
        if item is None:
            loop._record(serial).hw_drops += 1
        else:
            obj_id = id(item)
            if loop._record(serial).hw_last_obj_id != obj_id:
                loop._record(serial).hw_last_obj_id = obj_id
                loop._record_fresh_frame(serial, t)
        t += dt
    return t


class TestWindowComputation:
    def test_all_fresh_counts_every_poll(self):
        loop = _make_loop()
        frames = [{"n": i} for i in range(10)]  # 10 distinct objects
        _feed(loop, "AA", frames, dt=0.01)
        sig = loop._compute_health_window(window_seconds=0.1)
        per = sig["per_serial"]["AA"]
        assert per["update_hz"] == 100.0  # 10 fresh / 0.1s
        assert per["dropped_pct"] == 0.0
        # Consecutive frames 10ms apart -> max gap 10ms.
        assert abs(per["max_gap_ms"] - 10.0) < 1e-6

    def test_none_drops_count_as_drops_not_fresh(self):
        loop = _make_loop()
        # 8 fresh, 2 drops out of 10 polls.
        seq = [{"n": i} if i % 5 != 0 else None for i in range(10)]
        _feed(loop, "AA", seq, dt=0.01)
        sig = loop._compute_health_window(window_seconds=0.1)
        per = sig["per_serial"]["AA"]
        assert per["dropped_pct"] == 0.2  # 2/10
        assert per["update_hz"] == 80.0  # 8 fresh / 0.1s

    def test_stale_replay_not_counted_as_fresh(self):
        loop = _make_loop()
        same = {"n": 1}
        # 1 fresh frame then 9 replays of the SAME object.
        seq = [same] * 10
        _feed(loop, "AA", seq, dt=0.01)
        sig = loop._compute_health_window(window_seconds=0.1)
        per = sig["per_serial"]["AA"]
        # Only one genuinely-new frame in the window.
        assert per["update_hz"] == 10.0  # 1 fresh / 0.1s
        assert per["dropped_pct"] == 0.0  # replays are not drops

    def test_max_gap_is_largest_inter_fresh_gap(self):
        loop = _make_loop()
        a, b, c = {"n": 1}, {"n": 2}, {"n": 3}
        # fresh @0, stale*5 (50ms), fresh @60ms, fresh @70ms
        loop._record("AA").hw_last_obj_id = None
        for item, t in [(a, 0.0), (a, 0.01), (a, 0.02), (a, 0.03), (a, 0.04), (a, 0.05), (b, 0.06), (c, 0.07)]:
            loop._record("AA").hw_polls += 1
            oid = id(item)
            if loop._record("AA").hw_last_obj_id != oid:
                loop._record("AA").hw_last_obj_id = oid
                loop._record_fresh_frame("AA", t)
        # Gaps between fresh frames: a@0 -> b@60ms = 60ms, b@60 -> c@70 = 10ms.
        assert abs(loop._record("AA").hw_max_gap_ms - 60.0) < 1e-6

    def test_window_aggregates_worst_case_across_serials(self):
        loop = _make_loop()
        # Serial AA: healthy (10 fresh, no drops, 10ms gaps).
        _feed(loop, "AA", [{"n": i} for i in range(10)], dt=0.01)
        # Serial BB: degraded (2 fresh spaced 60ms, plus 2 drops).
        bb = [{"n": 100}, None, {"n": 101}, None]
        _feed(loop, "BB", bb, dt=0.06)
        sig = loop._compute_health_window(window_seconds=0.24)
        # event_gap = worst (max) across serials -> BB's 60ms.
        assert sig["event_gap_ms"] >= 60.0
        # movement_update_hz = min across serials -> BB's lower rate.
        bb_hz = sig["per_serial"]["BB"]["update_hz"]
        aa_hz = sig["per_serial"]["AA"]["update_hz"]
        assert sig["movement_update_hz"] == min(aa_hz, bb_hz)
        assert sig["active_controllers"] == 2

    def test_empty_window_zero_signals(self):
        loop = _make_loop()
        sig = loop._compute_health_window(window_seconds=1.0)
        assert sig["active_controllers"] == 0
        assert sig["event_gap_ms"] == 0.0
        assert sig["dropped_events_pct"] == 0.0
        assert sig["movement_update_hz"] == 0.0

    def test_zero_duration_does_not_divide_by_zero(self):
        loop = _make_loop()
        _feed(loop, "AA", [{"n": i} for i in range(3)], dt=0.0)
        sig = loop._compute_health_window(window_seconds=0.0)
        # Should not raise; rate is finite (huge but finite).
        assert sig["per_serial"]["AA"]["update_hz"] > 0


class TestSpanEmission:
    def test_span_attributes_and_events(self):
        backend = autospec_backend()
        backend.get_adapter_type.return_value = "rust"
        loop = _make_loop(backend)
        loop._window_start = 0.0
        _feed(loop, "AA", [{"n": i} if i % 5 != 0 else None for i in range(10)], dt=0.01)

        fake = FakeTracer()
        with patch("services.controller_manager.discovery_loop.tracer", fake):
            loop._emit_bluetooth_health(now=1.0)

        assert "controller.bluetooth_health" in fake.spans
        span = fake.spans["controller.bluetooth_health"]
        a = span.attributes
        assert a["bluetooth.active_controllers"] == 1
        assert a["bluetooth.dropped_events_pct"] == 0.2
        assert not {"bluetooth.target_backend", "bluetooth.rollout_count", "bluetooth.rollout_strategy"} & set(a)
        assert isinstance(a["bluetooth.event_gap_ms"], float)
        assert isinstance(a["bluetooth.movement_update_hz"], float)

        # One event per serial.
        sample_events = [e for e in span.events if e[0] == "bluetooth_controller_sample"]
        assert len(sample_events) == 1
        ev_attrs = sample_events[0][1]
        assert ev_attrs["controller.serial"] == "AA"
        assert ev_attrs["controller.adapter"] == "rust"
        assert ev_attrs["bluetooth.dropped_events_pct"] == 0.2
        assert "bluetooth.movement_update_hz" in ev_attrs

    def test_skip_when_no_controllers(self):
        loop = _make_loop()
        loop._window_start = 0.0
        fake = FakeTracer()
        with patch("services.controller_manager.discovery_loop.tracer", fake):
            loop._emit_bluetooth_health(now=1.0)
        assert "controller.bluetooth_health" not in fake.spans

    def test_window_resets_after_emit(self):
        loop = _make_loop(MagicMock())
        loop._window_start = 0.0
        _feed(loop, "AA", [{"n": i} for i in range(5)], dt=0.01)
        fake = FakeTracer()
        with patch("services.controller_manager.discovery_loop.tracer", fake):
            loop._emit_bluetooth_health(now=1.0)
        assert all(r.hw_polls == 0 and r.hw_fresh == 0 for r in loop._records.values())
        assert loop._window_start == 1.0


class TestUpdateControllerStatesWiring:
    """Verify _update_controller_states wires poll outcomes into the window."""

    @pytest.mark.asyncio
    async def test_fresh_stale_none_accumulated_via_real_path(self):
        loop = _make_loop()
        loop.backend_initialized = True
        loop.tracked_controllers = {"AA": {}}
        loop.controller_states = {}
        loop._cached_serials = ["AA"]
        loop.button_detector = autospec_button_detector()

        fresh1 = {"serial": "AA", "accel": {"x": 0.0, "y": 0.0, "z": 1.0}}
        stale = fresh1  # same object -> stale replay
        fresh2 = {"serial": "AA", "accel": {"x": 0.0, "y": 0.0, "z": 1.0}}
        # Sequence: fresh, stale (same obj), None, fresh2.
        loop.backend.get_controller_state = AsyncMock(side_effect=[fresh1, stale, None, fresh2])

        for _ in range(4):
            await loop._update_controller_states()

        assert loop._record("AA").hw_polls == 4
        assert loop._record("AA").hw_fresh == 2  # fresh1 + fresh2 (stale replay excluded)
        assert loop._record("AA").hw_drops == 1  # the None
