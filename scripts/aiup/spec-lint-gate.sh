#!/usr/bin/env bash
# Spec lint gate (#1263, #2799), run by the Spec Gate job in ci.yml and locally:
#
#   scripts/aiup/spec-lint-gate.sh
#
# Every finding of severity ERROR or WARN fails: the linter runs with --strict
# and --no-baseline, so there is no accepted warning debt and no label that
# lets it grow (#2799 removed scripts/aiup/spec-lint-baseline.json and the
# `spec-baseline-growth` label check). INFO findings never fail.
# Requirement status: the linter derives each FR's status from the use cases
# that list it; a hand-kept status that differs is REQ_STATUS_DRIFT and fails.
set -euo pipefail

LINT=scripts/aiup/spec-review/scripts/spec_lint.py
HELP="docs/CONTRIBUTING.md#ci-spec-gate"

python3 -I "$LINT" --self-test

if ! python3 -I "$LINT" --docs docs --strict --no-baseline; then
  echo "::error title=Spec lint::spec ERROR or WARN finding(s); every warning is an error, fix them. See $HELP"
  exit 1
fi
