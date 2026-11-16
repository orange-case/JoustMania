# Test Case: Operator Steers the Stack from Flagsmith

## Overview

**ID:** TC-021  
**Goal:** An operator changes JoustMania flags in hosted Flagsmith (stack-wide, for one live game, for one player, an event) and the stack runs them through the flag service; the stack's own changes, games and players reach Flagsmith; refusals, conflicts, outages and a disconnected flagd never block or loosen a local value.  
**Priority:** High  
**Status:** Automated  
**Use cases:** [UC-054](../use_cases/UC-054.md) main, A1, A2, A4, A5, BR-001, BR-003, BR-004, BR-005, BR-009; [UC-055](../use_cases/UC-055.md) main, A1, A2, A3, A4, BR-001, BR-002, BR-003

## Roles

- Operator (changes flags in Flagsmith, a person: audit rows with a user)
- Stage scripts and tests (local writes through the flag service HTTP API)
- Game coordinator (registers and ends games)

## Preconditions

- A service-tier stack is up with the Flagsmith link on (`Composition(fake_flagsmith=True, fake_flagsmith_preseed=True)`): every registry flag is a feature in the fake Flagsmith, the link is online, poll 5 s, Flagsmith timeout 1 s.
- Nothing is pending or out of sync; the fake holds no values, identities or audit rows (each step starts from `reset_values()`).

## Flow

| Step | Name                               | Description                                                                                                                       | Test Data                                                              | Use Case                                  |
|------|------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------|-------------------------------------------|
| 1    | Operator sets a stack value        | The operator turns `decision.capture_content` off in Flagsmith; within a poll the flag service writes it as writer `flagsmith`, flagd serves it, and it is not pushed back | `off`; within 15 s                                                      | [UC-054](../use_cases/UC-054.md) main     |
| 2    | Operator sets game and player values | Overrides on `game:<id>` and a shadow game's `game:<id>:<serial>` land in their own scope only, written by `flagsmith`          | `pacing_profile` = `calm`, `player_handicap_factor` = 1.5               | [UC-054](../use_cases/UC-054.md) main, BR-003, BR-009 |
| 3    | Operator sends an event            | One `audio_cue` payload on a game identity is appended once, never again over the next polls                                     | `{"sound": "bell"}`                                                     | [UC-054](../use_cases/UC-054.md) main     |
| 4    | Own push never comes back          | A local value is pushed; while the next local value's push waits (a 429), the document still shows the first: the newer local value stays | 12 then 13 on `experiment_tick_seconds`                              | [UC-054](../use_cases/UC-054.md) BR-004   |
| 5    | Local writes reach Flagsmith       | 15 rapid local writes are answered at once and reach Flagsmith as the last value in fewer than 15 requests; a local clear turns the feature off with the baseline value | 101..115                                         | [UC-055](../use_cases/UC-055.md) main, BR-001, BR-002 |
| 6    | Game starts and ends               | A registered shadow game and each player become identities, the game with its traits; when the game ends its identities and overrides go | 2 serials; traits `game_kind`, `coordinator_instance`                   | [UC-055](../use_cases/UC-055.md) A1, A2   |
| 7    | Flagsmith unreachable              | Flagsmith goes down: the link goes offline, local writes (a kill switch included) answer at once and wait; back online the queue drains | `experiment_tick_seconds` 14, `experiment_max_games` 20             | [UC-054](../use_cases/UC-054.md) A1, [UC-055](../use_cases/UC-055.md) A3 |
| 8    | Both sides changed                 | The merge table: local change pushed; same value adopted; both changed, Flagsmith wins; a kill switch keeps the tighter value (local 20 over 30, Flagsmith 10 over 20); one row truly offline across a reconnect | R/L/D per row (test data in the test)              | [UC-054](../use_cases/UC-054.md) A2, BR-005 |
| 9    | Flag service refuses a change      | An out-of-bounds value is refused: local value kept, `refused` counted, one unhealthy feature-health post, no repeat; a valid value is applied and a healthy post follows; an experiment arm is read-only | `experiment_tick_seconds` 5000, then 24; `experiment:x:a` | [UC-054](../use_cases/UC-054.md) A4, BR-003 |
| 10   | flagd not connected                | flagd is stopped: a Flagsmith change is not applied; once flagd is back it is                                                     | within 70 s                                                             | [UC-054](../use_cases/UC-054.md) A5       |
| 11   | Flagsmith refuses a change         | Flagsmith answers 409 (change request): the local value stays, the key is out of sync and refused and not sent again; after the refusal ends the next local change lands; a 429 is retried after its Retry-After | 31, 32, 33                         | [UC-055](../use_cases/UC-055.md) A4       |
| 12   | Real game on controllers           | A real game's players are the identities `controller:<serial>` with the traits `game_id`, `game_kind`, `coordinator_instance` (no `game:<id>:<serial>`); a player value is an override there; at game end the override and the traits go, the controller identity stays | 2 serials; `player_handicap_factor` = 1.25 | [UC-055](../use_cases/UC-055.md) A1, A2, BR-002, BR-003, BR-004 |
| 13   | Operator sets a controller value   | A value set on a controller that plays in no game is not applied and is removed when it joins a game; set while the game is live it is that game's player value; after the game ends it does not follow the controller into a later game | `player_handicap_factor` 1.75, then 1.5 | [UC-054](../use_cases/UC-054.md) main, BR-003, BR-010 |

## Validation

1. **Flagsmith is a writer** (UC-054 BR-001, BR-009): every applied value carries writer `flagsmith` and passes the registry bounds; flagd serves it.
2. **No echo** (UC-054 BR-004): a value applied from Flagsmith is never pushed back; the link's own push read back is not a change.
3. **Kill switches tighten** (UC-054 BR-005): no row of step 8 loosens `experiment_max_games` against the tighter side.
4. **Local first** (UC-055 BR-001, NFR-057): local writes never wait for Flagsmith, down or slower than the link's timeout (in this tier "slow" is a timeout: FLAGSMITH_TIMEOUT_SECONDS=1, the fake answers after 3 s).
5. **Change delay** (NFR-058): a Flagsmith change is in force within 30 s by polling alone; NFR-058 stays `Implemented` until measured live (maintainer decision D2, #2592).

## Postconditions

- Each step clears its local values (the client's `cleanup`), ends its games, waits until nothing is pending and a pull ran after the last push, then resets the fake, waits for one pull of the reset document and for nothing pending.

## Automation

**Tier:** T2 service (`tests/service/`, `make test-service`): flag service and game coordinator from source at HEAD, controller manager on mock controllers, one flagd, the in-process fake Flagsmith (`tests/service/harness/fakes/flagsmith.py`); one module-scoped stack. CI runs it in its own job, Flagsmith Service Tests (#2592).

- `tests/service/test_flagsmith_sync.py`: step 1 `test_person_stack_value_applied_as_flagsmith_write`, step 2 `test_person_game_and_player_override_applied`, step 3 `test_person_event_override_appends_once`, step 4 `test_own_push_never_comes_back`, step 5 `test_local_write_reaches_flagsmith_merged`, step 6 `test_game_start_lists_identities` and `test_game_end_removes_identities_and_values`, step 7 `test_offline_keeps_local_writes_and_drains_on_reconnect`, step 8 `test_conflict_table` and `test_conflict_after_reconnect_flagsmith_wins`, step 9 `test_refused_change_marks_feature_unhealthy`, step 10 `test_change_waits_for_flagd`, step 11 `test_flagsmith_refusal_marks_out_of_sync`, step 12 `test_real_game_players_are_controller_identities` and `test_real_game_end_releases_controllers`, step 13 `test_person_controller_override_applies_to_its_live_game_only`; validation 4 `test_local_write_latency_with_flagsmith_down_and_slow` (NFR-057, the NFR-050 probe and its CI bounds p95 0.5 s, max 1.5 s), validation 5 `test_change_delay_within_poll` (NFR-058).
- The fake's fidelity (document, 304, audit rows, identity search, controller traits and override listing, refusals) is pinned by `tests/service/test_fake_flagsmith.py` (no docker).
- Not covered here: spans and metrics of the link (the flag service has no OTLP endpoint in this tier; `services/flag-service/flagsmith/*_test.go` and the Grafana contract test are the evidence); the live hosted Flagsmith (edge-document identity overrides, audit authorship timing, the `__future__` API, request limits under 8 games): a Known gap on UC-054 and UC-055 (maintainer decision D1, #2592); the outbound-only rule (UC-054 BR-008) is `tests/contract/test_flagsmith_compose.py`.
