#!/usr/bin/env bash
# The operator pad's display needs the host's hidraw char major in a cgroup
# rule (UC-016, services.md §3.1). The rule is in
# docker-compose.operator-pad-hidraw.yml, which scripts/setup/joustmania-compose.sh
# adds (and exports HIDRAW_MAJOR for) only with a known major. The wrapper runs
# against a stub docker that records HIDRAW_MAJOR and its arguments, with a
# fake /proc/devices:
#  - a hidraw line: its major is exported and the overlay added, nothing on stderr
#  - no hidraw line, or no /proc/devices at all (macOS): no overlay, nothing exported, exit 0
#  - no hidraw line but a valid HIDRAW_MAJOR configured: the overlay with that value
#  - no hidraw line and an invalid HIDRAW_MAJOR (13, not a number): no overlay, one stderr line
#  - a different HIDRAW_MAJOR in .env.local or the shell: one stderr line, the host wins
#  - the same value configured: no stderr line
#  - the caller names the overlay itself: it is not added twice
# Runs as part of `make lint`, or directly:
#   ./scripts/tests/test-pi-compose-hidraw-major.sh
# UC: UC-016 A1
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/hermetic.sh"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
fails=0
fail() {
  echo "FAIL $*"
  fails=$((fails + 1))
}

DEVICES=$'Character devices:\n  1 mem\n 13 input\n240 hidraw\n241 usbmon\n\nBlock devices:\n  8 sd\n'
NO_HIDRAW=$'Character devices:\n 13 input\n'
OVERLAY="-f docker-compose.operator-pad-hidraw.yml"

# $1 = case, $2 = /proc/devices content ("-" = missing file), $3 = .env.local content ("-" = none);
# the rest: NAME=value for the wrapper's shell, then optionally `--` and the wrapper's arguments.
run_wrapper() {
  local name="$1" devices="$2" env_local="$3"
  shift 3
  local envs=() args=(up -d)
  while [[ $# -gt 0 && "$1" != "--" ]]; do
    envs+=("$1")
    shift
  done
  if [[ $# -gt 0 ]]; then
    shift
    args=("$@")
  fi
  local dir="${WORK}/${name}"
  mkdir -p "${dir}/repo/scripts/setup" "${dir}/bin"
  cp "${ROOT}/scripts/setup/joustmania-compose.sh" "${ROOT}/scripts/setup/lib-joustmania.sh" "${dir}/repo/scripts/setup/"
  cp "${ROOT}/docker-compose.operator-pad-hidraw.yml" "${dir}/repo/"
  printf 'IMAGE_TAG=1.0.0\n' >"${dir}/repo/.env"
  [[ "${env_local}" == "-" ]] || printf '%b' "${env_local}" >"${dir}/repo/.env.local"
  [[ "${devices}" == "-" ]] || printf '%s' "${devices}" >"${dir}/devices"
  printf '#!/usr/bin/env bash\necho "HIDRAW_MAJOR=${HIDRAW_MAJOR-<unset>} ARGS=$*" >>"%s/docker.log"\n' "${dir}" >"${dir}/bin/docker"
  printf '#!/usr/bin/env bash\ncase " $* " in *" symbolic-ref "*) echo refs/heads/main ;; *) exit 1 ;; esac\n' >"${dir}/bin/git"
  chmod +x "${dir}/bin/"*
  RC=0
  env -u HIDRAW_MAJOR "${envs[@]}" JOUSTMANIA_PROC_DEVICES="${dir}/devices" PATH="${dir}/bin:${PATH}" \
    bash "${dir}/repo/scripts/setup/joustmania-compose.sh" "${args[@]}" >"${dir}/out.log" 2>"${dir}/err.log" || RC=$?
  local last
  last="$(tail -n 1 "${dir}/docker.log" 2>/dev/null || true)"
  GOT="${last%% ARGS=*}"
  ARGS="${last#* ARGS=}"
  ERR="$(cat "${dir}/err.log")"
  [[ "${RC}" -eq 0 ]] || fail "${name}: the wrapper exited ${RC}"
}

with_overlay() { [[ " ${ARGS} " == *" ${OVERLAY} "* ]]; }

run_wrapper host "${DEVICES}" -
[[ "${GOT}" == "HIDRAW_MAJOR=240" ]] || fail "host: expected 240, got ${GOT}"
with_overlay || fail "host: overlay not added: ${ARGS}"
[[ -z "${ERR}" ]] || fail "host: unexpected stderr: ${ERR}"

run_wrapper no-hidraw "${NO_HIDRAW}" -
[[ "${GOT}" == "HIDRAW_MAJOR=<unset>" ]] || fail "no-hidraw: expected nothing exported, got ${GOT}"
with_overlay && fail "no-hidraw: overlay added without a major: ${ARGS}"
[[ -z "${ERR}" ]] || fail "no-hidraw: unexpected stderr: ${ERR}"

run_wrapper no-proc - -
[[ "${GOT}" == "HIDRAW_MAJOR=<unset>" ]] || fail "no-proc: expected nothing exported, got ${GOT}"
with_overlay && fail "no-proc: overlay added without a major: ${ARGS}"

run_wrapper configured-only "${NO_HIDRAW}" 'HIDRAW_MAJOR=236\n'
[[ "${GOT}" == "HIDRAW_MAJOR=236" ]] || fail "configured-only: expected 236, got ${GOT}"
with_overlay || fail "configured-only: overlay not added: ${ARGS}"
[[ -z "${ERR}" ]] || fail "configured-only: unexpected stderr: ${ERR}"

for bad in 13 abc 0; do
  run_wrapper "invalid-${bad}" "${NO_HIDRAW}" - HIDRAW_MAJOR="${bad}"
  with_overlay && fail "invalid-${bad}: overlay added: ${ARGS}"
  [[ "$(grep -c HIDRAW_MAJOR <<<"${ERR}")" -eq 1 ]] || fail "invalid-${bad}: expected one stderr line, got: ${ERR}"
done

run_wrapper mismatch-file "${DEVICES}" 'HIDRAW_MAJOR=13\n'
[[ "${GOT}" == "HIDRAW_MAJOR=240" ]] || fail "mismatch-file: the host value must win, got ${GOT}"
with_overlay || fail "mismatch-file: overlay not added: ${ARGS}"
[[ "$(grep -c HIDRAW_MAJOR <<<"${ERR}")" -eq 1 ]] || fail "mismatch-file: expected one stderr line, got: ${ERR}"

run_wrapper mismatch-shell "${DEVICES}" - HIDRAW_MAJOR=99
[[ "${GOT}" == "HIDRAW_MAJOR=240" ]] || fail "mismatch-shell: the host value must win, got ${GOT}"
[[ "$(grep -c HIDRAW_MAJOR <<<"${ERR}")" -eq 1 ]] || fail "mismatch-shell: expected one stderr line, got: ${ERR}"

run_wrapper same "${DEVICES}" 'HIDRAW_MAJOR=240\n'
[[ "${GOT}" == "HIDRAW_MAJOR=240" && -z "${ERR}" ]] || fail "same: got ${GOT}, stderr: ${ERR}"

# shellcheck disable=SC2086 # OVERLAY is two words on purpose
run_wrapper named-by-caller "${DEVICES}" - -- ${OVERLAY} up -d operator-pad
[[ "$(grep -o 'operator-pad-hidraw' <<<"${ARGS}" | wc -l)" -eq 1 ]] || fail "named-by-caller: overlay twice: ${ARGS}"

if [[ "${fails}" -gt 0 ]]; then
  echo "${fails} failure(s)"
  exit 1
fi
echo "ok: joustmania-compose.sh adds the hidraw overlay only with a known major"
