# Booth challenge: "What did the game master cost?"

> Guide for a presenter or an outside reader. It restates [UC-105](../use_cases/UC-105.md) and
> [telemetry.md §4.2](../specs/telemetry.md#42-genai-semantic-conventions); if they differ, the spec wins.
> Part of epic #2569 (booth challenges), issue #2718.

## 1. What the challenge shows

The agent, the game master, calls a model to decide in a game, to review a game after it ends (the retro) and to
propose experiments (the proposer). Every call records its tokens on its `chat {model}` span. The OTel GenAI
conventions define no cost attribute, so turning tokens into money is each backend's own work. Each backend gets the
same questions, from coarse to fine, and the result is how far down it gets. It is not a ranking: the results are
shown side by side, with no score and no order (UC-105 BR-006).

## 2. Setup

Preconditions (UC-105):

- The gateway export is on and forwards to the backends that take part (FR-086, UC-035 A7).
- The agent decides through a model in real games: `decision.backend` = `claude` and `llm.eligible_game_kinds`
  includes `real`. For the shadow half of question 2, `llm.eligible_game_kinds` must include `shadow` too;
  otherwise its reference is zero, and say so with the question (BR-001).
- The retro and the proposer call a model: `inference.backend` = `claude`, with the agent's model key set.
- The agent's spans follow the OTel GenAI agent conventions (`invoke_agent joustmania-agent`, #2584, done).

```bash
./scripts/agent-backend.sh claude haiku     # decide backend and model for the first game(s)
```

**The games (BR-001).** Play at least two real games that end normally with model decisions in each. Between two
of them, switch the decide backend or its model once (UC-044) and note the time:

```bash
date -u +%Y-%m-%dT%H:%M:%SZ                 # the switch time, for question 6
./scripts/agent-backend.sh claude sonnet
```

Note each game's id (`game_<12 hex>`, on the game's root span and the dashboard). Every game ends with a retro.
Jev, when it decides, charges no tokens: it is priced at zero with the note *compute, not priced*.

## 3. The reference

Read from the case's own Jaeger, never from a backend (BR-003). The counting rules are those of
[telemetry.md §4.2](../specs/telemetry.md#42-genai-semantic-conventions) "Spend":

| Scope | Spans | Rule |
|---|---|---|
| Per decision | the decide span `invoke_agent joustmania-agent` (old name `agent.decide`) | its `gen_ai.usage.*` totals; a span without `gen_ai.request.model` is the rules path and has no spend |
| Per game | spans with `gen_ai.conversation.id` = the game id | the game's decide spans plus its retro's `chat {model}` span; the decide spans' `chat {model}` children carry no conversation id, so nothing counts twice |
| Per caller | `chat {model}` by parent | decide: parent is the decide span; retro: parent `agent.llm.retro`; proposer: everything else (no conversation id) |
| Per model | `chat {model}` | `gen_ai.provider.name` and `gen_ai.response.model` (the model that answered); these are on the chat spans, not on the decide span |
| Real or shadow | the decide span's `game.kind` | the game root span's `game.kind` is the authority; a call without a game id is neither |

Never add a decide span and its `chat {model}` children: the totals repeat the children. `gen_ai.usage.input_tokens`
already includes the cache read and cache write tokens, and `gen_ai.usage.output_tokens` the reasoning tokens.

### 3.1 The reference script

`scripts/genai_cost_reference.py` applies these rules and the price table below. Read-only, stdlib only; it sends
GET requests to the Jaeger query API v3 and nothing else.

```bash
python3 scripts/genai_cost_reference.py \
  --prices docs/challenges/genai-prices-2026-11-27.json \
  --jaeger-url http://localhost/jaeger --start <epoch first game start> --end <epoch last retro end> \
  --game-id game_<12 hex> --game-id game_<12 hex>
```

It prints questions 1 to 4 (`--json` for the full answer, including every decision). It queries the window once
(the proposer's calls and anything else without a game id) and each game id once
(`query.attributes={"gen_ai.conversation.id":"<game id>"}`); a trace that comes back twice is counted once.
The window query returns at most `--search-depth` traces (default 1000).

**The cross-check (UC-105 A3).** For each game the script compares the tokens of the spans with the game's
conversation id (decide totals plus retro call) with the sum of the game's `chat {model}` spans. A game whose two sums
differ is printed `DOES NOT ADD UP`, and the script exits 3: do not use that game for questions 3 and 4. A game with
no model call (A1) has no spend. `scripts/tests/test-genai-cost-reference.sh` checks the rules on fixture spans.

### 3.2 By hand in the Jaeger UI

Without the script: in Jaeger (`/jaeger/`), service `agent`, tags `gen_ai.conversation.id=<game id>`. In the game's
trace, sum `gen_ai.usage.*` over the `invoke_agent joustmania-agent` spans and the `chat {model}` span under
`agent.llm.retro`. For the money, open each `chat {model}` child to read its `gen_ai.response.model`.

## 4. The price table

Frozen on **2026-11-27**, from the Anthropic pricing page
(<https://platform.claude.com/docs/en/about-claude/pricing>, the *Model pricing* table, read that day). USD per
million tokens. Machine-readable: [genai-prices-2026-11-27.json](genai-prices-2026-11-27.json) (the script reads it).

| Model (prefix of `gen_ai.response.model`) | Input | Cache write (5 min) | Cache read | Output |
|---|---|---|---|---|
| `claude-haiku-4-5` | 1.00 | 1.25 | 0.10 | 5.00 |
| `claude-sonnet-5-5` | 2.00 | 2.50 | 0.10 | 10.00 |
| `claude-opus-5-5` | 4.00 | 5.00 | 0.20 | 20.00 |
| Jev (`gen_ai.provider.name=jev`) | 0 | 0 | 0 | 0 (*compute, not priced*) |

These are the models the agent's flags can select (`decision.claude.model`, `inference.model`, [flags.md](../specs/flags.md)).
A model not in the table is reported as *unpriced*, never guessed: add its row from the same page with the date.

- **Pricing a call:** the cached share of the input at the cache price, the rest of `input_tokens` at the input
  price, `output_tokens` (reasoning included) at the output price. Never add `cache_*` or `reasoning` on top.
- **Cache writes** use the 5-minute price, the default cache lifetime. The agent's decide requests carry no
  `cache_control`, so cache writes are expected to be 0. They are read from `gen_ai.usage.cache_write.input_tokens`;
  the claude chat spans carry the same value as `gen_ai.usage.cache_creation.input_tokens` during the transition
  (#2729), and older images only that name, so the script reads the old name only when the new one is absent, never
  both.
- **Reasoning tokens:** the pricing page does not say how thinking is billed. TODO: confirm from a provider page
  that reasoning (thinking) tokens bill as output tokens; until then the table assumes it, as the agent's
  `output_tokens` include them.
- **Long context:** the page charges no surcharge above 200K tokens for Sonnet 5.5 and Opus 5.5. It says nothing
  either way for Haiku 4.5. The agent's prompts are far below that.

## 5. The questions, from coarse to fine

Ask them in this order, each with the game ids and the time window of the challenge (BR-004). The reference for
questions 1, 2 and 6 is every model call of the agent in the window, from the start of the first challenge game to
the end of the last game's retro.

| # | Question | Reference (script section) | Note |
|---|---|---|---|
| 1 | What is the total spend, per provider and model? | Q1 per provider/model | |
| 2 | What is the spend per caller: decide, retro, proposer; and real games against shadow games? | Q2 per caller, per game kind | The token metrics have one scope for retro and proposer, so a metrics-only backend cannot split those two |
| 3 | What did each of the challenge games cost? | Q3 per game | The metrics carry no game id: a backend that prices only from metrics cannot answer |
| 4 | What did each decision cost, what did the interventions that changed the game cost, and what did the decisions cost that were held or blocked and had no effect? | Q4 decisions and subsets | *changed the game*: an `agent.decision` child with `decision.applied` true; *held*: event `gen_ai.evaluation.result` with `gen_ai.evaluation.score.label` = `fail` on the decide span; *blocked*: an `agent.decision` child with `decision.blocked` true. A decide span counts once per subset it falls into |
| 5 | Are cache and reasoning tokens priced apart; is the price that of the answering model or the requested one; do refused, cut-off and failed calls count? | §4 rules | Answered by what the backend shows or documents. A refused or cut-off call keeps its billed tokens; a timeout or transport error has none (telemetry.md §4.2) |
| 6 | Does the spend change at the switch, and does the backend show the switch next to the change? | Q1 before and after the switch time | The switch is on the decide spans as `decide.backend` and `gen_ai.request.model`. A span event at the moment of a flag change is #2572 (open); until it lands, record only whether the backend shows the step |

## 6. Checking an answer and the result format

Tokens first, then money (BR-005):

| Answer | Meaning |
|---|---|
| matches | the tokens equal the reference, and the money is within 5 % of the reference |
| differs | the tokens or the money is off: record the backend's number and, when known, why (a decide span counted with its model calls, its own price list) |
| usage only | the tokens match, but the backend shows no money |
| cannot answer | the backend cannot show the tokens or the money of the question |

A backend's result is its answer to each question and the last question it answered with *matches* while every
question before it was *matches* or *usage only* (BR-006). A backend that did not receive the games is
*not received*, not a result (A2).

Template, one row per backend (price table 2026-11-27):

| Backend | Q1 | Q2 | Q3 | Q4 | Q5 | Q6 | How far down | Notes per question |
|---|---|---|---|---|---|---|---|---|
| | matches / differs / usage only / cannot answer | | | | | | Q<n> / not received | |

## 7. Open

- **No external backend has been run yet.** The acceptance of #2718 asks for at least one, with the result recorded
  here. That needs the booth setup; until then this page has no result row.
- **Question 6 on the trace** depends on #2572 (OpenFeature hook span event at a flag change).
- **Reasoning token billing:** see the TODO in §4.

Stretch ideas from #2718, not part of UC-105 (BR-007), not implemented:

- A deliberate cost spike: a flag injects a runaway (bloated prompt, higher `max_tokens`, more frequent decisions);
  does the backend raise a cost alert and point to the flag change? Same pattern as #2575.
- What it costs to watch the AI: with `decision.capture_content` on, the ingest and storage cost of the GenAI
  telemetry next to the model spend. Related: #2574, #2579.
- The backend's own assistant: what the dashboard generation of #2585 cost.
- Local model compute: Jev's compute next to the API spend; see #2659.

## 8. References

- [UC-105](../use_cases/UC-105.md) Find What the Game Master Cost
- FR-249 in [requirements.md](../requirements.md)
- [telemetry.md §4.2](../specs/telemetry.md#42-genai-semantic-conventions) (spans, counting rules, metrics)
- [UC-044](../use_cases/UC-044.md) (switch the decide backend), [UC-101](../use_cases/UC-101.md) (interventions)
- Issue #2718, epic #2569, #2572, #2584
