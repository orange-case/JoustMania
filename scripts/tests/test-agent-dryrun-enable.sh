#!/usr/bin/env bash
# Self-contained tests for scripts/agent-dryrun-enable.sh (FR-090): `on` sets the
# explicit candidate (experiment_dynamic_candidates = windows_frantic) and
# experiment_dynamic_enabled BEFORE the `enabled` (on) and `experiments_enabled`
# gates, `off` sets `enabled` off and clears the other three, and `status` shows them.
#
# Runs the helper against scripts/tests/fake_flag_service.py serving
# services/flagd/agent.json (default `watch`) with the REAL registry (epic #1407): the helper
# changes the flags only through the flag service, never a flag file.
#
# Runs as part of `make lint` (needs only bash + python3), or directly:
#   ./scripts/tests/test-agent-dryrun-enable.sh
#
# UC: UC-043 A16
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/hermetic.sh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
HELPER="${SCRIPT_DIR}/../agent-dryrun-enable.sh"
SHIPPED="${REPO_ROOT}/services/flagd/agent.json"
unset COMPOSE DOCKER || true
# A test that reaches for the real docker fails instead of using it
WORK="$(mktemp -d)"
# shellcheck source=scripts/tests/lib-fake-flag-service.sh
source "${SCRIPT_DIR}/lib-fake-flag-service.sh"
trap 'stop_fake_flag_service; rm -rf "${WORK}"' EXIT
export FAKE_AGENT_FLAGS="${WORK}/agent.json"
cp "${SHIPPED}" "${FAKE_AGENT_FLAGS}"
start_fake_flag_service "${WORK}"

pass=0
fail=0
ok() { echo "ok   - $1"; pass=$((pass + 1)); }
not_ok() { echo "FAIL - $1"; fail=$((fail + 1)); }

# expect <name> <expected-exit> <expected-output-substring> -- <args...>
expect() {
  local name="$1" want_rc="$2" want_out="$3"
  shift 4
  local out rc=0
  out="$("${HELPER}" "$@" 2>&1)" || rc=$?
  if [[ "${rc}" -eq "${want_rc}" && "${out}" == *"${want_out}"* ]]; then
    ok "${name}"
  else
    not_ok "${name} (rc=${rc}, want ${want_rc}; output: ${out})"
  fi
}

effective() {
  curl -sS "${FLAG_SERVICE_URL}/agent/$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["effective_default"])'
}

check() { # check <name> <flag> <want>
  local got
  got="$(effective "$2")"
  if [[ "${got}" == "$3" ]]; then ok "$1"; else not_ok "$1 (got ${got}, want $3)"; fi
}

# Line number of the first request matching a regex in the fake's request log.
line_of() { { grep -n -m1 -E "$1" "${FAKE_FLAG_LOG}" || true; } | cut -d: -f1; }

check "baseline: candidates are all" experiment_dynamic_candidates all
expect "status shows the candidates" 0 "experiment_dynamic_candidates = all" -- status
expect "status shows decision.backend" 0 "decision.backend = " -- status

# --- on: the candidate, then the gates ------------------------------------------
expect "on reports the candidate" 0 "experiment_dynamic_candidates -> windows_frantic" -- on
check "on sets the candidate windows_frantic" experiment_dynamic_candidates windows_frantic
check "on sets experiment_dynamic_enabled on" experiment_dynamic_enabled on
check "on sets enabled on" enabled on
check "on sets experiments_enabled on" experiments_enabled on
cand_line="$(line_of '^PUT /agent/experiment_dynamic_candidates ')"
dyn_line="$(line_of '^PUT /agent/experiment_dynamic_enabled ')"
enabled_line="$(line_of '^PUT /agent/enabled ')"
gate_line="$(line_of '^PUT /agent/experiments_enabled ')"
if [[ -n "${cand_line}" && -n "${dyn_line}" && -n "${enabled_line}" && -n "${gate_line}" &&
  "${cand_line}" -lt "${enabled_line}" && "${cand_line}" -lt "${gate_line}" &&
  "${dyn_line}" -lt "${enabled_line}" && "${dyn_line}" -lt "${gate_line}" ]]; then
  ok "on writes the candidate and the declarer gate before enabled and experiments_enabled"
else
  not_ok "on writes the candidate before the gates (candidate=${cand_line} dynamic=${dyn_line} enabled=${enabled_line} experiments_enabled=${gate_line})"
fi
expect "status shows the candidate variant" 0 "experiment_dynamic_candidates = windows_frantic" -- status

# --- off: all four back to the baseline, enabled by a write of off ---------------
expect "off reports the candidate baseline" 0 "experiment_dynamic_candidates -> all (baseline)" -- off
check "off clears the candidate" experiment_dynamic_candidates all
check "off clears experiment_dynamic_enabled" experiment_dynamic_enabled off
check "off sets enabled off over a watch default" enabled off
if grep -q "^DELETE /agent/enabled" "${FAKE_FLAG_LOG}"; then not_ok "off never DELETEs enabled"; else ok "off never DELETEs enabled"; fi
check "off clears experiments_enabled" experiments_enabled off
if grep -q "^DELETE /agent/experiment_dynamic_candidates" "${FAKE_FLAG_LOG}"; then ok "off is a flag-service DELETE of the candidate"; else not_ok "off is a flag-service DELETE of the candidate"; fi

# --- the baseline file is never written -------------------------------------------
if cmp -s "${SHIPPED}" "${FAKE_AGENT_FLAGS}"; then ok "the baseline file is unchanged"; else not_ok "the baseline file is unchanged"; fi

expect "unknown action is rejected" 2 "usage:" -- maybe

echo "${pass} passed, ${fail} failed"
[[ "${fail}" -eq 0 ]]
