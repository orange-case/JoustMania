#!/usr/bin/env bash
# On the Pi the two gateway variables are the switch of the gateway export (#2622).
#
# scripts/setup/joustmania-compose.sh adds `-f docker-compose.gateway.yml`
# exactly when ORANGECASE_GATEWAY_HOST and ORANGECASE_GATEWAY_TOKEN both have a
# non-empty value, read as compose would (the shell, else .env.local, else
# .env). The wrapper is run against a stub docker that records its arguments:
#  - both set in .env.local: the overlay is added, after the env files
#  - none set: no overlay, nothing on stderr
#  - only one set: no overlay, one line on stderr, exit 0 (the stack starts)
#  - both set: compose is asked once (`config -q`) before the overlay is added;
#    when compose refuses, no overlay, one line on stderr, exit 0
#  - nothing set: compose is not asked (no extra call on an unconfigured Pi)
#  - the shell wins over the files, also when it sets a variable to empty
#  - .env.local wins over .env; quotes and a CR line ending are stripped
#  - no overlay file in the checkout: never added
#  - a call that names the overlay itself gets it once
#  - no value is ever printed
# Runs as part of `make lint`, or directly:
#   ./scripts/tests/test-pi-compose-gateway.sh
# UC: UC-035 BR-015
# UC: UC-035 A7
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

HOST_VALUE="otlp.example.org"
TOKEN_VALUE="dummy-not-a-token-2622"
OVERLAY="-f docker-compose.gateway.yml"
BASE="compose -f docker-compose.yml --env-file .env"

# $1 = case name, $2 = content of .env.local ("-" = no file), $3 = content of .env,
# $4 = overlay file present (yes|no); the rest: wrapper arguments.
# Shell variables for the wrapper are taken from SHELL_ENV (array of NAME=value).
SHELL_ENV=()
run_wrapper() {
  local name="$1" env_local="$2" env="$3" overlay="$4"
  shift 4
  local dir="${WORK}/${name}"
  mkdir -p "${dir}/repo/scripts/setup" "${dir}/bin"
  cp "${ROOT}/scripts/setup/joustmania-compose.sh" "${ROOT}/scripts/setup/lib-joustmania.sh" "${dir}/repo/scripts/setup/"
  printf '%b' "${env}" >"${dir}/repo/.env"
  [[ "${env_local}" == "-" ]] || printf '%b' "${env_local}" >"${dir}/repo/.env.local"
  [[ "${overlay}" == yes ]] && : >"${dir}/repo/docker-compose.gateway.yml"
  # The stub records every call; `config -q` (the wrapper's question "does
  # compose render this?") fails when the case asks for it (COMPOSE_REFUSES=yes)
  printf '#!/usr/bin/env bash\necho "$*" >>"%s/docker.log"\nif [[ "$*" == *" config -q" && -e "%s/compose-refuses" ]]; then echo "required variable is missing a value" >&2; exit 1; fi\n' "${dir}" "${dir}" >"${dir}/bin/docker"
  [[ "${COMPOSE_REFUSES}" == yes ]] && : >"${dir}/compose-refuses"
  # On a branch: nothing is pinned, so IMAGE_TAG is not derived (not under test here)
  printf '#!/usr/bin/env bash\ncase " $* " in *" symbolic-ref "*) echo refs/heads/main ;; *) exit 1 ;; esac\n' >"${dir}/bin/git"
  chmod +x "${dir}/bin/"*
  : >"${dir}/docker.log"
  RC=0
  env -u ORANGECASE_GATEWAY_HOST -u ORANGECASE_GATEWAY_TOKEN "${SHELL_ENV[@]}" PATH="${dir}/bin:${PATH}" \
    bash "${dir}/repo/scripts/setup/joustmania-compose.sh" "$@" >"${dir}/out.log" 2>"${dir}/err.log" || RC=$?
  # CALL: the call the wrapper ends in. CONFIRMS: its `config -q` questions before that.
  CALL="$(tail -n 1 "${dir}/docker.log")"
  CONFIRMS="$(sed '$d' "${dir}/docker.log")"
  ERR="$(cat "${dir}/err.log")"
  [[ "${RC}" -eq 0 ]] || fail "${name}: the wrapper exited ${RC}"
  if grep -rqF -e "${TOKEN_VALUE}" -e "${HOST_VALUE}" "${dir}/out.log" "${dir}/err.log" "${dir}/docker.log"; then
    fail "${name}: a gateway value was printed or passed on the command line"
  fi
  SHELL_ENV=()
  COMPOSE_REFUSES=no
}
COMPOSE_REFUSES=no
# $2 = the final call. With the overlay in it the wrapper must have asked compose
# first, exactly once, with the same files; without it and without a refusal it
# must not have asked at all (no extra compose call on an unconfigured Pi).
expect_call() {
  [[ "${CALL}" == "$2" ]] || fail "$1: expected 'docker $2', got 'docker ${CALL}'"
  if [[ "$2" == *"${OVERLAY}"* && "$1" != named-by-caller ]]; then
    local files="${2%% up *}"
    [[ "${CONFIRMS}" == "${files} config -q" ]] || fail "$1: expected one 'docker ${files} config -q' before the call, got '${CONFIRMS}'"
  elif [[ "${3-}" != asked ]]; then
    [[ -z "${CONFIRMS}" ]] || fail "$1: unexpected compose call before the final one: ${CONFIRMS}"
  fi
}
expect_quiet() { [[ -z "${ERR}" ]] || fail "$1: unexpected stderr: ${ERR}"; }
expect_warning() {
  [[ "$(grep -c 'gateway export off' <<<"${ERR}")" -eq 1 ]] || fail "$1: expected one 'gateway export off' line on stderr, got: ${ERR}"
}

BOTH="ORANGECASE_GATEWAY_HOST=${HOST_VALUE}\nORANGECASE_GATEWAY_TOKEN=${TOKEN_VALUE}\n"

run_wrapper both-in-env-local "${BOTH}" "IMAGE_TAG=1\n" yes up -d
expect_call both-in-env-local "${BASE} --env-file .env.local ${OVERLAY} up -d"
expect_quiet both-in-env-local

run_wrapper none-set "IMAGE_TAG=2\n" "IMAGE_TAG=1\n" yes up -d
expect_call none-set "${BASE} --env-file .env.local up -d"
expect_quiet none-set

run_wrapper no-env-local - "IMAGE_TAG=1\n" yes up -d
expect_call no-env-local "${BASE} up -d"
expect_quiet no-env-local

run_wrapper host-only "ORANGECASE_GATEWAY_HOST=${HOST_VALUE}\n" "IMAGE_TAG=1\n" yes up -d
expect_call host-only "${BASE} --env-file .env.local up -d"
expect_warning host-only

run_wrapper token-only "ORANGECASE_GATEWAY_TOKEN=${TOKEN_VALUE}\n" "IMAGE_TAG=1\n" yes up -d
expect_call token-only "${BASE} --env-file .env.local up -d"
expect_warning token-only

run_wrapper token-empty "ORANGECASE_GATEWAY_HOST=${HOST_VALUE}\nORANGECASE_GATEWAY_TOKEN=\n" "IMAGE_TAG=1\n" yes up -d
expect_call token-empty "${BASE} --env-file .env.local up -d"
expect_warning token-empty

# The shell wins over the files: it can switch the export on ...
SHELL_ENV=("ORANGECASE_GATEWAY_HOST=${HOST_VALUE}" "ORANGECASE_GATEWAY_TOKEN=${TOKEN_VALUE}")
run_wrapper both-in-shell - "IMAGE_TAG=1\n" yes up -d
expect_call both-in-shell "${BASE} ${OVERLAY} up -d"
expect_quiet both-in-shell

# ... and an empty shell value switches it off although .env.local sets both
SHELL_ENV=("ORANGECASE_GATEWAY_HOST=" "ORANGECASE_GATEWAY_TOKEN=")
run_wrapper shell-empty "${BOTH}" "IMAGE_TAG=1\n" yes up -d
expect_call shell-empty "${BASE} --env-file .env.local up -d"
expect_quiet shell-empty

# .env.local wins over .env (here it empties the host that .env sets)
run_wrapper env-local-over-env "ORANGECASE_GATEWAY_HOST=\n" "${BOTH}" yes up -d
expect_call env-local-over-env "${BASE} --env-file .env.local up -d"
expect_warning env-local-over-env

# Both only in .env, .env.local without them: still on
run_wrapper both-in-env "IMAGE_TAG=2\n" "${BOTH}" yes up -d
expect_call both-in-env "${BASE} --env-file .env.local ${OVERLAY} up -d"

# Quoted values, `export`, a CR line ending and the last assignment of a name
run_wrapper quoted-crlf "ORANGECASE_GATEWAY_HOST=\nexport ORANGECASE_GATEWAY_HOST=\"${HOST_VALUE}\"\r\nORANGECASE_GATEWAY_TOKEN='${TOKEN_VALUE}'\r\n" "IMAGE_TAG=1\n" yes up -d
expect_call quoted-crlf "${BASE} --env-file .env.local ${OVERLAY} up -d"

# A value of spaces or of empty quotes is empty
run_wrapper blank-values "ORANGECASE_GATEWAY_HOST=''\r\nORANGECASE_GATEWAY_TOKEN=\"\"  \n" "IMAGE_TAG=1\n" yes up -d
expect_call blank-values "${BASE} --env-file .env.local up -d"
expect_quiet blank-values

# The `NAME = value` and `NAME: value` forms compose also reads
run_wrapper spaced-and-colon "ORANGECASE_GATEWAY_HOST = ${HOST_VALUE}\nORANGECASE_GATEWAY_TOKEN: ${TOKEN_VALUE}\n" "IMAGE_TAG=1\n" yes up -d
expect_call spaced-and-colon "${BASE} --env-file .env.local ${OVERLAY} up -d"
expect_quiet spaced-and-colon

# Compose has the last word (review of #2625): a value this script reads as set
# but compose interpolates to empty (`"" # note`, `${UNSET}`, `$name`) would make
# compose refuse the overlay and stop the whole stack. The wrapper asks compose
# first and leaves the overlay out, with one line, when compose refuses.
COMPOSE_REFUSES=yes
run_wrapper compose-refuses "ORANGECASE_GATEWAY_HOST=${HOST_VALUE}\nORANGECASE_GATEWAY_TOKEN=\${MY_UNSET_TOKEN}\n" "IMAGE_TAG=1\n" yes up -d
expect_call compose-refuses "${BASE} --env-file .env.local up -d" asked
[[ "${CONFIRMS}" == "${BASE} --env-file .env.local ${OVERLAY} config -q" ]] || fail "compose-refuses: compose was not asked: '${CONFIRMS}'"
expect_warning compose-refuses

# A commented-out line and a longer name do not count
run_wrapper commented "# ORANGECASE_GATEWAY_HOST=${HOST_VALUE}\nORANGECASE_GATEWAY_HOST_OLD=x\nORANGECASE_GATEWAY_TOKEN=${TOKEN_VALUE}\n" "IMAGE_TAG=1\n" yes up -d
expect_call commented "${BASE} --env-file .env.local up -d"
expect_warning commented

# A checkout without the overlay file (a release before #2571): never added
run_wrapper no-overlay-file "${BOTH}" "IMAGE_TAG=1\n" no up -d
expect_call no-overlay-file "${BASE} --env-file .env.local up -d"
expect_quiet no-overlay-file

# A call that names the overlay itself gets it once
run_wrapper named-by-caller "${BOTH}" "IMAGE_TAG=1\n" yes -f docker-compose.gateway.yml up -d otel-collector
expect_call named-by-caller "${BASE} --env-file .env.local ${OVERLAY} up -d otel-collector"

if [[ "${fails}" -ne 0 ]]; then
  exit 1
fi
echo "ok: on the Pi the gateway overlay is loaded exactly when host and token are both set"
