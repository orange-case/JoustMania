> **Superseded** by [docs/specs/telemetry.md](../../docs/specs/telemetry.md#32-game-coordinator) for span names (2026-10-16); the how-to here is historical, use [docs/agent-trace-viz.md](../../docs/agent-trace-viz.md) for reading traces. Kept for history.

# Distributed Tracing in GameCoordinator

## Overview

GameCoordinator now has **full distributed tracing** with OpenTelemetry, tracking both incoming requests (server-side) and outgoing calls to other services (client-side).

## Architecture

### OpenTelemetry Instrumentation

**Server-Side (Incoming RPCs):**
- `GrpcInstrumentorServer()` - Automatically traces all incoming gRPC requests
- Creates spans for: `StartGame`, `GetGameStatus`, `ForceEndGame`, `StreamGameEvents`

**Client-Side (Outgoing RPCs):**
- `GrpcInstrumentorClient()` - Automatically traces all outgoing gRPC calls
- Creates spans for calls to:
  - **ControllerManager service**: `GetReadyControllers`, `StreamGameplayData`

### Trace Hierarchy

**Phase 36 Normalized Hierarchy** - All game modes follow consistent span naming:

#### FFA (Free-For-All)
```
StartGame (incoming RPC to GameCoordinator)
├── Free-For-All (game session)
│   ├── game.phase.initialization
│   │   └── GetReadyControllers → ControllerManager (outgoing RPC)
│   │       └── controller_manager.get_ready_controllers
│   ├── game.phase.countdown
│   ├── gameplay_phase
│   │   ├── StreamGameplayData → ControllerManager (outgoing RPC)
│   │   │   └── controller_manager.stream_controller_states
│   │   ├── player_mock_controller_0_lifecycle
│   │   │   ├── player_warning (event)
│   │   │   └── player_death (event)
│   │   ├── player_mock_controller_1_lifecycle
│   │   │   ├── player_warning (event)
│   │   │   └── player_death (event)
│   │   └── player_mock_controller_2_lifecycle
│   │       └── player_survived (event)
│   └── game.phase.teardown
└── game_ended (event)
```

#### Teams / Random Teams (Hierarchical Spans)
```
StartGame (incoming RPC to GameCoordinator)
├── Teams (or "Random Teams") (game session)
│   ├── game.phase.initialization
│   │   └── GetReadyControllers → ControllerManager
│   ├── game.phase.team_formation (Random Teams only)
│   │   └── display_team_colors (event)
│   ├── game.phase.countdown
│   ├── gameplay_phase
│   │   ├── StreamGameplayData → ControllerManager
│   │   ├── team_0_lifecycle
│   │   │   ├── player_mock_controller_0_lifecycle
│   │   │   │   └── player_death (event)
│   │   │   ├── player_mock_controller_2_lifecycle
│   │   │   │   └── player_death (event)
│   │   │   └── team_eliminated (event)
│   │   └── team_1_lifecycle
│   │       ├── player_mock_controller_1_lifecycle
│   │       │   └── player_death (event)
│   │       └── player_mock_controller_3_lifecycle
│   │           └── player_survived (event)
│   └── game.phase.teardown
│       └── team_1_wins (event)
└── game_ended (event)
```

#### Nonstop Joust (Respawning Players)
```
StartGame (incoming RPC to GameCoordinator)
├── Nonstop Joust (game session)
│   ├── game.phase.initialization
│   │   └── GetReadyControllers → ControllerManager
│   ├── game.phase.countdown
│   ├── gameplay_phase
│   │   ├── StreamGameplayData → ControllerManager
│   │   └── players
│   │       ├── player_mock_controller_0_lifecycle (open until gameplay_phase ends)
│   │       │   ├── game.phase.countdown
│   │       │   ├── player_death (event)
│   │       │   ├── player_respawned (event)
│   │       │   ├── player_death (event)
│   │       │   └── final_score (event)
│   │       ├── player_mock_controller_1_lifecycle
│   │       │   ├── player_death (event)
│   │       │   ├── player_respawned (event)
│   │       │   └── final_score (event)
│   │       └── player_mock_controller_2_lifecycle
│   │           ├── player_death (event)
│   │           └── final_score (event)
│   └── game.phase.teardown
│       └── game.outcome (event)
└── game_ended (event)
```

NonStop opens one lifecycle span per player, under `players` like the other modes (#1955, #1338;
before that its game loop opened a second set directly under `gameplay_phase` and the first set,
with the `game.phase.countdown` children, never ended). The `final_score` event is added when the
spans close at the end of `gameplay_phase` (`_close_all_player_spans`), before the winner is known.

**Key Differences:**
- **FFA**: Flat player hierarchy, players die permanently (spans end)
- **Teams**: Hierarchical (team → player), team-based win condition
- **Random Teams**: Same as Teams + `game.phase.team_formation` before countdown
- **Nonstop Joust**: Player spans stay open while the player respawns (death and respawn events), close with a `final_score` event; scoring-based winner only on a time-limit end (UC-026 BR-003)

## Context Propagation

### Automatic Propagation

`GrpcInstrumentorClient` automatically:
1. Extracts trace context from incoming request
2. Injects trace context into outgoing gRPC calls via metadata
3. Maintains parent-child span relationships across service boundaries

### Manual Propagation (Not Required)

The game mode code (ffa.py, teams.py, random_teams.py) does **not** need to manually propagate context. The gRPC instrumentation handles this automatically.

## Implementation

### services/game_coordinator/server.py

```python
from opentelemetry.instrumentation.grpc import (
    GrpcInstrumentorServer,
    GrpcInstrumentorClient,  # Added for client calls
)


def init_telemetry():
    """Initialize OpenTelemetry with OTLP exporter."""
    # ... TracerProvider setup ...

    # Instrument both server and client-side gRPC calls
    GrpcInstrumentorServer().instrument()  # Incoming RPCs
    GrpcInstrumentorClient().instrument()  # Outgoing RPCs
```

### Game Mode Span Management

**Phase 36b Base Class** - Span creation is now centralized in `BaseGameMode`:

```python
# BaseGameMode.run() orchestrates all spans automatically
async def run(self, game_context=None):
    """Template method handling span hierarchy."""
    span_name = get_game_display_name(self.get_game_name())

    with tracer.start_as_current_span(span_name, context=game_context) as game_span:
        # game.phase.initialization span
        with tracer.start_as_current_span("game.phase.initialization", ...):
            await self._initialize_players()  # GetReadyControllers RPC automatically traced
            self._create_player_spans(game_context)

        # Additional phases (e.g., game.phase.team_formation)
        for phase in self._get_additional_phases():
            with tracer.start_as_current_span(f"game.phase.{phase.name}", ...):
                await phase.execute()

        # game.phase.countdown, gameplay_phase, game.phase.teardown...
```

**Subclasses** (FFA, Teams, Random Teams, Nonstop Joust) focus on game-specific logic:
- Inherit span orchestration from `BaseGameMode`
- Implement abstract methods (`_initialize_players_impl`, `_create_player_spans`, etc.)
- Make normal gRPC calls (automatically traced by `GrpcInstrumentorClient`)

**Benefits:**
- **Consistency**: All game modes use identical span hierarchy
- **DRY**: Span creation logic exists in one place
- **Maintainability**: Fix span bugs once in base class

## Viewing Traces in Jaeger

### 1. Start All Services

```bash
docker compose up
```

### 2. Run a Game

Via WebUI or gRPC call:
```bash
grpcurl -plaintext -d '{"mode":"FFA"}' localhost:50053 game_coordinator.GameCoordinatorService/StartGame
```

### 3. Open Jaeger UI

```
http://localhost:16686
```

### 4. Search for Traces

**By Service:**
```
service="game-coordinator-service"
```

**By Operation (Root Span):**
```
operation="StartGame"
```

**By Game Mode (Human-Readable):**
```
operation="Free-For-All"
operation="Teams"
operation="Random Teams"
operation="Nonstop Joust"
```

**By Game Phase:**
```
operation="game.phase.initialization"
operation="game.phase.countdown"
operation="gameplay_phase"
operation="game.phase.teardown"
operation="game.phase.team_formation"  # Random Teams only
```

**By Player:**
```
player.serial="mock_controller_0"
```

**By Team (Team Games):**
```
team.number=0
team.number=1
```

### 5. Explore the Trace

You should see:
- **Root span**: `StartGame` (incoming RPC to GameCoordinator)
- **Game session span**: Human-readable name (`Free-For-All`, `Teams`, `Random Teams`, `Nonstop Joust`)
- **Phase spans**: Consistent across all modes
  - `game.phase.initialization` (contains ControllerManager RPCs)
  - `game.phase.team_formation` (Random Teams only)
  - `game.phase.countdown`
  - `gameplay_phase` (contains player/team lifecycle spans)
  - `game.phase.teardown`
- **Lifecycle spans**:
  - FFA/Nonstop: `player_{serial}_lifecycle` (flat hierarchy)
  - Teams/Random Teams: `team_{number}_lifecycle` → `player_{serial}_lifecycle` (hierarchical)
- **RPC spans** (automatic from instrumentation):
  - `GetReadyControllers` (outgoing to ControllerManager)
  - `StreamGameplayData` (streaming from ControllerManager)

## Metrics Available

### Latency Metrics

**Cross-Service Latency:**
- Time from `game.phase.initialization` to `GetReadyControllers` response
- Time from `gameplay_phase` to first `StreamGameplayData` frame

**Game Phase Latency:**
- `game.phase.initialization` duration (player setup)
- `game.phase.team_formation` duration (Random Teams: ~3 seconds)
- `game.phase.countdown` duration (typically 3 seconds)
- `gameplay_phase` duration (entire game, varies by mode)
- `game.phase.teardown` duration (cleanup + winner declaration)

**Player-Level Latency:**
- Time to first warning (high acceleration detected)
- Time to death (span duration for FFA/Teams)
- Survival duration (player lifecycle span length)
- Respawn time (Nonstop Joust: 3 seconds between death and respawn events)

**Team-Level Latency (Teams/Random Teams):**
- Team lifecycle span duration (formation → elimination)
- Time to team elimination
- Player death intervals within team

### Error Tracking

**Service Errors:**
- ControllerManager service unavailable
- gRPC call failures

**Game Errors:**
- Player initialization failures
- Controller stream errors
- Death detection errors

## Troubleshooting

### Traces Not Appearing

**Check OTLP Endpoint:**
```bash
# In docker-compose.yml or environment
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

**Check Collector Logs:**
```bash
docker compose logs otel-collector
```

**Check Service Logs:**
```bash
docker compose logs game-coordinator
# Look for: "OpenTelemetry initialized: game-coordinator-service -> ..."
```

### Traces Are Disconnected

**Symptom:** You see multiple separate traces instead of one connected trace.

**Cause:** Missing `GrpcInstrumentorClient()` instrumentation.

**Solution:** Verify both instrumentors are called:
```python
GrpcInstrumentorServer().instrument()
GrpcInstrumentorClient().instrument()  # Must be present
```

### Missing Player Spans

**Symptom:** You see `Free-For-All` (or other game mode) but no `player_{serial}_lifecycle` spans.

**Cause:** Game didn't start properly or no players detected.

**Solution:**
- Check ControllerManager is returning controllers
- Verify `GetReadyControllers` RPC succeeds in `game.phase.initialization`
- Look for `players_initialized` event in logs
- Check that `gameplay_phase` span exists (player spans created there)

### Missing Team Spans

**Symptom:** You see `Teams` or `Random Teams` but no `team_{number}_lifecycle` spans.

**Cause:** Team initialization failed or no teams created.

**Solution:**
- Verify at least 2 players are ready (minimum for team games)
- Check team assignment logic in initialization
- Look for `teams_created` event in logs
- Verify player → team assignment completed

## Performance Impact

### Overhead

**gRPC Instrumentation:**
- Server-side: ~0.1-0.5ms per RPC
- Client-side: ~0.1-0.5ms per RPC
- Streaming: ~0.01ms per message

**Span Creation:**
- Per-player spans: ~0.1ms to create
- Events: <0.01ms per event

**Total Overhead:**
- ~1-2ms per game for full distributed trace
- Negligible impact on 60 FPS game loop (16.67ms budget per frame)

### Optimization

**Batch Span Processor:**
```python
provider.add_span_processor(BatchSpanProcessor(otlp_exporter))
```
- Batches spans before sending to collector
- Reduces network overhead
- Default batch size: 512 spans

**Sampling (Optional):**
If overhead becomes an issue, configure sampling:
```python
from opentelemetry.sdk.trace.sampling import TraceIdRatioBased

provider = TracerProvider(
    sampler=TraceIdRatioBased(0.1),  # Sample 10% of traces
    resource=resource,
)
```

## Service Dependencies

### GameCoordinator Makes Client Calls To:

1. **ControllerManager Service** (localhost:50052)
   - `GetReadyControllers` - Get available controllers
   - `StreamGameplayData` - Stream controller states at 60Hz
   - Called in `_initialize_players()` and `_game_loop()`

### GameCoordinator Receives Server Calls From:

1. **WebUI** (via HTTP → gRPC)
   - `StartGame` - User starts a game
   - `ForceEndGame` - User force ends a game
   - `GetGameStatus` - Poll game status

2. **Menu Service** (future)
   - `StartGame` - Menu system starts a game

3. **Supervisor Service** (future)
   - `GetGameStatus` - Health check

## Related Services

### Also Have Client Instrumentation:

**WebUI** (`services/webui/server.py`)
- Calls: ControllerManager, Menu, Supervisor, GameCoordinator
- Instrumented with: `GrpcInstrumentorClient()`

### Server-Only Instrumentation:

**ControllerManager, Menu, Supervisor, Audio**
- Only receive requests, don't make gRPC client calls
- Instrumented with: `GrpcInstrumentorServer()` only

## Completed Enhancements

### Phase 36: Span Hierarchy Normalization ✓
- **Human-readable game names**: `Free-For-All`, `Teams`, `Random Teams`, `Nonstop Joust`
- **Consistent phase spans**: `game.phase.initialization`, `game.phase.countdown`, `gameplay_phase`, `game.phase.teardown`
- **Standardized lifecycle spans**: `player_{serial}_lifecycle`, `team_{number}_lifecycle`
- **Hierarchical team spans**: Team spans contain player spans for Teams/Random Teams

### Phase 36b: Base Game Class Refactoring (In Progress)
- **BaseGameMode**: Template method pattern for span orchestration
- **Code reduction**: ~1,500 lines of duplicate code eliminated (56% reduction)
- **Consistent behavior**: All game modes inherit same span creation logic
- **Easier maintenance**: Fix span issues once in base class

## Future Enhancements

### 1. Menu → GameCoordinator Traces
When Menu service calls `StartGame`, trace will show:
```
ProcessInput (Menu) → StartGame (GameCoordinator) → Free-For-All → ...
```

### 2. Audio Service Integration
When GameCoordinator calls Audio service:
```
game.phase.countdown → PlaySound (Audio) → audio.play
```

### 3. Multi-Game Sessions
Track multiple games in sequence:
```
StartGame (game1) → Free-For-All → ... → game_ended
StartGame (game2) → Teams → ... → game_ended
StartGame (game3) → Random Teams → ... → game_ended
```

### 4. Advanced Analytics
- Player behavior across multiple games (survival rates, warning frequencies)
- Team composition impact on win rate
- Optimal sensitivity thresholds by player
- Controller hardware reliability (death patterns)
- Game duration comparisons by mode

## References

- **OpenTelemetry gRPC Instrumentation**: https://opentelemetry-python-contrib.readthedocs.io/en/latest/instrumentation/grpc/grpc.html
- **Per-Player Tracing**: See `PER_PLAYER_TRACING.md` in this directory
- **Game Modes Architecture**: See `README.md` in this directory
