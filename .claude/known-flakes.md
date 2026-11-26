# Known local flakes

Tests that fail intermittently on a developer machine or under load, not because of the change under review.
Read by the `pr-reviewer` agent and the `review-pr` skill. A failure listed here is reported as a known flake
(name the test), never as a PR defect, and never as proof that a mutation was caught.

A test that is NOT listed and fails once but passes on rerun is a finding (flake or race), not infrastructure.
Add a row only with the issue that tracks the flake.

| Test | Symptom | When | Tracking |
|---|---|---|---|
| `tests/service/test_tc019_flag_service_restart.py::test_restart_mid_game_reregisters_the_live_game` | `game … was not registered with the flag service within 5.0s` at the first `wait_game_registered`, before any restart; the coordinator log shows `reconciled 0 live game(s)` right after the game started | Service tier on a branch without the coordinator fix (the start-up reconcile's empty snapshot replaced the game's registration that landed first). After the fix this failure is a finding, not a flake | #2912 |

## Investigated, not flaky

Failures that looked like flakes but were real regressions. Do not dismiss these tests as flaky.

| Test | Symptom | Cause | Tracking |
|---|---|---|---|
| `tests/service/test_tc013_real_game_preempts_shadow.py::test_real_game_preempts_shadow_game_without_a_sample` | `AssertionError` at the `pattern_pair_rerun_preempted` check (queue runs 45783492965, 45784029555) | A queued agent refactor logged a preemption rerun as plain `experiment.pattern_pair_rerun`; fixed before merge. The `Shadow games blocked` line in the log is expected (UC-043 A13). | #2913 |

Infrastructure failures in CI (collector `exited (1)`, stale prebuilt images, `blob not found`) are listed in
`.claude/skills/review-pr/SKILL.md` step 5, not here.
