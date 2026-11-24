# Stage sheet: the demo from Flagsmith instead of the scripts

One page for the presenter: the live-demo beats of the
[talk runbook](two-standards-runbook.md) as changes in the Flagsmith UI. It assumes the Flagsmith
link is on (`FLAGSMITH_LINK=on`, the stage `.env.local`). How the link works:
[flagsmith-sync.md](../architecture/flagsmith-sync.md).

## How a change in Flagsmith reaches the game

- **Where:** project `joustmania`, the stage environment. Each flag is one **feature named by its key**
  (`interventions_allowed`, `decision.backend`, …), tagged with its flag set (`agent`). Change the
  **environment value**, not a segment or an identity.
- **What to type:** for a flag with variants, the **variant name** (`standard`, `jev`, `strict`). For a free
  string such as the brief, the text itself, **without quotes**. For an object flag such as
  `decision.magnitude_bounds`, compact JSON.
- **The toggle:** enabled with a value means "this value runs". **Disabled means the baseline applies, not
  "flag off"**: switching `enabled` (the kill switch) off in Flagsmith brings back its baseline, which is `off`,
  but for most flags it means "back to the file default". When the baseline is what you want, prefer typing the value.
- **A value under an off toggle is not in force.** Each disabled feature shows its file default as its value
  (FR-227); a value you type there while the toggle is off does nothing, and the flag service resets it to the file
  default at its next start. Stage a value by switching the toggle on.
- **How fast:** at once when the realtime stream announces the change (the stage `.env.local` sets
  `FLAGSMITH_REALTIME_URL`), otherwise with the next pull, at most `FLAGSMITH_POLL_SECONDS` (30 s). **Measure it
  in rehearsal**; if a flip takes longer than a few seconds on the venue network, use the script for that beat.
- **Who wins:** Flagsmith, on every conflict and at every restart, the kill switch included (UC-054 BR-005,
  BR-006). A script write while the link is online is pushed to Flagsmith too, so mixing both is safe; a
  script write made while the link was offline is discarded at the reconnect.
- **What the audience sees:** the flag service applies the change as an ordinary write with writer `flagsmith`,
  then the same flagd push as any other write; Flagsmith's own audit log shows the change too.
- A refused value (out of bounds, wrong type) keeps the old value and marks the feature **unhealthy** in
  Flagsmith; the game never sees it.

## Before the talk (scripts, T-1 h, link online)

Run these on the Pi while the link is online, so its deletes and values are pushed to Flagsmith:

1. `./scripts/talk-stage.sh reset` (the stage baseline).
2. In Flagsmith, or `./scripts/agent-backend.sh jev`: `decision.backend` = `jev`.
3. `interventions_allowed` = `ambient` (the demo starts there).
4. `inference.backend` = `claude` (the post-game summary; the stage baseline sets it, check that it is there).
5. `./scripts/talk-preflight.sh`: no FAIL. Then open Flagsmith and check the values above show as enabled.

## The beats

| Beat (deck) | Feature | Set to | Back to | What shows |
|---|---|---|---|---|
| 1 Observe | none | | | a green act or grey noop row; open its trace |
| 2 Ambient → standard | `interventions_allowed` | `standard` | `ambient` | the agent may shield: white LED, Shield badge |
| 3 Threshold | `decision.jev.act_confidence_threshold` | `strict` | `gate` | near-threshold rows turn amber `held` |
| 4 Kill switch | `enabled` | `off` | `on` | the feed goes silent, the game keeps playing; an in-flight answer lands `discarded` |
| 5 Degrade | `decision.jev.base_url` | `blackhole` | `default` | rows `jev → rules`, `fallback_reason=timeout`, the game never waits |

Beat 4 is the one to rehearse from Flagsmith: the kill switch must stop the agent within seconds. If the
realtime stream is not there, use `./scripts/agent-killswitch.sh off` on stage and show the value in Flagsmith
afterwards. That fallback needs a Flagsmith that accepts writes: with change requests or a key without write access
the local stop is reverted a few seconds later (#2631); then set `enabled` in Flagsmith, or stop the agent
container (`docker stop joustmania-agent`).

## Optional: the operator brief (runbook beat 4b)

Only with time left and a game that runs past 15 s. Set in this order:

| Feature | Value |
|---|---|
| `decision.backend` | `claude` |
| `decision.claude.model` | `sonnet` |
| `decision.act_confidence_threshold` | `gate` |
| `decision.magnitude_bounds` | `{"adjust_player_sensitivity":{"min":0.2,"max":1.5}}` |
| `decision.operator_brief` | `Make player <SERIAL> unkillable.` (no quotes) |

Afterwards: `decision.operator_brief` = `off`, `decision.magnitude_bounds` = `default`,
`decision.act_confidence_threshold` = `default`, `decision.claude.model` = `haiku`, `decision.backend` = `jev`.

## After the talk

`./scripts/talk-stage.sh reset` with the link online, or set the values back in Flagsmith. A restart is no reset
while the link is on: whatever Flagsmith holds comes back.
