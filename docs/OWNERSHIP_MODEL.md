# Human / Agent Settings Ownership & Arbitration Model

The contract for how the human operator and the adaptive agent share control of the system: the ownership
principle, the invariants that keep the agent inside it, and the composition of the runtime parameters both
touch. It states the rules once; the lists live elsewhere and are linked:

- who writes each runtime-mutable flag, in which scope and within which bounds: the Writers column of the
  mutable-flag registry, [flags.md §13](specs/flags.md#13-mutable-flag-registry), the canonical place it is stated;
- the owner and lifetime of each flag set: [flags.md §2](specs/flags.md#2-ownership-by-flag-set);
- the invalidation rule and the visibility contract: [flags.md §14](specs/flags.md#14-ownership-composition-invalidation-and-visibility);
- the agent-limiting settings and their per-setting ceilings: FR-152 in [requirements](requirements.md).

Where this document and the code disagree, one of them is a bug; fix both in the same PR. The section numbers
are stable because code comments cite them.

## 1. The Ownership Principle

> **The human admin owns pre-game intent. The agent owns bounded in-game deltas.**

- **Human admin** decides *what game is played and how it is calibrated*: the game mode, the baseline
  settings, when the game starts, and the agent's own policy and limits. Human writes reach the flag service
  from the menu (the operator pad, [UC-015](use_cases/UC-015.md)), the operator and stage scripts and
  Flagsmith ([UC-054](use_cases/UC-054.md)).
- **Agent** decides *how to nudge the experience while the game runs*: bounded overrides, factors, shields
  and one-shot events in the `interventions` set, scoped to one game, which the game coordinator's
  `InterventionManager` evaluates live with its `gameId`.

No process writes a flag file: the files in `services/flagd/` are the read-only baseline (C-033), every
runtime write is an HTTP call to the flag service, and only a flag with a registry row changes, within its
bounds and scope ([flags.md §13](specs/flags.md#13-mutable-flag-registry)).

Three invariants follow and are **load-bearing**:

1. **The agent never persists a human baseline it was not allowed to change.** Its in-game write surface is
   the `interventions` set, always game-scoped; the flag service drops a game's values when the game ends
   (UC-053 BR-003). The agent never writes the `user` or `agent` set and never passes `"persist": true`. Two
   exceptions write the `game` set, both tier 2 ([NORTHSTAR](NORTHSTAR.md)):
   - **Shadow-scoped experiment arms** of the experiment catalog (flags.md §13.3), which never resolve for a
     real game (FR-062, FR-138).
   - **Gated autonomous promotion**: a shadow-validated winner becomes an in-memory `real` overlay that a person
     or the kill switch reverts (FR-065), only when the FR-152 (a) gates are open; off in the stock stack. The durable path
     is a human change made from the promotion's evidence.
2. **The write surfaces are disjoint by scope.** Human writes are persisted admin values or stack overlays;
   agent writes are game-scoped `interventions` values, experiment-arm values and the `real` overlay. The flag
   service holds each kind of value separately per flag and serialises every write, so an agent write never
   overwrites a human one. The contested ground is therefore not the flag sets but the runtime state both
   paths converge on inside the game object; §4 defines those compositions.
3. **The agent cannot loosen its own limits (C-034).** A setting that limits the agent is never a flag the
   agent writes:
   - The agent never writes the `agent` set. Every agent-limiting setting of FR-152 is in `agent`: `enabled`, `code_improvement.*`, `llm.max_requests_per_minute`, `decision.claude.max_requests_per_minute`, `experiment_proposer_enabled` and `experiment_dynamic_max_concurrent`. The
     agent's governor is there too (`interventions_allowed`, `interventions_enabled`, `objectives`, the
     `policy.*` budgets, `decision.magnitude_bounds`).
   - In the sets it does write it writes only the `interventions` keys (game-scoped) and the `game`
     experiment-catalog keys (experiment+arm, or a `real` promotion), never stack-wide. Every other key of
     those sets is human-only, including the ones that bound the agent (`shadow_policy`, `max_shadow_games`,
     `max_concurrent_games`). A registry range on an agent-writable key is the human ceiling.
   - Each agent-limiting setting with an environment variable takes that variable as the operator's ceiling;
     a human-owned flag may only tighten it (effective value = the more restrictive of the two; the
     per-setting rules are FR-152). A missing flag or a lost sync resolves to the flag's code default, which
     is closed for the gates. A setting that only widens what the agent may do and has no safe flag form is
     env only (the substrate attestation of FR-152 (g)).
   - `decision.magnitude_bounds` moves an agent range only inside the hard limits that stay in code
     (FR-098).

   **Enforcement.** The shipped compose files leave the environment ceilings unset, so there the guarantee
   rests on the agent never writing the `agent` set. The flag service has no roles or tokens (local party game, architecture note D11). Its
   registry refuses the loosening `game` writes (`shadow_policy` takes only a stack overlay to `block`; the
   capacity keys are not mutable) and refuses every HTTP write of a Flagsmith-only row (`403 flagsmith_only`,
   flags.md §13). For the rest of the `agent` set the guarantee is the agent's client code, which writes fixed
   flag sets only, held by two tests: `services/agent/agent_set_never_written_c034_test.go` (every agent
   writer against a recording flag service) and `services/agent/agent_set_writers_guard_c034_test.go` (no new
   write call site or set argument). Every write is visible as a `flagservice.write` span with its writer.

## 2. Flag Domain Ownership

Owner and lifetime per flag set: [flags.md §2](specs/flags.md#2-ownership-by-flag-set). Writers per flag:
[flags.md §13](specs/flags.md#13-mutable-flag-registry). The human owns agent policy (§1, §6.2).

## 3. Parameter Ownership Table

The human's game settings and when each is read: [flags.md §4](specs/flags.md#4-game). The agent's live
levers, their kinds and bounds: [flags.md §13.1](specs/flags.md#131-interventions-all-writes-are-game-scoped).
Only two runtime parameters are touched by both; §4 composes them. Every other human setting has no agent
override, and every other agent lever has no human analogue.

### 3.1 Device-settable flags

The settings the host changes from the operator pad: [UC-015](use_cases/UC-015.md). Every other `game` flag
changes through Flagsmith, an operator script (registry rows only) or a reviewed baseline edit.

## 4. Composition Rules

These are the only two parameters both actors touch. Each gets a defined,
implementable composition rule.

### 4.1 Sensitivity

**Runtime state.** The game keeps two layers
([`base.py`](../services/game_coordinator/games/base.py), "Sensitivity arbitration"):
`baseline_sensitivity` (the human's value) and `sensitivity_override` (the agent's
layer, `None` when inactive). `game.sensitivity` is a read-only-by-agent property
returning the **effective** level (override if active, else baseline); this is
what `_compute_effective_thresholds` reads every frame. `game.configured_sensitivity`
is a game-start snapshot kept for telemetry only and is **not** the restore target.

The agent's `global_sensitivity_override` handler
([`difficulty_handlers.py:handle_global_sensitivity_override`](../services/game_coordinator/difficulty_handlers.py))
sets the override layer (`apply_sensitivity_override`) and never touches the
baseline. On clear (value -1) it drops the layer (`clear_sensitivity_override`), so
the effective level returns to the **current** baseline, not the start-of-game value.

**Rule (as implemented).**

1. The human baseline is **authoritative**; the agent override is a **transient
   delta** composed on top of it, never a new baseline.
2. **Restore-on-clear targets the current human baseline** (`baseline_sensitivity`).
3. **Invalidation (see section 5).** Assigning `game.sensitivity` is the admin path
   and routes to `set_baseline_sensitivity`, which sets the baseline and drops any
   active override, so the new human value is effective immediately. The agent may
   re-propose against the new baseline on its next decision.
4. The `player_sensitivity_factor` and `global_difficulty_factor` levers are pure
   **multiplicative deltas** in `_compute_effective_thresholds` (the combined factor
   is clamped to [0.5, 2.0], a floor only a per-player sensitivity factor below 0.5 lowers, to at most 0.2) and do **not** touch the human-owned baseline. They are
   the **preferred** agent levers because they compose without contention.

Note: `set_baseline_sensitivity` is the game-object API for a human change. No
coordinator code path outside it currently pushes a mid-game flag edit into it, so
in practice the baseline is the lobby value frozen at game start.


### 4.2 Grace / Invincibility / Shield

There are **three** distinct grace mechanisms, and they must not be conflated:

| Mechanism | Owner | State field | Set by |
|---|---|---|---|
| Spawn / post-death grace | Game logic | `player.grace_until` (`BaseGameMode`) | Death respawn, spawn protection |
| Round-start invincibility | Human baseline | `player.invincible_until` (Fight Club / Tournament only) | `invincibility_seconds` -> `_invincibility_duration`, applied at round/match start ([`fight_club.py`](../services/game_coordinator/games/fight_club.py), [`tournament.py`](../services/game_coordinator/games/tournament.py)) |
| Agent shield | Agent | `player.grace_until` (`BaseGameMode`) | `shield_seconds` -> `grant_shield` ([`shields.py`](../services/game_coordinator/games/shields.py)) |

Human invincibility uses a **mode-local** field (`invincible_until`), while the agent
shield extends the **mode-agnostic** `grace_until` on the base class. They are
composed by one helper, `BaseGameMode.effective_grace_until(player)`, with
`is_in_grace(player, now)` on top of it. The base per-frame gate and the Fight Club
and Tournament kill paths all go through it.

**Rule (as implemented).**

1. **Effective grace = the max of all active grace sources**:
   `effective_grace_until = max(grace_until, invincible_until)`; a player is
   protected while `now` is before it. Protection is **monotonic-extend,
   never-shorten**: `grant_shield` only extends `grace_until` and does not shorten a
   longer window already in force (it compares against the effective deadline).
2. **Human invincibility is the floor; the agent shield is additive on top.** The
   agent may extend protection beyond the human-configured window but cannot
   reduce it below the human baseline.
3. **The agent shield never shortens or revokes human invincibility.** Reverting
   `shield_seconds` to neutral (0) is a **no-op**: existing protection expires
   naturally ([`lifecycle_handlers.py:handle_shield_seconds`](../services/game_coordinator/lifecycle_handlers.py)).
   Nothing on the agent path clears `invincible_until`.


## 5. The Invalidation Rule

> **A human baseline change invalidates active agent overrides for that
> parameter.**

The normative statement lives in
[flags.md section 14.1](specs/flags.md#141-the-invalidation-rule). In short: the
human's new value wins immediately, the invalidation is per-parameter, and the
agent may re-propose afterwards. The sensitivity implementation is described in
section 4.1.

## 6. Visibility & Kill-Switch Contract

### 6.1 Visibility

The normative contract and the on-device agent cue live in
[flags.md section 14.2](specs/flags.md#142-the-visibility-contract).

### 6.2 Kill-switch / policy control

The operator stops, resumes and scopes the agent on the device, without a terminal
([UC-015](use_cases/UC-015.md) BR-002 to BR-004, BR-019; the keys in flags.md §13.2). The control is
**load-bearing at the gate**: the coordinator's allow-list (FR-050) rejects every agent intervention the
policy does not allow, whatever the agent process does, so the switch degrades safely. The guardrails
constrain the agent only; the human write path has no policy layer, by design.

## See Also

- [Services spec](specs/services.md)
- [Intervention Surface research](research/722-intervention-surface.md)
- [Agent README](../services/agent/README.md)
- [Feature flags](feature-flags.md)
