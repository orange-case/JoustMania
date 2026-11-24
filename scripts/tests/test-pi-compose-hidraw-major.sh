#!/usr/bin/env bash
# The operator pad's display needs the host's hidraw char major in its cgroup
# rule (UC-016, services.md §3.1); scripts/setup/joustmania-compose.sh exports
# HIDRAW_MAJOR from /proc/devices. The wrapper runs against a stub docker that
# records HIDRAW_MAJOR as it receives it, with a fake /proc/devices:
#  - a hidraw line: its major is exported, nothing on stderr
#  - no hidraw line, or no /proc/devices at all (macOS): nothing exported, exit 0
#  - a different HIDRAW_MAJOR in .env.local or the shell: one stderr line, the host wins
#  - the same value configured: no stderr line
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

# $1 = case, $2 = /proc/devices content ("-" = missing file), $3 = .env.local content ("-" = none);
# the rest: NAME=value for the wrapper's shell.
run_wrapper() {
  local name="$1" devices="$2" env_local="$3"
  shift 3
  local dir="${WORK}/${name}"
  mkdir -p "${dir}/repo/scripts/setup" "${dir}/bin"
  cp "${ROOT}/scripts/setup/joustmania-compose.sh" "${ROOT}/scripts/setup/lib-joustmania.sh" "${dir}/repo/scripts/setup/"
  printf 'IMAGE_TAG=1.0.0\n' >"${dir}/repo/.env"
  [[ "${env_local}" == "-" ]] || printf '%b' "${env_local}" >"${dir}/repo/.env.local"
  [[ "${devices}" == "-" ]] || printf '%s' "${devices}" >"${dir}/devices"
  printf '#!/usr/bin/env bash\necho "HIDRAW_MAJOR=${HIDRAW_MAJOR-<unset>}" >>"%s/docker.log"\n' "${dir}" >"${dir}/bin/docker"
  printf '#!/usr/bin/env bash\ncase " $* " in *" symbolic-ref "*) echo refs/heads/main ;; *) exit 1 ;; esac\n' >"${dir}/bin/git"
  chmod +x "${dir}/bin/"*
  RC=0
  env -u HIDRAW_MAJOR "$@" JOUSTMANIA_PROC_DEVICES="${dir}/devices" PATH="${dir}/bin:${PATH}" \
    bash "${dir}/repo/scripts/setup/joustmania-compose.sh" up -d >"${dir}/out.log" 2>"${dir}/err.log" || RC=$?
  GOT="$(tail -n 1 "${dir}/docker.log" 2>/dev/null || true)"
  ERR="$(cat "${dir}/err.log")"
  [[ "${RC}" -eq 0 ]] || fail "${name}: the wrapper exited ${RC}"
}

run_wrapper host "${DEVICES}" -
[[ "${GOT}" == "HIDRAW_MAJOR=240" ]] || fail "host: expected 240, got ${GOT}"
[[ -z "${ERR}" ]] || fail "host: unexpected stderr: ${ERR}"

run_wrapper no-hidraw $'Character devices:\n 13 input\n' -
[[ "${GOT}" == "HIDRAW_MAJOR=<unset>" ]] || fail "no-hidraw: expected nothing exported, got ${GOT}"
[[ -z "${ERR}" ]] || fail "no-hidraw: unexpected stderr: ${ERR}"

run_wrapper no-proc - -
[[ "${GOT}" == "HIDRAW_MAJOR=<unset>" ]] || fail "no-proc: expected nothing exported, got ${GOT}"

run_wrapper mismatch-file "${DEVICES}" 'HIDRAW_MAJOR=13\n'
[[ "${GOT}" == "HIDRAW_MAJOR=240" ]] || fail "mismatch-file: the host value must win, got ${GOT}"
[[ "$(grep -c HIDRAW_MAJOR <<<"${ERR}")" -eq 1 ]] || fail "mismatch-file: expected one stderr line, got: ${ERR}"

run_wrapper mismatch-shell "${DEVICES}" - HIDRAW_MAJOR=99
[[ "${GOT}" == "HIDRAW_MAJOR=240" ]] || fail "mismatch-shell: the host value must win, got ${GOT}"
[[ "$(grep -c HIDRAW_MAJOR <<<"${ERR}")" -eq 1 ]] || fail "mismatch-shell: expected one stderr line, got: ${ERR}"

run_wrapper same "${DEVICES}" 'HIDRAW_MAJOR=240\n'
[[ "${GOT}" == "HIDRAW_MAJOR=240" && -z "${ERR}" ]] || fail "same: got ${GOT}, stderr: ${ERR}"

if [[ "${fails}" -gt 0 ]]; then
  echo "${fails} failure(s)"
  exit 1
fi
echo "ok: joustmania-compose.sh exports the host's hidraw major"
