# Test Case: Host Tunes the Game with the Operator Pad and Force-Starts

## Overview

**ID:** TC-005  
**Goal:** The host changes the sensitivity, the team count and the game mode with the operator pad, and force-starts a game that uses the new settings.  
**Priority:** High  
**Status:** Automated  
**Use cases:** [UC-015](../use_cases/UC-015.md) main; [UC-011](../use_cases/UC-011.md) main; [UC-014](../use_cases/UC-014.md) main; [UC-010](../use_cases/UC-010.md) BR-015; [UC-053](../use_cases/UC-053.md) A2

Before #2819 this journey ran in the controller admin mode ([UC-012](../use_cases/UC-012.md), Obsolete); the pad's commands replace it.

## Roles

- Host (uses the operator pad; the test sends the pad's `ProcessInput` commands)
- Players (connected controllers that join the forced start)

## Preconditions

- The integration stack is up with the menu running; mock controllers `MOCK0001` … `MOCK0004` connected.
- `sensitivity` serves `medium` (the baseline in `services/flagd/ci/game.json`); `force_all_start` is `on` (a stack overlay on the flag service). The test runs on its own fresh flag plane, so the persisted admin values go away with it; the flag files are never written.

## Flow

| Step | Name                    | Description                                                                                          | Test Data                                   | Use Case                            |
|------|-------------------------|------------------------------------------------------------------------------------------------------|---------------------------------------------|-------------------------------------|
| 1    | Change sensitivity      | Host holds K2 and turns the knob one step up; the menu sends the next level to the flag service as a persisted admin value | `step_setting sensitivity up 1`: `medium` → next level | [UC-015](../use_cases/UC-015.md)    |
| 2    | Verify persisted        | flagd serves the level-3 variant as the `sensitivity` default                                         | -                                           | [UC-053](../use_cases/UC-053.md)    |
| 3    | Change the team count   | Host holds K5 and turns the knob one step up                                                          | `step_setting num_teams up 1`               | [UC-015](../use_cases/UC-015.md)    |
| 4    | Change the game mode    | Host holds K1 and turns the knob one step; the system announces the mode and persists `current_game`   | `JoustFFA` → `JoustTeams`                   | [UC-011](../use_cases/UC-011.md)    |
| 5    | Force-start             | Host holds the game key; the system builds the roster from all connected controllers and starts      | `force_start`                               | [UC-014](../use_cases/UC-014.md)    |
| 6    | Verify the game starts  | The menu publishes `game_requested` (source `operator_pad`) and the game starts in the selected mode  | -                                           | [UC-010](../use_cases/UC-010.md)    |

## Validation

1. **Settings reach the game**: the started game's mode is the one selected in step 4, its roster is every connected controller, and it runs at the sensitivity level set in step 1 (its `game_sensitivity` gauge, UC-010 BR-015).
2. **Flags persisted**: the flag service holds `sensitivity`, `num_teams` and `current_game` as persisted admin values (`admin_value` in `GET /game/sensitivity`, `GET /user/current_game`), UC-053 A2; the baseline flag files are byte-for-byte unchanged.

## Postconditions

- `sensitivity`, `num_teams` and `current_game` are persisted admin values; they live only in the test's flag plane, which is discarded with it.
- One started game, which the test force-ends at teardown.

## Automation

**Tier:** T2 service (`tests/service/`, `make test-service`): menu, game coordinator and controller manager (mock controllers) from source at HEAD, flag service and one flagd, fake audio, OTLP to the stack's sink (`LOBBY` composition with `otlp=True`).

- `tests/service/test_tc005_host_journey.py::test_host_tunes_and_force_starts` implements steps 1 to 6 and Validations 1 and 2 as one journey, with the pad's commands sent as `ProcessInput` (`tests/integration/helpers.py` `pad_command`); the pad service and its key timing are not part of it (UC-015's own tests cover them).
- Gaps (unit-tested only): the mode pulse on the lobby controllers (UC-011 step 3, UC-015 BR-015).
