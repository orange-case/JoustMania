# Test Case: Host Restricts the Agent with the Operator Pad

## Overview

**ID:** TC-006  
**Goal:** During a Free-for-All game the host steps the agent's permission down to `none` with the operator pad, and from then on the agent makes no remote call and nothing it decides takes effect in the game.  
**Priority:** Critical  
**Status:** Draft  
**Use cases:** [UC-015](../use_cases/UC-015.md) main step 5, BR-004, BR-019; [UC-040](../use_cases/UC-040.md) A14, A22; [UC-045](../use_cases/UC-045.md) A2

## Roles

- Host (uses the operator pad; the test sends its `ProcessInput` commands; before #2819 the controller admin mode, [UC-013](../use_cases/UC-013.md), Obsolete)
- Agent (decides interventions for the running game)

## Preconditions

- The agent built at HEAD runs next to the game services (service tier `AGENT` composition, `tests/service/harness`, #1600), agent `enabled` and `interventions_enabled` on, `interventions_allowed` = `standard`, `decision.backend` = `openai` against the fake OpenAI server (`tests/integration/fake_openai`) scripted to answer `adjust_global_difficulty` at high confidence.
- Mock controllers `MOCK0001` (host) … `MOCK0004` connected; a `JoustFFA` game runs with `MOCK0002` … `MOCK0004`.

## Flow

| Step | Name                      | Description                                                                                              | Test Data                         | Use Case                            |
|------|---------------------------|----------------------------------------------------------------------------------------------------------|-----------------------------------|-------------------------------------|
| 1    | Select the agent level    | Host holds the pad's agent-level key outside the game                                                    | -                                 | [UC-015](../use_cases/UC-015.md)    |
| 2    | (merged into step 3)      | -                                                                                                        | -                                 | -                                   |
| 3    | Restrict twice            | Host turns the knob two steps down; the system steps `interventions_allowed` down and says each level     | `standard` → `ambient` → `none` | [UC-015](../use_cases/UC-015.md)    |
| 4    | Verify the flag           | flagd serves `none` for `interventions_allowed`; the "off" line played                                    | -                                 | -                                   |
| 5    | Verify no remote call     | The agent's next cycles build and send no request to the fake server and record no `invoke_agent joustmania-agent` span    | 10 s window                       | [UC-040](../use_cases/UC-040.md)    |
| 6    | Send an intervention by hand | Operator appends `eliminate_player` for `MOCK0002` to the game's event list through the flag service; the coordinator blocks it | `POST /interventions/eliminate_player` | [UC-045](../use_cases/UC-045.md)    |
| 7    | Verify blocked            | `agent_intervention` with `blocked=true`, reason `not_allowed`; `MOCK0002` stays alive                    | -                                 | [UC-040](../use_cases/UC-040.md)    |

## Validation

1. **Nothing reached the game**: no unblocked `agent_intervention` event for this game after step 4.
2. **Ladder persisted**: the flag service holds `none` as the persisted admin value of `interventions_allowed` (UC-053 A2); `agent.json` is unchanged.

## Postconditions

- `interventions_allowed` is `none` as a persisted admin value; the test fixture deletes it and every stack overlay it set (`flag_service` fixture).
- One running game that the suite fixture ends; mock controllers stay connected.

## Automation

**Tier:** T2 service for steps 1 to 7 (`tests/service/`, `make test-service`): menu, game coordinator and controller manager (mock controllers) from source at HEAD, flag service and one flagd, fake audio (`LOBBY` composition for steps 1 to 4; `AGENT` composition for steps 5 to 7: plus the agent built at HEAD, the harness `OtlpSink` and the fake OpenAI server).

- Precondition difference: the ladder test runs the `LOBBY` composition with no game and no agent (the "outside the game" host); the pad's commands are sent as `ProcessInput` (`pad_command`).
- `tests/service/test_tc006_agent_ladder.py::test_host_walks_the_agent_ladder_from_the_pad` covers steps 1 to 4 and Validation 2: from an off-ladder `probe` the first step lands on `none` (UC-015 BR-019), a resume brings the agent back at `ambient` (BR-003), then the ladder up to `full`, a step past the top refused `at_limit` (no wrap, #1340), and down to `none`, asserting after every step the persisted admin value of `interventions_allowed` and the per-level voice (`adminop_off`, `adminop_1` to `adminop_3`) and, at the end, the value flagd serves.
- `tests/service/test_tc006_agent_blocked_by_host.py::test_agent_makes_no_remote_call_after_host_restricts_it` covers steps 5 to 7 (#1600, #1422): the host steps the agent from `standard` to `none` with two pad steps (persisted admin value, flagd serves `none`), a `JoustFFA` game starts, and once the agent reports `agent_llm_gated_total{reason=no_interventions_allowed}` a 10 s window shows no request to the fake server and no `invoke_agent joustmania-agent` span for the game while that counter keeps growing; no unblocked `agent_intervention` reaches the game (Validation 1 for that window). Precondition difference: the host restricts in the lobby before the game starts.
- The same test covers steps 6 and 7 (coordinator half, marked `# Step 6` / `# Step 7`): the `eliminate_player` event is appended to the running game's event list through the flag service like the agent would, the coordinator blocks it (`blocked=true`, `not_allowed`) and the victim stays alive, with `interventions_allowed` at `none` as set from the pad. The former integration-tier test `test_intervention_flow.py` is removed (#1422); the `ambient` case (`eliminate_player` blocked `not_allowed`, victim alive) is covered by the unit test `test_ambient_allow_list_blocks_eliminate_player_and_victim_stays_alive` in `services/game_coordinator/tests/test_flag_set_consistency.py` (UC-040 A22).
- **Status stays Draft** (partly automated). Gaps: the pad service itself (key and knob timing) is not part of the journey; the chained journey is one test for steps 1 to 4 (restrict with the pad, a helper) and 5 to 7 (blocked), but it asserts the ladder's persisted value only, not the voice of step 3 (that stays in `test_tc006_agent_ladder`).
