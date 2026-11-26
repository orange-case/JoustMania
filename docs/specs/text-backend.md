# Text backend (post-game retro and proposer)

The text backend serves the post-game retro ([UC-042](../use_cases/UC-042.md)) and the experiment proposer. It is not part of the decide contract ([decide-contract.md](decide-contract.md)), but it shares the key, the budget and the eligibility gate with it. The agent talks to models **directly**: there is no gateway or router (NORTHSTAR:
hosted inference is allowed, local inference is not a goal, rules are the offline floor).

To enable the model-backed retro and proposer (the compose `agent` service has no profile, so a plain `up` starts it): give the agent the key, then set the flag (the flag is the only
switch; the proposer additionally needs its gate, FR-152 d):

```bash
ANTHROPIC_API_KEY=<key> docker compose up -d agent
./scripts/flags.sh set agent inference.backend claude
```

| Seam | Used for | Selected by | Backends |
|---|---|---|---|
| Decision backend | in-game decisions (FFA) | `decision.backend` flag, live | `rules` (default), `jev`, `claude` |
| Text backend | post-game retro (`agent.llm.retro`), experiment proposer | `inference.backend` flag, per use (retro / propose cycle); `stub` while flagd is unresolved (no env fallback) | `stub` (default), `claude` (needs the key and a valid base URL) |

The text backend takes a System + User prompt and returns raw text (`Infer(ctx, llm.Prompt) (string, error)`,
`inference/claude.go`, official Anthropic Go SDK). Env vars (`AGENT_INFERENCE_BASE_URL`, `ANTHROPIC_API_KEY`) are in the
agent env table in [services.md](services.md) section 6. Keys live in the agent environment only (compose passes
`ANTHROPIC_API_KEY`, `JEV_API_KEY`); a backend without its key is unavailable and
decisions fall back to rules ([decide-contract.md](decide-contract.md) §7). Keep keys out of the tracked `.env`: export them or use the git-ignored
`.env.local` (`docker compose --env-file .env --env-file .env.local up`).

- **Stub** (default, or `claude` without a key, which logs a warning): the proposer proposes nothing, the retro is
  capture-only (`inference.used=none`, `inference.fallback_reason=no_backend_available`).
- **Retro**: calls the model only for game kinds in `llm.eligible_game_kinds` (default `real`); a shadow-game
  retro stays capture-only (`inference.fallback_reason=llm_not_eligible`). A model call draws one slot of the
  shared `llm.max_requests_per_minute` budget ([decide-contract.md](decide-contract.md) §8). With `enabled` off the retro is capture-only
  (`inference.fallback_reason=agent_disabled`): no model call, no slot.
- **Proposer**: runs on every game end while `enabled` and `experiments_enabled` are both on (read at each game
  end; `enabled` fails closed when flagd is unreachable) and checks both again just before writing the
  experiment value to the flag service, so switching the agent off during a model call still blocks the write.
  With `claude`, a proposal that passes the cadence floor (`code_improvement.min_interval_seconds`) calls the
  model only for eligible game kinds and only when a budget slot is free; otherwise it records
  `agent_llm_gated_total{reason=llm_not_eligible|llm_budget_exhausted}` and retries at the next game end.
  The stub never draws from the budget. The `inference.backend` flag selects the backend per cycle; a stub cycle is never charged. The proposer runs at all only while the proposer gate is open:
  `AGENT_EXPERIMENT_PROPOSER_ENABLED=true` AND `experiment_proposer_enabled` (FR-152 d, C-034); closed, it makes no
  model call and draws no budget slot.
- **The budget is tight during a remote-decide session.** The window is a sliding 60 s shared by decisions, the
  retro and the proposer. With a remote `decision.backend`, `decision.min_interval_ms` 3000 and
  `decision.min_gap_after_answer_ms` 1000 one game wants about 15 to 20 calls a minute against the default cap of 6, so at game end the retro and proposer usually find it
  full (`llm_budget_exhausted`). For retros or proposals in such a session use the `stage` cap
  (`llm.max_requests_per_minute` = 30) or `decision.backend=rules`.
- **Shadow-only runs make no proposals** with the default `llm.eligible_game_kinds = real`. `real_and_shadow` and `shadow_only`
  allow them; both also let shadow games use remote decide calls and retros.
- On models that accept it (Opus 5.5, Opus 5, Sonnet 5.5, Fable 5.1) the client sends `fallbacks: "default"`
  (beta `server-side-fallback-2026-07-01`), so a policy refusal is re-run on a fallback model inside the same call.
- Telemetry: every model call is one GenAI-semconv `chat {model}` CLIENT span ([decide-contract.md](decide-contract.md) §12), nested under `invoke_agent joustmania-agent`
  or `agent.llm.retro`, with W3C `traceparent` injected. Failures record only a low-cardinality `error.type`;
  vendor error text never reaches a span.
