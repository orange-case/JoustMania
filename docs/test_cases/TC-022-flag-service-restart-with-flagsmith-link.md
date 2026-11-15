# Test Case: Flag Service Restart with the Flagsmith Link

## Overview

**ID:** TC-022  
**Goal:** A start or restart of the flag service with the Flagsmith link on serves at once, creates missing features, brings back only what a person set and the admin settings, clears the link's own values in Flagsmith without ever loosening a kill switch, waits for games to register before touching their values, removes stale game identities only after the grace, and loses unsent local changes.  
**Priority:** High  
**Status:** Automated  
**Use cases:** [UC-054](../use_cases/UC-054.md) main step 5, A1, A3, BR-002, BR-005, BR-006, BR-007; [UC-055](../use_cases/UC-055.md) A3, A5

## Roles

- Operator (restarts the flag service; changes flags in Flagsmith as a person)
- Stage scripts and tests (in-memory local writes, pushed with the link's API key)
- Game coordinator (registers its games again after the restart)

## Preconditions

- A service-tier stack is up with the Flagsmith link on (`Composition(fake_flagsmith=True)`, no preseed): the fake Flagsmith starts empty.

## Flow

| Step | Name                              | Description                                                                                                         | Test Data                                              | Use Case                              |
|------|-----------------------------------|---------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------|---------------------------------------|
| 1    | First start                       | The link creates every registry flag as a disabled feature with its flag-set tag, never an initial value; restart rule `audit_log` | ~130 features at 5/s                       | [UC-054](../use_cases/UC-054.md) A3, BR-002 |
| 2    | Start while Flagsmith is down     | The flag service serves at once, `/healthz` 200, writes accepted; the link is offline with `http_503`                | Flagsmith answers 503                                  | [UC-054](../use_cases/UC-054.md) A3, BR-007 |
| 3    | Admin setting survives offline    | A person's admin setting is applied and persisted; after a restart with Flagsmith down it is served from the state file | `decision.backend` = `claude`                    | [UC-054](../use_cases/UC-054.md) main step 5, BR-007 |
| 4    | No author known                   | The audit log has no row for a value: it is a person's, the rule is `degraded`                                       | `experiment_tick_seconds` 40, no audit rows           | [UC-054](../use_cases/UC-054.md) A3 step 5 |
| 5    | Restart clears the link's own value | A local overlay pushed with the link's API key is cleared in Flagsmith after the restart (enabled false) and not applied | `decision.capture_content` = `off`               | [UC-054](../use_cases/UC-054.md) A3, BR-006 |
| 6    | Restart keeps a person's value    | A person's value is applied again after the restart, and not cleared                                                  | `decision.capture_content` = `off` (person)           | [UC-054](../use_cases/UC-054.md) A3, BR-006 |
| 7    | Kill switch at restart            | A person loosened a kill switch: after the restart the tighter baseline stays and is pushed; an API key tightened it: it stays tight over the clear | `experiment_max_games` baseline + 30, baseline - 30 | [UC-054](../use_cases/UC-054.md) A3, BR-005 |
| 8    | Game values wait for registration | After the restart no game value is applied before its game registers; then a person's override is applied and the link's own is removed | `pacing_profile` `calm` (person), `frantic` (API key) | [UC-054](../use_cases/UC-054.md) A3 step 4 |
| 9    | Stale identities                  | A game identity whose game never registers is deleted after the 45 s grace (never before, bound 75 s); a registered game's stays | `game:<stale>`, `game:<live>`                     | [UC-054](../use_cases/UC-054.md) A3 step 4 |
| 10   | Restart with unsent changes       | A local change waiting while Flagsmith is down is lost by a restart: never sent, not in force                        | `experiment_tick_seconds` 77                           | [UC-055](../use_cases/UC-055.md) A5   |
| 11   | Identity overrides off            | The environment does not use identity overrides in local evaluation: the link stays offline with the reason; local writes work | `use_identity_overrides_in_local_eval: false` | [UC-054](../use_cases/UC-054.md) A3 step 2 |

## Validation

1. **Offline first** (UC-054 BR-007): no step waits for Flagsmith before serving flagd or answering a write.
2. **A restart is a reset** (UC-054 BR-006): only a person's values and admin settings come back; the link's own values are cleared in Flagsmith.
3. **Safety over reset** (UC-054 BR-005): no restart loosens a kill switch.
4. **Games first** (UC-054 A3 step 4): no game value is applied or removed before its game registers; no identity is deleted at the moment of start.

## Postconditions

- Each step resets the fake, clears its local values and ends its games; `fake.down` and `identity_overrides_in_local_eval` are restored in a `finally` and the link is online again before the next step.

## Automation

**Tier:** T2 service (`tests/service/`, `make test-service`): flag service and game coordinator from source at HEAD, controller manager on mock controllers, one flagd, the in-process fake Flagsmith; one module-scoped stack per module, restarted with `FlagPlane.restart_flag_service()` (same ports and state file). CI runs it in its own job, Flagsmith Service Tests (#2592).

- `tests/service/test_flagsmith_link.py`: step 1 `test_start_creates_missing_features`, step 2 `test_offline_start_serves_baseline`, step 4 `test_degraded_rule_in_state`, step 5 `test_restart_clears_own_overlay`, step 6 `test_restart_keeps_persons_value`, step 7 `test_restart_never_loosens_kill_switch`, step 8 `test_restart_game_overrides_wait_for_registration`, step 9 `test_restart_removes_stale_identities_after_grace`, step 10 `test_restart_loses_unsent_changes`, step 11 `test_identity_overrides_disabled_keeps_link_offline`.
- Step 3 is `tests/service/test_flagsmith_sync.py::test_person_admin_setting_persisted_and_survives_offline_restart`.
- Step 8 nudges the sender with an unrelated local write while it waits for the removal: a pull round that queues a push does not wake the sender yet (#2608); drop the nudge when #2608 lands.
- Unit detail: `services/flag-service/flagsmith/start_test.go` (ping first, audit parsing, pacing), `in_rounds_test.go` (the start round's edge-lag skip).
- Not covered here: the live hosted Flagsmith's audit authorship timing (a Known gap, maintainer decision D1, #2592).
