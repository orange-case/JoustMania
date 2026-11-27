# Booth challenge: "How deep can you go?"

> Guide for a presenter or an outside reader. It restates [UC-103](../use_cases/UC-103.md),
> [telemetry.md §3.2 and §3.5](../specs/telemetry.md#35-stowaway); if they differ, the spec wins.
> Part of epic #2569 (booth challenges), issue #2574.

## 1. What the challenge shows

Each round is one game and so one trace. Every round carries more spans than the last: 10 thousand,
100 thousand, then 1 million spans of padding on top of the game's own spans. A backend's result is the
largest round whose trace it still shows whole. Results are shown side by side, never ranked (UC-103 BR-007).

## 2. Before the booth

- **The case's own ceiling.** Once per stack version, on the stage Pi, run the rounds with the padding on the
  Pi in the booth's setup and record the largest round the case carries in NFR-062 (UC-103 BR-008). Rounds
  above it take their padding from a laptop. A stack without a recorded ceiling does not run the challenge.
- **The agent is protected.** The collector drops the per-sample spans from the agent's copy of the traces
  (`filter/agent_per_sample`) and the padding is synthetic, so the agent never gets either (UC-103 BR-005).

## 3. Running a round

1. Switch the per-sample spans on once, before the first round: observability flag
   `telemetry.per_sample_spans` = `on` (read once per game, off by default). Each alive player then sends
   about 60 spans per second (`game.player.sample`): 8 players over 2 minutes are about 58 thousand spans.
2. Set the round: `stowaway.span_target` = `10k`, `100k` or `1m`, with `stowaway.enabled` = `on` and
   `stowaway.challenge` = `span-rounds`.

   ```bash
   curl -sS -X PUT http://localhost/flags/observability/telemetry.per_sample_spans \
     -H 'Content-Type: application/json' -d '{"variant": "on", "writer": "stage"}'
   curl -sS -X PUT http://localhost/flags/observability/stowaway.span_target \
     -H 'Content-Type: application/json' -d '{"variant": "100k", "writer": "stage"}'
   ```

3. Play the game. `stowaway` follows it and pads its trace at up to 10 thousand spans per second, so every
   target is reached 100 s into the game at the latest. A game that ends earlier carries less padding than
   the target: replay the round with a longer game (UC-103 A2).
4. **Judge after the game end.** The game's root span ends only when the game ends, so mid-game the trace
   has no root and no backend can show it whole. Open the trace 2 minutes after the game end and once more
   10 minutes after it, and keep the better result (UC-103 BR-006).

## 4. The reference: the delivered count

After a normal game end each producer counts the spans of the game's trace its exporter sent successfully
(UC-103 BR-003). Grafana, **Game & Players**, collapsed row **Span rounds (UC-103)**, shows per game the
game coordinator's count, `stowaway`'s and their sum. The same counts are in the trace itself, as one
`orangecase.span_count` span per producer (attribute `orangecase.span_count.delivered`). A force-ended game
has no count and is not judged (UC-103 A1).

## 5. The stop rule

| Result | Meaning |
|---|---|
| whole | The trace opens within 60 s of the request and shows, for the game coordinator and for `stowaway` each, at least the producer's delivered count |
| partial | The trace opens with fewer spans of either producer than its count: record the counts it shows |
| too slow | The trace has not opened 60 s after the request |
| not found | No trace with that id |

The tolerance is 0 spans below a producer's count; spans above it (the count spans themselves, a batch
received twice after a retried export) never fail a round. A backend passes a round only with *whole* and
is not tested at larger rounds after a failed one. A round the case did not deliver in full (its collector or
the gateway dropped spans) is *not delivered*, never a backend's failure (UC-103 A3).

## 6. After the booth

Reset `telemetry.per_sample_spans`, `stowaway.span_target` and `stowaway.enabled` (a `DELETE` of each
overlay): every later game, shadow and pattern games included, keeps them until they are cleared.
