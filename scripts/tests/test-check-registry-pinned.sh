#!/usr/bin/env bash
# Tests for scripts/ci/check-registry-pinned.sh (#2435): a missing release tag is a
# SKIP when IMAGE_TAG is the version in .release-please-manifest.json (the release
# PR cuts that tag), and stays exit 2 otherwise. git is stubbed to fail, so no network.
#
#   ./scripts/tests/test-check-registry-pinned.sh
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/hermetic.sh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK_SRC="${SCRIPT_DIR}/../ci/check-registry-pinned.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

mkdir -p "${WORK}/scripts/ci" "${WORK}/bin"
cp "${CHECK_SRC}" "${WORK}/scripts/ci/check-registry-pinned.sh"
printf '#!/usr/bin/env bash\nexit 1\n' >"${WORK}/bin/git"
chmod +x "${WORK}/bin/git"
export PATH="${WORK}/bin:${PATH}"

fail() { echo "FAIL: $*" >&2; exit 1; }

# run_case <IMAGE_TAG> <manifest version>; sets rc and out
run_case() {
  echo "IMAGE_TAG=$1" >"${WORK}/.env"
  printf '{\n  ".": "%s"\n}\n' "$2" >"${WORK}/.release-please-manifest.json"
  rc=0
  out="$("${WORK}/scripts/ci/check-registry-pinned.sh" 2>&1)" || rc=$?
}

run_case 0.10.0 0.10.0
[[ $rc == 0 ]] || fail "equal manifest: want rc 0, got ${rc}: ${out}"
grep -q '^SKIP: IMAGE_TAG=0.10.0 is the release this checkout cuts' <<<"$out" || fail "no SKIP line: ${out}"

run_case 0.9.0 0.10.0
[[ $rc == 2 ]] || fail "manifest mismatch: want rc 2, got ${rc}: ${out}"
grep -q 'not found locally or on origin' <<<"$out" || fail "no error line: ${out}"

# The checker loads the list compose passes, so that list must be the tree's
# baseline.DefaultFiles in every compose file that mounts services/flagd.
REPO="${SCRIPT_DIR}/../.."
want="$(sed -n '/^var DefaultFiles = \[\]string{/,/^}/s/^[[:space:]]*"\([^"]*\)",.*/\1/p' \
  "${REPO}/services/flag-service/baseline/baseline.go" | tr -d '\r' | paste -sd, -)"
[[ -n "$want" ]] || fail "no DefaultFiles in baseline.go"
for f in docker-compose.yml docker-compose.lite.yml; do
  got="$(sed -n 's/^[[:space:]]*- FLAG_SERVICE_BASELINE_FILES=\([^ #]*\).*/\1/p' "${REPO}/${f}" | tr -d '\r')"
  [[ "$got" == "$want" ]] || fail "${f} FLAG_SERVICE_BASELINE_FILES=${got}, want ${want}"
done

echo "PASS: 4 checks"
