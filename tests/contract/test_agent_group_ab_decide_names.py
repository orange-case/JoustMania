"""scripts/agent_group_ab.py reads the decide span under both names (#2584).

Games recorded before the rename carry `agent.decide`; since #2584 the span is
`decision.SpanDecide` ("invoke_agent joustmania-agent"). The report and the
first-game verification must accept both, and must search Jaeger for both.
"""

from __future__ import annotations

import json
import sys
import urllib.parse

import infra_config as ic
import pytest

sys.path.insert(0, str(ic.REPO_ROOT / "scripts"))
import agent_group_ab as ab  # noqa: E402


def _span(name: str, game: str, profile: str) -> dict:
    return {
        "name": name,
        "attrs": {"game.id": game, "decide.prompt_profile": profile},
        "start_ns": 1,
        "end_ns": 2,
    }


def test_decide_span_names_are_the_go_const_and_the_old_name():
    assert ab.DECIDE_SPANS == (ic.decide_span_name(), ic.OLD_DECIDE_SPAN)


@pytest.mark.uc("UC-040", flow="A47")
@pytest.mark.parametrize("name", [ic.decide_span_name(), ic.OLD_DECIDE_SPAN])
def test_report_keeps_a_game_under_either_decide_span_name(name):
    entry = {"game_id": "g1", "profile": "v1", "record": {}}
    analysed = ab.analyse_game(entry, [_span(name, "g1", "v1")])
    assert "excluded" not in analysed, analysed


@pytest.mark.uc("UC-040", flow="A47")
def test_other_span_names_do_not_count_as_a_decide():
    entry = {"game_id": "g1", "profile": "v1", "record": {}}
    assert ab.analyse_game(entry, [_span("agent.decision", "g1", "v1")]) == {
        "excluded": "no_remote_decide"
    }


@pytest.mark.uc("UC-040", flow="A47")
@pytest.mark.parametrize("name", [ic.decide_span_name(), ic.OLD_DECIDE_SPAN])
def test_verify_game_accepts_either_name(tmp_path, name):
    doc = {
        "result": {
            "resourceSpans": [
                {
                    "scopeSpans": [
                        {
                            "spans": [
                                {
                                    "name": name,
                                    "startTimeUnixNano": "1",
                                    "endTimeUnixNano": "2",
                                    "attributes": [
                                        {
                                            "key": "game.id",
                                            "value": {"stringValue": "g1"},
                                        },
                                        {
                                            "key": "decide.prompt_profile",
                                            "value": {"stringValue": "neutral"},
                                        },
                                    ],
                                }
                            ]
                        }
                    ]
                }
            ]
        }
    }
    traces = tmp_path / "traces.json"
    traces.write_text(json.dumps(doc))
    assert ab.verify_game(None, str(traces), "g1", "neutral") == 0
    assert ab.verify_game(None, str(traces), "g1", "v1") == 1


def test_fetch_decide_spans_searches_both_names(monkeypatch):
    seen = []

    def fake_fetch(base, operation, game_id):
        seen.append(operation)
        return [_span(operation, game_id, "v1")]

    monkeypatch.setattr(ab, "fetch_spans", fake_fetch)
    spans = ab.fetch_decide_spans("http://h/jaeger/api", "g1")
    assert seen == [ic.decide_span_name(), ic.OLD_DECIDE_SPAN]
    assert len(spans) == 2


def test_jaeger_search_url_encodes_the_space(monkeypatch):
    urls = []

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def read(self):
            return b'{"result": {"resourceSpans": []}}'

    def fake_urlopen(url, timeout):
        urls.append(url)
        return _Resp()

    monkeypatch.setattr(ab.urllib.request, "urlopen", fake_urlopen)
    ab.fetch_spans("http://h/jaeger/api", ic.decide_span_name(), "g1")
    query = urllib.parse.parse_qs(urllib.parse.urlsplit(urls[0]).query)
    assert " " not in urls[0]
    assert query["query.operationName"] == [ic.decide_span_name()]
