# Booth challenge: "Can you see the swing?"

> Guide for a presenter or an outside reader. It restates [UC-102](../use_cases/UC-102.md); if they differ,
> the use case wins. Part of epic #2569 (booth challenges), issue #2586.

## 1. What the challenge shows

A player swings, and a fraction of a second later dies. The case sends each player's motion as a metric
with a new value about every 100 ms, and the same game goes through the gateway to every enabled
observability backend. Many backends store or draw metrics at a 10 to 60 s step by default, so a swing that
short is averaged away. The challenge asks each backend one question: in the seconds before this death, does
it still show the values the case sent?

The question is density, not the exact curve. The case sends a sample of the motion (UC-102 BR-001), and
each backend is compared with the same series in the case's own VictoriaMetrics, never with the value
recorded on the death (BR-003). A result is `full density`, `rolled up` or `gone`, shown side by side for
all backends. There is no score and no order.

The challenge adds no telemetry: it uses what the case sends in every game (BR-005).

## 2. Setup

Preconditions (UC-102):

- The full stack runs (the case's VictoriaMetrics and envoy; the lite stack has neither).
- The gateway export is on and forwards to the backends that take part (FR-086, UC-035 A7).
- The game coordinator exports its metrics every 100 ms. That is the stack default: the observability flag
  `metrics_export_interval_ms` serves `realtime` (100 ms) to the game coordinator by targeting (NFR-022). The
  coordinator reads it once when it starts. **Pre-flight:** its log must show
  `metrics_export_interval_ms from flagd: 100ms`. A coordinator that could not read flagd at start uses
  1000 ms until it restarts.
- The game loop runs at 60 Hz: `game_loop.update_frequency_hz` at `high`, the default. At 30 Hz the case
  sends a new value only every 200 ms.
- Enough controllers are connected for a game (UC-010 preconditions). Any mode with motion deaths works.

## 3. The series

| What | Value |
|---|---|
| Metric | `game_player_accel_magnitude` (observable gauge, in g) |
| Labels | `serial` (the player's controller), `game_id` |
| Rate | Set on every 6th controller frame of the gameplay stream: about every 100 ms at 60 Hz; exported every 100 ms |
| Value | The raw acceleration magnitude of that frame: a sample, so the curve need not reach the `accel_magnitude` recorded on the death (BR-001) |
| After a death | The series repeats its last value while the player is out, and is removed when the player leaves or the game ends. Only the seconds before a death show the swing |
| Path | OTLP to the case's collector, then to the case's VictoriaMetrics and through the gateway's metrics pipeline, with the same processors (BR-002) |
| Values per player | About 10 new values per second; about 100 in the 10 s window |

## 4. The question and how to check an answer

**Pick a death (UC-102 step 4).** Open the game's trace in Jaeger (`/jaeger/`). The root span (named after the
mode) carries `game.id`. After a normal game end it holds one `challenge.ground_truth` event per player: take a
player with a `challenge.death_times_ns` entry, and note that time and the `player.serial`. After a force-end
there is no ground truth: use the player's `player_death` moment instead (a span event or span on the trace,
or a log event in the `log_event` shape). The window is the **10 s ending at the death**. If no motion death
has 10 s of play before it, take the last 10 s of a player who was alive in them (A3).

**Read the reference (step 5, BR-003).** The case's VictoriaMetrics keeps the raw samples (7 days). Export
them for the player, game and window (times in Unix seconds):

```bash
curl -sS -G http://localhost/victoria-metrics/api/v1/export \
  --data-urlencode 'match[]=game_player_accel_magnitude{game_id="<game.id>",serial="<player.serial>"}' \
  --data-urlencode 'start=<death - 10>' --data-urlencode 'end=<death>'
```

The answer is one JSON line with a `values` and a `timestamps` array. Count how often the value changes from
one sample to the next: that is the reference's count, about 100. Do not use a range query (`query_range`, or a
graph): it fills every step with the last stored value and makes any series look dense. If the median time
between changes is longer than 200 ms, or there is no series, the game is not counted (A1).

**Read each backend (step 5).** Open the same metric, filtered to the same `game_id` and `serial`, in the
backend's own metric view, zoomed to the 10 s window and nothing wider. Count how often the shown value
changes. A flat run of repeated values is one value, however many points the graph draws for it.

## 5. Result format

For each backend, one of three results (UC-102 BR-004):

| Result | Meaning |
|---|---|
| full density | The backend's count of value changes is at least 90 % of the reference's |
| rolled up | The series is there in the window with fewer than 90 % of the reference's changes: write down the step it shows (for example 10 s, 60 s) |
| gone | No value of the series in the window (dropped, or past its retention) |

A backend that did not receive the game (gateway or backend unreachable, data refused, or dropped after the
gateway export's bounded retry) gets no result and is recorded as *not received* (A2). The game is still played
to its end, and the case keeps feeding its own backends and the agent unchanged (NFR-029).

Template, one row per backend:

| Backend | Game id | Player | Window (end = death) | Reference changes | Backend changes / step | Result |
|---|---|---|---|---|---|---|
| | | | | | | full density / rolled up / gone / not received |

**When a game is not counted:**

| Case | Why | Flow |
|---|---|---|
| The reference's median time between changes is over 200 ms, or there is no series | The coordinator exports at another interval (another flag variant, or flagd unreachable at its start), or the loop runs below 60 Hz | A1 |

## 6. Known limits

- **A sample, not the peak.** Only every 6th frame is sent, so the spike that killed may fall between two
  values. That is fine for this challenge, which asks about density; compare a backend only with the
  reference, never with the death's `accel_magnitude`.
- **Before the death only.** After a death the series repeats one value, so a window after the death says
  nothing about density.
- **Display versus storage.** A backend may store every value and still draw a coarser step at a wide time
  range. Zoom to the 10 s window before you read it; record what the backend shows at that zoom.

## 7. References

- [UC-102](../use_cases/UC-102.md) See a Swing at Full Density
- FR-223, NFR-022, FR-086 and NFR-029 in [requirements.md](../requirements.md)
- [flags.md](../specs/flags.md) (`metrics_export_interval_ms`, `game_loop.update_frequency_hz`)
- [telemetry.md](../specs/telemetry.md) (the game coordinator's metrics; §3.2 for the ground truth)
- [glossary](../glossary.md): booth challenge, gateway, backend (observability)
- ["Where did it go?"](where-did-it-go.md) (UC-100) and ["Who saved them?"](who-saved-them.md) (UC-101)
- Issue #2586, epic #2569
