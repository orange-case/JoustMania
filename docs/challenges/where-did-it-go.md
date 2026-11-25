# Booth challenge: "Where did it go?"

> Guide for a presenter or an outside reader. It restates [UC-100](../use_cases/UC-100.md) and
> [telemetry.md appendix A](../specs/telemetry.md#appendix-a-booth-profile); if they differ, the spec wins.
> Part of epic #2569 (booth challenges), issue #2658.

## 1. What the challenge shows

A player is warned, and a moment later dies. The game coordinator records each warning in one of three
shapes: a span, a span event or a log event (the *moment shape*). The same game goes through the gateway
to every enabled observability backend. The challenge asks each backend one question: does it still find
the player's warning, and does it correlate it with the death? One game is played per shape, and the three
games' results are shown side by side.

It is not a ranking. A result is `found`, `found but not correlated` or `not found`, with the backend's
answer next to the ground truth. There is no score and no order (UC-100 BR-008). A result is about what a
backend shows for a shape, not a verdict on the backend.

## 2. Setup

Preconditions (UC-100):

- The gateway export is on and forwards to the backends that take part (FR-086, UC-035 A7).
- The game mode is Joust Free-for-All, so every death is an elimination and each player dies at most once.
- Enough controllers are connected for a game (UC-010 preconditions).

**Choose the shape for the next game.** Set the observability flag `telemetry.moment_shape` through the
flag service ([UC-053](../use_cases/UC-053.md)). Values: `span_event` (default), `span`, `log_event`
(`both` was retired by #2786 and now runs as `span_event`, UC-100 A6). The flag is a stack overlay without targeting, so a write applies to the next game
([flags.md §10](../specs/flags.md#10-observability)). On the case, through the envoy route of the flag service's
HTTP API ([services.md §7.2](../specs/services.md#72-httpjson-api-resource-style)):

```bash
# set the shape for the next game (an in-memory stack overlay, like the stage scripts write)
curl -sS -X PUT http://localhost/flags/observability/telemetry.moment_shape \
  -H 'Content-Type: application/json' -d '{"variant": "log_event", "writer": "stage"}'
# back to the default (clears the in-memory overlay): do this after the booth too
curl -sS -X DELETE http://localhost/flags/observability/telemetry.moment_shape
```

- Reset the shape after the booth (the DELETE above). The overlay stays in effect for every later game,
  shadow and pattern games included, until it is cleared. The shape never changes the agent's eliminations:
  they come from the elimination-order gauge in every shape (UC-100 BR-007, UC-041 BR-009).
- One shape per game. The coordinator reads the flag once when the game starts. Changing it during a game
  does not affect that game; the new value applies from the next game (UC-100 A2, BR-002).
- A flag that is absent, unknown or unreadable gives `span_event`. A game started before the
  `observability` flag provider is ready also reads the default.

**Check the persistence before and after the booth (UC-100 BR-007).** The shape stays for every later game,
and over a restart of the game coordinator, until it is cleared or the flag service restarts (it is held in the
flag service's memory). No CI test covers this; check it by hand.

- Before the booth: `curl -sS http://localhost/flags/observability/telemetry.moment_shape` shows no overlay
  (the default), and the root span of a test game shows `telemetry.moment_shape` = `span_event`.
- After the booth: send the DELETE above, then play one game and confirm its root span shows
  `telemetry.moment_shape` = `span_event` (its player span carries the `death_warning` and `player_death`
  span events again). A shape that is still set shows on the root span of that next game.

**Confirm the shape the game ran in.** Open the game's root span (named after the mode, for example
`Joust Free-for-All`) in any backend and read its attribute `telemetry.moment_shape`. That value is the
shape the game used. If it differs from the one you chose, the game is not counted (UC-100 A1).

## 3. The three shapes

The warning and the death follow the chosen shape (UC-100 BR-007). The table names the warning; the death is
the same with the name `player_death` (span `game.player_death` is a point in time: start = end = the death).

| Shape | What is sent | Where it hangs in the trace | When it reaches a backend | What a backend needs to find it |
|---|---|---|---|---|
| `span_event` (default) | Span event `death_warning` on the player span | On the player span | With the player span. In Free-for-All, Teams, Traitor and Swapper (and for the Tournament loser) the player span ends at the death, so the event arrives then. In NonStop, Fight Club and Zombies it arrives at game end | Span events on a span, searchable by name and by `moment.id` |
| `span` | Span `game.death_warning`, start at the warning, end = start + the warning window, ended at once | Child of the player span | At once (the trace exporter batches for 2 s) | Search of spans by name and attributes, and the parent link to the player span |
| `log_event` | Log record with `event_name` `death_warning`, severity INFO, the moment's attributes as log attributes, carrying the player span's trace and span id | Not a span: linked to the player span by trace id and span id | Up to about 6 s (SDK batch up to 1 s plus the collector's 5 s logs batch) | A log store that accepts OTLP logs, searchable by event name and `moment.id`, and a way to jump from the log to the trace |

Every record of a moment carries the same identity attributes in every shape: `moment.id`, `player.serial`,
`player.name` (once known), `game.id` and `game.kind`. `moment.id` has the form `<game.id>/<player.serial>/<n>`
and is unique within the game (UC-100 BR-003).

## 4. The question and how to check an answer

**The question (UC-100 step 7).** Pick an eliminated player that had at least one warning before the death.
Ask each backend: when was this player's last warning before the death, and how long before the death?

**The ground truth.** At a normal game end the coordinator records, on the game's root span, one span event
`challenge.ground_truth` per player (UC-100 step 6, FR-219). Read it from there, not from any backend and not
from the agent's game summary (BR-006):

| Attribute | Meaning |
|---|---|
| `player.serial`, `player.name` | The player (the name once known) |
| `challenge.warning_times_ns` | Int array: the time of each warning, in order. Left out when the player was never warned |
| `challenge.death_times_ns` | Int array: the time of each death, in order. Left out when the player never died |

Times are wall-clock epoch nanoseconds. In Free-for-All a player dies at most once.

**Check.**

1. Take the asked player's `challenge.death_times_ns` entry and the last `challenge.warning_times_ns` entry
   before it.
2. Warning-to-death time = death time minus that warning time, in seconds (divide the difference by 1e9).
3. Compare it with the time the backend gave. They match when they differ by at most 0.1 s (UC-100 BR-011).

**What counts as the moment record (BR-009).** Only a record with the moment's name and its `moment.id`: the
span event, the span or the log record of section 3. These do not count, even when they describe the same
happening: the INFO log lines the game writes for a warning or a death, the game event `player_death` that the
event bus records on the current span, and the controller manager's effect records (`effect_player_death`,
`player_warning`).

## 5. Result format

For each backend and shape, one of three results (UC-100 BR-011):

| Result | Meaning |
|---|---|
| found | The backend shows the player's warning record, and its time from the last warning to the death matches the ground truth within 0.1 s |
| found but not correlated | It shows the player's warning record, but gives no time to the death, or a time that differs by more than 0.1 s |
| not found | It shows no warning record of that player in that game |

A backend that did not receive the game (gateway or backend unreachable, data refused) gets no result and is
recorded as *not received* (A3). That is not a result about the shape. The game is still played to its end and
the case keeps feeding its own backends and the agent unchanged (NFR-029).

Template, one row per backend and shape. Fill the backend's answer and the ground truth from section 4.

| Backend | Shape | Game id | Shape on root span | Backend's answer (warning time, time to death) | Ground truth (last warning, death, difference) | Result |
|---|---|---|---|---|---|---|
| | `span_event` | | | | | found / found but not correlated / not found / not received |
| | `span` | | | | | |
| | `log_event` | | | | | |

**When a game is not counted** (play another game in the same shape unless noted):

| Case | Why | Flow |
|---|---|---|
| The root span's `telemetry.moment_shape` differs from the chosen shape | The game ran in the default shape (flag absent, unknown, unreadable, or no log channel in the process) | A1 |
| No eliminated player had a warning before the death | There is no question to ask: record the round as without a question | A4 |
| The game was force-ended or the coordinator stopped before the end | No ground truth is recorded for it | A5 |

## 6. Known limits today

- **The shape never changes what the agent sees** (UC-100 BR-007, BR-010): in every shape its elimination order
  comes from the elimination-order gauge alone, and it reads no moment ([UC-041](../use_cases/UC-041.md) BR-009,
  #2786). The shapes and the ground truth are the booth profile of
  [telemetry.md appendix A](../specs/telemetry.md#appendix-a-booth-profile).
- **The case keeps no log store** (BR-006). A log-event round can only be checked against the ground truth on
  the game's root span, never against a copy of the log in the case.
- **A game may run in the default shape when flagd is not reachable.** Game admission waits for the
  `observability` provider up to 5 s, so only a flagd that does not answer within that deadline gives
  `span_event` (A1), also right after a restart. Check the root
  span's `telemetry.moment_shape` before you count a game.
- A `log_event` game in a process without OTLP logging runs as `span_event` and records that
  shape on the root span.

## 7. References

- [UC-100](../use_cases/UC-100.md) Find a Game Moment in Every Shape
- FR-216 to FR-219 and NFR-060 in [requirements.md](../requirements.md)
- [telemetry.md appendix A](../specs/telemetry.md#appendix-a-booth-profile) (booth profile: moment shapes, ground truth)
- [flags.md §10](../specs/flags.md#10-observability) (`telemetry.moment_shape`)
- [glossary](../glossary.md): booth challenge, moment, moment shape, gateway, backend (observability)
- Issue #2658, epic #2569
