# Test Cases (AIUP test-case layer)

End-to-end journeys that chain several use cases, in the format of the AIUP `test-case` skill
(`aiup-core` plugin; template: Overview, Roles, Preconditions, Flow, Validation, Postconditions).

**Only manual or hardware journeys keep a document here (#2799).** A journey that an automated
end-to-end test implements is described by that test: the test file names the journey (`test_tcNNN_*`),
carries `@pytest.mark.uc("UC-xxx", flow=..., tc="TC-NNN")` per use-case flow it exercises and a
`# Step <n>: <name>` comment per step (see [.claude/skills/README.md](../../.claude/skills/README.md)).
The `TC-NNN` id stays the journey's name in the `tc=` tags; its retired document is in git history.
A test case never makes a requirement Verified on its own: that status is derived from the use cases
(`Tested`), see CONTRIBUTING, "CI spec gate".

JoustMania additions to the template:

- **`**Use cases:**`** in the Overview lists every linked use case with the flows the journey exercises.
- **`## Automation`** names the tests that cover parts of the journey, what they do not cover, and
  the issue that plans the rest. `Manual` means the journey needs real hardware or real model backends.

Two deliberate deviations from the skill's writing rules:

- **Verification rows may link a use case.** The skill puts `-` in the Use Case column of verification
  rows; here a verification row links the use case whose outcome it checks when that use case appears in
  no action row (e.g. UC-020 when the system, not an actor, declares the winner), so every covered flow
  has a Flow row.
- **Protocol terms are allowed.** As in the use cases (see CONTRIBUTING, "Identifiers in use cases"),
  steps name flags, game events, span names and attributes, and connect-proxy paths, because those
  identifiers are what the tests observe.

## How the spec lint uses them

`scripts/aiup/spec-review/scripts/spec_lint.py` (the Spec Gate) reads every `TC-*.md` here: ids must be
unique and every `[UC-xxx](../use_cases/UC-xxx.md)` link must resolve. A use case in no test case is
reported as `INFO UC_UNUSED_BY_TC`, which never fails the gate.

## Index

| ID | Journey | Use cases | Priority | Status | Automation |
|---|---|---|---|---|---|
| [TC-001](TC-001-ffa-game-night-hardware.md) | FFA game night on real controllers | UC-001, UC-002, UC-010, UC-020, UC-030, UC-031 | Critical | Draft | Manual (hardware); mock twin TC-002 (`tests/integration/test_tc002_ffa_game.py`) |
| [TC-014](TC-014-progressive-rollout-and-rollback.md) | Progressive rollout with automatic rollback | UC-050, UC-051, UC-045, UC-041 | High | Draft | Manual (real controllers); mock support + e2e #1390 |
| [TC-015](TC-015-canary-by-hand.md) | Canary a backend by hand | UC-052, UC-035 | Medium | Draft | Manual; contract tests for routes; `/canary/*` handler tests (#1388) |
| [TC-016](TC-016-talk-demo-rehearsal.md) | Talk demo rehearsal | UC-070, UC-044, UC-045, UC-040, UC-042, UC-035 | Medium | Draft | Manual; script tests in CI (#1389) |

Status follows the skill: `Draft` = not or only partly automated, or written for review.

## Automated journeys (documents retired by #2799)

The tests are the journeys; search for `tc="TC-NNN"` to find every tagged test.

| ID | Journey | Tests |
|---|---|---|
| TC-002 | FFA game to a winner on mock controllers | `tests/integration/test_tc002_ffa_game.py` |
| TC-003 | Player disconnects during an FFA game | `tests/service/test_tc003_ffa_player_disconnects.py` |
| TC-004 | Operator stops an FFA game | `tests/service/test_tc004_operator_stops_ffa_game.py` |
| TC-005 | Host tunes the game with the operator pad and force-starts | `tests/service/test_tc005_host_journey.py` |
| TC-006 | Host restricts the agent with the operator pad | `tests/service/test_tc006_agent_ladder.py`, `tests/service/test_tc006_agent_blocked_by_host.py` |
| TC-007 | Agent intervenes in an FFA game on rules | `tests/integration/test_tc007_closed_loop_full_stack.py`, `tests/service/test_tc007_rules_intervention_applied.py` |
| TC-008 | Switch to a remote backend and route by confidence | `tests/service/test_tc008_remote_backend_routing.py`, `test_tc008_jev_backend.py`, `test_tc008_claude_backend.py` |
| TC-009 | Kill switch during an FFA game | `tests/service/test_tc009_kill_switch.py` |
| TC-010 | FFA game end: retro, summary, proposal | `tests/service/test_tc010_game_end_retro_and_proposal.py` |
| TC-011 | Shadow experiment runs to a verdict | `tests/service/test_tc011_shadow_experiment.py`, `test_tc011_promotion.py` |
| TC-012 | Replay a pattern-driven game exactly | `tests/service/test_tc012_pattern_game_replay.py` |
| TC-013 | Shadow game alongside a real FFA game | `tests/integration/test_tc013_shadow_alongside_real.py`, `tests/service/test_tc013_end_game_one_shadow.py`, `test_tc013_real_game_preempts_shadow.py` |
| TC-017 | One round of every non-FFA mode | `tests/service/test_tc017_parallel_mode_lifecycle.py` |
| TC-018 | Agent intervention through the flag service | `tests/service/test_tc018_flag_service_eliminations.py` |
| TC-019 | Flag service restart | `tests/service/test_tc019_flag_service_restart.py` |
| TC-020 | Host and stage change flags at runtime | `tests/service/test_tc020_host_and_stage_flags.py` |
| TC-021 | Operator steers the stack from Flagsmith | `tests/service/test_flagsmith_sync.py` |
| TC-022 | Flag service restart with the Flagsmith link | `tests/service/test_flagsmith_link.py`, `test_flagsmith_sync.py` |
