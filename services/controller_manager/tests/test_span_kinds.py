"""UC-035 BR-016 (#2654): the controller effect spans start from the command's trace context, so they are CONSUMER.

Each test runs one effect handler against an in-memory span exporter with a remote parent, then checks the
span's kind and that its parent is still the context carried in the command (only the kind changed).
"""

from unittest.mock import AsyncMock

import pytest
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import SpanKind

import services.controller_manager.feedback_manager as fm
import services.controller_manager.shield_effects as se
from lib.telemetry import extract_trace_context
from services.controller_manager.feedback_manager import FeedbackManager

TRACE_ID = "0af7651916cd43dd8448eb211c80319c"
PARENT_SPAN_ID = "b7ad6b7169203331"
SERIAL = "test_ctrl"


class _Backend:
    """Backend stand-in: accepts every LED, rumble and effect-state call."""

    def __init__(self):
        self.effect_active: dict[str, bool] = {}

    async def set_led_color(self, serial: str, r: int, g: int, b: int) -> bool:
        return True

    async def set_rumble(self, serial: str, intensity: int) -> bool:
        return True

    def set_effect_active(self, serial: str, active: bool) -> None:
        self.effect_active[serial] = active


@pytest.fixture
def exporter(monkeypatch):
    """Route both modules' tracers to an in-memory exporter."""
    monkeypatch.delenv("OTEL_SDK_DISABLED", raising=False)
    memory = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(memory))
    tracer = provider.get_tracer("test")
    monkeypatch.setattr(fm, "tracer", tracer)
    monkeypatch.setattr(se, "tracer", tracer)
    return memory


@pytest.fixture
def manager(monkeypatch):
    """A FeedbackManager whose LED, rumble and sleep calls return at once."""
    manager = FeedbackManager(backend=_Backend(), tracked_controllers={SERIAL: {"name": "Test Controller"}})
    monkeypatch.setattr(manager, "play_effect_with_restore", AsyncMock())
    monkeypatch.setattr(manager, "set_vibration", AsyncMock())
    monkeypatch.setattr(manager, "_set_led_color", AsyncMock())
    monkeypatch.setattr(manager, "set_controller_color", AsyncMock())
    monkeypatch.setattr(manager, "_effect_fade_out", AsyncMock())
    monkeypatch.setattr(fm.asyncio, "sleep", AsyncMock())
    return manager


@pytest.fixture
def command_context():
    """The trace context a coordinator command carries (W3C traceparent of the player span)."""
    return extract_trace_context(f"00-{TRACE_ID}-{PARENT_SPAN_ID}-01", "")


def _only_span(exporter: InMemorySpanExporter, name: str):
    spans = [s for s in exporter.get_finished_spans() if s.name == name]
    assert len(spans) == 1, [s.name for s in exporter.get_finished_spans()]
    return spans[0]


def _assert_consumer_child_of_command(span) -> None:
    assert span.kind == SpanKind.CONSUMER
    assert span.parent is not None
    assert format(span.parent.trace_id, "032x") == TRACE_ID
    assert format(span.parent.span_id, "016x") == PARENT_SPAN_ID
    assert span.parent.is_remote


@pytest.mark.asyncio
@pytest.mark.uc("UC-035", br=["BR-016"])
async def test_death_effect_span_is_consumer(exporter, manager, command_context):
    await manager._effect_player_death(SERIAL, trace_context=command_context)
    await manager.active_effects[SERIAL].task

    _assert_consumer_child_of_command(_only_span(exporter, "effect_player_death"))


@pytest.mark.asyncio
@pytest.mark.uc("UC-035", br=["BR-016"])
async def test_winner_rainbow_span_is_consumer(exporter, manager, command_context):
    await manager._effect_winner_rainbow(SERIAL, restore_color=(0, 0, 255), trace_context=command_context)

    _assert_consumer_child_of_command(_only_span(exporter, "effect_winner_rainbow"))


@pytest.mark.asyncio
@pytest.mark.uc("UC-035", br=["BR-016"])
async def test_countdown_span_is_consumer(exporter, manager, command_context):
    await manager._countdown_sequence(SERIAL, restore_color=(0, 0, 255), trace_context=command_context)

    _assert_consumer_child_of_command(_only_span(exporter, "effect_countdown"))


@pytest.mark.asyncio
@pytest.mark.uc("UC-035", br=["BR-016"])
async def test_shield_effect_span_is_consumer(exporter, manager, command_context):
    # One span per shield layer (#2669): a 1 ms layer whose span ends `expired` on the renderer's second tick.
    manager._shield_layers[SERIAL] = {
        "full": se.ShieldLayer(
            start=manager._clock.time(), total_s=0.001, color=(255, 255, 255), trace_context=command_context
        )
    }
    await manager._shield_render_loop(SERIAL)

    _assert_consumer_child_of_command(_only_span(exporter, "controller.shield_effect"))
