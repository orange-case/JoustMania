# Test Case: Flag Service Restart with the Flagsmith Link

## Overview

**ID:** TC-022  
**Goal:** A start or restart of the flag service with the Flagsmith link on serves at once, creates missing features, and, once Flagsmith can be reached, takes every value Flagsmith holds whoever made it (Flagsmith wins at a restart, #2621: the link's own pushes, a person's values, admin settings and kill switches alike), waits for games to register before touching their values, removes stale game identities only after the grace, and loses unsent local changes.  
**Priority:** High  
**Status:** Automated  
**Use cases:** [UC-054](../use_cases/UC-054.md) main step 5, A1, A3, BR-002, BR-005, BR-006, BR-007; [UC-055](../use_cases/UC-055.md) A3, A5

## Roles

- Operator (restarts the flag service; changes flags in Flagsmith as a person)
- Stage scripts and tests (in-memory local writes, pushed with the link's API key); an API key (Terraform, the Flagsmith MCP server) setting values in Flagsmith
- Game coordinator (registers its games again after the restart)

## Preconditions

- A service-tier stack is up with the Flagsmith link on (`Composition(fake_flagsmith=True)`, no preseed): the fake Flagsmith starts empty.

## Flow

| Step | Name                              | Description                                                                                                         | Test Data                                              | Use Case                              |
|------|-----------------------------------|---------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------|---------------------------------------|
| 1    | First start                       | The link creates every registry flag as a disabled feature with its flag-set tag, never an initial value; `GET /state` has no `restart_rule` | ~130 features at 5/s                       | [UC-054](../use_cases/UC-054.md) A3, BR-002 |
| 2    | Start while Flagsmith is down     | The flag service serves at once, `/healthz` 200, writes accepted; the link is offline with `http_503`                | Flagsmith answers 503                                  | [UC-054](../use_cases/UC-054.md) A3, BR-007 |
| 3    | Admin setting survives offline    | A person's admin setting is applied and persisted; after a restart with Flagsmith down it is served from the state file | `decision.backend` = `claude`                    | [UC-054](../use_cases/UC-054.md) main step 5, BR-007 |
| 4    | Admin setting set by an API key   | While the flag service cannot see Flagsmith an API key sets an admin setting; after the restart the value is applied and persisted, not replaced in Flagsmith | `decision.backend` = `claude` (API key) | [UC-054](../use_cases/UC-054.md) A3, BR-006 |
| 5    | Restart brings back the link's own value | A local overlay pushed with the link's API key comes back from Flagsmith after the restart and is not cleared there; the restart reads no audit log | `decision.capture_content` = `off`               | [UC-054](../use_cases/UC-054.md) A3, BR-006 |
| 6    | Restart keeps a person's value    | A person's value is applied again after the restart, and not cleared                                                  | `decision.capture_content` = `off` (person)           | [UC-054](../use_cases/UC-054.md) A3, BR-006 |
| 7    | Kill switch at restart            | A kill switch loosened in Flagsmith (by a person, then by an API key) comes back loosened after the restart; the tighter baseline is neither kept nor pushed | `experiment_max_games` baseline + 30, then + 40 (API key) | [UC-054](../use_cases/UC-054.md) A3, BR-005 |
| 8    | Game values wait for registration | After the restart no game value is applied before its game registers; then both the person's and the API key's overrides are applied | `pacing_profile` `calm` (person), `frantic` (API key) | [UC-054](../use_cases/UC-054.md) A3 step 4 |
| 9    | Stale identities                  | A game identity whose game never registers is deleted after the 45 s grace (never before, bound 75 s); a registered game's stays; the controller of the live real game keeps the person's override and it applies; a controller of no live game keeps its identity but loses its overrides and traits | `game:<stale>`, `game:<live>`, `controller:MOCK0013` (`game_id` = live), `controller:MOCK0014` (`game_id` = stale) | [UC-054](../use_cases/UC-054.md) A3 step 4 |
| 10   | Restart with unsent changes       | A local change waiting while Flagsmith is down is lost by a restart: never sent, not in force                        | `experiment_tick_seconds` 77                           | [UC-055](../use_cases/UC-055.md) A5   |
| 11   | Identity overrides off            | The environment does not use identity overrides in local evaluation: the link stays offline with the reason; local writes work | `use_identity_overrides_in_local_eval: false` | [UC-054](../use_cases/UC-054.md) A3 step 2 |

## Validation

1. **Offline first** (UC-054 BR-007): no step waits for Flagsmith before serving flagd or answering a write.
2. **Flagsmith wins at a restart** (UC-054 BR-006, FR-215): every value Flagsmith holds comes back, whoever made it; nothing is cleared in Flagsmith. A restart is no stage reset while Flagsmith is reachable.
3. **No restart tightening** (UC-054 BR-005): a kill switch takes the Flagsmith value like any flag.
4. **Games first** (UC-054 A3 step 4): no game value is applied or removed before its game registers; no identity is deleted at the moment of start.

## Postconditions

- Each step resets the fake, clears its local values and ends its games; `fake.down` and `identity_overrides_in_local_eval` are restored in a `finally` and the link is online again before the next step.

## Automation

**Tier:** T2 service (`tests/service/`, `make test-service`): flag service and game coordinator from source at HEAD, controller manager on mock controllers, one flagd, the in-process fake Flagsmith; one module-scoped stack per module, restarted with `FlagPlane.restart_flag_service()` (same ports and state file). CI runs it in its own job, Flagsmith Service Tests (#2592).

- `tests/service/test_flagsmith_link.py`: step 1 `test_start_creates_missing_features`, step 2 `test_offline_start_serves_baseline`, step 4 `test_restart_applies_api_key_admin_setting`, step 5 `test_restart_brings_back_own_overlay`, step 6 `test_restart_keeps_persons_value`, step 7 `test_restart_takes_flagsmith_kill_switch`, step 8 `test_restart_game_overrides_wait_for_registration`, step 9 `test_restart_removes_stale_identities_after_grace`, step 10 `test_restart_loses_unsent_changes`, step 11 `test_identity_overrides_disabled_keeps_link_offline`.
- Step 3 is `tests/service/test_flagsmith_sync.py::test_person_admin_setting_persisted_and_survives_offline_restart`.
- Unit detail: `services/flag-service/flagsmith/start_test.go` (ping first, nothing seeded at start, pacing), `in_start_test.go` (Flagsmith wins at a restart end to end), `in_rounds_test.go` (game values and event lists at a restart).
- Not covered here: the live hosted Flagsmith SaaS (a Known gap, maintainer decision D1, #2592).
