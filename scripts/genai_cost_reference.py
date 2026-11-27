#!/usr/bin/env python3
"""Spend reference for the booth challenge "What did the game master cost?" (UC-105, #2718).

Reads the agent's spans from the case's own Jaeger (query API v3) or from saved OTLP JSON answers
and prints the token and money reference of UC-105 BR-003: per provider and model (question 1),
per caller and game kind (question 2), per game (question 3) and per decision with the subsets of
question 4. Counting rules: docs/specs/telemetry.md §4.2 "Spend". Read-only: it sends GET requests
to the Jaeger query API and nothing else.

Stdlib only. Usage:

  genai_cost_reference.py --prices F (--traces FILE ... | --jaeger-url U --start EPOCH --end EPOCH)
                          [--game-id ID ...] [--json]

Exit codes: 0 every game adds up, 3 a game does not add up (UC-105 A3: its decide spans' totals
differ from the sum of their model calls), 2 usage or input error.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from collections import defaultdict

AGENT_SERVICE = "agent"
DECIDE_SPANS = ("invoke_agent joustmania-agent", "agent.decide")  # agent.decide: old name
RETRO_SPAN = "agent.llm.retro"
DECISION_SPAN = "agent.decision"
EVALUATION_EVENT = "gen_ai.evaluation.result"
CONVERSATION_ID = "gen_ai.conversation.id"
# gen_ai.usage.input_tokens already includes both cache counts and output_tokens the reasoning.
TOKEN_KEYS = {
    "input": "gen_ai.usage.input_tokens",
    "output": "gen_ai.usage.output_tokens",
    "cache_read": "gen_ai.usage.cache_read.input_tokens",
    "cache_write": "gen_ai.usage.cache_write.input_tokens",
    "reasoning": "gen_ai.usage.reasoning.output_tokens",
}
# The chat spans carry both names during the transition (#2729); older images only the old one.
# Read the old one only when the new one is absent, never both.
FALLBACK_KEYS = {"cache_write": "gen_ai.usage.cache_creation.input_tokens"}
PER_MTOK = 1_000_000


# ------------------------------------------------------------------ OTLP JSON


def _scalar(v: dict):
    for key in ("stringValue", "boolValue", "doubleValue"):
        if key in v:
            return v[key]
    if "intValue" in v:
        return int(v["intValue"])
    if "arrayValue" in v:
        return [_scalar(x) for x in v["arrayValue"].get("values") or []]
    return None


def _attrs(items) -> dict:
    return {a["key"]: _scalar(a.get("value") or {}) for a in items or []}


def spans_from_doc(doc: dict) -> list[dict]:
    """Flatten one Jaeger v3 answer (or a bare OTLP export) into span dicts."""
    out = []
    for rs in (doc.get("result") or doc).get("resourceSpans") or []:
        service = _attrs((rs.get("resource") or {}).get("attributes")).get("service.name")
        for ss in rs.get("scopeSpans") or []:
            scope = (ss.get("scope") or {}).get("name", "")
            for sp in ss.get("spans") or []:
                out.append(
                    {
                        "trace": sp.get("traceId", ""),
                        "id": sp.get("spanId", ""),
                        "parent": sp.get("parentSpanId", ""),
                        "name": sp.get("name", ""),
                        "start": int(sp.get("startTimeUnixNano") or 0),
                        "service": service,
                        "scope": scope,
                        "attrs": _attrs(sp.get("attributes")),
                        "events": [
                            {"name": e.get("name"), "attrs": _attrs(e.get("attributes"))}
                            for e in sp.get("events") or []
                        ],
                    }
                )
    return out


def dedupe(spans: list[dict]) -> list[dict]:
    # One trace can come back from several queries (a game id query and the window query).
    seen, out = set(), []
    for s in spans:
        key = (s["trace"], s["id"])
        if key not in seen:
            seen.add(key)
            out.append(s)
    return out


# ------------------------------------------------------------------ Jaeger


TRACES_PATH = "/api/v3/traces"


def jaeger_base(url: str) -> tuple[str, str, str]:
    """(scheme, host, path prefix) of an http(s) Jaeger base; anything else is refused."""
    parts = urllib.parse.urlsplit(url)
    if parts.scheme not in ("http", "https") or not parts.hostname:
        raise ValueError(f"--jaeger-url must be an http(s) URL with a host: {url!r}")
    if parts.query or parts.fragment or parts.username or parts.password:
        raise ValueError(f"--jaeger-url takes no query, fragment or credentials: {url!r}")
    return parts.scheme, parts.netloc, parts.path.rstrip("/")


def _get(base: tuple[str, str, str], query: dict, timeout: float = 60) -> dict | None:
    scheme, netloc, prefix = base
    url = urllib.parse.urlunsplit((scheme, netloc, prefix + TRACES_PATH, urllib.parse.urlencode(query), ""))
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:  # NOSONAR: operator CLI input, not a trust boundary
            return json.load(r)
    except urllib.error.HTTPError as e:
        if e.code == 404:  # "No traces found" is an empty answer
            return None
        raise


def fetch(base: tuple[str, str, str], start: float, end: float, game_ids: list[str], depth: int) -> list[dict]:
    fmt = "%Y-%m-%dT%H:%M:%SZ"
    common = {
        "query.serviceName": AGENT_SERVICE,
        "query.searchDepth": str(depth),
        "query.startTimeMin": time.strftime(fmt, time.gmtime(start)),
        "query.startTimeMax": time.strftime(fmt, time.gmtime(end)),
    }
    queries = [dict(common)]  # the window: proposer calls and anything without a game id
    for gid in game_ids:
        q = dict(common)
        q["query.attributes"] = json.dumps({CONVERSATION_ID: gid})
        queries.append(q)
    spans = []
    for q in queries:
        doc = _get(base, q)
        if doc:
            spans.extend(spans_from_doc(doc))
    return dedupe(spans)


# ------------------------------------------------------------------ prices


def load_prices(path: str) -> dict:
    with open(path, encoding="utf-8") as f:  # NOSONAR: operator CLI input, not a trust boundary
        table = json.load(f)
    for key in ("date", "source", "currency", "models"):
        if key not in table:
            raise ValueError(f"price table {path}: missing {key!r}")
    return table


def price_of(table: dict, provider: str | None, model: str | None) -> dict | None:
    """The price row of a call: zero for a provider priced at zero, else the longest model prefix."""
    zero = table.get("zero_priced_providers") or {}
    if provider in zero:
        return {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0, "note": zero[provider]}
    if not model:
        return None
    best = None
    for prefix, row in table["models"].items():
        if model.startswith(prefix) and (best is None or len(prefix) > len(best[0])):
            best = (prefix, row)
    if best is None or any(best[1].get(k) is None for k in ("input", "output", "cache_read", "cache_write")):
        return None  # unknown model, or a row still marked TODO
    return best[1]


def money(tokens: dict, row: dict) -> float:
    uncached = max(tokens["input"] - tokens["cache_read"] - tokens["cache_write"], 0)
    return (
        uncached * row["input"]
        + tokens["cache_read"] * row["cache_read"]
        + tokens["cache_write"] * row["cache_write"]
        + tokens["output"] * row["output"]
    ) / PER_MTOK


# ------------------------------------------------------------------ reference


def _tokens(attrs: dict) -> dict:
    out = {}
    for k, key in TOKEN_KEYS.items():
        v = attrs.get(key)
        if v is None and k in FALLBACK_KEYS:
            v = attrs.get(FALLBACK_KEYS[k])
        out[k] = int(v or 0)
    return out


def _add(into: dict, tokens: dict) -> None:
    for k, v in tokens.items():
        into[k] = into.get(k, 0) + v


def _empty() -> dict:
    return dict.fromkeys(TOKEN_KEYS, 0)


class Bucket:
    def __init__(self) -> None:
        self.tokens = _empty()
        self.money = 0.0
        self.calls = 0
        self.unpriced: set[str] = set()

    def add(self, call: dict) -> None:
        _add(self.tokens, call["tokens"])
        self.calls += 1
        if call["money"] is None:
            self.unpriced.add(call["model"] or "?")
        else:
            self.money += call["money"]

    def view(self) -> dict:
        return {
            "calls": self.calls,
            "tokens": self.tokens,
            "money": round(self.money, 6),
            "unpriced_models": sorted(self.unpriced),
        }


def _decide_flags(decide: dict, decisions: list[dict]) -> list[str]:
    """The question-4 subsets a decide span falls into (UC-105 BR-004)."""
    subsets = []
    if any(d["attrs"].get("decision.applied") is True for d in decisions):
        subsets.append("changed_the_game")
    if any(
        e["name"] == EVALUATION_EVENT and e["attrs"].get("gen_ai.evaluation.score.label") == "fail"
        for e in decide["events"]
    ):
        subsets.append("held")
    if any(d["attrs"].get("decision.blocked") is True for d in decisions):
        subsets.append("blocked")
    return subsets


def _call(s: dict, by_id: dict, table: dict) -> dict | None:
    """One model call with its caller, game, model and price; None for a span that is not one."""
    a = s["attrs"]
    if not s["name"].startswith("chat ") or a.get("gen_ai.operation.name", "chat") != "chat":
        return None
    parent = by_id.get((s["trace"], s["parent"]))
    pname = parent["name"] if parent else None
    if pname in DECIDE_SPANS:
        caller, conv = "decide", parent["attrs"].get(CONVERSATION_ID)
    elif pname == RETRO_SPAN:
        caller, conv = "retro", a.get(CONVERSATION_ID)
    else:
        caller, conv = "proposer", None
    kind = parent["attrs"].get("game.kind") if conv and parent else None
    provider = a.get("gen_ai.provider.name")
    # Price by the model that answered; a call without a response has none.
    model = a.get("gen_ai.response.model") or a.get("gen_ai.request.model")
    row = price_of(table, provider, model)
    tokens = _tokens(a)
    return {
        "span": s,
        "caller": caller,
        "game": conv,
        "kind": kind,
        "provider": provider,
        "model": model,
        "tokens": tokens,
        "money": None if row is None else money(tokens, row),
        "decide": (parent["trace"], parent["id"]) if caller == "decide" else None,
    }


def _group(calls: list[dict], key) -> dict:
    out: dict = defaultdict(Bucket)
    for c in calls:
        k = key(c)
        if k:
            out[k].add(c)
    return out


def _views(groups: dict) -> dict:
    return {k: v.view() for k, v in sorted(groups.items())}


def _conversation_tokens(decide_spans: list[dict], calls: list[dict]) -> dict:
    """Per game, the tokens of its spans with the conversation id: decide totals plus retro call."""
    by_conv: dict[str, dict] = defaultdict(_empty)
    for s in decide_spans:
        conv = s["attrs"].get(CONVERSATION_ID)
        if conv:
            _add(by_conv[conv], _tokens(s["attrs"]))
    for c in calls:
        if c["caller"] == "retro" and c["game"]:
            _add(by_conv[c["game"]], c["tokens"])
    return by_conv


def _games(calls: list[dict], decide_spans: list[dict], game_ids: list[str] | None) -> dict:
    per_game = _group(calls, lambda c: c["game"])
    # Cross-check (UC-105 A3): the conversation-id tokens must equal the sum of the model calls.
    by_conv = _conversation_tokens(decide_spans, calls)
    wanted = game_ids or sorted(set(per_game) | {g for g, t in by_conv.items() if any(t.values())})
    games = {}
    for gid in wanted:
        b = per_game.get(gid, Bucket())
        conv_tokens = by_conv.get(gid, _empty())
        mine = [c for c in calls if c["game"] == gid]
        games[gid] = {
            **b.view(),
            "kind": next((c["kind"] for c in mine if c["kind"]), None),
            "by_caller": {
                caller: _sum_view([c for c in mine if c["caller"] == caller]) for caller in ("decide", "retro")
            },
            "conversation_tokens": conv_tokens,
            "adds_up": conv_tokens == b.tokens,
            "has_model_call": b.calls > 0,
        }
    return games


def _decisions(spans: list[dict], decide_spans: list[dict], calls: list[dict]) -> tuple[list[dict], dict]:
    children: dict[tuple, list[dict]] = defaultdict(list)
    for s in spans:
        if s["parent"] and s["name"] == DECISION_SPAN:
            children[(s["trace"], s["parent"])].append(s)
    per_decide = _group(calls, lambda c: c["decide"])
    decisions = []
    subsets: dict[str, Bucket] = defaultdict(Bucket)
    for s in sorted(decide_spans, key=lambda x: x["start"]):
        key = (s["trace"], s["id"])
        b = per_decide.get(key)
        if b is None:
            continue  # rules path: no model call, no tokens
        flags = _decide_flags(s, children.get(key, []))
        decisions.append(
            {
                "span_id": s["id"],
                "game": s["attrs"].get(CONVERSATION_ID),
                "start_ns": s["start"],
                **b.view(),
                "totals_on_span": _tokens(s["attrs"]),
                "subsets": flags,
            }
        )
        mine = [c for c in calls if c["decide"] == key]
        for f in flags:  # counted once per subset it falls into
            for c in mine:
                subsets[f].add(c)
    return decisions, subsets


def reference(spans: list[dict], table: dict, game_ids: list[str] | None = None) -> dict:
    by_id = {(s["trace"], s["id"]): s for s in spans}
    calls = [c for c in (_call(s, by_id, table) for s in spans) if c]
    decide_spans = [s for s in spans if s["name"] in DECIDE_SPANS]
    decisions, subsets = _decisions(spans, decide_spans, calls)
    return {
        "price_table": {"date": table["date"], "source": table["source"], "currency": table["currency"]},
        "q1_per_model": _views(_group(calls, lambda c: f"{c['provider']}/{c['model']}")),
        "q2_per_caller": _views(_group(calls, lambda c: c["caller"])),
        "q2_per_game_kind": _views(_group(calls, lambda c: c["kind"] or "none")),
        "q3_per_game": _games(calls, decide_spans, game_ids),
        "q4_per_decision": decisions,
        "q4_subsets": {k: subsets[k].view() for k in ("changed_the_game", "held", "blocked")},
        "total": _sum_view(calls),
    }


def _sum_view(calls: list[dict]) -> dict:
    b = Bucket()
    for c in calls:
        b.add(c)
    return b.view()


# ------------------------------------------------------------------ output


def _line(label: str, v: dict, cur: str) -> str:
    t = v["tokens"]
    m = f"{v['money']:.6f} {cur}"
    if v["unpriced_models"]:
        m += " + unpriced " + ",".join(v["unpriced_models"])
    return (
        f"  {label:<44} calls {v['calls']:>4}  in {t['input']:>9}  out {t['output']:>8}  "
        f"cache_r {t['cache_read']:>8}  cache_w {t['cache_write']:>7}  reason {t['reasoning']:>7}  {m}"
    )


def render(ref: dict) -> str:
    cur = ref["price_table"]["currency"]
    p = ref["price_table"]
    out = [f"Price table {p['date']} ({p['source']})", "Q1 per provider/model"]
    out += [_line(k, v, cur) for k, v in ref["q1_per_model"].items()]
    out.append("Q2 per caller")
    out += [_line(k, v, cur) for k, v in ref["q2_per_caller"].items()]
    out.append("Q2 per game kind (none = proposer)")
    out += [_line(k, v, cur) for k, v in ref["q2_per_game_kind"].items()]
    out.append("Q3 per game")
    for gid, g in ref["q3_per_game"].items():
        note = "" if g["adds_up"] else "  DOES NOT ADD UP (UC-105 A3)"
        if not g["has_model_call"]:
            note += "  no model call (UC-105 A1)"
        out.append(_line(f"{gid} [{g['kind']}]", g, cur) + note)
    out.append("Q4 subsets")
    out += [_line(k, v, cur) for k, v in ref["q4_subsets"].items()]
    out.append(f"Q4 decisions with a model call: {len(ref['q4_per_decision'])}")
    out.append(_line("total", ref["total"], cur))
    return "\n".join(out)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--prices", required=True, help="price table JSON (docs/challenges/)")
    ap.add_argument("--traces", nargs="*", default=[], help="saved Jaeger v3 / OTLP JSON answers")
    ap.add_argument("--jaeger-url", help="Jaeger base incl. path, e.g. http://localhost/jaeger")
    ap.add_argument("--start", type=float, help="window start, epoch seconds")
    ap.add_argument("--end", type=float, help="window end, epoch seconds")
    ap.add_argument("--game-id", action="append", default=[], help="a challenge game (repeat)")
    ap.add_argument("--search-depth", type=int, default=1000)
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args(argv)
    if bool(a.traces) == bool(a.jaeger_url):
        ap.error("give either --traces or --jaeger-url")
    if a.jaeger_url and (a.start is None or a.end is None):
        ap.error("--jaeger-url needs --start and --end")
    try:
        table = load_prices(a.prices)
        if a.traces:
            spans = []
            for path in a.traces:
                with open(path, encoding="utf-8") as f:  # NOSONAR: operator CLI input, not a trust boundary
                    spans.extend(spans_from_doc(json.load(f)))
            spans = dedupe(spans)
        else:
            spans = fetch(jaeger_base(a.jaeger_url), a.start, a.end, a.game_id, a.search_depth)
    except (OSError, ValueError) as e:  # URLError is an OSError
        print(f"error: {e}", file=sys.stderr)
        return 2
    ref = reference(spans, table, a.game_id or None)
    print(json.dumps(ref, indent=2, sort_keys=True) if a.json else render(ref))
    return 0 if all(g["adds_up"] for g in ref["q3_per_game"].values()) else 3


if __name__ == "__main__":
    sys.exit(main())
