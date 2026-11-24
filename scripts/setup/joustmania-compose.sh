#!/bin/bash
# `docker compose` exactly as the Pi runs it. Used by joustmania-start.sh, the
# systemd unit's ExecStop and the Pi helper scripts; run it by hand the same way:
#   ./scripts/setup/joustmania-compose.sh up -d agent
#
# - Compose files: docker-compose.yml, named with -f, plus the gateway overlay
#   when it is configured (below); never anything else. A bare
#   `docker compose` also auto-loads docker-compose.override.yml, the dev/test
#   file that publishes the mock control port (50062) and the gRPC ports on all
#   interfaces, i.e. to the venue network (#1319).
# - Env files: the tracked .env (release IMAGE_TAG), then the git-ignored
#   .env.local when it exists (stage IMAGE_TAG, decision-backend API keys,
#   agent overrides; later files win). Passing --env-file switches off compose's
#   implicit .env, so both are named. A reboot through systemd therefore comes
#   back exactly like the runbook's manual bring-up (#1488).
# - Pin (UC-060 BR-003, C-040): when the checkout is pinned (detached HEAD at a
#   release tag or a commit), IMAGE_TAG is derived from HEAD (X.Y.Z of the
#   joustmania-vX.Y.Z tag, else the 40-character sha) and exported, so it wins
#   over .env, .env.local and the shell (a different shell IMAGE_TAG gets one
#   stderr line). On a branch nothing is derived.
# - Gateway export (UC-035 BR-015, #2622): docker-compose.gateway.yml is added
#   exactly when ORANGECASE_GATEWAY_HOST and ORANGECASE_GATEWAY_TOKEN both have
#   a non-empty value (the shell, else .env.local, else .env) and compose
#   renders the stack with it. The two variables are the switch, so a reboot
#   and an upgrade keep the export on. The wrapper reads plain NAME=value lines
#   itself and then lets compose confirm (`config -q`), because compose is the
#   one that interpolates: with a value compose reads as empty (`"" # note`,
#   `${UNSET}`) it would refuse the overlay and the whole stack would not
#   start. In every other case the overlay is left out and one line says so.
# - Pad display (UC-016): HIDRAW_MAJOR is exported from the host's
#   /proc/devices for the operator-pad cgroup rule (below).
# Never prints the env files or their values. Never run `config` without
# `--services`/`--images`/`-q` where output is logged; it prints interpolated
# secrets.
set -euo pipefail

JOUSTMANIA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$JOUSTMANIA_DIR"

if [[ -f scripts/setup/lib-joustmania.sh ]]; then
    # shellcheck source=lib-joustmania.sh
    source scripts/setup/lib-joustmania.sh
    if PIN_IMAGE_TAG="$(joustmania_pin_image_tag "$JOUSTMANIA_DIR")"; then
        if [[ -n "${IMAGE_TAG:-}" && "$IMAGE_TAG" != "$PIN_IMAGE_TAG" ]]; then
            echo "joustmania-compose: IMAGE_TAG=$IMAGE_TAG from the shell is ignored; the pinned checkout uses $PIN_IMAGE_TAG (UC-060 BR-003)" >&2
        fi
        export IMAGE_TAG="$PIN_IMAGE_TAG"
    fi
fi

COMPOSE_FILES=(-f docker-compose.yml)
[[ -f .env ]] && COMPOSE_FILES+=(--env-file .env)
[[ -f .env.local ]] && COMPOSE_FILES+=(--env-file .env.local)

# Prints the value compose would use for $1: the shell when the variable is set
# there (also when empty), else the last assignment in .env.local, else in .env.
compose_env_value() {
    local name="$1" file line first
    if [[ -n "${!name+set}" ]]; then
        printf '%s' "${!name}"
        return 0
    fi
    for file in .env.local .env; do
        [[ -f "$file" ]] || continue
        # `NAME=value`, and the `NAME = value` and `NAME: value` forms compose also reads
        line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?${name}[[:space:]]*[=:]" "$file" | tail -n 1 || true)"
        [[ -n "$line" ]] || continue
        line="${line#*[=:]}"
        # Leading whitespace, and trailing whitespace (which includes the CR of
        # a CRLF line ending)
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        # One pair of surrounding quotes, as compose strips them
        first="${line:0:1}"
        if [[ ${#line} -ge 2 && "$first" == "${line: -1}" ]] && [[ "$first" == '"' || "$first" == "'" ]]; then
            line="${line:1:${#line}-2}"
        fi
        printf '%s' "$line"
        return 0
    done
    return 0
}

# Operator pad display (UC-016, services.md §3.1): the hidraw char major is
# dynamic, so the operator-pad cgroup rule takes it from the host's
# /proc/devices. A different HIDRAW_MAJOR (shell, .env.local, .env) gets one
# stderr line and the host's value wins. No hidraw driver (macOS, a kernel
# without it loaded yet) exports nothing: compose's fallback grants nothing new.
# JOUSTMANIA_PROC_DEVICES is a test seam (scripts/tests), not a tunable (C-035).
host_hidraw_major="$(awk '$2 == "hidraw" { print $1; exit }' "${JOUSTMANIA_PROC_DEVICES:-/proc/devices}" 2>/dev/null || true)"
if [[ -n "$host_hidraw_major" ]]; then
    configured_hidraw_major="$(compose_env_value HIDRAW_MAJOR)"
    if [[ -n "$configured_hidraw_major" && "$configured_hidraw_major" != "$host_hidraw_major" ]]; then
        echo "joustmania-compose: HIDRAW_MAJOR=$configured_hidraw_major is not this host's hidraw major; using $host_hidraw_major (UC-016)" >&2
    fi
    export HIDRAW_MAJOR="$host_hidraw_major"
    unset configured_hidraw_major
fi
unset host_hidraw_major

GATEWAY_OVERLAY=docker-compose.gateway.yml
if [[ -f "$GATEWAY_OVERLAY" && " $* " != *" $GATEWAY_OVERLAY "* ]]; then
    gateway_host="$(compose_env_value ORANGECASE_GATEWAY_HOST)"
    gateway_token="$(compose_env_value ORANGECASE_GATEWAY_TOKEN)"
    if [[ -n "$gateway_host" && -n "$gateway_token" ]]; then
        # Compose has the last word: it interpolates the env files, this script
        # does not. `config -q` prints nothing and needs no docker daemon.
        if docker compose "${COMPOSE_FILES[@]}" -f "$GATEWAY_OVERLAY" config -q >/dev/null 2>&1; then
            COMPOSE_FILES+=(-f "$GATEWAY_OVERLAY")
        else
            echo "joustmania-compose: gateway export off: compose does not render the stack with $GATEWAY_OVERLAY; check ORANGECASE_GATEWAY_HOST and ORANGECASE_GATEWAY_TOKEN (UC-035 BR-015)" >&2
        fi
    elif [[ -n "$gateway_host" || -n "$gateway_token" ]]; then
        echo "joustmania-compose: gateway export off: ORANGECASE_GATEWAY_HOST and ORANGECASE_GATEWAY_TOKEN must both be set (UC-035 BR-015)" >&2
    fi
    unset gateway_host gateway_token
fi

exec docker compose "${COMPOSE_FILES[@]}" "$@"
