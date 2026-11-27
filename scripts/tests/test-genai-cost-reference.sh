#!/usr/bin/env bash
# Self-contained tests for scripts/genai_cost_reference.py (#2718): on fixture spans, a game's
# reference tokens add up to its `chat {model}` span tokens; the decide span's totals and its model
# calls are never both counted; per-caller split (decide, retro, proposer) and game kind; money from
# the frozen price table (cached input at the cache price, reasoning within output, the answering
# model with a dated id, jev at zero, an unknown model unpriced); the question-4 subsets; a game
# whose decide totals differ from its calls is flagged and exits 3 (A3); and the Jaeger mode sends
# only GET requests to the v3 query API, one per game id plus the window.
#
# UC: UC-105 BR-003 A3
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/hermetic.sh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="${SCRIPT_DIR}/../genai_cost_reference.py"
PRICES="${SCRIPT_DIR}/../../docs/challenges/genai-prices-2026-11-27.json"
WORK="$(mktemp -d)"
FAKE_PID=""
trap '[[ -n "${FAKE_PID}" ]] && kill "${FAKE_PID}" 2>/dev/null; rm -rf "${WORK}"' EXIT
fails=0
ok() { # ok <name> <exit code of the condition>
  if [[ "$2" == 0 ]]; then echo "ok   $1"; else echo "FAIL $1"; fails=$((fails + 1)); fi
}

python3 - "${WORK}/traces.json" <<'PY'
import json, sys

def av(v):
    if isinstance(v, bool):
        return {"boolValue": v}
    if isinstance(v, int):
        return {"intValue": str(v)}
    return {"stringValue": v}

def span(tid, sid, parent, name, attrs, events=(), start=0):
    return {"traceId": tid, "spanId": sid, "parentSpanId": parent, "name": name,
            "startTimeUnixNano": str(start),
            "attributes": [{"key": k, "value": av(v)} for k, v in attrs.items()],
            "events": [{"name": n, "attributes": [{"key": k, "value": av(v)} for k, v in a.items()]}
                       for n, a in events]}

def chat(tid, sid, parent, provider, model, inp, out, cr=0, cw=0, reason=0, conv=None, resp=None):
    a = {"gen_ai.operation.name": "chat", "gen_ai.provider.name": provider,
         "gen_ai.request.model": model, "gen_ai.usage.input_tokens": inp,
         "gen_ai.usage.output_tokens": out}
    if resp:
        a["gen_ai.response.model"] = resp
    if cr:
        a["gen_ai.usage.cache_read.input_tokens"] = cr
    if cw:
        a["gen_ai.usage.cache_write.input_tokens"] = cw
    if reason:
        a["gen_ai.usage.reasoning.output_tokens"] = reason
    if conv:
        a["gen_ai.conversation.id"] = conv
    return span(tid, sid, parent, f"chat {model}", a)

def decide(tid, sid, game, kind, totals, events=(), start=0):
    a = {"gen_ai.operation.name": "invoke_agent", "gen_ai.conversation.id": game,
         "game.id": game, "game.kind": kind}
    a.update({f"gen_ai.usage.{k}": v for k, v in totals.items()})
    return span(tid, sid, "c0", "invoke_agent joustmania-agent", a, events, start)

A, B = "game_aaaaaaaaaaaa", "game_bbbbbbbbbbbb"
agent = [
    # Game A, real. D1: two haiku calls (dated response model, cache reads), an applied decision.
    decide("t1", "d1", A, "real", {"input_tokens": 1500, "output_tokens": 300,
                                    "cache_read.input_tokens": 400}, start=1),
    chat("t1", "c11", "d1", "anthropic", "claude-haiku-4-5", 1000, 200, cr=400,
         resp="claude-haiku-4-5-20251001"),
    chat("t1", "c12", "d1", "anthropic", "claude-haiku-4-5", 500, 100),
    span("t1", "x1", "d1", "agent.decision", {"decision.applied": True}),
    # D2: one sonnet call with reasoning, held below the threshold, and a blocked decision.
    decide("t1", "d2", A, "real", {"input_tokens": 2000, "output_tokens": 500,
                                    "reasoning.output_tokens": 100},
           events=[("gen_ai.evaluation.result", {"gen_ai.evaluation.score.label": "fail"})], start=2),
    chat("t1", "c21", "d2", "anthropic", "claude-sonnet-5-5", 2000, 500, reason=100,
         resp="claude-sonnet-5-5"),
    span("t1", "x2", "d2", "agent.decision", {"decision.blocked": True}),
    # D3: rules path, no model call.
    decide("t1", "d3", A, "real", {}, start=3),
    # Retro of game A.
    span("t1", "r1", "root", "agent.llm.retro", {"game.id": A, "game.kind": "real"}),
    chat("t1", "c31", "r1", "anthropic", "claude-opus-5-5", 3000, 1000, conv=A,
         resp="claude-opus-5-5"),
    # Game B, shadow: a jev call whose decide totals do not match (A3).
    decide("t2", "d4", B, "shadow", {"input_tokens": 999, "output_tokens": 10}, start=4),
    chat("t2", "c41", "d4", "jev", "jev-1", 300, 10),
    # Proposer: no game id; one priced, one unknown model.
    span("t3", "p0", "", "code_improvement.propose_attempt", {}),
    chat("t3", "c51", "p0", "anthropic", "claude-opus-5-5", 1000, 100, resp="claude-opus-5-5"),
    chat("t3", "c52", "p0", "other", "mystery-1", 10, 10),
]
coordinator = [span("t1", "root", "", "Joust Free-for-All", {"game.id": A, "game.kind": "real"})]
doc = {"result": {"resourceSpans": [
    {"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "agent"}}]},
     "scopeSpans": [{"scope": {"name": "github.com/joustmania/agent"}, "spans": agent}]},
    {"resource": {"attributes": [{"key": "service.name",
                                  "value": {"stringValue": "game-coordinator-service"}}]},
     "scopeSpans": [{"spans": coordinator}]},
]}}
json.dump(doc, open(sys.argv[1], "w"))
PY

# --- file mode, every game: B does not add up -> exit 3
set +e
python3 "${TOOL}" --prices "${PRICES}" --traces "${WORK}/traces.json" "${WORK}/traces.json" --json \
  >"${WORK}/all.json"
rc=$?
set -e
ok "a game that does not add up exits 3 (A3)" "$([[ ${rc} == 3 ]]; echo $?)"

check() { # check <name> <python expression over r>
  python3 - "${WORK}/all.json" "$2" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
sys.exit(0 if eval(sys.argv[2]) else 1)
PY
}
A=game_aaaaaaaaaaaa
B=game_bbbbbbbbbbbb
t() { set +e; check "$1" "$2"; local c=$?; set -e; ok "$1" "${c}"; }

t "game A tokens = its chat span tokens" \
  "r['q3_per_game']['$A']['tokens'] == {'input': 6500, 'output': 1800, 'cache_read': 400, 'cache_write': 0, 'reasoning': 100}"
t "game A adds up (decide totals + retro = calls, nothing counted twice)" \
  "r['q3_per_game']['$A']['adds_up'] and r['q3_per_game']['$A']['conversation_tokens'] == r['q3_per_game']['$A']['tokens']"
t "game B does not add up" "not r['q3_per_game']['$B']['adds_up']"
t "game A money (cache at cache price, reasoning within output, dated model id)" \
  "abs(r['q3_per_game']['$A']['money'] - 0.04364) < 1e-9"
t "game A by caller" \
  "r['q3_per_game']['$A']['by_caller']['decide']['calls'] == 3 and r['q3_per_game']['$A']['by_caller']['retro']['calls'] == 1"
t "game kinds" "r['q3_per_game']['$A']['kind'] == 'real' and r['q3_per_game']['$B']['kind'] == 'shadow'"
t "callers" \
  "{k: v['calls'] for k, v in r['q2_per_caller'].items()} == {'decide': 4, 'retro': 1, 'proposer': 2}"
t "game kind split (proposer is none)" \
  "r['q2_per_game_kind']['none']['calls'] == 2 and r['q2_per_game_kind']['shadow']['calls'] == 1"
t "jev is priced at zero" "r['q1_per_model']['jev/jev-1']['money'] == 0 and not r['q1_per_model']['jev/jev-1']['unpriced_models']"
t "price by the answering model" "'anthropic/claude-haiku-4-5-20251001' in r['q1_per_model']"
t "unknown model is unpriced, not guessed" "r['total']['unpriced_models'] == ['mystery-1']"
t "total money" "abs(r['total']['money'] - 0.04964) < 1e-9"
t "rules-path decide has no decision row" "len(r['q4_per_decision']) == 3"
t "q4 subsets" \
  "abs(r['q4_subsets']['changed_the_game']['money'] - 0.00264) < 1e-9 and abs(r['q4_subsets']['held']['money'] - 0.009) < 1e-9 and abs(r['q4_subsets']['blocked']['money'] - 0.009) < 1e-9"
t "price table date carried" "r['price_table']['date'] == '2026-11-27'"

# --- one game that adds up -> exit 0
set +e
python3 "${TOOL}" --prices "${PRICES}" --traces "${WORK}/traces.json" --game-id "${A}" >"${WORK}/a.txt"
rc=$?
set -e
ok "a game that adds up exits 0" "$([[ ${rc} == 0 ]]; echo $?)"
ok "text report names the game" "$(grep -q "${A} \[real\]" "${WORK}/a.txt"; echo $?)"

# --- refused inputs exit 2 without reading or fetching
OUTSIDE="$(mktemp -d -p "${HOME}")"
cp "${WORK}/traces.json" "${OUTSIDE}/traces.json"
set +e
python3 "${TOOL}" --prices "${PRICES}" --traces "${OUTSIDE}/traces.json" >/dev/null 2>"${WORK}/err1"
rc1=$?
python3 "${TOOL}" --prices "${PRICES}" --traces "${OUTSIDE}/traces.json" --allow-dir "${OUTSIDE}" >/dev/null 2>&1
rc2=$?
python3 "${TOOL}" --prices "${PRICES}" --jaeger-url "file:///etc" --start 1 --end 2 >/dev/null 2>"${WORK}/err3"
rc3=$?
set -e
rm -rf "${OUTSIDE}"
ok "a traces file outside the allowed directories is refused" "$([[ ${rc1} == 2 ]] && grep -q 'outside the allowed' "${WORK}/err1"; echo $?)"
ok "--allow-dir admits it" "$([[ ${rc2} == 3 ]]; echo $?)"
ok "a non-http Jaeger URL is refused" "$([[ ${rc3} == 2 ]] && grep -q 'http(s) URL' "${WORK}/err3"; echo $?)"

# --- Jaeger mode against a fake v3 query API: GET only, one window query + one per game id
python3 - "${WORK}" <<'PY' &
import http.server, sys, os
work = sys.argv[1]
body = open(os.path.join(work, "traces.json"), "rb").read()
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def do_GET(self):
        with open(os.path.join(work, "requests.log"), "a") as f:
            f.write("GET " + self.path + "\n")
        if not self.path.startswith("/jaeger/api/v3/traces?"):
            self.send_response(404); self.end_headers(); return
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
        self.wfile.write(body)
    def do_POST(self):
        with open(os.path.join(work, "requests.log"), "a") as f:
            f.write("POST " + self.path + "\n")
        self.send_response(405); self.end_headers()
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(os.path.join(work, "port"), "w").write(str(s.server_address[1]))
s.serve_forever()
PY
FAKE_PID=$!
for _ in $(seq 50); do [[ -s "${WORK}/port" ]] && break; sleep 0.1; done
PORT="$(cat "${WORK}/port")"
set +e
python3 "${TOOL}" --prices "${PRICES}" --jaeger-url "http://127.0.0.1:${PORT}/jaeger" \
  --start 1700000000 --end 1700003600 --game-id "${A}" --json >"${WORK}/j.json"
rc=$?
set -e
ok "jaeger mode exits 0 for a game that adds up" "$([[ ${rc} == 0 ]]; echo $?)"
ok "jaeger mode sends GET only" "$(! grep -qv '^GET ' "${WORK}/requests.log"; echo $?)"
ok "one window query and one per game id" "$([[ $(wc -l <"${WORK}/requests.log") == 2 ]]; echo $?)"
ok "game query filters by conversation id" \
  "$(grep -q 'query.attributes=%7B%22gen_ai.conversation.id%22%3A+%22game_aaaaaaaaaaaa%22%7D' "${WORK}/requests.log"; echo $?)"
ok "queries the agent service" "$(grep -c 'query.serviceName=agent' "${WORK}/requests.log" | grep -qx 2; echo $?)"
ok "duplicate traces across queries are counted once" \
  "$(python3 -c "import json,sys; r=json.load(open('${WORK}/j.json')); sys.exit(0 if r['q3_per_game']['${A}']['calls']==4 else 1)"; echo $?)"

echo
if [[ ${fails} -gt 0 ]]; then echo "${fails} FAILED"; exit 1; fi
echo "all passed"
