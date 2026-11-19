# Booth challenge: "Can you see the swing?"

> Guide for a presenter or an outside reader. It restates [UC-102](../use_cases/UC-102.md); if they differ,
> the use case wins. Part of epic #2569 (booth challenges), issue #2586.

## 1. What the challenge shows

A player swings, and a fraction of a second later dies. The case sends each player's motion as a metric
with a point about every 100 ms, and the same game goes through the gateway to every enabled observability
backend. Most backends store or draw metrics at a 10 to 60 s step by default, so a swing that short is
averaged away. The challenge asks each backend one question: around this death, does it still show the
points the case sent?

The question is density, not the exact curve. The case sends a sample of the motion (UC-102 BR-001), and
each backend is compared with the same series in the case's own VictoriaMetrics, never with the value
recorded on the death (BR-003). A result is `full density`, `rolled up` or `gone`, shown side by side for
all backends. There is no score and no order.

The challenge adds no telemetry: it uses what the case sends in every game (BR-005).

## 2. Setup

Preconditions (UC-102):

- The gateway export is on and forwards to the backends that take part (FR-086, UC-035 A7).
- The game coordinator exports its metrics every 100 ms. That is the stack default: the observability flag
  `metrics_export_interval_ms` serves `realtime` (100 ms) to the game coordinator and the controller manager
  by targeting ([flags.md §10](../specs/flags.md#10-observability)). The flag is read when a service starts;
  a change applies only after the coordinator restarts.
- Game analytics are on (the default), so the coordinator sends the per-player motion metrics.
- Enough controllers are connected for a game (UC-010 preconditions). Any mode with motion deaths works.

## 3. The series

| What | Value |
|---|---|
| Metric | `game_player_accel_magnitude` (gauge, in g) |
| Labels | `serial` (the player's controller), `game_id` |
| Rate | Set every 6th frame of the 60 Hz game loop: about one point every 100 ms, exported every 100 ms |
| Value | The raw acceleration magnitude of that frame: a sample, so the curve need not reach the `accel_magnitude` recorded on the death (BR-001) |
| Path | OTLP to the case's collector, then unchanged to the case's VictoriaMetrics and through the gateway's metrics pipeline (BR-002) |
| Points per player | About 10 per second; about 100 in a 10 s window |

## 4. The question and how to check an answer

**Pick a death (UC-102 step 4).** Open the game's trace in Jaeger (`/jaeger/`) or any backend. The root span
(named after the mode) carries `game.id`. Pick a player's `player_death` moment: note its time and the
player's `player.serial`. The window is 10 s around it (5 s before, 5 s after) unless you choose otherwise. If
the game had no motion death, pick 10 s of one player's play instead (A3).

**Read the reference (step 5, BR-003).** The case's VictoriaMetrics keeps the raw points (7 days). Export
them for the player, game and window (times in Unix seconds):

```bash
curl -sS -G http://localhost/victoria-metrics/api/v1/export \
  --data-urlencode 'match[]=game_player_accel_magnitude{game_id="<game.id>",serial="<player.serial>"}' \
  --data-urlencode 'start=<death - 5>' --data-urlencode 'end=<death + 5>'
```

The answer is one JSON line with a `values` and a `timestamps` array: their length is the reference's point
count, and the difference between neighbouring timestamps is its step (about 100 ms). If the step is much
longer than 100 ms, or there is no series, the game is not counted (A1).

**Read each backend (step 5).** Open the same metric, filtered to the same `game_id` and `serial`, over the
same window, in the backend's own metric view or a dashboard on it. Count the points it shows in the window,
or read the step it reports.

## 5. Result format

For each backend, one of three results (UC-102 BR-004):

| Result | Meaning |
|---|---|
| full density | A point about every 100 ms: the backend's point count in the window is at least 90 % of the reference's |
| rolled up | The window is there, with fewer points: write down the step it shows (for example 10 s, 60 s) |
| gone | No point of the series in the window (dropped, or past its retention) |

A backend that did not receive the game (gateway or backend unreachable, data refused) gets no result and is
recorded as *not received* (A2). The game is still played to its end, and the case keeps feeding its own
backends and the agent unchanged (NFR-029).

Template, one row per backend:

| Backend | Game id | Player | Window | Reference points / step | Backend points / step | Result |
|---|---|---|---|---|---|---|
| | | | | | | full density / rolled up / gone / not received |

**When a game is not counted:**

| Case | Why | Flow |
|---|---|---|
| The reference step is much longer than 100 ms, or there is no series | The coordinator ran with another export interval, or analytics were off | A1 |

## 6. Known limits

- **A sample, not the peak.** Only every 6th frame is sent, so the spike that killed may fall between two
  points. That is fine for this challenge, which asks about density; compare a backend only with the
  reference, never with the death's `accel_magnitude`.
- **Display versus storage.** A backend may store every point and still draw a coarser step at a wide time
  range. Zoom to the 10 s window before you read it; record what the backend shows at that zoom.

## 7. References

- [UC-102](../use_cases/UC-102.md) See a Swing at Full Density
- FR-223, NFR-022, FR-086 and NFR-029 in [requirements.md](../requirements.md)
- [flags.md §10](../specs/flags.md#10-observability) (`metrics_export_interval_ms`)
- [telemetry.md](../specs/telemetry.md) (the game coordinator's metrics)
- [glossary](../glossary.md): booth challenge, gateway, backend (observability)
- ["Where did it go?"](where-did-it-go.md) (UC-100) and ["Who saved them?"](who-saved-them.md) (UC-101)
- Issue #2586, epic #2569
