# Booth challenge: "What is happening?"

> Guide for a presenter or an outside reader. It restates [UC-104](../use_cases/UC-104.md) and
> [telemetry.md appendix B](../specs/telemetry.md#appendix-b-benchmark-schema); if they differ, the spec wins.
> Part of epic #2569 (booth challenges), issue #2585.

## 1. What the challenge shows

Each observability backend's own AI assistant gets the same published prompts and builds a game dashboard from
the telemetry the case sent it. The dashboard is checked panel by panel against what really happened in the
seeded games, so a dashboard that looks right but shows wrong numbers is caught. It is then the dashboard of
"Can you see the swing?" ([can-you-see-the-swing.md](can-you-see-the-swing.md), UC-102).

The results are a gallery, shown side by side: no score and no order (UC-104 BR-005).

**The prompts are published upfront.** Anyone, a vendor included, can read and rehearse them before the booth.
That is on purpose: the challenge shows what a backend's assistant builds from standard OpenTelemetry and this
page, not whether it survives a surprise.

## 2. The telemetry a dashboard can use

Everything below is in [appendix B](../specs/telemetry.md#appendix-b-benchmark-schema) (its rows list UC-104
where this challenge reads them). The prompts name only these keys.

**The trace (B.1).** One trace per game, from the service `game-coordinator-service` (namespace `joustmania`).
The root span is named after the mode (`Joust Free-for-All` for both seeding games) and carries `game.id`,
`game.kind` (`real`, `shadow`), `game.name`, `player.count`, and at game end `game.outcome` and, when someone won,
`game.winner` (the winner's controller serial). Under it, `game.players` holds one `game.player` span per player
(`player.serial`, `player.name`). With the moment shape at its default `span_event`, each death is a span event
`player_death` on the player's `game.player` span, with `moment.id`, `game.id`, `player.serial`, `player.name`,
`death.reason` (`motion`, `agent_intervention`) and `accel_magnitude` (g). The winner's span ends with the event
`victory`. At a normal end the root span gets one `challenge.ground_truth` event per player, with
`challenge.death_times_ns` for a player who died.

**The metrics.**

| Metric | Labels | What it is |
|---|---|---|
| `game_player_alive` | `serial`, `game_id` | 1 while the player is alive, 0 after the death |
| `game_player_elimination_order` | `serial`, `game_id` | set at the death: 1 = first out; the winner has none |
| `game_player_accel_magnitude` | `serial`, `game_id` | the player's acceleration in g, a new value about every 100 ms |
| `game_effective_death_threshold` | `game_kind`, `game_id` | the game's death threshold in g (before per-player factors), changes with the music tempo |

Not usable for this dashboard: `game_players_alive` (game-wide, set by the primary game only, so it stays empty for
the pattern game, which runs as a shadow game) and `game_player_deaths_total` (no `game_id`).

**Synthetic telemetry.** `stowaway`'s own span `stowaway.players.game` reaches the external backends too, marked
`orangecase.synthetic` = `true` (§1.1). It is not part of the game's dashboard; the prompts say to ignore it.

## 3. The panels (the checklist)

The five panels of UC-104 BR-003, the metrics a game dashboard needs:

| Panel | What it must show | Prompt |
|---|---|---|
| players alive | the players alive over the game's time | 1 |
| elimination order | the order in which the players went out, and the winner | 1 |
| last knock-out | the player of the last death and its time | 1 |
| deaths | each player's death, with its time | 3 |
| motion | each player's acceleration over the game's time, with the death threshold | 2 |

## 4. The prompts (prompt set `v1`)

### 4.1 One prompt or a sequence: a fixed sequence of three

Decided for `v1`: three prompts in a fixed order (overview, per-player motion, death drill-down), in one
conversation (UC-104 BR-002). Why:

- **Retries stay small.** A retry repeats one prompt (at most two per prompt). With one long prompt a retry
  rebuilds the whole dashboard, and the record cannot say which part failed.
- **The motion panel stands alone.** It is the panel challenge 1 uses and the hardest one (raw 100 ms samples,
  a threshold line). On its own it cannot be dropped silently from a long list of asks.
- **Each step has a visible result.** A backend that gets the overview right and fails the drill-down shows that
  in the gallery, panel by panel.
- **Short enough to run at the booth.** Three prompts, not one per panel: the time is from the first prompt to the
  last panel shown, so more steps would mostly measure typing.

### 4.2 Running them word for word

Copy each prompt unchanged. The only substitution is the two game ids: `<GAME_PATTERN>` and `<GAME_MATCH>`, the
ids of the seeding games (§5), in the form `game_<12 hex>`. Any other change, also a fix of a typo, is a new
prompt set version (`v2`, ...); a recorded result always names its version (§7).

**Prompt 1: overview**

```text
Build a dashboard named "JoustMania game (generated)" for JoustMania, a party game played with motion
controllers. Its telemetry is OpenTelemetry from the service "game-coordinator-service" (service.namespace
"joustmania"). Each game is one trace. The root span is named after the game mode, for example
"Joust Free-for-All", and has the attributes game.id, game.kind, player.count and, when the game has ended,
game.outcome and game.winner (the controller serial of the winner). Under the root, a span "game.players" has one
span "game.player" per player with player.serial and player.name. A death is a span event "player_death" on the
player's "game.player" span, with game.id, player.serial, player.name, death.reason and accel_magnitude.
Ignore all telemetry that has the attribute orangecase.synthetic = true.

Give the dashboard a variable game_id that selects one game. The games to look at are <GAME_PATTERN> and
<GAME_MATCH>; default to <GAME_MATCH>. Do not filter on game.kind. For the selected game, add these panels:
1. "Players alive": the number of players alive over the game's time, from the metric game_player_alive
   (labels serial and game_id; 1 = alive, 0 = dead), summed over serial.
2. "Elimination order": a table of the players in the order they went out, from the metric
   game_player_elimination_order (labels serial and game_id; 1 = first out), and the winner from game.winner.
3. "Last knock-out": the player of the last player_death event of the game and its time.
Use the time range of the selected game's root span. Do not average values over more than 1 second.
```

**Prompt 2: per-player motion**

```text
Add a panel "Motion" to the same dashboard: the acceleration of each player of the selected game over the
game's time, from the metric game_player_accel_magnitude (in g; labels serial and game_id; about one value
every 100 ms per player), one line per serial. Show the values at the finest resolution this backend has
stored, with no rollup to a step above 1 second. In the same panel, draw the game's death threshold as a line,
from the metric game_effective_death_threshold (in g; labels game_kind and game_id) for the selected game_id.
```

**Prompt 3: death drill-down**

```text
Add a panel "Deaths" to the same dashboard: one row per player_death event of the selected game, in time order,
with the time, player.serial, player.name, death.reason and accel_magnitude, and a link from each row to the
game's trace. Then list every panel of the dashboard with the query it uses.
```

### 4.3 The published answers

The assistant may ask before or while it builds. It gets only these answers, word for word (UC-104 A4):

| The assistant asks about | Answer |
|---|---|
| the time range | `Use the time range of the root span of the selected game.` |
| where the data is, which service or data source | `The service game-coordinator-service in the namespace joustmania; traces and metrics arrive by OTLP.` |
| how a metric or attribute is named in this backend | `Use the name as this backend stores it; the OpenTelemetry names are the ones in the prompt.` |
| real or shadow games, `game.kind` | `Show both games; do not filter on game.kind.` |
| where the deaths are, the moment shape | `Deaths are span events player_death on the game.player span of the player.` |
| alerts, notifications, saved searches | `No, only the dashboard.` |
| anything else (the default) | `Use the telemetry of the game with this game id and choose yourself.` |

## 5. Running the challenge

### 5.1 Before the booth (UC-104 preconditions)

- The full stack runs and the gateway export forwards to the backends that take part (FR-086, UC-035 A7).
- The moment shape is the default `span_event` for every seeding game (the prompts name the span events): the
  observability flag `telemetry.moment_shape` has no overlay, see [where-did-it-go.md §2](where-did-it-go.md#2-setup).
- The agent runs at `watch` (`enabled` = `watch`, the default of the `agent` set) for the pattern game, so its
  deaths come from motion only (BR-001).
- No backend holds a game dashboard from an earlier run, generated or hand-built.
- Per backend, a hand-built dashboard with the five panels of §3, titled `JoustMania game (hand-built)`, is built
  from this page, exported and removed from the backend (BR-006). It is loaded only in A3 or A5.
- Rehearse the prompts once per backend on an earlier game, then delete the rehearsal dashboard and conversation.

### 5.2 Seeding (UC-104 step 1, BR-001)

Both games are Joust Free-for-All games that end normally, played through the normal pipe (the case's collector,
then the gateway). Play them before the prompts run.

**The pattern game.** `stowaway` in players mode reserves three mock controllers (tagged `stowaway`), starts a
shadow Joust Free-for-All game with the archetype `quick_cascade` and lets it run to its end
([UC-106](../use_cases/UC-106.md) A2, BR-005). It starts only while no real match runs, and plays one game after
another while it is on, so switch it off once the first game ended:

```bash
docker compose --profile stowaway up -d stowaway
for f in stowaway.challenge:none stowaway.mode:players stowaway.enabled:on; do
  curl -sS -X PUT "http://localhost/flags/observability/${f%%:*}" \
    -H 'Content-Type: application/json' -d "{\"variant\": \"${f#*:}\", \"writer\": \"stage\"}"
done
# wait until the pattern game ended (dashboard, or its root span has game.outcome), then:
curl -sS -X PUT http://localhost/flags/observability/stowaway.enabled \
  -H 'Content-Type: application/json' -d '{"variant": "off", "writer": "stage"}'
```

The pattern game's own telemetry is the game coordinator's and is not synthetic, so it reaches the case's Jaeger
and VictoriaMetrics as well as the backends. Its id: in the case's Jaeger (`/jaeger/`), service
`game-coordinator-service`, operation `Joust Free-for-All`, tag `game.kind=shadow`, the game in the seeding
window. In an external backend, the span `stowaway.players.game` carries the same `game.id`.

**The real-controller match.** A Joust Free-for-All game with real controllers, started from the menu
([UC-010](../use_cases/UC-010.md)), with the agent as it is at the booth. Its id: the root span with
`game.kind=real`, or the dashboard.

**Check each game before using it.** Its root span has `game.outcome` = `winner` (or `tie`), never
`force_ended`, and `telemetry.moment_shape` = `span_event`, and it has `challenge.ground_truth` events. A game that
fails this is replaced by a new game of the same kind (A1). Note both ids as `<GAME_PATTERN>` and `<GAME_MATCH>`.

Wait until both games are visible in each backend before its first prompt; a backend that never shows them is
*not received* (A2).

### 5.3 The run (steps 3 and 4, BR-002)

- The backend's own assistant only; no other model, no plug-in the backend does not ship.
- One new conversation per backend; prompts 1, 2, 3 in order, word for word (§4.2).
- At most two retries per prompt, each the same prompt again. Record every retry.
- Questions get only the answers of §4.3.
- Nobody edits the dashboard. A change by hand makes it *hand-built*.
- Time from sending prompt 1 to the last panel shown.
- No assistant that builds dashboards (A3), or no usable dashboard after the retries (A5): load the prepared
  hand-built dashboard, labelled *hand-built*, and check that one; record *failed* with the reason and the
  assistant's last answer for A5.

## 6. The reference and the checklist (steps 5 and 6, BR-003, BR-004)

Check every panel for **both** game ids (switch the dashboard's `game_id`). The reference comes from the case's
own stores, never from a backend. A time is correct within 1 s.

Find the game's window first: in the case's Jaeger, tag `game.id=<id>`, read the root span's start and end, in
Unix seconds (`START`, `END`). On the root span read `game.outcome`, `game.winner` and the
`challenge.ground_truth` events (`player.serial`, `challenge.death_times_ns`; divide by 1e9 for seconds).

```bash
VM=http://localhost/victoria-metrics
G=game_0123456789ab START=<root start s> END=<root end s>
# players alive over time
curl -sG "$VM/api/v1/query_range" --data-urlencode "query=sum(game_player_alive{game_id=\"$G\"})" \
  -d start="$START" -d end="$END" -d step=1s
# elimination order (1 = first out); the series is removed shortly after the game, so read it over the window
curl -sG "$VM/api/v1/query" \
  --data-urlencode "query=last_over_time(game_player_elimination_order{game_id=\"$G\"}[$((END - START + 60))s])" \
  -d time="$((END + 60))"
# motion: the raw samples
curl -sG "$VM/api/v1/export" --data-urlencode "match[]=game_player_accel_magnitude{game_id=\"$G\"}" \
  -d start="$START" -d end="$END"
# the death threshold
curl -sG "$VM/api/v1/query_range" --data-urlencode "query=game_effective_death_threshold{game_id=\"$G\"}" \
  -d start="$START" -d end="$END" -d step=1s
```

| Panel | Correct when it shows | Reference |
|---|---|---|
| players alive | the count over time, stepping down at each death time | `sum(game_player_alive)` above, and the ground-truth death times |
| elimination order | the players in the order of their deaths, and the winner (none on a `tie`) | the elimination order query, the ground truth ordered by death time, `game.winner` |
| last knock-out | the player with the latest death and its time | the largest `challenge.death_times_ns` |
| deaths | every death, each with its player and time | `challenge.death_times_ns` per player |
| motion | a series per player of the game, and the threshold line | the export (one series per `serial`) and the threshold query |

A panel is *correct*, *wrong* (other values) or *missing* (no such panel). How dense the motion panel is, is not
judged here: that is challenge 1, which uses this dashboard.

## 7. The result format

Per backend (BR-005): the result *generated*, *hand-built* or *failed*; the prompt set version; the time; the
retries per prompt; each panel for each game. A backend that did not receive the games is *not received*.

| Backend | Result | Prompt set | Time | Retries (P1/P2/P3) | Pattern game: alive / order / last KO / deaths / motion | Match: alive / order / last KO / deaths / motion | Notes |
|---|---|---|---|---|---|---|---|
| | generated / hand-built / failed / not received | v1 | mm:ss | 0/0/0 | correct / wrong / missing ×5 | correct / wrong / missing ×5 | |

## 8. Open

- **No external backend has been run yet.** The acceptance of #2585 asks for one run, recorded here. That needs a
  backend with an assistant behind the gateway; until then this page has no result row and prompt set `v1` is
  unrehearsed.
- **The pattern game is a shadow game.** `stowaway` starts it as a shadow game (origin unspecified,
  `game.kind=shadow`, UC-106 BR-005). A dashboard limited to real games hides it, and `game_players_alive` stays
  empty for it; the prompts work around both. Whether it should be a real game instead is open.
- **Modes other than Free-for-All.** The prompts and the checklist assume each player dies at most once. Challenge
  1 may use the dashboard live in any mode; in NonStop, Fight Club, Zombies or Swapper a player can die more than
  once and `game_player_elimination_order` counts only the deaths that take a player out (telemetry.md §3.2).
  Whether the dashboard must hold for those modes is open.
- **The resolution ask in prompt 2.** Prompt 2 asks for the finest stored resolution, so challenge 1 sees what the
  backend kept, not its default drawing step. Whether challenge 1 should instead see the backend's defaults is
  open.

## 9. References

- [UC-104](../use_cases/UC-104.md) Generate the Game Dashboard with a Backend's Assistant
- FR-247, FR-248 in [requirements.md](../requirements.md)
- [telemetry.md appendix B](../specs/telemetry.md#appendix-b-benchmark-schema) (trace shape, attributes, metrics),
  [§6](../specs/telemetry.md#6-metrics) (metric semantics)
- [UC-106](../use_cases/UC-106.md) `stowaway` players mode, [UC-102](../use_cases/UC-102.md) the challenge that uses
  the dashboard
- Issue #2585, epic #2569, #2576, #2582 (a recorded reference dataset, later)
