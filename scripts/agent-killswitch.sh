#!/usr/bin/env bash
# Set the agent's state, the `enabled` flag (agent flag set): off / watch / act (#999 dry-run helper; epic #1407).
#
#   off    the agent does not run (the stop)
#   watch  the agent observes, decides and records, but writes nothing to a game
#   act    decisions are applied
#
# Every state, `off` included, is an in-memory stack overlay written through the
# flag service (scripts/flags.sh). `off` is a PUT, never a clear: clearing the
# overlay falls back to the flagd default, which is `watch`, so it would not stop
# the agent. Nothing is persisted: a flag-service restart returns to the default.
#
# Usage:
#   ./scripts/agent-killswitch.sh act     # apply decisions
#   ./scripts/agent-killswitch.sh watch   # decide and record, write nothing
#   ./scripts/agent-killswitch.sh off     # stop the agent
#   ./scripts/agent-killswitch.sh status
#
# The change takes effect LIVE (the flag service pushes it to flagd at once): the
# agent reads `enabled` each cycle, so no agent restart is needed.
#
# If the flag service does not take the `off`, the helper prints the fallback
# brake (stop the agent container; the game keeps running without it) and exits
# non-zero.
#
# Env: FLAG_SERVICE_URL (default http://localhost/flags, the envoy route),
#      AGENT_CONTAINER (default joustmania-agent, named in the fallback).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FLAGS="${FLAGS_SH:-${SCRIPT_DIR}/flags.sh}"
export FLAGS_WRITER="${FLAGS_WRITER:-operator}"
AGENT_CONTAINER="${AGENT_CONTAINER:-joustmania-agent}"

fallback() {
  cat >&2 <<EOF_FALLBACK
agent kill-switch: the flag service did not take the change.
Fallback brake (the game keeps running without the agent):
  docker stop ${AGENT_CONTAINER}
Start it again later with: docker start ${AGENT_CONTAINER}
EOF_FALLBACK
  exit 1
}

case "${1:-status}" in
  act | watch)
    "${FLAGS}" set agent enabled "$1" >/dev/null
    echo "agent kill-switch -> $1"
    ;;
  off)
    "${FLAGS}" set agent enabled off >/dev/null || fallback
    echo "agent kill-switch -> off"
    ;;
  status)
    echo "agent kill-switch = $("${FLAGS}" variant agent enabled)"
    ;;
  *)
    echo "usage: $0 {act|watch|off|status}" >&2
    exit 2
    ;;
esac
