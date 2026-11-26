"""One ``controller.shield_effect`` span per shield layer the controller shows (#2669, UC-048 A1, A2, A11, BR-010).

Before #2669 one span covered the controller's whole render loop: the first layer's kind (often the
empty energy gauge), ``cancelled`` at game end, a span for a controller that never had a shield and
no span of its own for a hold activation. The tests run the real renderer on a blocking FakeClock
and read the spans from an in-memory exporter.
"""

import asyncio
import contextlib
from unittest.mock import AsyncMock

import pytest
import pytest_asyncio
from opentelemetry.sdk.trace import SpanProcessor, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lib.clock import FakeClock
from proto import controller_manager_pb2 as pb
from services.controller_manager import shield_effects as shield_module
from services.controller_manager.effects_base import ChargeCues
from services.controller_manager.feedback_manager import FeedbackManager

SERIAL = "MOCK0001"
PLAYER = (0, 200, 0)
START = 1000.0
SPAN = "controller.shield_effect"
TRACE_ID = "0af7651916cd43dd8448eb211c80319c"
ENERGY_PARENT = "b7ad6b7169203331"
HOLD_PARENT = "c8be7c827a314442"


def _traceparent(span_id: str) -> str:
    return f"00-{TRACE_ID}-{span_id}-01"


def _cues() -> pb.ShieldCueParams:
    return pb.ShieldCueParams(
        rumble_intensity=0,
        pulse_ms=150,
        ending_lead_ms=0,
        hold_rumble_intensity=70,
        empty_pulse_ms=400,
        hold_rumble_max_intensity=0,
        hold_led_floor_pct=30,
    )


def _state(energy=0, cap=5000, min_burst=300, activation=0) -> pb.ShieldHoldState:
    return pb.ShieldHoldState(
        energy_ms=energy,
        cap_ms=cap,
        full_ms=cap,
        min_burst_ms=min_burst,
        rate_ms_per_s=0,
        activation_ms=activation,
        cues=_cues(),
    )


class _Backend:
    def __init__(self):
        self.effect_active: dict[str, bool] = {}

    async def set_led_color(self, serial, r, g, b):
        return True

    async def set_rumble(self, serial, intensity):
        return True

    def set_effect_active(self, serial, active):
        self.effect_active[serial] = active


class _OpenSpans(SpanProcessor):
    """The spans started and not yet ended (the in-memory exporter only sees finished ones)."""

    def __init__(self):
        self.open: set[int] = set()

    def on_start(self, span, parent_context=None):
        self.open.add(span.context.span_id)

    def on_end(self, span):
        self.open.discard(span.context.span_id)


@pytest.fixture
def open_spans():
    return _OpenSpans()


@pytest.fixture
def exporter(monkeypatch, open_spans):
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    memory = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(memory))
    provider.add_span_processor(open_spans)
    monkeypatch.setattr(shield_module, "tracer", provider.get_tracer("test"))
    monkeypatch.setattr(shield_module, "read_charge_cues", lambda: ChargeCues(intensity=0))
    return memory


@pytest.fixture
def clock():
    clock = FakeClock(start_time=START)
    clock.enable_blocking()
    return clock


@pytest_asyncio.fixture
async def manager(clock):
    fm = FeedbackManager(backend=_Backend(), tracked_controllers={SERIAL: {"name": "P1"}})
    fm._clock = clock
    fm.base_colors[SERIAL] = PLAYER
    yield fm
    clock.disable_blocking()
    await fm.end_shield_effect(SERIAL, restore=False)
    for task in [*fm.vibration_tasks.values(), *(e.task for e in fm.active_effects.values())]:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task


async def _spin(n=20):
    for _ in range(n):
        await asyncio.sleep(0)


async def _effect(manager, effect, duration_ms, state=None, trace_parent=""):
    assert await manager.handle_game_effect(
        serial=SERIAL, effect=effect, duration_ms=duration_ms, shield_hold=state, trace_parent=trace_parent
    )
    await _spin()


async def _step_until(clock, until):
    """Release the renderer's sleeps one tick at a time until the fake time reaches ``until``."""
    for _ in range(10_000):
        await _spin(8)
        if clock.time() >= until - 1e-9:
            return
        if not clock.release_all():
            clock.advance(until - clock.time())  # the renderer ended: nothing sleeps any more
    raise AssertionError(f"clock stuck at {clock.time()}")


def _shield_spans(exporter):
    return [s for s in exporter.get_finished_spans() if s.name == SPAN]


def _reasons(exporter):
    return [(s.attributes["shield.kind"], _end_reason(s)) for s in _shield_spans(exporter)]


def _end_reason(span):
    ends = [e for e in span.events if e.name == "shield.end"]
    assert len(ends) == 1, [e.name for e in span.events]
    return ends[0].attributes["shield.end_reason"]


class TestUC040ShieldSpanPerLayer:
    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A2", br=["BR-003"])
    async def test_game_end_ends_the_shield_span_with_game_end(self, exporter, manager, clock):
        """UC-048 A2: SHIELD 0 at game end ends the span game_end, not cancelled."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 5000)
        await _step_until(clock, START + 1.0)

        await _effect(manager, pb.GAME_EFFECT_SHIELD, 0)

        (span,) = _shield_spans(exporter)
        assert span.attributes["shield.kind"] == "full"
        assert span.attributes["effect.duration_ms"] == 5000
        assert _end_reason(span) == "game_end"

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A2", br=["BR-003"])
    async def test_death_ends_the_shield_span_with_death(self, exporter, manager, clock, monkeypatch):
        """UC-048 A2: the death effect ends the shield layer; its span says death."""
        monkeypatch.setattr(manager, "set_vibration", AsyncMock())
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 5000)

        await _effect(manager, pb.GAME_EFFECT_PLAYER_DEATH, 0)

        (span,) = _shield_spans(exporter)
        assert _end_reason(span) == "death"

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A1", br=["BR-003"])
    async def test_a_new_shield_replaces_the_running_one_with_its_own_span(self, exporter, manager, clock):
        """UC-048 BR-003: a restart is a new layer: the old span ends replaced, the new one runs its time."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 5000)
        await _step_until(clock, START + 1.0)
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 2000)
        await _step_until(clock, START + 3.2)

        first, second = _shield_spans(exporter)
        assert (first.attributes["effect.duration_ms"], _end_reason(first)) == (5000, "replaced")
        assert (second.attributes["effect.duration_ms"], _end_reason(second)) == (2000, "expired")
        assert ("shield.blink", "full") in [(e.name, e.attributes.get("shield.kind")) for e in second.events]

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A11", br=["BR-010"])
    async def test_an_empty_energy_gauge_has_no_shield_span(self, exporter, manager, clock):
        """UC-048 BR-010: a gauge at 0 s shows the base colour, so no shield span, also not at game end."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD_ENERGY, 5000, _state(energy=0))
        await _step_until(clock, START + 2.0)

        await _effect(manager, pb.GAME_EFFECT_SHIELD, 0)

        assert _shield_spans(exporter) == []

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A8", br=["BR-010"])
    async def test_a_hold_activation_has_its_own_span(self, exporter, manager, clock):
        """UC-048 BR-010: a hold gets a hold span for its own length, a child of the hold command; the
        charged gauge keeps its own span until game end."""
        await _effect(
            manager, pb.GAME_EFFECT_SHIELD_ENERGY, 5000, _state(energy=3000), trace_parent=_traceparent(ENERGY_PARENT)
        )
        await _step_until(clock, START + 1.0)
        await _effect(
            manager,
            pb.GAME_EFFECT_SHIELD_HOLD,
            1500,
            _state(energy=3000, activation=1500),
            trace_parent=_traceparent(HOLD_PARENT),
        )
        await _step_until(clock, START + 3.0)

        (hold,) = _shield_spans(exporter)
        assert hold.attributes["shield.kind"] == "hold"
        assert hold.attributes["effect.duration_ms"] == 1500
        assert _end_reason(hold) == "expired"
        assert format(hold.parent.span_id, "016x") == HOLD_PARENT

        await _effect(manager, pb.GAME_EFFECT_SHIELD, 0)

        hold, energy = _shield_spans(exporter)
        assert energy.attributes["shield.kind"] == "energy"
        assert "effect.duration_ms" not in energy.attributes
        assert _end_reason(energy) == "game_end"
        assert format(energy.parent.span_id, "016x") == ENERGY_PARENT

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A11", br=["BR-010"])
    async def test_hold_release_ends_the_hold_span_cancelled_and_the_gauge_runs_on(
        self, exporter, manager, clock, open_spans
    ):
        """UC-048 BR-010: SHIELD_HOLD 0 ends only the hold span (cancelled); a re-grant during the hold
        replaced the first one; the gauge keeps its span until game end."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD_ENERGY, 5000, _state(energy=3000))
        await _effect(manager, pb.GAME_EFFECT_SHIELD_HOLD, 2000, _state(energy=3000, activation=2000))
        await _step_until(clock, START + 0.5)
        await _effect(manager, pb.GAME_EFFECT_SHIELD_HOLD, 2500, _state(energy=3000, activation=3000))
        await _step_until(clock, START + 1.0)

        await _effect(manager, pb.GAME_EFFECT_SHIELD_HOLD, 0)
        await _step_until(clock, START + 1.5)

        assert _reasons(exporter) == [("hold", "replaced"), ("hold", "cancelled")]
        assert len(open_spans.open) == 1  # the energy gauge
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 0)
        assert _reasons(exporter)[-1] == ("energy", "game_end")
        assert manager._shield_spans == {}
        assert not open_spans.open

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A2", br=["BR-002", "BR-003"])
    async def test_toughness_boost_end_cancels_only_its_span(self, exporter, manager, clock, open_spans):
        """UC-048 BR-003: TOUGHNESS_BOOST 0 ends the boost span cancelled; the full shield runs on to expired."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 5000)
        await _effect(manager, pb.GAME_EFFECT_TOUGHNESS_BOOST, 5000)
        await _step_until(clock, START + 0.5)

        await _effect(manager, pb.GAME_EFFECT_TOUGHNESS_BOOST, 0)
        await _step_until(clock, START + 6.0)

        assert _reasons(exporter) == [("toughness_boost", "cancelled"), ("full", "expired")]
        assert manager._shield_spans == {}
        assert not open_spans.open

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A10", br=["BR-010"])
    async def test_an_emptied_gauge_ends_empty_and_a_refill_opens_a_new_span(
        self, exporter, manager, clock, open_spans
    ):
        """UC-048 BR-010: the gauge span ends empty when the energy falls below the minimum burst; a
        refill is a new span."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD_ENERGY, 5000, _state(energy=3000))
        await _step_until(clock, START + 0.5)
        await _effect(manager, pb.GAME_EFFECT_SHIELD_ENERGY, 5000, _state(energy=0))
        await _step_until(clock, START + 1.0)

        assert _reasons(exporter) == [("energy", "empty")]
        await _effect(manager, pb.GAME_EFFECT_SHIELD_ENERGY, 5000, _state(energy=2000))
        await _step_until(clock, START + 1.5)
        await _effect(manager, pb.GAME_EFFECT_SHIELD_ENERGY, 0)

        assert _reasons(exporter) == [("energy", "empty"), ("energy", "cancelled")]
        assert manager._shield_spans == {}
        assert not open_spans.open

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A3", br=["BR-006"])
    async def test_a_charge_resend_that_extends_keeps_one_span(self, exporter, manager, clock, open_spans):
        """UC-048 BR-006: a charge re-send while it is held only extends it: one charge span, expired."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD_CHARGE, 1000)
        await _step_until(clock, START + 0.5)
        await _effect(manager, pb.GAME_EFFECT_SHIELD_CHARGE, 1000)
        await _step_until(clock, START + 2.0)

        assert _reasons(exporter) == [("charge", "expired")]
        assert manager._shield_spans == {}
        assert not open_spans.open

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A2", br=["BR-003"])
    async def test_a_removed_controller_ends_every_layer_span_cancelled(self, exporter, manager, clock, open_spans):
        """UC-048 A2: a controller removed mid-shield (renderer cancelled, not awaited) leaves no span open."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 5000)
        await _effect(manager, pb.GAME_EFFECT_SHIELD_CHARGE, 5000)
        await _step_until(clock, START + 0.5)

        manager.clear_controller(SERIAL)
        await _spin()

        assert sorted(_reasons(exporter)) == [("charge", "cancelled"), ("full", "cancelled")]
        assert manager._shield_spans == {}
        assert not open_spans.open

    @pytest.mark.asyncio
    @pytest.mark.uc("UC-048", flow="A2", br=["BR-003"])
    async def test_a_failing_renderer_ends_its_spans_cancelled(self, exporter, manager, clock, open_spans, monkeypatch):
        """UC-048 A2: a renderer that raises (HID write error) leaves no span open."""
        await _effect(manager, pb.GAME_EFFECT_SHIELD, 5000)
        await _step_until(clock, START + 0.2)
        monkeypatch.setattr(manager, "_set_led_color", AsyncMock(side_effect=RuntimeError("hid")))

        clock.release_all()
        await _spin()

        assert _reasons(exporter) == [("full", "cancelled")]
        assert manager._shield_spans == {}
        assert not open_spans.open
