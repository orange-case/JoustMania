#!/usr/bin/env bash
# Self-contained tests for scripts/agent_archetype_fit.py: eligible real games and their
# exclusion reasons, bands by elimination pace, per-archetype ratios against control-arm shadow
# games run on the current parameters, the clamp to the archetype bounds, the journal as the real
# source, a deterministic proposal, --write changes only the fitted values of the flag file, and
# too few eligible games or an unreadable input propose nothing. A fixture, no stack, no GitHub.
#
# UC: UC-047 A10 A11 A12 A13 A14 BR-002 BR-005 BR-010
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/hermetic.sh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="${SCRIPT_DIR}/../agent_archetype_fit.py"
FLAGS="${SCRIPT_DIR}/../../services/flagd/agent.json"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
fails=0
ok() { # ok <name> <exit code of the condition>
  if [[ "$2" == 0 ]]; then echo "ok   $1"; else echo "FAIL $1"; fails=$((fails + 1)); fi
}

# fixture <dir> <eligible real games>
fixture() {
  python3 - "$1" "$2" "${FLAGS}" <<'PY'
import json, os, sys
out, n_real, flags = sys.argv[1], int(sys.argv[2]), sys.argv[3]
arch = json.load(open(flags))["flags"]["experiment.mock_archetype"]["variants"]
games, players, journal = [], [], []

def add(gid, kind, mode, dur, pace, per_player, arm=""):
    games.append({"game_id": gid, "kind": kind, "mode": mode, "duration": dur,
                  "elimination_pace": pace, "arm": arm})
    for j, (i, v) in enumerate(per_player):
        players.append({"game_id": gid, "kind": kind, "serial": f"{gid}S{j}",
                        "intensity": i, "movement_variance": v})
    if kind == "real":
        journal.append({"schema_version": 1, "game_id": gid,
                        "mode": "JoustFFA" if mode == "FFA" else mode,
                        "duration_seconds": dur, "metrics": {"elimination_pace": pace},
                        "players": [{"mean_intensity": i, "last_movement_variance": v}
                                    for i, v in per_player]})

# Eligible real games in three bands: pace low 1.0x, middle 3.0x, high 6.0x.
per_band = {"low": (1.0, 1.5, 0.04), "middle": (3.0, 1.2, 0.09), "high": (6.0, 2.0, 0.16)}
for b, (pace, inten, var) in per_band.items():
    for i in range(n_real // 3):
        add(f"r_{b}_{i}", "real", "FFA", 40.0, pace + 0.01 * i, [(inten, var)] * 4)
add("r_short", "real", "FFA", 5.0, 1.0, [(1.5, 0.04)] * 4)
add("r_dominant", "real", "FFA", 40.0, 1.0, [(1.5, 1.0)] + [(1.0, 0.01)] * 3)
add("r_still", "real", "FFA", 40.0, 1.0, [(1.0, 0.001)] * 4)
add("r_teams", "real", "Teams", 40.0, 1.0, [(1.5, 0.04)] * 4)

events, seq = [], 0
def ev(kind, **kw):
    global seq
    seq += 1
    events.append({"seq": seq, "kind": kind, **kw})

def shadow(name, i, inten, var, pace, arm="control"):
    gid = f"s_{name}_{i}"
    add(gid, "shadow", "FFA", 30.0, pace, [(inten, var)] * 4, arm)
    ev("game_assigned", game_id=gid, arm=arm, pattern={"archetype": name})

stale = dict(arch["slow_cascade"], amp=0.2)
names = ["calm_long", "quick_cascade", "slow_cascade", "jittery_novices"]
params = [arch["calm_long"], arch["quick_cascade"], stale, arch["jittery_novices"]]
ev("pattern_pinned", pattern={"archetypes": names,
                              "params_json": [json.dumps(p, sort_keys=True) for p in params]})
for i in range(4):
    shadow("calm_long", i, 1.0, 0.01, 0.5)
    shadow("quick_cascade", i, 0.1, 0.04, 2.015)
    shadow("slow_cascade", i, 1.0, 0.01, 0.5)
shadow("jittery_novices", 0, 1.0, 0.01, 0.5, arm="experimental")
add("s_oneshot", "shadow", "FFA", 30.0, 1.0, [(1.0, 0.01)] * 4)

os.makedirs(f"{out}/exp/exp_a")
with open(f"{out}/exp/exp_a/events.jsonl", "w") as f:
    f.write("\n".join(json.dumps(e) for e in events) + "\n")
json.dump({"games": games, "players": players}, open(f"{out}/ds.json", "w"))
with open(f"{out}/real_games.jsonl", "w") as f:
    f.write("\n".join(json.dumps(r) for r in journal) + "\n")
PY
}

fixture "${WORK}/a" 30
run() { python3 "${TOOL}" --dataset "${WORK}/a/ds.json" --experiments "${WORK}/a/exp" --flags "${FLAGS}" "$@"; }

rc=0; run --json >"${WORK}/rep.json" || rc=$?
ok "A proposal exits 0" "$([[ ${rc} == 0 ]]; echo $?)"

check() { # check <name> <python expression over rep, by(name)>
  python3 - "${WORK}/rep.json" "$2" <<'PY'
import json, sys
rep = json.load(open(sys.argv[1]))
by = {a["archetype"]: a for a in rep["archetypes"]}
sys.exit(0 if eval(sys.argv[2]) else 1)
PY
  ok "$1" $?
}
set +e
check "BR-002 30 eligible, one game per exclusion reason" \
  'rep["eligible"] == 30 and rep["excluded"] == {"dominant": 1, "other_mode": 1, "too_few_moved": 1, "too_short": 1}'
check "BR-005 three bands of 10 by elimination pace" \
  'all(rep["bands"][b]["games"] == 10 for b in ("low", "middle", "high")) and rep["bands"]["low"]["intensity"] == 1.5'
check "BR-005 calm_long: low band, base x1.5, amp x sqrt(4), a zero drift stays zero" \
  'by["calm_long"]["band"] == "low" and by["calm_long"]["params"]["base"] == 1.5 and by["calm_long"]["params"]["amp"] == 0.3 and by["calm_long"]["params"]["drift"] == 0.0 and by["calm_long"]["params"]["d_phase"] == 0.25'
check "A12 quick_cascade: intensity ratio 20 is clamped so every slot stays within 10" \
  'by["quick_cascade"]["params"]["base"] == 10.0 and "clamped:intensity" in by["quick_cascade"]["notes"] and by["quick_cascade"]["params"]["drift"] == 0.06 and by["quick_cascade"]["params"]["d_drift"] == 0.09'
check "BR-010 shadow games: changed params, experimental arm and unlabelled are left out" \
  'rep["shadow_skipped"] == {"experimental_arm": 1, "params_changed": 4, "unlabelled": 1}'
check "A11 an archetype without 4 usable shadow games keeps its parameters" \
  'by["slow_cascade"]["status"] == "insufficient_shadow" and by["slow_cascade"]["params"] == by["slow_cascade"]["old"] and by["late_burst"]["status"] == "insufficient_shadow"'

rc=0; run --json >"${WORK}/rep2.json" || rc=$?
cmp -s "${WORK}/rep.json" "${WORK}/rep2.json"
ok "BR-005 the same inputs give the same proposal" $?

rc=0; run --json --journal "${WORK}/a/real_games.jsonl" >"${WORK}/rep3.json" || rc=$?
python3 - "${WORK}/rep.json" "${WORK}/rep3.json" <<'PY'
import json, sys
a, b = (json.load(open(p)) for p in sys.argv[1:])
sys.exit(0 if [x["params"] for x in a["archetypes"]] == [x["params"] for x in b["archetypes"]] else 1)
PY
ok "A13 real games from the game journal give the same proposal" $?

cp "${FLAGS}" "${WORK}/agent.json"
run --write "${WORK}/agent.json" >/dev/null
python3 - "${FLAGS}" "${WORK}/agent.json" "${WORK}/rep.json" <<'PY'
import difflib, json, sys
old, new = (open(p).read().replace("\r\n", "\n") for p in sys.argv[1:3])
rep = json.load(open(sys.argv[3]))
want = {a["archetype"]: a["params"] for a in rep["archetypes"]}
v = json.loads(new)["flags"]["experiment.mock_archetype"]["variants"]
changed = sum(k != o for a in rep["archetypes"] for k, o in zip(a["params"].values(), a["old"].values()))
diff = [l for l in difflib.unified_diff(old.splitlines(), new.splitlines(), lineterm="", n=0)
        if l[:1] in "+-" and l[:3] not in ("+++", "---")]
ok = all(v[k] == p for k, p in want.items()) and len(diff) == 2 * changed and changed > 0
ok = ok and v["burst_holder"]["trigger_policy"] == {"kind": "burst"}
sys.exit(0 if ok else 1)
PY
ok "step 6 --write rewrites only the fitted values, layout and other flags unchanged" $?

fixture "${WORK}/b" 21
cp "${FLAGS}" "${WORK}/b.json"
rc=0
python3 "${TOOL}" --dataset "${WORK}/b/ds.json" --experiments "${WORK}/b/exp" --flags "${FLAGS}" \
  --write "${WORK}/b.json" >"${WORK}/b.out" || rc=$?
ok "A10 fewer than 24 eligible real games exit 3" "$([[ ${rc} == 3 ]]; echo $?)"
cmp -s "${FLAGS}" "${WORK}/b.json"
ok "A10 nothing is written" $?

rc=0
python3 "${TOOL}" --dataset "${WORK}/missing.json" --experiments "${WORK}/a/exp" --flags "${FLAGS}" \
  >/dev/null 2>&1 || rc=$?
ok "A14 an unreadable input exits 2" "$([[ ${rc} == 2 ]]; echo $?)"

echo "${fails} failure(s)"
[[ ${fails} == 0 ]]
