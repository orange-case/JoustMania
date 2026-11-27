"""Challenge flag changes on the match trace (FR-255, UC-035 BR-018, flags.md §1.4)."""

from __future__ import annotations

import ast
import re
from pathlib import Path
from unittest.mock import create_autospec

import pytest
from openfeature import api
from openfeature.evaluation_context import EvaluationContext
from openfeature.flag_evaluation import FlagEvaluationDetails, FlagType, Reason
from openfeature.hook import HookContext
from openfeature.provider.in_memory_provider import InMemoryFlag, InMemoryProvider
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import Span

from lib import challenge_flags
from lib.challenge_flags import (
    CHALLENGE_FLAG_PREFIXES,
    EVENT_END,
    EVENT_START,
    challenge_name,
    current_match,
    is_challenge_flag,
    match_scope,
    poll_watched_flags,
)
from lib.openfeature_otel_hooks import DedupingTracingHook

pytestmark = pytest.mark.uc("UC-035", br=["BR-018"])

REPO = Path(__file__).resolve().parents[2]


@pytest.fixture
def tracer():
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    t = provider.get_tracer("t")
    t.exporter = exporter
    return t


def _evaluate(hook, key, value, variant=None, reason=Reason.STATIC):
    ctx = HookContext(key, FlagType.STRING, "off", EvaluationContext())
    hook.finally_after(ctx, FlagEvaluationDetails(key, value, variant=variant or value, reason=reason), {})


def _span(tracer, name):
    (span,) = [s for s in tracer.exporter.get_finished_spans() if s.name == name]
    return span


def _flag_events(span):
    return [
        (e.attributes["feature_flag.key"], e.attributes.get("feature_flag.result.variant"))
        for e in span.events
        if e.name == "feature_flag.evaluation"
    ]


def _challenge_events(span):
    return [
        (e.name, e.attributes["orangecase.challenge.name"], e.attributes.get("orangecase.challenge.end_reason"))
        for e in span.events
        if e.name in (EVENT_START, EVENT_END)
    ]


def test_prefixes_match_the_go_agent():
    go = (REPO / "services/agent/flags/challenge.go").read_text(encoding="utf-8")
    body = re.search(r"ChallengeFlagPrefixes = \[\]string\{([^}]*)\}", go).group(1)
    assert tuple(ast.literal_eval(f"[{body}]")) == CHALLENGE_FLAG_PREFIXES == ("challenge.", "stowaway.")


@pytest.mark.parametrize(
    ("key", "expected"),
    [("stowaway.challenge", True), ("challenge.depth", True), ("telemetry.moment_shape", False), ("stowaway", False)],
)
def test_is_challenge_flag(key, expected):
    assert is_challenge_flag(key) is expected


@pytest.mark.parametrize(
    ("value", "expected"),
    [("swing", "swing"), ("off", None), ("None", None), ("", None), (False, None), (None, None), (True, "challenge.x")],
)
def test_challenge_name(value, expected):
    assert challenge_name("challenge.x", value) == expected


def test_a_flip_in_a_match_records_one_event_per_change_across_spans(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("match") as root, match_scope(root):
        for tick in range(3):
            with tracer.start_as_current_span(f"tick{tick}"):
                _evaluate(hook, "stowaway.challenge", "off")
        with tracer.start_as_current_span("tick3"):
            _evaluate(hook, "stowaway.challenge", "swing")
            _evaluate(hook, "stowaway.challenge", "swing")
        with tracer.start_as_current_span("tick4"):
            _evaluate(hook, "stowaway.challenge", "swing")
    assert _flag_events(_span(tracer, "tick0")) == [("stowaway.challenge", "off")]
    assert _flag_events(_span(tracer, "tick1")) == []  # per match, not per span
    assert _flag_events(_span(tracer, "tick3")) == [("stowaway.challenge", "swing")]
    assert _flag_events(_span(tracer, "tick4")) == []
    assert _challenge_events(_span(tracer, "match")) == [
        (EVENT_START, "swing", None),
        (EVENT_END, "swing", "match_end"),
    ]


def test_switching_challenges_ends_then_starts(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("match") as root, match_scope(root):
        _evaluate(hook, "stowaway.challenge", "swing")
        _evaluate(hook, "stowaway.challenge", "depth")
        _evaluate(hook, "stowaway.challenge", "off")
    assert _challenge_events(_span(tracer, "match")) == [
        (EVENT_START, "swing", None),
        (EVENT_END, "swing", "flag_change"),
        (EVENT_START, "depth", None),
        (EVENT_END, "depth", "flag_change"),
    ]
    start = next(e for e in _span(tracer, "match").events if e.name == EVENT_START)
    assert start.attributes["feature_flag.key"] == "stowaway.challenge"
    assert start.attributes["feature_flag.result.variant"] == "swing"


def test_each_match_starts_fresh(tracer):
    hook = DedupingTracingHook()
    for name in ("m1", "m2"):
        with tracer.start_as_current_span(name) as root, match_scope(root):
            _evaluate(hook, "stowaway.challenge", "off")
            _evaluate(hook, "stowaway.challenge", "off")
    assert _flag_events(_span(tracer, "m1")) == [("stowaway.challenge", "off")]
    assert _flag_events(_span(tracer, "m2")) == [("stowaway.challenge", "off")]
    assert current_match() is None


def test_outside_a_match_a_challenge_flag_dedupes_per_span(tracer):
    hook = DedupingTracingHook()
    for name in ("a", "b"):
        with tracer.start_as_current_span(name):
            _evaluate(hook, "stowaway.challenge", "swing")
            _evaluate(hook, "stowaway.challenge", "swing")
    assert _flag_events(_span(tracer, "a")) == [("stowaway.challenge", "swing")]
    assert _flag_events(_span(tracer, "b")) == [("stowaway.challenge", "swing")]


def test_other_flags_keep_per_span_dedupe_in_a_match(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("match") as root, match_scope(root):
        for name in ("a", "b"):
            with tracer.start_as_current_span(name):
                _evaluate(hook, "sensitivity", "low")
    assert _flag_events(_span(tracer, "a")) == [("sensitivity", "low")]
    assert _flag_events(_span(tracer, "b")) == [("sensitivity", "low")]


def test_a_failing_root_never_raises_into_the_evaluation(tracer):
    root = create_autospec(Span, instance=True)
    root.add_event.side_effect = RuntimeError("exporter gone")
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("tick"), match_scope(root):
        _evaluate(hook, "stowaway.challenge", "swing")
    assert _flag_events(_span(tracer, "tick")) == []  # the change was not recorded, nothing raised


def test_error_outcome_counts_as_a_change(tracer):
    hook = DedupingTracingHook()
    with tracer.start_as_current_span("match") as root, match_scope(root):
        _evaluate(hook, "stowaway.challenge", "swing")
        _evaluate(hook, "stowaway.challenge", "off", variant="", reason=Reason.ERROR)
        _evaluate(hook, "stowaway.challenge", "off", variant="", reason=Reason.ERROR)
    assert len(_flag_events(_span(tracer, "match"))) == 2


@pytest.fixture
def challenge_domain():
    domain = "challenge_flags_test"
    provider = InMemoryProvider({"stowaway.challenge": InMemoryFlag("off", {"off": "off", "swing": "swing"})})
    api.set_provider(provider, domain=domain)
    client = challenge_flags._client(domain)
    client.add_hooks([DedupingTracingHook()])
    yield domain, provider
    client.hooks.clear()
    challenge_flags._clients.pop(domain, None)


def test_poll_evaluates_watched_flags_only_in_a_match(tracer, challenge_domain):
    domain, provider = challenge_domain
    watched = ((domain, "stowaway.challenge", "off"),)
    with tracer.start_as_current_span("idle"):
        poll_watched_flags(watched)
    assert _flag_events(_span(tracer, "idle")) == []
    with tracer.start_as_current_span("match") as root, match_scope(root):
        poll_watched_flags(watched)
        provider._flags["stowaway.challenge"] = InMemoryFlag("swing", {"off": "off", "swing": "swing"})
        poll_watched_flags(watched)
        poll_watched_flags(watched)
    assert _flag_events(_span(tracer, "match")) == [("stowaway.challenge", "off"), ("stowaway.challenge", "swing")]


def test_poll_never_raises(tracer):
    with tracer.start_as_current_span("match") as root, match_scope(root):
        poll_watched_flags((("no_such_domain_2572", "challenge.x", object()),))
