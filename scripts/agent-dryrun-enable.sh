#!/usr/bin/env bash
# Enable the experiment cohort loop for a `make dry-run` (#1077; epic #1407).
#
# The LIVE gate of the cohort loop is the `experiments_enabled` flag (agent set),
# `off` in the baseline; experiment arms are written only while `enabled` is `act`.
# The experiment to run is the dynamic declarer's explicit candidate (UC-043
# BR-012): `experiment_dynamic_candidates` = `windows_frantic` (`windows=frantic`)
# with `experiment_dynamic_enabled` on.
# This helper sets them through the flag service (scripts/flags.sh):
#   on   -> the candidate and `experiment_dynamic_enabled` FIRST (so the first
#           enabled tick already sees them), then `enabled` to `act` and
#           `experiments_enabled` on (arm writes need `enabled` = `act`, so the agent
#           also writes the interventions the CI allow-list permits)
#   off  -> `enabled` set to `off` (a clear would fall back to the flagd default
#           `watch` and leave the agent running), the other three overlays cleared
# Nothing is persisted (re-review N4). The in-game decision backend is NOT touched
# here: use ./scripts/agent-backend.sh (decision.backend, #1232).
#
# Usage:
#   ./scripts/agent-dryrun-enable.sh on
#   ./scripts/agent-dryrun-enable.sh off
#   ./scripts/agent-dryrun-enable.sh status
#
# The flips take effect LIVE (no agent restart). A restart is only needed for the
# AGENT_INFERENCE_BASE_URL env value and the ANTHROPIC_API_KEY (read at start); the
# text backend and its model are the inference.backend / inference.model flags
# (live, #1810, #1907).
#
# Env: FLAG_SERVICE_URL (default http://localhost/flags, the envoy route).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FLAGS="${FLAGS_SH:-${SCRIPT_DIR}/flags.sh}"
export FLAGS_WRITER="${FLAGS_WRITER:-operator}"
# The reviewed experiment_dynamic_candidates variant `on` selects (services/flagd/ci/agent.json).
CANDIDATE_VARIANT="windows_frantic"

case "${1:-status}" in
  on)
    "${FLAGS}" set agent experiment_dynamic_candidates "${CANDIDATE_VARIANT}" >/dev/null
    echo "  experiment_dynamic_candidates -> ${CANDIDATE_VARIANT}"
    "${FLAGS}" set agent experiment_dynamic_enabled on >/dev/null
    echo "  experiment_dynamic_enabled -> on"
    "${FLAGS}" set agent enabled act >/dev/null
    echo "  enabled -> act"
    "${FLAGS}" set agent experiments_enabled on >/dev/null
    echo "  experiments_enabled -> on"
    echo "dry-run experiment gate -> on"
    ;;
  off)
    "${FLAGS}" set agent enabled off >/dev/null
    echo "  enabled -> off"
    for flag in experiments_enabled experiment_dynamic_enabled experiment_dynamic_candidates; do
      "${FLAGS}" clear agent "${flag}"
      echo "  ${flag} -> $("${FLAGS}" variant agent "${flag}") (baseline)"
    done
    echo "dry-run experiment gate -> off"
    ;;
  status)
    for flag in enabled experiments_enabled experiment_dynamic_enabled experiment_dynamic_candidates decision.backend; do
      echo "${flag} = $("${FLAGS}" variant agent "${flag}")"
    done
    ;;
  *)
    echo "usage: $0 {on|off|status}" >&2
    exit 2
    ;;
esac
