# Agent Dry-Run Runbook

Runs the experiment cohort loop on mock controllers with the full local observability stack: the agent declares
one seeded experiment and spawns its shadow games. Behaviour: [UC-043](use_cases/UC-043.md) (A16 is this
enable step). Live interventions: [agent-act-runbook.md](agent-act-runbook.md).

## Start

```bash
make dry-run                          # current images (IMAGE_TAG=latest; BUILD=1 builds locally)
./scripts/agent-dryrun-enable.sh on   # the loop is inert until you do this
./scripts/agent-dryrun-enable.sh status
```

`make dry-run` runs:

```bash
docker compose -f docker-compose.yml -f docker-compose.override.yml \
  -f docker-compose.ci.yml -f docker-compose.dry-run.yml \
  --profile agent --profile dashboard up -d
```

`agent-dryrun-enable.sh on` sets, as in-memory `operator` overlays through the flag service, the `experiment_seed`
flag to `windows_frantic` first, then `enabled` to `on` (arm writes need it, so the agent also writes the
interventions the CI allow-list `ambient` permits) and `experiments_enabled` to `on`. Nothing is persisted:
a flag-service restart or `down` drops them; `off` sets `enabled` to `off` and clears the other two. A plain `docker compose up` runs the release images pinned in `.env`,
so use `make dry-run`.

## Gate chain

All gates are fail-closed in the baseline.

1. `experiments_enabled` (agent set): the only gate of the loop. Off: no shadow spawns, no arm writes, no
   promotions.
2. `enabled` (agent set): the agent state (`off` / `watch` / `on`), read every cycle. `off`: the decision loop
   emits `agent.disabled` and running experiments are torn down with `kill-switch: agent disabled`. `watch`: the
   agent decides and records but writes no intervention and no experiment arm (running experiments are torn down
   with `agent watch: arm writes need enabled=on`); arms and interventions are written only at `on`.
3. Promotion of a winner to the real default: the `code_improvement.*` flags plus the env gates
   `AGENT_CODE_IMPROVEMENT_ENABLED=true` and `AGENT_AUTONOMOUS_ENABLED=true` (issue mode: `target=github` with
   `GITHUB_TOKEN`, `GITHUB_REPOSITORY`). No compose file passes them, so a dry run records verdicts and no-op
   promotions only.

Optional:
- Decide backend: `./scripts/agent-backend.sh claude` (needs `ANTHROPIC_API_KEY`); shadow games stay on `rules`
  unless `llm.eligible_game_kinds` includes shadow games.
- Post-game proposer: `inference.backend=claude`, `AGENT_EXPERIMENT_PROPOSER_ENABLED=true` and the
  `experiment_proposer_enabled` flag; it runs after a real game ends (shadow ends too with
  `llm.eligible_game_kinds` = `real_and_shadow` or `shadow_only`).

## Useful commands

```bash
./scripts/flags.sh set agent experiment_tick_seconds fast        # 10 s refill tick (default 30 s)
./scripts/flags.sh set agent experiment_dynamic_enabled on       # keep declaring after the seed concludes
./scripts/flags.sh set agent experiment_seed <variant>           # another reviewed seed variant
./scripts/flags.sh get game <seed flag>                          # the experiment-arm overlay
docker compose -f docker-compose.yml -f docker-compose.override.yml \
  -f docker-compose.ci.yml -f docker-compose.dry-run.yml logs -f agent
```

A new seed is a new variant of `experiment_seed` in `services/flagd/agent.json` and `services/flagd/ci/agent.json`;
the seeded flag must be in the experiment catalog ([flags.md §13.3](specs/flags.md#13-mutable-flag-registry)).
To re-run the same seed: `off`, wait one tick, `on`.

## Verify

- Agent log: `experiment.declared`, `experiment.started`, `experiment.game_assigned`, then a conclude. The
  `Experiment cohort loop constructed` line alone does not mean it runs.
- Jaeger (`http://localhost/jaeger/`, service `agent`): the experiment spans (`agent.experiment` and its
  children). The dashboard's Experiments view (`http://localhost/`) shows the experiment cards, read from the
  agent's `http://localhost/experiments/state`; `http://localhost/experiments` is the agent's experiment dossier
  (an HTML list).
- `inconclusive` verdicts are expected on mock controllers.
- Editing `services/flagd/` changes nothing on a running stack; use `scripts/flags.sh`. `scripts/flags.sh restart`
  drops every overlay, so run `agent-dryrun-enable.sh on` again.

## Tear down

```bash
./scripts/agent-dryrun-enable.sh off
docker compose -f docker-compose.yml -f docker-compose.override.yml \
  -f docker-compose.ci.yml -f docker-compose.dry-run.yml \
  --profile agent --profile dashboard down
```
