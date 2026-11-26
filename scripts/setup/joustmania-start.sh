#!/bin/bash
# JoustMania start on the stage Pi (UC-060, #2027). Run by joustmania.service.
#
# Offline-safe (UC-060 BR-001, D1): no `git pull` and no image pull at boot.
# The checkout changes only through joustmania-upgrade.sh, which pulls in its
# prepare step. Boot runs `up --pull never` with the images on the Pi; a
# missing image is named with the upgrade command that recovers it.
# Two phases (BR-004, R2): the game services (and connect-proxy, dashboard,
# envoy) start and are judged first; observability and the agent start after,
# and only make the state `degraded`, never fail the unit.
#
#   joustmania-start.sh                  both phases (a start by hand, a rev-2 unit)
#   joustmania-start.sh --game           phase 1 only; start.env gets phase=game
#                                        and the observability services (obs=)
#   joustmania-start.sh --observability  phase 2 for the services phase 1 recorded
# The rev-3 unit runs --game, then the root LED helper, then --observability,
# so the ACT LED shows the game's state as soon as the game is playable.

set -euo pipefail

case "${1:-}" in
    "") MODE=all ;;
    --game) MODE=game ;;
    --observability) MODE=observability ;;
    *)
        echo "usage: joustmania-start.sh [--game|--observability]" >&2
        exit 64
        ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOUSTMANIA_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"
# Start state lives in the unit's RuntimeDirectory (RUNTIME_DIRECTORY); an old
# unit has none, so fall back to /tmp. JOUSTMANIA_STATUS_DIR is the test override.
# Writing state is never fatal.
STATE_DIR="${JOUSTMANIA_STATUS_DIR:-${RUNTIME_DIRECTORY:-/tmp/joustmania}}"
STATE_FILE="$STATE_DIR/start.env"
START_RESULT="failed"

cd "$JOUSTMANIA_DIR"

# JOUSTMANIA_UNIT_REV comes from the installed unit (rev 2+). Without it the
# unit is outdated (it still has the teardown ExecStartPre and `down` at stop)
# or the script runs by hand: say so on every start, and never fail, so the
# old unit's Restart=on-failure cannot loop.
STATE_MISSING=""
STATE_DOWN=""
STATE_REASON=""
STATE_PHASE="$([[ $MODE == game ]] && echo game || echo start)"
OBS_PHASE=()
write_state() { # $1 = phase, $2 = result
    {
        mkdir -p "$STATE_DIR" &&
            printf 'phase=%s\nresult=%s\nreason=%s\nhead=%s\nimage_tag=%s\nmissing=%s\ndown=%s\nobs=%s\nts=%s\n' \
                "$1" "$2" "$STATE_REASON" "${HEAD_SHORT:-unknown}" "${EFFECTIVE_IMAGE_TAG:-unknown}" \
                "${STATE_MISSING# }" "${STATE_DOWN# }" "${OBS_PHASE[*]:-}" "$(date -Iseconds)" \
                >"$STATE_FILE"
    } 2>/dev/null || true
}
on_exit() {
    local rc=$?
    write_state "$STATE_PHASE" "$([[ $rc -eq 0 ]] && echo "$START_RESULT" || echo failed)"
    if [[ $rc -ne 0 && -z "${JOUSTMANIA_UNIT_REV:-}" ]]; then
        echo "unit outdated: sudo $JOUSTMANIA_DIR/scripts/setup/install_autostart.sh (start failed, exit $rc; exiting 0 so the old unit does not loop)"
        exit 0
    fi
}
trap on_exit EXIT

# Every compose call goes through joustmania-compose.sh: docker-compose.yml
# (plus the gateway overlay when it is configured, UC-035 BR-015), never the
# dev override that publishes the mock control and gRPC ports
# to the venue network (#1319), with .env and, when present, the git-ignored
# .env.local (stage IMAGE_TAG and decision-backend API keys, #1488).
COMPOSE_WRAPPER="$SCRIPT_DIR/joustmania-compose.sh"
compose() {
    "$COMPOSE_WRAPPER" "$@"
}

# Phase 2: observability and the agent, with their own timeout. A failure only
# makes the state degraded (UC-060 A1); the unit still succeeds.
observability_phase() {
    [[ ${#OBS_PHASE[@]} -gt 0 ]] || return 0
    local obs_timeout="${JOUSTMANIA_OBS_TIMEOUT_S:-300}" obs_rc=0 obs_deadline obs_left
    echo "Phase 2/2: starting observability (timeout ${obs_timeout} s): ${OBS_PHASE[*]}"
    obs_deadline=$((SECONDS + obs_timeout))
    timeout "$((obs_timeout + 30))" "$COMPOSE_WRAPPER" up -d --pull never --wait --wait-timeout "$obs_timeout" "${OBS_PHASE[@]}" || obs_rc=$?
    # One retry inside the same budget: a slow Jaeger can be unhealthy for the
    # first attempt and fine a minute later, which left Grafana down (#2202).
    # `up` only starts what is not running, so nothing is recreated (R1).
    if [[ $obs_rc -ne 0 ]]; then
        obs_left=$((obs_deadline - SECONDS - ${JOUSTMANIA_OBS_RETRY_PAUSE_S:-5}))
        if [[ $obs_left -ge 30 ]]; then
            echo "WARN: observability did not come up (exit $obs_rc); retrying once (${obs_left} s left)"
            sleep "${JOUSTMANIA_OBS_RETRY_PAUSE_S:-5}"
            obs_rc=0
            timeout "$((obs_left + 30))" "$COMPOSE_WRAPPER" up -d --pull never --wait --wait-timeout "$obs_left" "${OBS_PHASE[@]}" || obs_rc=$?
        fi
    fi
    if [[ $obs_rc -ne 0 ]]; then
        STATE_DOWN+=" observability(exit $obs_rc)"
        START_RESULT="degraded"
        echo "WARN: observability did not come up (exit $obs_rc); the game keeps running (UC-060 A1)"
    fi
}

if [[ $MODE == observability ]]; then
    # Phase 1 ran in the unit's previous ExecStart and left its record; only
    # observability services from it are started.
    # shellcheck source=lib-joustmania.sh
    source "$SCRIPT_DIR/lib-joustmania.sh"
    recorded() { sed -n "s/^$1=//p" "$STATE_FILE" 2>/dev/null | head -n 1; }
    START_RESULT="$(recorded result)"
    if [[ "$(recorded phase)" != game || ! "$START_RESULT" =~ ^(ok|degraded)$ ]]; then
        START_RESULT="failed"
        echo "FAIL: $STATE_FILE records no finished game phase; run joustmania-start.sh --game first"
        exit 1
    fi
    STATE_REASON="$(recorded reason)"
    HEAD_SHORT="$(recorded head)"
    EFFECTIVE_IMAGE_TAG="$(recorded image_tag)"
    STATE_MISSING="$(recorded missing)"
    STATE_DOWN="$(recorded down)"
    for svc in $(recorded obs); do
        if joustmania_in_list "$svc" "${JOUSTMANIA_OBS_SERVICES[@]}"; then OBS_PHASE+=("$svc"); fi
    done
    observability_phase
    echo "JoustMania started: state $START_RESULT"
    exit 0
fi

echo "=== JoustMania Startup ==="
echo "Working directory: $JOUSTMANIA_DIR"
if [[ -z "${JOUSTMANIA_UNIT_REV:-}" ]]; then
    echo "unit outdated or not run by the rev-2 unit (no JOUSTMANIA_UNIT_REV): sudo $JOUSTMANIA_DIR/scripts/setup/install_autostart.sh"
fi

mkdir -p "$STATE_DIR" 2>/dev/null || echo "state dir $STATE_DIR not writable; continuing"

# Docker may still be coming up: wait for it (the unit has Requires=docker.service)
for _ in $(seq 1 "${JOUSTMANIA_DOCKER_WAIT_S:-60}"); do
    docker info >/dev/null 2>&1 && break
    sleep 1
done
# compose config works without the daemon, so without this check every image
# would look missing and the operator would be sent to re-pull them.
if ! docker info >/dev/null 2>&1; then
    echo "FAIL: docker not reachable (daemon not running, or $(id -un 2>/dev/null || echo this user) not in the docker group); nothing started"
    exit 1
fi

# shellcheck source=lib-joustmania.sh
source "$SCRIPT_DIR/lib-joustmania.sh"

# No git operation that changes the checkout and no registry contact at boot
# (UC-060 BR-001, D1): the clock may be hours stale before NTP sync (no RTC),
# so TLS fails, and a pull would only recreate containers (R1).
HEAD_SHORT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
HEAD_TAG="$(git describe --tags --exact-match 2>/dev/null || echo none)"
# Effective IMAGE_TAG as compose resolves it (shell env, then .env.local, then
# .env; quotes, `export` and inline comments handled by compose): the tag of the
# game-coordinator image from `config --images`, which prints only image refs.
EFFECTIVE_IMAGE_TAG="unknown"
COMPOSE_IMAGES="$(compose config --images 2>/dev/null || true)"
GC_IMAGE="$(grep -m 1 '/game-coordinator-service[:@]' <<<"$COMPOSE_IMAGES" || true)"
GC_IMAGE="${GC_IMAGE%%@*}"
if [[ "${GC_IMAGE##*/}" == *:* ]]; then
    EFFECTIVE_IMAGE_TAG="${GC_IMAGE##*:}"
fi
echo "Version: commit $HEAD_SHORT, tag $HEAD_TAG, IMAGE_TAG $EFFECTIVE_IMAGE_TAG (boot pulls nothing; upgrade with scripts/setup/joustmania-upgrade.sh)"
write_state start starting

# Say which env files apply, never their contents (they hold API keys)
if [[ -f .env.local ]]; then
    echo "Env files: .env + .env.local (.env.local wins)"
else
    echo "Env files: .env only (no .env.local: release IMAGE_TAG, no decision-backend keys)"
fi
# Pin (UC-060 BR-003): joustmania-compose.sh exports the IMAGE_TAG derived from
# a pinned HEAD, so an IMAGE_TAG in .env.local is ignored; say so.
if PIN_IMAGE_TAG="$(joustmania_pin_image_tag "$JOUSTMANIA_DIR")"; then
    echo "Pinned checkout: IMAGE_TAG $PIN_IMAGE_TAG derived from HEAD"
    if joustmania_env_local_sets .env.local IMAGE_TAG; then
        echo "WARN: .env.local sets IMAGE_TAG; it is ignored (the pinned checkout decides, UC-060 BR-003). Remove it."
    fi
elif git -C "$JOUSTMANIA_DIR" symbolic-ref -q HEAD >/dev/null 2>&1; then
    # A4: a branch (proven: HEAD is a symbolic ref) runs whatever was pulled or
    # checked out by hand, with the image tag of .env/.env.local, so config and
    # images need not match (C-040). Start nothing; the unit fails (ACT LED
    # 5 Hz) and status shows the reason. A version older than MIN_PINNABLE has
    # no such check: the upgrade command refuses to switch to one (A2 step 1).
    BRANCH="$(git -C "$JOUSTMANIA_DIR" symbolic-ref -q --short HEAD 2>/dev/null || echo unknown)"
    STATE_REASON="not pinned (branch $BRANCH); pin with scripts/setup/joustmania-upgrade.sh"
    echo "FAIL: the checkout is not pinned (branch $BRANCH at commit $HEAD_SHORT); nothing started (UC-060 A4)."
    echo "Pin it to a release tag or an exact commit sha, $(joustmania_missing_hint "$JOUSTMANIA_DIR")"
    exit 1
else
    # git cannot read the checkout (no .git, a damaged object after a power
    # loss, no git binary, a different owner): a branch is not proven, so fail
    # open (A4): start with the image tag of .env/.env.local, state degraded.
    STATE_REASON="git cannot read the checkout"
    echo "WARN: git cannot read the checkout, so the pin is unknown; starting with the IMAGE_TAG of .env/.env.local, state degraded (UC-060 A4). Check: git -C $JOUSTMANIA_DIR status"
fi

# Targeted cleanup only: a joustmania-* container that is not ours blocks the
# fixed container names, so remove it and say so. Compose itself says which
# containers are ours (`ps -a` resolves the project name from the shell env,
# .env and .env.local exactly as `up` does, and lists profile-inactive services
# too). A container whose compose working dir is this checkout is kept only when
# its project is one compose listed: one from this checkout under another project
# name (created before COMPOSE_PROJECT_NAME was set, or by a plain
# `docker compose up`) would block `up -d` with a name conflict, so it is
# foreign. If compose cannot list them, remove nothing. Our own containers are
# never touched here: compose keeps or recreates them (no blanket rm, prune or
# down; NFR-026).
echo "Checking for foreign joustmania containers..."
if OWN_LIST="$(compose ps -a --format '{{.Name}}|{{.Project}}' 2>/dev/null)"; then
    OWN_CONTAINERS="$(cut -d'|' -f1 <<<"$OWN_LIST")"
    OWN_PROJECTS="$(cut -s -d'|' -f2 <<<"$OWN_LIST" | sort -u)"
    CONTAINERS="$(docker ps -a --filter "name=joustmania-" --format '{{.Names}}|{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}' 2>/dev/null || true)"
    while IFS='|' read -r cname cproject cworkdir; do
        [[ -n "$cname" ]] || continue
        grep -qxF -- "$cname" <<<"$OWN_CONTAINERS" && continue
        if [[ "$cworkdir" == "$JOUSTMANIA_DIR" && -n "$cproject" ]] && grep -qxF -- "$cproject" <<<"$OWN_PROJECTS"; then
            continue
        fi
        echo "Removing foreign container $cname (project '${cproject:-none}')"
        docker rm -f "$cname" >/dev/null 2>&1 || true
    done <<<"$CONTAINERS"
else
    echo "Could not list this project's containers; removing none"
fi

# Set all audio devices to max volume
echo "Setting audio volume..."
for card in "${JOUSTMANIA_ASOUND_DIR:-/proc/asound}"/card[0-9]*; do
  [[ -e "$card" ]] || continue
  card_num=$(basename "$card" | sed 's/card//')
  # pipefail + errexit: a card without controls or a failing amixer is fine
  { amixer -c "$card_num" scontrols 2>/dev/null || true; } | sed -e "s/^Simple mixer control //" | while read -r ctrl; do
    amixer -c "$card_num" set "$ctrl" 100% unmute 2>/dev/null || true
  done || true
done
echo "Audio volume configured"

# Real controllers are read through rust-hid, the only HID backend. A
# joustmania-python-hid container from an older install is an orphan now
# (no compose service), so `up -d --remove-orphans` below removes it.

# The operator pad (UC-015, #2050) is opt-in: COMPOSE_PROFILES=operator-pad,
# in .env.local or a systemd drop-in (never in .env: it is tracked); a unit
# drop-in Environment= wins over .env.local.
# A pad container left over from a run with the profile on would keep its
# exclusive grab of the pad and keep sending commands (restart:
# unless-stopped, and `up -d --remove-orphans` keeps a profile-inactive
# container), so remove it whenever the profile is off. Idempotent.
# The services go through a variable: `config | grep -q` under pipefail dies of
# SIGPIPE when the list is long and grep exits early (audit F4).
COMPOSE_SERVICES="$(compose config --services 2>/dev/null || true)"
if ! grep -qx operator-pad <<<"$COMPOSE_SERVICES"; then
    docker rm -f joustmania-operator-pad >/dev/null 2>&1 || true
fi

# Which active service runs which image and needs which others, and which
# images are on the Pi. The full config holds interpolated secrets: it goes
# straight into the parser.
if ! SERVICE_GRAPH="$(compose config 2>/dev/null | joustmania_service_graph)" || [[ -z "$SERVICE_GRAPH" ]]; then
    echo "FAIL: compose config failed or lists no service; nothing started"
    exit 1
fi
MISSING_SVCS=()
MISSING_GAME=""
MISSING_OTHER=""
while IFS='|' read -r svc img _; do
    [[ -n "$svc" ]] || continue
    docker image inspect "$img" >/dev/null 2>&1 && continue
    MISSING_SVCS+=("$svc")
    if joustmania_in_list "$svc" "${JOUSTMANIA_GAME_SERVICES[@]}"; then
        MISSING_GAME+=" $svc($img)"
    else
        MISSING_OTHER+=" $svc($img)"
    fi
done <<<"$SERVICE_GRAPH"
# `up` starts a named service's dependencies and aborts as a whole on a missing
# image, so a service whose depends_on chain reaches a missing image is not
# named either (BR-002): everything else still starts.
BLOCKED=()
if ((${#MISSING_SVCS[@]})); then
    mapfile -t BLOCKED < <(joustmania_blocked "$SERVICE_GRAPH" "${MISSING_SVCS[@]}")
fi
BLOCKED_GAME=""
BLOCKED_OTHER=""
GAME_PHASE=()
OBS_PHASE=()
while IFS='|' read -r svc _ _; do
    [[ -n "$svc" ]] || continue
    ((${#MISSING_SVCS[@]})) && joustmania_in_list "$svc" "${MISSING_SVCS[@]}" && continue
    if ((${#BLOCKED[@]})) && joustmania_in_list "$svc" "${BLOCKED[@]}"; then
        if joustmania_in_list "$svc" "${JOUSTMANIA_GAME_SERVICES[@]}"; then BLOCKED_GAME+=" $svc"; else BLOCKED_OTHER+=" $svc"; fi
    elif joustmania_in_list "$svc" "${JOUSTMANIA_OBS_SERVICES[@]}"; then
        OBS_PHASE+=("$svc")
    else
        GAME_PHASE+=("$svc")
    fi
done <<<"$SERVICE_GRAPH"
STATE_MISSING="$MISSING_GAME$MISSING_OTHER"
STATE_DOWN="$BLOCKED_GAME$BLOCKED_OTHER"
if [[ -n "$STATE_MISSING" ]]; then
    echo "Images missing on the Pi (boot never pulls, UC-060 BR-001):$STATE_MISSING"
    [[ -z "$STATE_DOWN" ]] || echo "Not started, they need a service whose image is missing:$STATE_DOWN"
    echo "Recover: $(joustmania_missing_hint "$JOUSTMANIA_DIR")"
fi

# Phase 1: the game. Services whose image is on the Pi start, nothing is torn
# down (BR-002). Not exec: the EXIT trap records the result.
if [[ ${#GAME_PHASE[@]} -eq 0 ]]; then
    echo "FAIL: no game service has its image on the Pi; nothing started"
    exit 1
fi
echo "Phase 1/2: starting the game (Bluetooth backend: rust-hid${COMPOSE_PROFILES:+, profiles: ${COMPOSE_PROFILES}}): ${GAME_PHASE[*]}"
# One bound for the whole game phase, `up` (which waits for the health-gated
# depends_on) and the readiness poll together: JOUSTMANIA_GAME_READY_S, default
# 180 s. With the docker wait (60 s) a failing attempt stays under the 270 s
# for which the unit's start limit holds (UC-060 A5), and a full start (game
# 180 s + observability 330 s) stays well inside TimeoutStartSec=900.
GAME_TIMEOUT="${JOUSTMANIA_GAME_READY_S:-180}"
READY_DEADLINE=$((SECONDS + GAME_TIMEOUT))
UP_RC=0
timeout "$GAME_TIMEOUT" "$COMPOSE_WRAPPER" up -d --pull never --remove-orphans "${GAME_PHASE[@]}" || UP_RC=$?
[[ $UP_RC -eq 0 ]] || echo "compose up exited $UP_RC (only a hint; judging the containers)"
if [[ -n "$MISSING_GAME$BLOCKED_GAME" ]]; then
    echo "FAIL: game services cannot start: images missing on the Pi:${MISSING_GAME:- none}${BLOCKED_GAME:+; waiting on those:$BLOCKED_GAME}. Recover: $(joustmania_missing_hint "$JOUSTMANIA_DIR")"
    exit 1
fi

# The game is up when every judged game service runs (and is healthy where it
# has a healthcheck) within the readiness timeout (UC-060 main step 3, FR-181).
# Fails closed when none of the game services is active.
game_not_ready() { # prints the services that are not ready
    local ps svc line state health bad="" judged=0
    ps="$(compose ps -a --format '{{.Service}}|{{.State}}|{{.Health}}' 2>/dev/null || true)"
    for svc in "${GAME_PHASE[@]}"; do
        joustmania_in_list "$svc" "${JOUSTMANIA_GAME_SERVICES[@]}" || continue
        judged=1
        line="$(grep -m 1 "^$svc|" <<<"$ps" || true)"
        IFS='|' read -r _ state health <<<"$line"
        if [[ "$state" != running || (-n "$health" && "$health" != healthy) ]]; then
            bad+=" $svc(${state:-missing}${health:+/$health})"
        fi
    done
    [[ $judged -eq 1 ]] || bad=" (no game service active)"
    printf '%s' "$bad"
}
while :; do
    NOT_READY="$(game_not_ready)"
    [[ -z "$NOT_READY" ]] && break
    if ((SECONDS >= READY_DEADLINE)); then
        STATE_DOWN+="$NOT_READY"
        echo "FAIL: game services not ready within ${GAME_TIMEOUT} s:$NOT_READY; observability not started"
        exit 1
    fi
    sleep "${JOUSTMANIA_POLL_S:-2}"
done
echo "Game services ready"
START_RESULT="ok"
if [[ -n "$MISSING_OTHER$BLOCKED_OTHER" || -n "$STATE_REASON" ]]; then
    START_RESULT="degraded"
fi
if [[ $MODE == game ]]; then
    echo "Game phase done: state $START_RESULT; observability follows (joustmania-start.sh --observability)"
    exit 0
fi

observability_phase
echo "JoustMania started: state $START_RESULT"
