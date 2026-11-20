# Stage sheet: the three-game demo from Flagsmith

One page for the presenter: the live demo as three games, driven from the Flagsmith UI instead of the
scripts. It assumes the Flagsmith link is on (`FLAGSMITH_LINK=on`, the stage `.env.local`). The scripts and
every other beat stay in the [talk runbook](two-standards-runbook.md); how the link works:
[flagsmith-sync.md](../architecture/flagsmith-sync.md).

## How a change in Flagsmith reaches the game

- **Where:** project `joustmania`, the stage environment. Each flag is one **feature named by its key**
  (`interventions_allowed`, `decision.backend`, …), tagged with its flag set (`agent`). Change the
  **environment value**, not a segment or an identity.
- **What to type:** for a flag with variants, the **variant name** (`standard`, `jev`, `chaos_focused`). For a
  free string such as the brief, the text itself, **without quotes**. For an object flag such as
  `decision.magnitude_bounds`, compact JSON.
- **The toggle:** enabled with a value means "this value runs". **Disabled means the baseline applies, not
  "flag off"**: for the kill switch `enabled` the baseline happens to be `off`, for most flags it is the file
  default. Prefer typing the value.
- **How fast:** at once when the realtime stream announces the change (the stage `.env.local` sets
  `FLAGSMITH_REALTIME_URL`), otherwise with the next pull, at most `FLAGSMITH_POLL_SECONDS` (30 s). **Not
  measured yet.** If a flip takes longer than a few seconds on the venue network, make that change with the
  script (the runbook beat in brackets) and show the value in Flagsmith afterwards.
- **Who wins:** Flagsmith, on every conflict and at every restart, the kill switch included (UC-054 BR-005,
  BR-006). A script write while the link is online is pushed to Flagsmith too, so mixing both is safe; a
  script write made while the link was offline is discarded at the reconnect.
- **What the audience sees:** the flag service applies the change as an ordinary write with writer `flagsmith`,
  then the same flagd push as any other write; Flagsmith's own audit log shows the change too.
- A refused value (out of bounds, wrong type) keeps the old value and marks the feature **unhealthy** in
  Flagsmith; the game never sees it.

## Screens

| Screen | URL (from the Pi's address) | Used for |
|---|---|---|
| Dashboard, **Agent** tab, Decisions | `/` | the live rows: act / noop / held, backend, target, rationale, confidence |
| Grafana, **Talk: Two Standards** | `/grafana/d/talk?kiosk` | decisions per minute by backend and outcome, interventions by type, OpenFeature evaluations |
| Jaeger, one game | `/jaeger/search?service=agent&operation=agent.game.summary&lookback=30m&limit=5` | one trace per game: every `agent.decide`, every action, the post-game summary at the end |
| Flagsmith | the stage environment | the changes, in front of the audience |

## Before the talk (T-1 h, link online)

1. `./scripts/talk-stage.sh reset` (the stage baseline; its deletes are pushed while the link is online).
2. `decision.backend` = `jev`, `interventions_allowed` = `standard`.
3. Ready for game 2, no effect while Jev decides: `decision.claude.model` = `sonnet`,
   `decision.act_confidence_threshold` = `gate`.
4. `objectives` = `balanced_focused` (the reset does not touch the goal).
5. Note the serial of the controller you will protect in game 2.
6. `./scripts/talk-preflight.sh`: no FAIL. In Flagsmith, check the values above show as enabled.

Play every game with 3 to 4 players: two-player games ended in 7 to 14 s in rehearsal, before Claude acted.

## Game 1: Jev decides, we watch

Nothing to change. Show the Decisions rows and the Grafana board during the game; afterwards open the game's
trace in Jaeger and click one `agent.decide`: rationale, confidence, threshold, the `chat jev-latest` children.

## Game 2: Claude and one sentence (runbook beat 4b)

| Order | Feature | Value |
|---|---|---|
| 1 | `decision.backend` | `claude` |
| 2 | `decision.magnitude_bounds` | `{"adjust_player_sensitivity":{"min":0.2,"max":1.5}}` |
| 3 | `decision.operator_brief` | `Make player <SERIAL> unkillable.` (no quotes) |

Expect: a `noop` first (stale readings), the first act 6 to 9 s into the game, rows on `claude` quoting the
brief, a shield for the protected player, then `adjust_player_sensitivity` 0.2 on them. Amber `held` rows mean
the threshold held a move. After the game: the summary span at the end of the trace ("The brief was served",
"served only by luck", "not served").

## Game 3: Jev, goal chaos, then the kill switch

| Order | Feature | Value | Why |
|---|---|---|---|
| 1 | `decision.operator_brief` | `off` | |
| 2 | `decision.magnitude_bounds` | `default` | **before chaos**: chaos with the 0.2 floor can make a random player unkillable |
| 3 | `decision.backend` | `jev` | |
| 4 | `objectives` | `chaos_focused` | the goal becomes high activity and frequent change |
| 5 (during the game) | `enabled` | `off`, then `on` | the kill switch: rows stop, the game keeps playing |
| 6 (after) | `objectives` | `balanced_focused` | |

Chaos is **not rehearsed**: the Jev gate was calibrated on the balanced goal, so it may act a lot or hardly at
all. Expect, if it acts: tempo changes, pacing switches, controller effects (rumble, LEDs, no longer held back
by the shield reserve), audio cues. If nothing shows within about 20 s, go to the kill switch. If Flagsmith is
slow for the kill switch: `./scripts/agent-killswitch.sh off` / `on`.

## After the talk

`./scripts/talk-stage.sh reset` with the link online, and `objectives` = `balanced_focused`. A restart is no
reset while the link is on: whatever Flagsmith holds comes back.
