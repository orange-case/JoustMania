# Flag service migration (history)

> **Historical.** The writer and reader inventory, the migration plan, the alternatives and the superseded issues of the
> file-to-flag-service move (epic #1407, delivered in #1417). Nothing here is a current contract: who writes each
> runtime-mutable flag today is the Writers column of [flags.md §13](../specs/flags.md#13-mutable-flag-registry); the
> design of the flag service is [architecture/flag-service.md](../architecture/flag-service.md).

## 3. Today: the writer and reader inventory

Historical inventory (pre-migration). #2784 removed W2, W7 and R1 with the rollout and canary, and `tests/service/test_rollout_plumbing.py`.

Every writer below does a read-modify-write **in place** (truncate and write; temp+rename fails with
EBUSY on the bind mount). Locks are per process or per writer instance; there is no cross-process lock.

| # | Writer | Code | File | Flags | Mechanism | Trigger | Game-scoped? |
|---|---|---|---|---|---|---|---|
| W1 | Agent intervention sink | `services/agent/actions/writer.go` (`Apply` :249, `edit` :292, `writeInPlace` :745), `nonce.go`, `heal.go`, `gated.go` | `interventions.json` | edge: `eliminate_player`, `revive_player`, `audio_cue`, `controller_effect`, `end_game` (`<nonce>:<payload>` in variant `active`); state: `music_tempo_override`, `volume_override`, `global_sensitivity_override`, `global_difficulty_factor`, `pacing_profile`, `tempo_ramp`, `auto_rubberband`; level: `player_sensitivity_factor`, `player_handicap_factor` (`agent_<serial>` + if-ladder on `targetingKey`); grant: `shield_seconds`, `toughness_boost_seconds`, `soft_penalty_action` (`agent_<serial>#<nonce>`) | in place, re-parse after write, restore last good bytes; startup self-heal writes the embedded neutral file | each decision, gated by `interventions_enabled` | only `end_game` (payload); the rest per serial or global (#1269) |
| W2 | Agent rollout writer | `services/agent/actions/rollout_writer.go` (`SetControllerCount` :97) | `rollout.json` | `current_controller_count` | in place, own mutex | infra loop, gated by `rollout_enabled` | no |
| W3 | Agent experiment writer | `services/agent/experiment/writer.go` (`Apply` :70, `Strip` :91), `targeting.go`, `gate.go` | `game.json` | variant `agent_experiment__<id>` plus a chained if on `experiment_id`/`arm`; the proposer path writes the legacy unscoped `agent_experiment` with `game_kind != "real"`, which nothing strips | in place, `O_NOFOLLOW`; **two Writer instances on one file** (`experiment_loop.go:280`, `main.go:801`) | experiment start / teardown / proposer | by experiment, never by game |
| W4 | Dynamic declarer | `services/agent/dynamic_declarer.go` | none | none ("performs NO writes", :189); it feeds W3 | n/a | experiment tick | n/a |
| W5 | Autonomous promotion | `services/agent/promote/realdefault.go` (`SetRealDefault` :157, `RevertRealDefault` :285) | any file `AGENT_REAL_DEFAULT_FLAG_PATH` names (unchecked, #1293) | the real `defaultVariant` (or a new `promoted` variant); prior state snapshotted in `real_default_snapshots.json`; revert error cooldown from #1065 | in place, `O_NOFOLLOW` | promote verdict, gated by env (off in the stock stack) | no |
| W6 | Menu admin | `lib/flag_config_writer.py` (`update_default_variant` :35, `cycle_variant` :215), `services/menu/menu_setup.py:build_menu`, `handlers/admin.py` | `user.json`, `game.json`, `agent.json` | `current_game`, `game_instructions`; `sensitivity`, `num_teams`, `force_all_start`; `interventions_allowed` | in place, no lock | host in admin mode, game selection | no |
| W7 | connect-proxy | `canary.go` (`setCanaryRouting` :103) | `controller.json` | `bluetooth_backend` (`defaultVariant` or `fractional`) | typed remarshal (drops unknown keys), `os.WriteFile`; in CI it writes the base dir while flagd serves `ci/` | operator HTTP POST | no |
| W8 | Stage and operator scripts | `scripts/talk-stage.sh` (`set`, `reset`, `permissions`), `scripts/agent-backend.sh`, `scripts/agent-dryrun-enable.sh`, `scripts/agent-killswitch.sh` | `ci/agent.json` (default), `talk-stage.sh reset` also `interventions.json` | `decision.*`, `enabled`, `experiments_enabled`, `experiment_seed` (`agent-dryrun-enable.sh`, #1907), `interventions_allowed`, the stage baseline (`scripts/talk-baseline.json`) | Python heredoc, in place | presenter / operator | no |
| W9 | Demo validation harness | removed in #1322 (`tools/demo/validation_harness.py` deleted; nothing ran it) | none | none | n/a | n/a | n/a |
| W10 | Integration fixtures | `tests/integration/helpers.py` (`intervention_env`, `set_interventions_allowed`), `test_parallel_intervention_flow.py` (`_branches` side registry, uuid5 variant names, `_FILE_LOCK`), `test_intervention_smoke.py`, `test_gameid_intervention_isolation.py`, `tests/service/test_experiment_shadow_isolation.py`, `tests/service/agent_flow.py` (agent journeys, #1600), `tests/service/test_tc005_admin_journey.py`, `tests/service/test_tc020_host_and_stage_flags.py`, `tests/service/test_rollout_plumbing.py`, `helpers.py` (`wait_flag_file_restored`) | `services/flagd/ci/*.json` | every intervention, `policy.*` budgets, agent switches, `game.json`, `user.json`, `rollout.json` | in place; snapshot and byte-for-byte restore | per test | `_branches` ladders (tests only) |
| W11 | Script tests | `scripts/tests/test-talk-stage.sh`, `test-talk-preflight.sh`, `test-agent-backend.sh` | copies of `ci/agent.json`, `ci/interventions.json` in a temp dir | the flags W8 writes | file fixtures | CI "Script Tests" job | no |
| W12 | `flag-dir-init` | busybox one-shot in `docker-compose.yml:65`, `docker-compose.ci.yml:75`, `docker-compose.lite.yml:40` | the flag directory | none: `chgrp 65532` + `g+w` so the nonroot menu can write | permissions only | stack start | n/a |

**Direct file readers** (they bypass flagd and must move too):

| # | Reader | Reads | After | Owner |
|---|---|---|---|---|
| R1 | `services/agent/actions/remediation_reader.go:76` | `remediation_allowed` from `rollout.json` (defaultVariant only) | evaluate through flagd | #1411 |
| R2 | `services/agent/dynamic_declarer.go:328` | `game.json` flag keys and variants | `GET /game` | #1415 |
| R3 | `services/agent/main.go:1123` `gameFlagVocab` | `game.json` keys (proposer vocabulary) | `GET /game` | #1415 |
| R4 | `services/agent/experiment/gate.go:170` | `game.json` before the write (invariant gate) | the service renders the shadow condition; the gate checks `GET /game/{flag}` | #1415 |
| R5 | menu `FlagConfigWriter.cycle_variant` / `get_variant_names` (`lib/flag_config_writer.py:213`) | the variant list and current default of admin flags | `GET /{flagSetId}/{flagName}` | #1415 |
| R6 | `scripts/talk-preflight.sh` (12 `json.load` / `open` reads: `agent.json`, `controller.json`, `interventions.json`, the stage baseline) | stage baseline, controller backend, neutral interventions | `GET /agent`, `GET /controller`, `GET /state` | #1412 |

Non-writers (checked): the game coordinator (it keeps the in-memory nonce baselines,
`InterventionManager.forget_session` / `_prune_stale_buckets`), `lib/` apart from
`flag_config_writer.py`, the dashboard (no flagd client), rust-hid.

## 4. After: the same writers through the service

| # | Writer / reader | After | Sub-issue |
|---|---|---|---|
| W1 | Agent intervention sink | `PUT /interventions/{flag}` with `scope.game_id` (and `scope.serial` for levels and grants); `POST /interventions/{flag}` appends a one-shot event; `nonce.go`, `heal.go` and `writeInPlace` are deleted in #1416 | #1411 |
| W2, R1 | Agent rollout writer, remediation reader | `PUT /rollout/current_controller_count`; `remediation_allowed` through flagd | #1411 |
| W3, W4, R2 to R4 | Experiment writer, declarer, vocabulary, gate | `PUT /game/{flag}` with `scope.experiment_id` + `scope.arm` (the service adds `game_kind != "real"`); `DELETE /game/{flag}?experiment_id=` at teardown; the proposer path must carry an experiment id; readers use `GET /game` | #1415 |
| W5 | Autonomous promotion | `PUT /game/{flag}` with `scope.game_kind=real` and `writer=promotion`: an in-memory overlay; the durable path is the promotion PR (FR-064, C-007). The snapshot store is not needed (a restart reverts), but the **revert error cooldown of #1065 is lost** with `RevertRealDefault` unless #1415 re-implements it around `DELETE` | #1415 |
| W6, R5 | Menu admin | `PUT /game/sensitivity`, `/game/num_teams`, `/game/force_all_start`, `/user/current_game`, `/user/game_instructions` (persisted, D10) and `PUT /agent/interventions_allowed` (persisted; #1412); `cycle_variant` reads `GET`; `FlagConfigWriter` deleted in #1416 | #1412 (`interventions_allowed`), #1415 (rest) |
| W7 | connect-proxy | `PUT /controller/bluetooth_backend` with `fraction_percent` (the `chaos_fault_type` writer was removed in #1322); the CI mount gap disappears | #1420 |
| W8, W11, R6 | Stage scripts, their tests, pre-flight | `curl -X PUT http://localhost/flags/agent/decision.backend -d '{"variant":"jev"}'` through envoy; `talk-stage.sh reset` becomes `DELETE /agent?writer=stage` (clears stage overlays); the script tests stub the HTTP endpoint instead of copying files | #1412 |
| W9 | Validation harness | none (removed in #1322) | #1412 |
| W10 | Integration fixtures | a `flag_service` fixture over HTTP; overlays cleared with `DELETE` between tests; no snapshots, no `_branches`, no `_reload_marker`, no `wait_flag_file_restored` | with the cutover of the set they write (#1411, #1412, #1415, #1420); shared fixture in #1413 |
| W12 | `flag-dir-init` | removed; flag mounts read-only, on the flag service only, once every set is cut over | #1414 checklist |

## 7. Alternatives considered

### 7.1 One binary with flagd embedded (rejected for now)

- `github.com/open-feature/flagd/core` is a v0 module; its semver ADR puts the Sync, Evaluation and
  OFREP APIs outside semver ([pkg.go.dev](https://pkg.go.dev/github.com/open-feature/flagd/core),
  [semantic versioning ADR](https://github.com/open-feature/flagd/blob/main/docs/architecture-decisions/semantic-versioning-policy.md)).
- The daemon has no plugin hook for custom syncs; appending an `ISync` to the exported `Runtime.Syncs`
  relies on undocumented internals, and we would own the evaluation, sync and OFREP servers.
- The talk is about **stock** OpenFeature and OTel.
- Embedding would avoid the reconnect backoff and one container; §5.4 covers both. Revisit when
  flagd-core is versioned independently.

### 7.2 Keep the files, fix the writers

Each fix so far (#1306, #1315) added state to the coordinator, and #1323 lists the next ones. PR #1418
(interim pruning of retired games' per-player entries, #1323 / #1317) still merges as the interim fix;
#1416 deletes that pruning with the file paths. Rejected as the end state.

### 7.3 flagd's HTTP sync (polling)

`provider: http` polls every `interval` (default 5 s); it adds up to one interval to every write, which
defeats NFR-050. Rejected.

### 7.4 One sync source per set

Needed only if per-flag `flagSetId` were not honoured (§5.3). Rejected in favour of D3: more sources,
more streams, no benefit.

### 7.5 In-process clients straight to the flag service

Rejected: it would split evaluation between flagd and the service (C-031).

### 7.6 Typed RPC per intervention (first draft)

Replaced by the generic per-set endpoint (D4) at the maintainer's request: the registry's `kind` and
bounds carry the per-flag knowledge, and new mutable flags need a registry row, not a new RPC.

## 12. Migration plan

The maintainer decided on 2026-10-16: the talk is "later in November 2026" (**T**, not pinned yet), so
**every writer migrates before the talk and the old mechanisms are deleted**. There is no file-based
fallback.

**Cutover per flag set.** flagd takes the `file:` sources of the sets not yet migrated and the one gRPC
source side by side; the service includes a set in its payload only once it is listed in
`FLAG_SERVICE_SETS`. A set moves in the same PR that moves **all** of its writers and readers (code,
scripts, fixtures), so the suite stays green after every step. Until a set is cut over, the pre-flight
(`scripts/talk-preflight.sh`) keeps reading that set's file; it switches to `GET /{set}` set by set.

| Set | Writers and readers that move with it | Sub-issue |
|---|---|---|
| `system`, `observability` | none at runtime (proves the plumbing) | #1408 |
| `interventions`, `rollout` | W1, W2, R1; fixtures in `tests/integration/helpers.py` (`intervention_env`), `test_parallel_intervention_flow.py`, `test_intervention_smoke.py`, `test_gameid_intervention_isolation.py`, `tests/service/test_rollout_plumbing.py` | #1411 |
| `agent` | W8, W11, R6, the menu's `interventions_allowed`; agent-set fixtures | #1412 |
| `game`, `user` | W3, W4, W5, the rest of W6, R2 to R5; `tests/service/test_tc005_admin_journey.py`, `tests/service/test_tc020_host_and_stage_flags.py`, `tests/service/test_experiment_shadow_isolation.py`, the game part of `test_intervention_smoke.py` | #1415 |
| `controller` | W7 | #1420 |
| (after every cutover) | W12 removed; the writable flag mounts of menu, agent and connect-proxy removed; every flag mount read-only | #1414 checklist |

| Order | Sub-issue | Scope | Depends on |
|---|---|---|---|
| 1 | #1408 | Service skeleton (Go), baseline load and merge, `flagd.sync.v1` server (one stream, per-flag `flagSetId`, keepalive), flagd wiring in every compose file, health check, start order, `scripts/flags.sh restart`; serves `system`, `observability`; exit test of §5.2 | |
| 2 | #1409 | Mutable-flag registry (kind, scope, bounds, persist), write validation, admin persistence to the state volume, `GET` endpoints and `/state`, write spans and metrics | #1408 |
| 3 | #1410 | Write endpoint (`PUT` / `POST` / `DELETE /{flagSetId}/{flagName}`), game lifecycle (`/games`, `/coordinators/{instance}/games`, expiry), renderer (rungs, defaultVariant substitution, event lists, epoch); coordinator registers games (bounded await) and gains the event-list reader | #1409 |
| 4 | #1411 | Agent writer and remediation reader; cut over `interventions`, `rollout`; edge flags become object event lists; coordinator switches to the event lists and the variant-change grants | #1410 |
| 5 | #1412 | Stage scripts, script tests, pre-flight, harness, menu kill switch; envoy `/flags/` route; cut over `agent` | #1410 |
| 6 | #1413 | Integration tests for UC-053, shared `flag_service` fixture, NFR measurements, alerts | #1411 |
| 7 | #1414 | **Readiness checkpoint at T−14 days**: every set on the service; then remove `flag-dir-init` and the writable flag mounts (every mount read-only); the full suite green; a talk rehearsal on the Raspberry Pi 5 with real controllers | #1408 to #1413, #1415, #1420 |
| 8a | #1415 | `game`, `user`: experiments, declarer, gate, vocabulary, promotion (+ #1065 cooldown), menu admin with persistence, their fixtures | #1410 |
| 8b | #1420 | `controller`: connect-proxy canary (chaos writer removed in #1486 / #1322) | #1410 |
| 9 | #1416 | Delete the file-write paths, nonce and grant baselines, `_prune_stale_buckets`, the interim pruning of #1418, `_branches` / `_reload_marker`, direct file reads | #1414 passed |

The list order is the issue order; #1415 and #1420 run in parallel with #1411 to #1413 (they touch other
flag sets) and must be merged before the checkpoint. #1415 keeps menu and promotion together with the
experiments because all three write `game.json` (and the menu `user.json`): a set can only cut over when
every one of its writers moves.

### 12.1 Delivery (2026-10-17)

The maintainer widened the scope while the spec was under review ("do a full change, also remove the old
files"): the whole epic shipped in **one PR, #1417**, one commit (or two) per sub-issue, in the order of the
table above, with #1414 run as a verification step before the merge instead of a separate checkpoint at
T−14 days. Every set is served by the flag service, flagd has no `file:` source and no flag mount, and the
deletions of #1416 are in the same PR. Deviations from the plan, all reflected in the specs:

- **Experiment catalog.** The registry (flags.md §13.3) lists the gameplay calibration flags an experiment may
  change, each with scopes `experiment+arm` and `real` and a numeric range (or the new `shape` bound for the
  object flag `windows`); capacity, lobby, team-structure and presentation flags are not experimentable. A
  proposer proposal is declared as a registry experiment (`exp_<hex>`, PR #1417 review B2); the unscoped "every shadow game" rule is gone.
- **Promotion** is a `real` overlay with writer `promotion`; the revert is `DELETE ?game_kind=real` and is refused
  when no overlay is present. The agent keeps only the post-revert cooldown stamps (no value snapshots).
- **Alerts.** Prometheus alerting was removed on `main` (#1425) while this PR was open, so the two planned alert
  rules were not added; the Grafana **Flags / OpenFeature** dashboard (uid `feature-flags`) has a "Flag service" row instead.
- **connect-proxy.** A fractional canary sets the stable counterpart as the default and the canary backend as a
  fraction overlay, which also fixes #1386 (the rest of the serials no longer resolve an undeclared variant).
  The canary and the `rollout` set were removed later (#2784); the `fraction` scope has no writer since then.
- **`set_not_served`** (409) stays for a set a stack does not list in `FLAG_SERVICE_SETS`; every stack lists all
  of them (nine then, eight since the `rollout` set was removed, #2784). `experiment_unknown` is reserved and not returned.

## 13. Issues

Superseded (closed when the migration is done):

| Issue | How |
|---|---|
| #1269 game scoping (FR-069) | every intervention is rendered on `gameId` (FR-136) |
| #1323 flag-file growth, unprimed grant serial, collapse within one poll, `end_game` from the fallback partition | scopes dropped at game end; event lists; push instead of a 1 s poll; unregistered games rejected |
| #920 per-game targeting design | the renderer generates the rungs from live scopes only |
| #1293 unchecked `AGENT_REAL_DEFAULT_FLAG_PATH` | promotion writes `PUT /game/{flag}`, limited by the registry |
| nonce and baseline mechanisms of #1299 (PR #1306) and #1310 / #1313 (PR #1315) | already closed; their code is deleted in #1416 |

PR #1418 (interim pruning for #1323 / #1317) still merges; #1416 deletes its pruning.

Related but not superseded: #1329 (coordinator evaluates before its provider is ready), #1092 (agent-tunable
flag catalog: an input to the registry bounds), #1194, #1277, #1221, the flagd v0.17 / v0.18 upgrades.
