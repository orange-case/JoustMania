# Agent

Adaptive-difficulty agent that turns live telemetry into intervention decisions.

## Overview

The Agent sits between JoustMania's **observation layer** (OpenTelemetry) and its
**control layer** ([OpenFeature](https://openfeature.dev/) / flagd). It receives
spans, metrics and its own lifecycle log events from the OTel Collector over OTLP, accumulates a rolling
`GameContext`, gates on whether the context is worth acting on, and calls stub
decision hooks that will eventually drive feature-flag changes.

The Collector fans telemetry out to the Agent via an `otlp_http/agent` exporter wired
into the existing **traces** and **metrics** pipelines — the Agent receives the
same signals as the rest of the observability stack, no new instrumentation
required.

Written in Go using the OpenTelemetry Collector `pdata` libraries and standard
`net/http` for health checks.

### Signal timing (design note)

**Metrics are the primary, timely signal source.** Counter/gauge updates reach
the Agent in roughly **100 ms – 1 s** end to end, so difficulty decisions are
driven by metrics.

**Spans are late by design.** `player_lifecycle` spans only flush at game end,
and even then sit behind the ~10 s trace batch window. They are used for
**audit/confirmation**. A span batch still wakes the
evaluation and adds span-derived state (`game.mode`,
`game.kind`, experiment id/arm) that decisions may read, so that state lags the
game (UC-041 BR-001).

The metric extractor already recognizes **both** today's metric names **and** the
five #722-proposed metrics, so each signal lights up automatically the moment its
producer ships:

- `game_player_movement_variance`
- `game_player_skill_level`
- `controller_battery_pct`
- `game_player_elimination_order`
- `game_interventions_total`

## GameContext

The Agent accumulates context at two scopes.

| Scope | Fields |
|-------|--------|
| **Per-player** | `movement_intensity`, `movement_variance`, `battery_pct`, `skill_level`, `active` |
| **Per-session** | `duration_seconds`, `active_player_count`, `elimination_sequence` |

### Session identity heuristic

Sessions are not carried in every signal, so the Agent synthesizes them:

- A new synthetic session (`session-N`) is created when `game_active` transitions
  **0 → 1**.
- Once a `game_id` label is observed, the synthetic session **adopts** that id.
- A synthetic `session-N` is never actuated: the intervention writer refuses any id
  without the `game_` prefix (UC-040 A18, BR-002).

### Staleness & eviction

| Entity | Rule |
|--------|------|
| Player | Evicted after a **5 s** TTL with no fresh signal |
| Session | Kept for a **15 s** grace period after it goes inactive, then evicted |

## Per-game partitioning (#845)

With `GAME_MAX_CONCURRENT_GAMES` allowing several games at once, the Agent
partitions everything stateful **by `game_id`** so two concurrent games never
share player state, a session, or a decision budget.

### Context: the Multiplexer (PR B)

The `gamecontext.Multiplexer` holds **one `Store` partition per `game_id`**. Each
received datapoint/span is routed to its partition on the resolved `game_id`
**before** it is applied, so players, sessions, and elimination order stay
disjoint. Signals that carry no `game_id` land on the **fallback partition**
(`FallbackGameID = ""`) — the zero-regression linchpin: in single-game mode (or
against a coordinator without the `game_id` labels) every signal lands there and
the Multiplexer behaves byte-for-byte like the old single `Store`.

Partitions are created **lazily** on first touch and removed by `EvictStale` once
a game's session has ended past grace and retains no fresh players. The fallback
partition is **never** wholesale-removed (its session still resets on grace).

### Decisions: the LoopSet (PR C)

The `decision.LoopSet` is the per-game counterpart to the Multiplexer: **one
`decision.Loop` per partition**, lazily created on first evaluation. Each Loop
owns its:

- **weighted rate-limit budget** (`policy.max_interventions_per_minute`) — one
  game exhausting its per-minute budget does **not** block another game;
- **log/span throttle slot** (`decision.throttle_seconds`) — one game's throttle
  never silences another's `agent.evaluate` log or `agent.disabled` span;
- **per-cycle `LayerState`** (the `#729` span-attribution source of truth).

**Shared vs per-game wiring.** The factory (`main.go`) builds, per Loop:

| Component | Per-loop? | Why |
|-----------|-----------|-----|
| Rules engine (`ObjectiveRules`) | **Fresh per loop** | Carries per-cycle mutable state the loop drives every cycle — `SetObjectives`/`SetFitness` published before `Evaluate`, `LastFitness` read after. A shared engine would let two games **race** each other's objective weights and fitness reads, stamping the wrong game's `agent.objectives`/`fitness.evaluated` onto a span. |
| Action sink (`actions.Writer`) | **Shared** | Holds no per-game state — it serializes a read-modify-write of the flagd interventions file under its own mutex, keyed by the decision's target. Safe to close over one instance. |
| Flag source, tracer | **Shared** | Stateless / concurrency-safe. |

### Loop lifecycle / eviction

The eviction ticker runs `mux.EvictStale()` and then
`loops.Retain(mux.Partitions())`: a removed partition's Loop — and its budget +
throttle state — is dropped **in lockstep** with its Store. The **fallback loop
is permanent** (`Retain` skips it), mirroring the fallback Store. A dropped
game's Loop is **recreated lazily with a fresh budget** if the game resumes —
acceptable because a partition is only dropped after its game fully ended past
grace.

### Span attribution

Decision spans are independently attributable per game. The root
`agent.signal_received` span carries both `session.id` and the `game.id` alias
(`session.id` **is** the real `game_id` since PR A's early adoption); the
decision / `agent.disabled` / `invoke_agent joustmania-agent` spans carry `game.kind` and
`game.id`. So a Jaeger query by `game.id` is
symmetrical with the coordinator's own `game.id`-tagged spans, and two concurrent
games' traces never blur together.

### Multi-game decision flow

```
OTLP Export (metrics/traces batch)
        │  per datapoint/span: resolve game_id
        ▼
  Multiplexer ── route ──► Store[game-A]      Store[game-B]      Store[""]   (fallback)
        │                      │                  │                 │
        │  touched game_ids    │ Snapshot         │ Snapshot        │ Snapshot
        ▼                      ▼                  ▼                 ▼
  signalUpdated ──► gate.ShouldEvaluate(snap) per partition
                            │ (if eligible)
                            ▼
                 LoopSet.For(game_id) ──► Loop[game-A]   Loop[game-B]   Loop[""]
                                              │  own budget   │ own budget   │ own budget
                                              │  own throttle │ own throttle │ own throttle
                                              ▼               ▼              ▼
                                        agent.decision spans (game.id / game.kind)
```

> **#847 hook:** the per-game LLM gating (per-game cadence / eligibility) will
> attach to this per-game Loop state; the **global** LLM budget stays shared
> across loops (one inference-cost ceiling for the whole agent).

## Gating & decisions

After each context update the Agent evaluates `should_evaluate`. When the gate
opens it runs the decision loop, which evaluates the **OpenFeature** control
flags from flagd on **every cycle** (never cached). The flags form a **four-layer
model**, applied in order:

### The four flag layers

1. **Existence** (gates the loop) —
   - `enabled` (string: `off` / `watch` / `act`): `off` is the kill switch: the
     loop short-circuits before any rules run (only the throttled `agent.disabled`
     span is emitted). `watch` runs everything (decide, journal, spans, dashboard)
     but writes nothing that changes a game. `act` applies decisions. An
     unresolvable or unknown value (the legacy `on` included) reads `off`, so the
     agent comes up inert when flagd is unreachable.
2. **Objective** (steers the rules) —
   - `objectives` (object → `map[string]float64`): per-session goal weights. The
     loop publishes the per-cycle value into the rules engine through a
     `LiveObjectives` source (`decision/objectives.go`); the engine reads it
     inside `Evaluate`, falling back to `{endurance: 1.0}` when the flag resolves
     nothing. This replaces the engine's static objective source (#726).
   - `fitness.*` (numbers): the per-objective fitness thresholds (#731). The loop
     publishes the per-cycle values into the engine through a `LiveFitness`
     source (`decision/fitnesssource.go`); the engine evaluates the fitness
     functions against the live game context each cycle and amplifies the
     candidates serving a *failing* objective. See **[Fitness functions
     (#731)](#fitness-functions-731)** below.
3. **Decision backend** (who decides, epic #1225) —
   - `decision.backend` (string, default `rules`): `rules` runs the deterministic
     rules engine; `claude` / `jev` hand the decision to that remote
     backend, with the rules engine as the fallback. See **Decision backends**
     below and [text-backend.md](../../docs/specs/text-backend.md). The
     legacy `mode` / `model` / `prompt_variant` flags were removed in #1234.
4. **Permission** (constrains which actions dispatch, and how fast) — applied to
   each candidate decision in this order, blocking with an attributed reason:
   - `interventions_allowed` (string of comma-separated ids → `[]string`): the
     allow-list gate (#727). A decision whose intervention is not on the list is
     blocked (`reason=not_allowed`). An empty allow-list dispatches nothing. It is
     a STRING flag (not a LIST/object flag): the flagd RPC resolver the agent uses
     silently TYPE_MISMATCHes a top-level LIST flag to its empty default, which
     would block every intervention (#1127); a string is read cleanly.
   - `policy.battery_threshold` (int %, default 20): a **player-targeted**
     decision is blocked (`reason=battery_threshold`) when the target player's
     battery is below the threshold — a low-battery controller signals unreliable
     input. Session-scoped decisions are unaffected; missing battery data is
     treated as unknown (does not block, but is noted).
   - `policy.max_interventions_per_minute` (int, default 2; variants
     `conservative` 1, `aggressive` 4, operator-only `lively` 8 and
     `frenzy` 20, #2518): a **weighted
     sliding-window** rate limiter across all dispatched interventions. Weights:
     soft (`play_audio_cue`, `send_controller_effect`, `adjust_volume`) = 0.5;
     medium (`adjust_music_tempo`, `adjust_player_sensitivity`, `grant_shield`) =
     1; hard (`adjust_global_sensitivity`, `eliminate_player`, `revive_player`,
     `end_game`) = 2 (see `docs/research/722-intervention-surface.md` §5). When
     the budget for the trailing minute is exhausted, further decisions are
     blocked (`reason=rate_limit`), not queued.

Every value evaluated this cycle plus the per-decision outcomes (dispatched /
blocked + reason, with the rate-limit weight charged) are captured in a single
cohesive **`LayerState`** (`decision/layerstate.go`), returned from `OnEvaluate`
and retained via `LastLayerState()`. It is the span-attribute **source of truth**
that #729 lifts onto the decision span verbatim.

The two decision hooks:

- **Rules engine** (#726) — `ObjectiveRules`, the objective-weighted decision
  logic below. Active by default; its objectives are driven live by the flag.
- **Action sink** (#730) — applies permitted intents via OpenFeature/flagd.
  **No-op unless `enabled` is `act`**: the gated sink re-reads the live `enabled`
  flag per `Apply`. With it `act` the real `actions.Writer` is dispatched and
  decisions are written to the `interventions` set through the flag service (see
  **Action sink** below); `watch`/`off`/flagd-unreachable is **fail-closed** —
  decisions are traced (`decision.not_applied_reason=watch`) and discarded. Flip
  the flag through the flag service (`scripts/flags.sh set agent enabled act`), no
  restart.

The flags wrapper lives in [`flags/`](flags/) and uses the OpenFeature Go SDK
with the flagd **RPC** resolver against flagd's gRPC evaluation port. Flag keys
are flat (`enabled`, `mode`, `objectives`, `interventions_allowed`) and match
[`services/flagd/agent.json`](../flagd/agent.json) (flagSetId `agent`). See
[`docs/research/722-intervention-surface.md`](../../docs/research/722-intervention-surface.md)
for the intervention-surface design.

## Rules engine (#726)

`ObjectiveRules` is the `rules_decide(context, objectives)` path — the non-LLM
intelligence and the final link of the inference fallback chain. Each rule
yields candidates with an urgency (0–1) and the objective they serve; the
final score is `urgency × weight[objective]`. Candidates below a minimum score
(0.10) are dropped, the rest are admitted best-first (ties: cheaper first, then
name) — at most 2 decisions per evaluation, and the rule set runs at most once
per second. The weighted per-minute budget is enforced downstream by the loop
(see the permission layer above), not by the engine.

| Rule | Objective | Trigger | Intervention |
|------|-----------|---------|--------------|
| R1/R2 | endurance | session younger than `fitness.endurance.min_session_seconds` while players are eliminated | `adjust_music_tempo` 1.3 (fast: raises the death threshold, #2487) / `play_audio_cue` |
| R3 | balanced | skill spread > `fitness.balanced.max_skill_gap`, among players with fresh readings (below) | `adjust_player_sensitivity` → highest-skill outlier |
| R4 | balanced | after the first elimination: a struggling player (below the field mean, never the strongest; fewest shields first) while the field shrinks (needs ≥ 2 players with known skill and fresh readings — "struggling" is only meaningful relative to others). Before it: a player in sustained danger (the last `policy.shield_danger_samples` readings ≥ `policy.shield_danger_ratio` of the death threshold, the newest value at most 3 s old since it was last new, the window at most n × 2.5 s; off with `policy.shield_danger_enabled`; decide-contract.md §15.3, #2461), so a duel can get shields | `grant_shield` → struggling / endangered player |
| R5 | accelerate | duration > `fitness.accelerate.target_session_seconds` | `adjust_music_tempo` (1.0, slow: lowers the death threshold; decide-contract.md §15.5) |
| R6 | accelerate | duration > 1.5× target, > 2 players | `eliminate_player` → least active |
| R7 | accelerate | duration > 2× target **and accelerate strictly dominant** (a tie never ends a game) | `end_game` |
| R8 | chaos | movement variance ≈ 0 ("statue"), player with fresh readings | `send_controller_effect` |
| R9 | chaos | periodic random nudge (injectable rng) | `send_controller_effect` |

**Fresh readings (R3, R4, R8; decide-contract.md §15.4, #2511).** The rules read a
player's skill level or movement variance only while the player's movement track
is live: the newest `game_player_accel_magnitude` reading is finite, not newer
than the context's `UpdatedAt`, and was last new at most 3 s before it
(`decide.FreshReadings`, the R4d check; a frozen gauge is re-exported with its last
value, so the age counts from the first reading of the newest run of equal
values). A player with no, stopped or frozen readings is not a candidate and is
left out of R3's spread and R4's mean and strongest player (fail closed).

**LLM-only interventions (#800).** Five intervention types are fully plumbed
(flag schema, action-sink mapping, game-side handlers, weight table) but are
**deliberately not emitted by any rule**: `adjust_volume`, `revive_player`,
`adjust_global_sensitivity`, `adjust_global_difficulty`, and
`set_pacing_profile`. They require judgment the deterministic ruleset cannot
encode (who deserves a revive; when reshaping the whole session's difficulty
or pacing is socially right) and are reserved for a remote decision backend. They remain
reachable today via the permission layer for manual/operator dispatch — they
are intentionally unreached by `rules_decide`, not dead code.

**Policy constraints** (`policy.*` flags):

- `battery_threshold` (20): players below it lose controller effects and
  difficulty raises; session-wide demand raises are blocked while *anyone* is
  low; `eliminate_player` of a low-battery player stays available only as the
  accelerate-dominant graceful exit.
- movement variance window (fixed 10s, not a flag): ALL chaos candidates (variance-triggered
  statue nudges and the random R9 nudge alike) are suppressed for one window
  after any difficulty intervention — the variance baseline is invalid, and a
  random rumble right after a tempo change would muddy attribution of the
  difficulty intervention's effect.
- `max_interventions_per_minute` (2): a **weighted** sliding-window budget per
  the [#722 research §5](../../docs/research/722-intervention-surface.md):
  soft 0.5 (audio cue, controller effect, volume), medium 1 (tempo, player
  sensitivity, shield), hard 2 (global sensitivity, eliminate, revive,
  end_game). **Enforced by the decision loop's permission layer, not the
  engine** (the reconciled #727/#728 stack unified #726's and #728's limiters
  into one — `decision/ratelimit.go`). Because the loop sees the allow-list and
  battery gates, the budget is charged only on decisions that pass those and
  fit — an improvement over #726's original "charge every emitted decision"
  (the engine could not see permissions). The engine now only caps emission at
  2 decisions per evaluation; `cd.cost` survives as a deterministic
  cheaper-first tie-break.
  **Scarce budget (#1326, UC-040 BR-014/BR-015, `decision/budget.go`):** the
  loop admits a cycle's decisions in rank order and stops at the first one
  that does not fit (lower-ranked ones are blocked `rate_limit` without
  charge, so a 0.5 nudge cannot take the half slot a 1.0 shield is waiting
  for); `send_controller_effect` must leave 1.0 free for a protective
  intervention unless `chaos` strictly dominates the weights; and a refusal
  that repeats while the budget is unchanged emits no new `agent.decision`
  span (counter + debug log only). The ranking itself is unchanged.

**Configuration seam (#726/#727/#731):** objectives, policy, and fitness
thresholds come from the `ObjectivesSource` / `PolicySource` / `FitnessSource`
interfaces (`decision/config.go`). #727 wires the **objectives** source to
OpenFeature: the loop publishes the per-cycle `objectives` flag into a
`LiveObjectives` source (`decision/objectives.go`) that the engine reads each
evaluation. #731 wires the **fitness** source the same way: the loop publishes
the per-cycle `fitness.*` flags into a `LiveFitness` source
(`decision/fitnesssource.go`), so threshold changes take effect on the next
cycle with no restart. Only **policy** still runs on `DefaultStaticConfig()`
(the flagd-schema defaults); objectives fall back to `{endurance: 1.0}` whenever
the flag resolves nothing.

## Fitness functions (#731)

Fitness functions turn "is this session *succeeding* for objective X?" into
observable, runtime-tunable numbers. The thresholds come entirely from the
`fitness.*` flags (never code), are evaluated **every cycle** against the live
`GameContext`, and both (a) **steer action selection** and (b) ride onto the
decision span as `fitness.evaluated`.

Three objectives have a fitness function; **chaos has none** — chaos is
*unpredictability* by definition ([#722 research
§4](../../docs/research/722-intervention-surface.md)), so there is no
success/degradation target to measure. It contributes no fitness value and no
selection pressure.

Each function produces a normalized `progress` in `0..1` (1 = satisfied, 0 =
maximally failing) and a `pressure = 1 − progress`. Functions whose required
signals are missing are **skipped** — they emit no value rather than fabricating
one.

| Objective | Flag (threshold) | Signal | Progress |
|-----------|------------------|--------|----------|
| `endurance` | `fitness.endurance.min_session_seconds` (120) | `session.duration_seconds` | `duration / min`, clamped to 1 — long sessions win |
| `balanced` | `fitness.balanced.max_skill_gap` (0.4) | spread (max−min) of per-player `skill_level` (≥ 2 known) | `1 − gap/(2·max)` — the 0.5 point is exactly at the threshold |
| `balanced` | `fitness.balanced.spike_survival_threshold` (0.8) | derived (see below) | `survival_ratio / threshold`, clamped to 1 |
| `accelerate` | `fitness.accelerate.target_session_seconds` (60) | `session.duration_seconds` | 1 up to the target, then `1 − overshoot/target` — overshoot = failing |

**Balanced is two sub-checks** (skill gap AND spike survival). The result's
`progress` is the *worse* of the two computable sub-checks and it is satisfied
only when every computed sub-check passes; the result is emitted when at least
one sub-check is computable.

**Spike-survival derivation.** There is no direct "survived a spike" signal, so
it is derived from the available per-player signals: a player **survives** when
their `movement_variance ≤ movement_intensity` — erratic swings (variance) that
exceed sustained effort (intensity) indicate a player thrown by spikes. The
`survival_ratio` is `survivors / active players with both signals`; the session
passes when `ratio ≥ spike_survival_threshold`. When no active player has both
signals the sub-check is skipped (the gap is honest, not fabricated).

**How results steer decisions.** In `scoreAndSort`, each candidate's score is
`urgency × weight[objective] × (1 + pressure[objective])`. A satisfied,
unevaluated, or fitness-less (chaos) objective contributes `pressure = 0` (no
change); a maximally failing one contributes `pressure = 1` (up to double the
effective urgency). So a failing endurance fitness amplifies the
endurance-serving candidates and can **flip the selected winner between
objectives** — without ever blocking a candidate outright. The cycle's full
evaluation is retained on the engine (`LastFitness()`) and read back by the loop
into `LayerState.FitnessEvaluated`.

**Runtime tunability.** Because the thresholds are evaluated and published every
cycle, changing a `fitness.*` flag mid-session changes the **next** cycle's
evaluation and the resulting action selection, with no restart (covered by
`TestLoop_MidSessionFlagChangeChangesOutcome`).

## Span schema (#724) — the trace is the audit log

Every evaluation that produces decisions emits one trace (hierarchy always
parent → child):

```
agent.signal_received          one per triggering OTLP Export (backdated to arrival); only without game ids, in a game it is an event on invoke_agent joustmania-agent (#2648)
  ├─ invoke_agent joustmania-agent   the decide span; rules path: sibling of agent.decision
  └─ agent.decision          one per Decision the rules engine returns
       └─ agent.action       one per decision, wrapping the ActionSink call

remote path:
invoke_agent joustmania-agent
  ├─ chat {model}            one per model call (CLIENT)
  └─ agent.decision ─ agent.action
```

In a game the decide span (and, on the rules path, `agent.decision`) is a remote child of `game_cycle` (telemetry.md §4.1).

The root span is **signal-agnostic** (#1053): a metric-triggered cycle and a
trace-triggered cycle both emit `agent.signal_received`, distinguished only by
`otlp.signal` (`metrics`|`traces`) + `rpc.service` — the name never implies
spans-only activity (metrics are in fact the primary trigger; see *Signal
timing*).

**Traces are emitted only when the rules engine returns ≥ 1 decision** —
including decisions that end up *blocked*. Idle evaluations cost no spans (they
are instead counted by `agent_evaluations_total{outcome="idle"}`, #1053).
Blocked actions are recorded (`decision.blocked = true` on both the decision
and action spans, ActionSink **not** called), never silently dropped.

### Decision-span attributes (#724 + #729)

Every `agent.decision` span always carries the **full** schema. Issue #729 lifts
the cycle's entire `LayerState` (every flag evaluated this cycle) onto the span
verbatim, so a single Jaeger trace answers: **which flags were in effect, which
backend decided, which objective was served, why this action, and was it
permitted.** Subsystems that do not exist yet contribute explicit placeholders so
the trace shows its complete shape from day one.

**Cycle-level flag attribution** (lifted from `LayerState`, same on every
decision in the cycle):

| Attribute | Source | Today | Real value arrives with |
|-----------|--------|-------|-------------------------|
| `agent.enabled` | existence flag `enabled` | live string `off` / `watch` / `act` | — |
| `agent.objectives` | objective flag `objectives` | sorted `k=v` weights / `"unset"` | — |
| `decide.backend` | the backend that decided this cycle (only when the decide hook ran) | `"rules"` / `"claude"` / ... | — |
| `interventions.allowed` | permission flag `interventions_allowed` | `"none"` / `a,b,c` | — |
| `policy.battery_threshold` | policy flag | live int | — |
| `policy.max_interventions_per_minute` | policy flag | live int | — |
| `fitness.evaluated` (cycle) | `LayerState.FitnessEvaluated` — dotted per-objective thresholds + results (#731) | live (e.g. `endurance.session_progress=0.5`) | — |
| `gen_ai.agent.name` | agent identity | `"joustmania-agent"` | — |

**Per-decision attribution** (one decision per `agent.decision` span):

| Attribute | Today | Real value arrives with |
|-----------|-------|-------------------------|
| `decision.action` / `decision.reason` | real (rules/probe) | — |
| `decision.objective_served` | the objective the rule served / `"unset"` | — |
| `decision.blocked` | from the permission chain | — |
| `decision.block_reason` | `not_allowed` / `battery_threshold` / `rate_limit` (only when blocked) | — |
| `fitness.evaluated` (per-decision fallback) | the cycle-level evaluation (above) is authoritative; only Probe/Noop engines fall back to the rule's own `Decision.Fitness` | — |

The attribution attaches where the rules and remote decide paths converge
(`decisionAttributes`, fed by the shared `LayerState`). The full decide record
(requested backend, fallback reason, answers, confidence, outcome) is on the
`invoke_agent joustmania-agent` span. #1234 removed `agent.mode`, `agent.model`,
`agent.prompt_variant` and `inference.*` from this span.

#### Kill-switch trace (`agent.disabled`)

When the existence layer reports the agent **off** (`enabled=off`) the loop
short-circuits before any rules run, but still emits a **throttled** kill-switch
trace so "agent off" is visible in Jaeger: a root `agent.signal_received` with a
single `agent.disabled` child (no `agent.action` child — nothing was decided)
carrying the same cycle-level flag attribution above (`agent.enabled=off` and
the permission flags that were in effect). Throttled to one per second
so a disabled agent under heavy signal load does not flood the trace backend.

The in-game LLM prompt capture (`agent.llm.prompt`, #739) and the async
`agent.llm.apply` / `agent.llm.infer` spans were retired with the legacy
`mode=llm` path in #1234; remote decisions are recorded on `invoke_agent joustmania-agent`.

#### Post-game retrospective capture (M4, #844)

When a game **ends** (the `GameActive` true→false transition fires the store's
`OnGameEnd` hook with a pre-reset snapshot), the `decision.RetroCoordinator`
builds the prompt for an **offline analyst** asking for tuning suggestions for
the next game and records it on a dedicated **`agent.llm.retro`** span. With
`inference.backend=claude` (the flag; `stub` while flagd is unresolved; and `ANTHROPIC_API_KEY`) it also calls Claude
directly, asynchronously, and stamps the decoded assessment / suggestions on the
same span (#1179, #1234); with the stub default it is capture-only. It emits
**exactly once per session** (dedup on `SessionID`) and is
structurally isolated from the decision loop: it never touches the gate, the loop,
or the rate limiter, so a retrospective **cannot consume the in-game intervention
budget**.

The span carries the full retro prompt plus attribution (single builder
`retroPromptAttributes`, schema-complete every emission):

| Attribute | Value |
|-----------|-------|
| `gen_ai.*` | none since #2584: the model call is the `chat {model}` child |
| `agent.mode` | `"retro"` |
| `agent.objectives` | sorted `k=v` weights |
| `interventions.allowed` | the allow-list summary |
| `session.id` | the finished session's id |
| `llm.retro.system_sha256` | 16-hex **fingerprint** of the retro system prompt (#1168) — NOT the text; the full text is on the `agent.llm.system_prompt` log line |
| `llm.retro.user` | the retro user prompt text, capped at 32 KiB (a `...[truncated]` marker is appended if it exceeds the cap); only with `decision.capture_content` on |
| `llm.retro.bytes` | `len(system)+len(user)` |
| `inference.configured` | the text backend's model (`""` when capture-only) |
| `inference.used` | `"claude"`, or **`"none"`** when capture-only (see below) |
| `inference.fallback_reason` | `""`, or `"no_backend_available"` when capture-only |

**Divergence from the in-game path:** a capture-only retro reports
`inference.used="none"`, **not** `"rules"`. A retrospective has **no** rules
fallback — nothing runs in place of the analyst at game end — so `"none"` is
the honest value. A failed call records `llm.retro.parse_ok=false` and only the
error **type** (`llm.infer.error`, `error.type`, span status), never the error
text (#1242). The companion log line `agent.llm.retro_captured` carries only metadata
(`session_id`, `model`, `bytes`, `fallback_reason`). The suggestion contract maps
to the **calibration surface** (#766: `global_difficulty_factor`,
`pacing_profile`, `threshold_table`, `objective_variant`), and `replay-prompt.sh`
replays an `agent.llm.retro` span identically — the system text comes from the
`agent.llm.system_prompt` log line keyed by the span's `llm.retro.system_sha256`,
the user text from the span's `llm.retro.user`. See
[docs/research/844-post-game-retro.md](../../docs/research/844-post-game-retro.md).

#### `fitness.evaluated` (#731)

`LayerState.FitnessEvaluated` (`map[string]float64`) holds the cycle-level
fitness evaluation — every threshold **and** every computed result, under dotted
`objective.signal` keys. The loop fills it each cycle by reading the engine's
`LastFitness().Evaluated()` and the existing span wiring lifts it onto the
decision (and disabled) span as the `fitness.evaluated` attribute, rendered as a
sorted `k=v` string slice. When the cycle-level evaluation is present it is
**authoritative** for the attribute; only engines without a fitness function
(Probe/Noop) fall back to the per-decision `Decision.Fitness` view, so the
attribute is always present on the span.

The full key vocabulary recorded on the span:

```
endurance.min_session_seconds   endurance.session_seconds   endurance.session_progress
balanced.max_skill_gap          balanced.skill_gap          balanced.skill_gap_progress
balanced.spike_survival_threshold  balanced.spike_survival_ratio  balanced.spike_survival_progress
accelerate.target_session_seconds  accelerate.session_seconds  accelerate.session_progress
```

Keys for an objective whose signals were missing that cycle are simply absent
(the function was skipped, not fabricated). chaos has no fitness function and so
no keys.

### Intervention-effect feedback (#918) — the game-path measurement loop

Outbound attribution (above) records the agent's *reasoning*; it does not measure
whether an intervention **helped**. #918 adds that **measurement** (it is
measurement only — it never reverts a bad intervention):

1. **At dispatch** (the applied-decision path in `runDecision`) the loop stamps a
   **baseline** of the game's fitness/objective signals — the same dotted-key
   quantities `fitness.evaluated` carries — and tags it with a dispatch-unique
   `decision.intervention_id` (also stamped on the `agent.decision` span, so a Jaeger
   query joins *what the agent did* to *what happened next*).
2. **After a follow-up window** (`DefaultEffectWindow` = 20 s, the live
   `effect.window_seconds` flag since #1788; the compiled default while unresolved) a per-game sampler re-reads the **same** game's
   signals (through the same `ContextProvider` the #917 async path uses, so per-game
   isolation holds) and recomputes the **same** fitness functions with the
   dispatch-time thresholds. When the game ends inside the window, the game-end hook
   closes every open window of that game at once: the follow-up is read at game end
   (#2668).
3. It emits an **effect record** attributed to the intervention id:

   - **Span** `agent.intervention.effect`: one span per intervention (#2640), a
     direct child of the `agent.decision` that dispatched it and backdated to dispatch
     time (no wrapper span, so effect spans never nest; #1325).
     Attributes: `intervention.id`, `intervention.type`, `intervention.objective`,
     `intervention.window_seconds` (the window actually measured),
     `intervention.window_end_reason` (`elapsed`, or `game_ended` when the game ended
     inside the window and the follow-up was read at game end, #2668),
     `intervention.signals` (compact JSON of every
     signal: baseline, followup, delta, or `aborted` for a signal gone at follow-up),
     the objective's primary signal as `intervention.signal`,
     `intervention.fitness_baseline`, `intervention.fitness_followup`,
     `intervention.fitness_delta`, and `intervention.effect_aborted=true` when the
     follow-up could not be measured (the game's partition was gone at follow-up, or
     no signal left).
   - **Metric** `agent_intervention_effect_delta_ratio` (`Float64Histogram`, unit `1`): the
     `follow_up − baseline` change, labeled `intervention.type`,
     `intervention.objective`, `intervention.signal`. No point is recorded for an
     aborted follow-up. Only the 0..1 progress-type signals (`*_progress`, `*_ratio`,
     `balanced.skill_gap`) reach the metric; the elapsed-time (`*_seconds`) and
     constant-threshold keys stay on the spans (#1444).

   The M5 intervention dashboard (#791/#792) plots the intervention → effect pairs, and
   the #844 retro prompt reads per-intervention effect evidence; both consume this
   span + metric.

**Eval-cycle liveness (#1053):** the audit trace is emitted **only** when a rule
fires, so idle cycles (signal arrives, nothing decided) are span-silent. To keep
signal arrival observable without a span per idle cycle, **every** enabled eval
cycle increments the metric `agent_evaluations_total` (`Int64Counter`), labeled
`signal` (`metrics`|`traces`|`logs`) and `outcome` (`decided`|`idle`). The labels are
deliberately tiny (6 series max) — no per-game/per-serial dimensions. Because
metrics are the agent's primary (~100 ms–1 s) trigger, `rate(agent_evaluations_total{signal="metrics"}[1m])`
answers "is the agent receiving metrics?" directly, and the `outcome` split
distinguishes "metrics arrive but trigger nothing" from "metrics aren't arriving"
— the exact ambiguity that made the metric-driven loop look span-driven.

**Deaths (UC-041 BR-008, BR-009):** the agent learns of an elimination only from
`game_player_elimination_order`, which the coordinator sets in every moment shape and only for a
death that takes the player out. It reads no booth moment record: a span carrying `moment.id` is
skipped like `game.shield_charge`, and the `player_death` span event and moment log events change
nothing (telemetry.md appendix A).

**Lifecycle safety (#923 goleak):** each follow-up runs in a goroutine tracked on a
`WaitGroup` (joined by `AwaitInflight` at shutdown) and bounded by the agent root
context. It selects on the follow-up timer **and** `rootCtx.Done()`: on shutdown it
stops the timer and returns without emitting; the in-flight pending set is capped
(`maxPendingEffects`) so a burst of dispatches cannot launch unbounded goroutines.
Measurement is best-effort and never on the hot decision path.

### Semantic conventions

OTel semantic conventions ([semconv v1.34.0](https://pkg.go.dev/go.opentelemetry.io/otel/semconv/v1.34.0))
are used wherever one honestly applies; the GenAI spans follow
`open-telemetry/semantic-conventions-genai` v0.1.0 (`docs/specs/telemetry.md` §4.2):

| Where | Convention |
|-------|-----------|
| `agent.signal_received` | `rpc.system=grpc`, `rpc.service` (OTLP `TraceService`/`MetricsService`/`LogsService`), `rpc.method=Export`; span kind `SERVER` |
| `invoke_agent joustmania-agent` | GenAI *Invoke agent internal span*, GenAI semconv v0.1.0; deviations in telemetry.md §4.2 |
| `chat {model}` | GenAI inference client span (`gen_ai.provider.name`, model, usage tokens; telemetry.md §4.2) |
| `agent.decision` | `gen_ai.agent.name` identity |
| `agent.decision` event | `feature_flag` span event (`feature_flag.key=interventions.allowed`, `feature_flag.provider.name`, `feature_flag.result.variant`) — same shape the openfeature OTel hooks emit in the Python services; provider is `"stub"` until flagd (#725) |
| `agent.action` failures | `span.RecordError` + `error.type` + status `ERROR` |

`decision.*`, `fitness.*`, `agent.mode`, `agent.objectives` and
`interventions.allowed` have no semantic convention and are custom to this
project.

### Self-ingestion safety

The collector's `otlp_http/agent` exporter fans the **agent's own spans back to the
agent**. Two layers prevent a feedback loop:

1. Naturally: the agent's spans carry no recognized game signals, so extraction
   reports "nothing updated" and no evaluation (hence no new span) is triggered.
2. Defense-in-depth: the extractors skip any resource whose `service.name`
   equals the agent's own `OTEL_SERVICE_NAME`.

A collector-side filtered pipeline (excluding the agent from its own fan-out)
is a possible follow-up but not needed today.

## Configuration

The environment variable table (names, defaults, whether `docker-compose.yml` passes them through, purpose) lives in
[docs/specs/services.md](../../docs/specs/services.md#6-per-service-notes) under `### agent`, so there is one
table to keep in sync with the code. Live gates (`enabled`, `experiments_enabled`,
`verdict_*`, `llm.*`) are flagd flags, documented in [flags.md](../../docs/specs/flags.md) and
[feature-flags.md](../../docs/feature-flags.md). The text backend (`inference.*` flags) is specified in
[text-backend.md](../../docs/specs/text-backend.md).

> **#1044 — runtime-tunable experiment knobs are now LIVE `agent.json` flags.**
> The experiment loop reads these on **every tick** from the `agent` flagd domain.
> When a flag is absent or unreadable it falls back to its compiled default (equal
> to the flag default); the two gates fall back to **off**. The env fallbacks were
> removed in #1907 (FR-151 Obsolete): a host `.env` entry for them is ignored. So
> a change takes effect with **no container restart**: through the flag service
> (`scripts/flags.sh set agent <flag> <variant>`) for the flags in its mutable
> registry (`experiments_enabled`, `experiment_dynamic_enabled`,
> `experiment_tick_seconds`, `verdict_min_pairs`), or by a baseline edit plus `scripts/flags.sh restart` for
> the rest. The
> migrated flags (see [`services/flagd/agent.json`](../flagd/agent.json)):
>
> | flag (`agent.json`) | fallback when unresolved |
> |---|---|
> | `experiments_enabled` | off (fails closed) |
> | `experiment_dynamic_enabled` | off (fails closed) |
> | `experiment_tick_seconds` | 30 s (first refill within 5 s after start, `firstTickCap`) |
> | `experiment_dynamic_max_concurrent` | _none — compiled 3; `AGENT_EXPERIMENT_DYNAMIC_MAX_CONCURRENT` is a ceiling, effective = min(env, flag) (#1810, C-034)_ |
> | `experiment_shadow_effective_concurrency` | 4 (`DefaultEffectiveConcurrency`) |
> | `experiment_completed_grace_seconds` (#1810) | 60 s (flag 0 = off) |
> | `experiment_proposer_enabled` (#1810) | _none — reads false when unresolved; ANDed with the `AGENT_EXPERIMENT_PROPOSER_ENABLED` ceiling for the M7-4 proposer (C-034)_ |
> | `verdict_min_pairs` | 5 (`DefaultMinPairs`) |
> | `verdict_effect_threshold` (#1214) | _none — `DefaultEffectThreshold` (0.5)_ |
> | `verdict_min_raw_effect` (#1214) | _none — `DefaultMinRawEffect` (0.02)_ |
> | `verdict_sd_floor` (#1214) | _none — `DefaultSDFloor` (1e-4)_ |
>
> **Startup-gate inversion:** the loop + dynamic declarer are now **always built and
> run**; they self-gate each tick on the live `experiments_enabled` /
> `experiment_dynamic_enabled` flags. An off→on flip **starts** the loop (it runs the
> deferred rehydrate and begins allocating); an on→off flip **stops**
> it cleanly (`AbortAll` clears the experiment-arm values and frees capacity — in-flight shadow games
> conclude/release normally). The master `enabled` flag still wins: experiments write
> experiment-arm values into game flags, so they run only while `enabled` is `act`; in
> `watch` or `off` the loop aborts them regardless of these flags. **The `agent` set is human-owned** —
> the agent has **no** write path to it, so it can never enable its own experiments.
> The agent writes two flag sets, both through the flag service (epic #1407):
> `interventions` (game-scoped in-game interventions, `actions/writer.go`) and `game`
> (experiment-arm values via `experiment.Writer`, plus a `real` overlay on gated
> autonomous promotion). It must never write the `agent` set. The guards are in the client code:
> `experiment/gate.go` rejects a proposal that names another set
> (`non_game_target`), the promotion writer's set is always `game` (#1293), and the
> intervention writer uses a fixed set. The agent mounts no flag file;
> only the flag service mounts the baseline directory, read-only.
>
> **Stays env (NOT migrated):** `AGENT_EXPERIMENT_DIR` (filesystem concern). The
> operator names an experiment as the dynamic declarer's explicit candidate
> (`experiment_dynamic_candidates` = `flag=variant`, UC-043 BR-012).
>
> **#1907 — dynamic declarer settings are flags.** `experiment_dynamic_candidates`,
> `experiment_dynamic_denylist` and `experiment_dynamic_objective`
> replace the `AGENT_EXPERIMENT_DYNAMIC_*` env vars
> (only `AGENT_EXPERIMENT_DYNAMIC_MAX_CONCURRENT` stays, a C-034 ceiling). The two
> lists are comma-separated **strings** read with `StringValue`, so the flagd
> list-flag trap does not apply. They are read every experiment tick. The compiled
> `defaultDynamicDenylist` is a floor: the denylist flag only adds keys, and the
> floor also filters an explicit candidate list (UC-070 BR-008).
>
> **#1214 — verdict-tuning cluster completed.** The verdict knobs
> (`verdict_effect_threshold`, `verdict_min_raw_effect`, `verdict_sd_floor`) are live
> `agent.json` flags next to `verdict_min_pairs` (#1044); their env vars are **removed**; the verdict's `Default*` constants are the
> fail-open fallback the flag resolves to when flagd is undefined/unreachable. They are
> read via the **typed float getter** (never `ObjectValue` — the #903/#1127
> TYPE_MISMATCH trap) and live-mirrored onto the verdict each tick via the registry's
> `Reconfigure` → `Verdict.SetThresholds` / `SetFloatThresholds`. The paired d_z is the
> only verdict (UC-043 step 5); `verdict_min_n`, `verdict_anchor_margin`,
> `experiment_max_games` and `experiment_dynamic_target_n` were removed (#2811).

> Since #1713 the Go agent uses the flagd **in-process** resolver on the sync port
> `8015`, like the Python services: selectors `flagSetId=agent` and (read-only,
> for the shield flags) `flagSetId=game`. `flags/synced_provider.go` waits for
> flagd's first sync however late it is, and every read fails closed until then and
> while the sync stream is lost ([flags.md §1.1](../../docs/specs/flags.md#11-clients)).

### Decision backends (#1225, #1244)

`decision.backend` (agent.json) picks who answers the in-game decide questions:
`rules` (default, in-process) or a remote backend, `claude` or `jev`,
called directly with no gateway. Any other value (for example `openai`,
retired in #2788) falls back to rules with `unknown_backend`. Flip it, and the backend's model, with
`./scripts/agent-backend.sh` (a `stage` overlay through the flag service, applied
live on the next cycle with no restart; a flag-service restart drops it):

```bash
./scripts/agent-backend.sh                 # show current settings
./scripts/agent-backend.sh claude sonnet   # decision.backend=claude, decision.claude.model=sonnet
./scripts/agent-backend.sh rules           # back to rules
```

Each remote backend needs its API key in the agent's environment (passed through
by `docker-compose.yml`, read once at startup) plus its flag config:

| Backend | Env key | Flags it also needs |
|---------|---------|---------------------|
| `claude` | `ANTHROPIC_API_KEY` | `decision.claude.model` (defaults to haiku), `decision.claude.base_url` (defaults to `https://api.anthropic.com`, no `/v1`; must be key-safe) |
| `jev` | `JEV_API_KEY` | none: `decision.jev.base_url` defaults to `https://api.typesafe.ai/v1`, `decision.jev.model` to `latest` (`jev-latest`; `pinned` = `jev-1.13.0`) |

**Where to put the keys.** Put API keys in your shell environment or in the
git-ignored `.env.local`, **never in `.env`**: `.env` is tracked in git (despite
`.gitignore`) and compose reads it automatically. Nothing loads `.env.local`
implicitly; pass it explicitly, together with `.env` (an explicit `--env-file`
disables the implicit `.env`, and later files win):

```bash
docker compose --env-file .env --env-file .env.local up -d agent
```

Compose passes the three variables through from the shell too, so a shell or
devcontainer that already exports `ANTHROPIC_API_KEY` hands it to the agent, and
`./scripts/agent-backend.sh claude` then spends that key.

A backend without its key, model or base URL resolves to `backend_unavailable`
and the decision falls back to rules (`decide.fallback_reason` on the
`invoke_agent joustmania-agent` span), as do timeouts (`decision.<backend>.timeout_ms`), errors and
invalid answers. Keys are never flags and never land on spans; backend errors are
recorded only as `error.type`. Wiring: `decide_backends.go`.

### Experiment cohort loop dry run (#982/#991/#999)

`make dry-run` brings the cohort loop up **runnable out-of-the-box** — latest
images, mock controllers, full observability, the loop enabled, and one seeded
experiment. See the [dry-run runbook](../../docs/agent-dry-run-runbook.md) for
the operator steps and the silent-failure traps it avoids (stale `IMAGE_TAG`,
and the kill-switch default; the former agent↔flagd flag-dir mismatch is gone
since the agent writes through the flag service).

The loop ACTS only when **three independent gates** are all satisfied — kept
separate so a dry run can spawn shadow games without ever risking a real-default
change:

1. **Experiments opt-in** — the `agent` flag `experiments_enabled` = `on`
   (read live each tick; `off` and an unresolved flag are closed, #1907). The
   loop is always built; while the gate is closed it runs no declare, spawn,
   experiment-arm write or promotion.
2. **Master kill-switch** — `agent` set `enabled` = `act` (read live each
   cycle; `off` is the fail-closed fallback). When `off` the decision loop
   short-circuits with a throttled `agent.disabled` span (`decision/telemetry.go`
   `SpanDisabled`), and the experiment loop aborts running experiments with the
   teardown reason `kill-switch: agent disabled` (in `watch`: `agent watch: arm
   writes need enabled=act`). Flip it with
   `./scripts/agent-killswitch.sh act` (an in-memory overlay through the flag
   service) — the baseline stays `watch` (`off` in CI).
3. **`code_improvement.*` promotion gates** — separate flags that gate the
   **real-default promotion** action only (still subject to the invariant gate +
   kill-switch). They stay at defaults in the dry run, so a concluded experiment
   yields a **verdict** but does **not** set a `real` promotion overlay.

### Probe mode (`decision.probe_decisions`)

The flag `decision.probe_decisions` (agent set, default off; off while
flagd is unreachable; the env var was removed, #1907) swaps the rules engine for
`ProbeRules` for the next decision cycle after a flip (no restart); pattern games
always keep the real engine. `ProbeRules` emits
one synthetic `noop` decision at most every 5 s. Its only purpose is to drive a
**full audit trace** (`agent.signal_received → agent.decision → agent.action`)
without waiting for a live game to trigger the real rules — invaluable for
verifying the trace pipeline end to end.

**With the default `interventions_allowed` variant, probe decisions are blocked
by design.** `noop` is not in the `ambient`/`standard`/`full` allow-lists, so the
permission layer blocks every probe decision with `decision.blocked=true`,
`decision.block_reason=not_allowed`. This is still useful: the
`agent.signal_received → agent.decision` spans are emitted (a blocked decision is
traced, not dropped), so the OBSERVE→DECIDE path and the span schema are fully
exercised — only the `agent.action` dispatch is withheld.

**To make probe decisions dispatch** (exercise the *whole* path including
`agent.action`), flip `interventions_allowed` to the dedicated **`probe`**
variant (`"noop"`) through the flag service:

```bash
scripts/flags.sh set agent interventions_allowed probe
```

`noop` is a **harmless no-op in the action sink**: the `Writer` recognizes it and
returns success **without writing any flag** (`actions/writer.go`,
`InterventionNoop`). So with the `probe` variant + `enabled=act`, a probe decision flows through allow-list, battery, and rate-limit
gates, reaches the real `Writer`, and completes the `agent.action` span — all
without touching the game. Revert with `scripts/flags.sh clear agent
interventions_allowed` (back to the baseline `ambient`) or set `none`.

### Lifecycle flags — live via a 1 s reload (#766 F5, #927, #1709)

The agent's lifecycle and throttle calibration values live in the flagd `agent`
domain ([`services/flagd/agent.json`](../flagd/agent.json)). Like the four
decision-cycle flag layers above they are **live**, but via a different
mechanism. #766 F5 originally read them once at startup, so retuning the store
TTLs / eviction cadence / throttle needed a restart. #927 made them live through
the OpenFeature configuration-change listener (`openfeature.ProviderConfigChange`).
Since #1713 the in-process resolver emits that event on every flag sync, and
`LifecycleHolder.Poll` (#1709) re-evaluates these four flags every
`LifecycleReloadInterval` (1 s) as the safety net, so a lost or never-started
event path cannot freeze them (#1709: such a path once kept the kill switch
stale). Both write a shared, concurrency-safe `flags.LifecycleHolder`
([`flags/holder.go`](flags/holder.go)). The consumers read the
holder on their hot path (the GameContext store reads the TTLs at eviction time,
`main.go`'s eviction ticker re-reads (and `Reset`s to) the interval each loop,
and the decision loop reads the throttle in `shouldLog()`), so **changing one
takes effect within about a second, with no agent restart** (a holder read is a
lock-free atomic load). The holder is **primed once at startup**; if flagd is not
yet reachable each value falls back to its safe default, which reproduces the
former hardcoded constant exactly.

| Flag | Default | Configures |
|------|---------|------------|
| `lifecycle.player_ttl_seconds` | `5` | How long a silent player is retained before eviction |
| `lifecycle.session_grace_seconds` | `15` | How long an ended session lingers before its session-scoped state resets |
| `lifecycle.evict_interval_seconds` | `1` | How often the eviction ticker fires |
| `decision.throttle_seconds` | `1` | How often the `agent.evaluate` log line and the `agent.disabled` span are emitted |

A non-positive value for any of these falls back to its default (a zero TTL or
ticker interval would be unsafe), both at startup and on every reload, so a
transient bad flag read can never collapse a TTL.

These four flags are not in the flag service's mutable registry: a change is a
reviewed edit of the baseline `services/flagd/agent.json` plus `scripts/flags.sh
restart`; the agent picks the new values up from the config-change event.

## Action sink (#730) — applying decisions as flag writes

The agent **never calls the game services over gRPC**. It applies a decision as
a write to the flag service's HTTP API (`FLAG_SERVICE_URL`, epic #1407,
[services.md §7](../../docs/specs/services.md#7-flag-service-epic-1407));
the flag service renders the value as targeting on the deciding game and pushes
the merged configuration to flagd over its gRPC sync stream, flagd fires
`PROVIDER_CONFIGURATION_CHANGED`, and the game coordinator re-evaluates the
intervention flags with its `gameId` (see
`docs/research/722-intervention-surface.md` §8). The agent writes no flag file;
the baseline `services/flagd/interventions.json` holds only the neutral values.

### Transport / write semantics

- **One HTTP call per decision**, 500 ms deadline, fails closed. The flag service
  validates the write against its mutable-flag registry
  ([flags.md §13.1](../../docs/specs/flags.md#13-mutable-flag-registry)) and
  answers `422 out_of_bounds` / `flag_read_only`, `409 game_not_live` (game not
  registered or already ended; the `Writer` retries once after 250 ms) or
  `503 no_sync_stream` (flagd not connected). A rejected write changes nothing and
  is visible as `flagservice_writes_total{result="rejected"}` and a
  `flagservice.write` span.
- **Game-scoped.** Every write carries `scope.game_id`; a game's values and events
  die with the game when the coordinator ends it (UC-053 BR-003), so nothing has
  to be reverted or pruned between games.

### Flag shapes (game-side reader contract)

- **One-shot events** (`audio_cue`, `controller_effect`, `eliminate_player`,
  `revive_player`, `end_game`): `POST /interventions/<flag>` with
  `scope.game_id` appends an event to the game's event list
  (`{"epoch","seq","events"}`, last 16 events). The coordinator applies every
  event with a `seq` above the last one it applied, once. Payloads: eliminate /
  revive = `{"serial"}`; `audio_cue` = `{"sound"}`; `controller_effect` =
  `{"serial","effect"}` (empty serial = broadcast); `end_game` = `{}`.
- **Game state** (`music_tempo_override`, `volume_override`,
  `global_sensitivity_override`, `global_difficulty_factor`, `pacing_profile`,
  `tempo_ramp`, `auto_rubberband`): `PUT /interventions/<flag>` with
  `scope.game_id` and the typed value; reverting is `DELETE
  /interventions/<flag>?game_id=<id>`, and the game resolves the neutral
  baseline again.
- **Per-player levels and grants** (`player_sensitivity_factor`,
  `player_handicap_factor`; `shield_seconds`, `toughness_boost_seconds`,
  `soft_penalty_action`): `PUT` with `scope.game_id` + `scope.serial`; the flag
  service renders a rung on `gameId` + `targetingKey`. Every write gets a fresh
  variant, so a re-grant is a change the coordinator applies once (UC-040 BR-012).

### Decision value contract

The rules engine (#726) leaves `Decision.Value` empty, so the Writer supplies
per-type defaults: audio cue `agent_cue`, controller effect `rumble`, music tempo
`1.15`, volume `0.7`, global sensitivity `2`, player sensitivity `1.5`, shield
`5`. An explicit `Decision.Value` (sound id / effect name / numeric target as a
decimal string) overrides the default.

| Property | Value |
|----------|-------|
| **OTLP ports** | 4318 OTLP/HTTP (the collector's `otlp_http/agent`), 4317 OTLP gRPC |
| **Health port** | 13134 |
| **Health check** | `GET /healthz` |

## Shadow-game runner (#778)

The `gamerunner/` package launches **mock-only "shadow" games** on the live
stack: games fielded entirely with RESERVED mock controllers (#777), so they
never appear in the menu lobby and never collide with a real, menu-driven game.
Unlike the action sink (#730), which acts through flag-service writes, game-start
has no flag representation — it is a **direct gRPC** capability, and these are
the agent's first gRPC clients toward the game stack.

Every shadow game is a **pattern game** (UC-046; maintainer decision on #1258:
experiments are archetype-only). `RunShadowGame(ctx, spec)` and
`StartExperimentGame` refuse a spec without a motion archetype, so no random-motion
or scripted-kill game can be started from the agent.

1. `AddControllers(count=players, reserved=true, tag="agent:<runID>",
   archetype=<name>, archetype_params=<canonical JSON>)` → serials; slot i drives
   the i-th serial with intensity_i(k / r).
2. `StreamGameEvents(StartGameConfig{...})` — headless start (no lobby) with the
   pair's `rng_seed`; the `game_id` is captured from the first stamped event.
3. No drive: players die organically. The runner records `player_death`,
   `game_winner` and `game_tie` events for the elimination order, and checks once
   per second whether another game is live (non-exact `concurrent_game`).
4. Awaits a terminal event; at the safety cap (`experiment.max_game_seconds`,
   default 300 s) calls `ForceEndGame{game_id}`: outcome `timeout`, a valid,
   right-censored sample. A force-end the agent did not request (a real game
   preempting shadows) is an abort.
5. ALWAYS removes its reserved controllers (deferred) and builds the replay record
   from their `RemoveController` answers.

`SweepOrphans(ctx, "agent:")` is the crash backstop: it removes reserved
controllers whose tag has the prefix but which no live game (`ListGames`)
references. The env trigger sweeps before each run.

A run gets an `agent.shadow_game.run` span with `run_id`, `mode`, `game_id`,
`outcome`, `terminal_event`, and `events_seen` attributes.

### Trigger (env, one-shot)

The trigger is intentionally minimal — one env-gated path. It mirrors the
original env-gate shape the actuation gate used before **#1213** migrated it to
a LIVE `agent.json` flag;
`AGENT_SHADOW_GAME` is a one-shot startup trigger, so it stays an env var. When
`AGENT_SHADOW_GAME=true`, the agent sweeps orphans then runs ONE shadow game at
startup, then continues its normal observe loop.

| Env var | Default | Purpose |
|---------|---------|---------|
| `AGENT_SHADOW_GAME` | `false` | Enable the one-shot shadow game |
| `SHADOW_GAME_MODE` | `JoustFFA` | Game mode |
| `SHADOW_GAME_SENSITIVITY` | `2` | Death sensitivity 0-4 |
| `SHADOW_GAME_ARCHETYPE` | first of `experiment.mock_archetypes` | Archetype of the one-shot game (required: the runner refuses without one) |
| `SHADOW_GAME_SEED` | FNV-1a/64 of the archetype name | Game seed, for replays (`scripts/pattern-replay-check.sh`) |
| `FLAGD_OFREP_URL` | `http://<FLAGD_HOST>:8016` | flagd OFREP endpoint for the pattern flag digest (UC-046 BR-011) |
| `GAME_COORDINATOR_ADDR` | `game-coordinator:50053` | Coordinator gRPC |
| `MOCK_CONTROLLER_ADDR` | `controller-manager:50062` | Mock control gRPC |

The Go stubs for the two services live committed under `gen/` (generated by
`make protos-agent` — self-contained so `go test` and the Dockerfile build with
no buf step).

## Files

```
services/agent/
├── Dockerfile      # Two-stage: cross-compiled static Go build, alpine runtime
├── main.go         # Wiring: config, OTLP receiver, health server, lifecycle, shadow-game trigger
├── otel.go         # OTLP/self-telemetry setup
├── receiver.go     # OTLP span/metric ingestion + extraction into GameContext
├── gamecontext/    # GameContext accumulation, session identity, eviction
├── gate/           # should_evaluate gating logic
├── flags/          # OpenFeature/flagd four-layer control flags (existence, objective, capability, permission)
├── actions/        # Action sink (#730): game-scoped intervention writes to the flag service
├── decision/       # Decision loop + LayerState + rate limiter + rules engine (#726) + ActionSink interface
├── gamerunner/     # Shadow-game runner (#778): mock-only games driven via direct gRPC
├── gen/            # Committed grpc-go stubs for GameCoordinator + MockController (make protos-agent)
├── go.mod
└── go.sum
```

## Running tests

```bash
go test -race ./...
```

## Local smoke test

With the compose stack up (Agent reachable as `agent:4317` on the `joustmania`
network), push synthetic metrics with
[telemetrygen](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/cmd/telemetrygen):

```bash
docker run --rm --network joustmania \
  ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:latest \
  metrics --otlp-insecure --otlp-endpoint agent:4317 --duration 5s
```

## See Also

- [Intervention Surface research](../../docs/research/722-intervention-surface.md)
- [Ownership Model](../../docs/OWNERSHIP_MODEL.md) -- human/agent settings arbitration & composition rules (#814/#820)
- [OTel Collector config](../otel-collector/) -- defines the `otlp_http/agent` exporter
- [Services spec](../../docs/specs/services.md)
