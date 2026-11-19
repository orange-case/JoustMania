# Booth challenge: "Who saved them?"

> Guide for a presenter or an outside reader. It restates [UC-101](../use_cases/UC-101.md),
> [telemetry.md §3.2](../specs/telemetry.md#32-game-coordinator) and
> [§4.3](../specs/telemetry.md#43-decision-attributes); if they differ, the spec wins.
> Part of epic #2569 (booth challenges), issue #2665. The moment shapes are those of
> ["Where did it go?"](where-did-it-go.md) (UC-100).

## 1. What the challenge shows

The agent decides in its own trace: an `agent.decision` span, with its `agent.intervention.effect` child. The
effect lands in the game's trace, on a player: a shield, a protecting sensitivity factor, a revive. The two traces
share one key, the intervention id. The challenge asks each backend one question: "player X got an intervention:
which agent decision gave it, and was the player still alive 10 s later?" One game is played per moment shape
(span, span event, log event), and the three games' results are shown side by side.

It is not a ranking. A result is `found`, `found but not correlated` or `not found`, with the backend's answer next
to the ground truth. There is no score and no order (UC-101 BR-009). A result is about what a backend shows for a
shape, not a verdict on the backend.

## 2. Setup

Preconditions (UC-101):

- The gateway export is on and forwards to the backends that take part (FR-086, UC-035 A7).
- The agent is enabled, its intervention writes are on (`interventions_enabled` = `on`) and its allow-list
  (`interventions_allowed`) names at least one lever that can save a player. `standard` names two: `grant_shield`
  and `adjust_player_sensitivity` (BR-001, [flags.md §3](../specs/flags.md#3-agent)).
- **Shields are instant.** The stack default of `shield.activation` is `trigger`, so set it to `instant` before the
  first round: a shield charge (trigger) or shield energy (hold) does not protect from the moment it is applied,
  and the 10 s rule (BR-007) assumes it does. Only Free-for-All honours `trigger` and `hold`; every other mode is
  instant anyway.
- Enough controllers are connected for a game (UC-010 preconditions).

On the case, through the envoy route of the flag service's HTTP API
([services.md §7.2](../specs/services.md#72-httpjson-api-resource-style)); write `shield.activation` while no game
is live (a live game refuses it):

```bash
# instant shields for the booth (an in-memory stack overlay)
curl -sS -X PUT http://localhost/flags/game/shield.activation \
  -H 'Content-Type: application/json' -d '{"variant": "instant", "writer": "stage"}'
# the agent writes interventions, with the standard allow-list
curl -sS -X PUT http://localhost/flags/agent/interventions_enabled \
  -H 'Content-Type: application/json' -d '{"variant": "on", "writer": "stage"}'
curl -sS -X PUT http://localhost/flags/agent/interventions_allowed \
  -H 'Content-Type: application/json' -d '{"variant": "standard", "writer": "stage"}'
```

The agent must also be `enabled`; the operator pad or `scripts/talk-stage.sh` set these too. After the booth, clear
`shield.activation` (`curl -sS -X DELETE http://localhost/flags/game/shield.activation`) and put the agent gates
back where the next use of the case expects them.

**Choose the shape for the next game** exactly as in ["Where did it go?" §2](where-did-it-go.md#2-setup): the flag
`telemetry.moment_shape` (`span_event` default, `span`, `log_event`, `both`), read once when the game starts; reset
it after the booth. The intervention follows the same shape as the warning and the death (UC-101 BR-002).

**Confirm the shape the game ran in.** Read `telemetry.moment_shape` on the game's root span (named after the mode).
If it differs from the chosen shape, or is `both`, the game is not counted (A1).

## 3. The two sides of the join

**The decision (agent trace).** Every written intervention has an `agent.decision` span with
`decision.intervention_id`, also a write that failed. Its `agent.intervention.effect` child carries the same id as
`intervention.id`. The flag service's `flagservice.write` span carries it as `decision.intervention_id` too
([telemetry.md](../specs/telemetry.md), FR-221).

**The effect (game trace).** The coordinator records the moment `intervention` on the player, in the game's shape:

| Shape | What is sent | Where it hangs in the trace | What a backend needs to find it |
|---|---|---|---|
| `span_event` (default) | Span event `intervention` on the player span | On the player span; it reaches a backend with the player span (at the death in Free-for-All, at game end in NonStop, Fight Club, Zombies) | Span events searchable by name and attribute |
| `span` | Span `game.intervention`, start = end = the apply | Child of the player span | Search of spans by name and attribute |
| `log_event` | Log record with `event_name` `intervention`, carrying the player span's trace and span id | Not a span: linked to the player span | A log store for OTLP logs, and a jump from log to trace |
| `both` | `span_event` and `log_event`, same `moment.id` | As the two shapes | Not a booth round |

Every record carries `intervention.type`, `intervention.id` (left out when the write carried none, A3), `moment.id`,
`player.serial`, `player.name` (once known), `game.id` and `game.kind` (BR-004). `intervention.type` is the
decision's intervention name: `grant_shield`, `adjust_player_sensitivity`, and in wider allow-lists or shadow games
`grant_group_shield`, `toughness_boost`, `set_player_handicap`, `revive_player`, `soft_penalty`, `eliminate_player`
(BR-001).

The join: the moment's `intervention.id` equals the decision's `decision.intervention_id`. The two traces are
different traces; a backend must join them by this attribute, not by parent links.

## 4. The question and how to check an answer

**The question (UC-101 step 8).** Pick a player whose ground truth entry (below) has an `intervention.id`, a
`moment.id`, and a type that can save the player: `grant_shield`, `grant_group_shield`, `toughness_boost`,
`adjust_player_sensitivity`, `set_player_handicap` or `revive_player` (BR-001). Ask each backend: which agent
decision gave this player that intervention, and was the player still alive 10 s later?

**The ground truth.** At a normal game end the coordinator records on the game's root span one span event
`challenge.intervention_ground_truth` per applied intervention, in apply order (UC-101 step 7, FR-222). Read it from
there, not from any backend and not from the agent's game summary (BR-006):

| Attribute | Meaning |
|---|---|
| `intervention.type`, `intervention.id` | The intervention; the id is left out when the write carried none (do not ask about it, A3) |
| `player.serial`, `player.name` | The player (the name once known) |
| `moment.id` | The intervention moment's id; left out when no moment was sent (do not ask about it) |
| `challenge.applied_at_ns` | When it was applied, epoch nanoseconds |
| `challenge.first_death_at_ns` | That player's first death at or after the apply; left out when the player did not die before the game ended |

**Check.**

1. The decision: the backend's `agent.decision` must carry `decision.intervention_id` equal to the entry's
   `intervention.id`.
2. Survival (BR-007): if `challenge.first_death_at_ns` is absent, or `first_death_at_ns − applied_at_ns` is more
   than 10 s (10e9 ns), the player survived. Otherwise the player did not. A game that ended within the 10 s
   without that player's death counts as survived.
3. Compare the backend's survival answer with step 2.

**What counts as the moment record (BR-008).** Only the record named `intervention` with its `moment.id` and
`intervention.id`, joined to the decision by `decision.intervention_id`. These do not count, even when they describe
the same happening: a join by player and time alone; the lever's own span event (`shield_granted`,
`sensitivity_factor_applied`, `player_revived`, ...); the `agent_intervention` game event (type, target, blocked),
which carries no intervention id.

## 5. Result format

For each backend and shape, one of three results (UC-101 BR-009):

| Result | Meaning |
|---|---|
| found | The backend shows the intervention moment, names the decision with the same intervention id, and its survival answer matches the ground truth |
| found but not correlated | It shows the intervention moment, but names no decision, another decision, or a survival answer that differs from the ground truth |
| not found | It shows no record of the asked intervention moment (its `moment.id` and `intervention.id`); another intervention moment of the same player does not count |

A backend that did not receive the game or the decision (gateway or backend unreachable, data refused) gets no
result and is recorded as *not received* (A5). That is not a result about the shape; the game and the agent go on
unchanged (NFR-029).

Template, one row per backend and shape:

| Backend | Shape | Game id | Shape on root span | Asked player, `intervention.type`, `intervention.id` | Backend's answer (decision, survived?) | Ground truth (applied, first death, survived?) | Result |
|---|---|---|---|---|---|---|---|
| | `span_event` | | | | | | found / found but not correlated / not found / not received |
| | `span` | | | | | | |
| | `log_event` | | | | | | |

**When a game is not counted** (play another game in the same shape unless noted):

| Case | Why | Flow |
|---|---|---|
| The root span's `telemetry.moment_shape` differs from the chosen shape, or is `both` | The game ran in another shape | A1 |
| No entry has an `intervention.id`, a `moment.id` and a lever that can save the player | There is no question to ask: record the round as without a question | A6 |
| The game was force-ended or the coordinator stopped before the end | No ground truth is recorded for it | A7 |

A blocked, refused or superseded intervention, or one that changed nothing, has a decision with an id but no moment
and no ground truth entry (A2, BR-005): do not ask about it, and a backend that finds such a decision without a
moment is not wrong.

## 6. Known limits today

- **The agent may not intervene.** The rules backend grants a shield when a player is in danger; a calm game can end
  without any intervention that saves (A6). A shadow game uses a wider allow-list and shows the other levers.
- **No CI test plays the booth round.** Steps 8 to 10 (the question, the comparison, the three shapes side by side),
  A1 step 2, A5, A6 and the postconditions about recorded results are the presenter's check at the booth.
- **The case keeps no log store** (UC-100 BR-006). A `log_event` round is checked only against the ground truth on
  the root span.
- **A late effect after teardown.** An effect that lands late after the game's teardown recorded the ground truth
  gets its moment but no ground truth entry (accepted gap, telemetry.md §3.2). Ask only about entries in the ground
  truth.
- A `log_event` or `both` game in a process without OTLP logging runs as `span_event` and records that shape on the
  root span.

## 7. References

- [UC-101](../use_cases/UC-101.md) Trace an Agent Intervention to its Effect
- FR-216, FR-217, FR-220 to FR-222 and NFR-061 in [requirements.md](../requirements.md)
- [telemetry.md §3.2](../specs/telemetry.md#32-game-coordinator) (intervention moment, ground truth of the
  interventions), [§4.3](../specs/telemetry.md#43-decision-attributes) (`decision.intervention_id`)
- [flags.md §5](../specs/flags.md#5-interventions) (intervention id), [§13.3](../specs/flags.md#133-game)
  (`shield.activation`)
- ["Where did it go?"](where-did-it-go.md) (UC-100, moment shapes)
- Issue #2665, epic #2569
