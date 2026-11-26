package decision

import (
	"sort"
	"sync"
	"time"

	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// FlagState is the effective flag set of one game partition as its newest evaluated
// cycle resolved it: the flag panel of the dashboard's Agent tab reads it from
// GET /experiments/state (docs/specs/services.md §5). Recorded on every evaluated
// cycle, before the kill-switch short-circuit, so enabled=false is visible.
type FlagState struct {
	GameID   string `json:"game_id"`
	GameKind string `json:"game_kind"`
	// GameMode is the ingested mode, "" when unknown.
	GameMode string `json:"game_mode"`
	// TraceID is the game's trace (the Jaeger drill-down), "" before a game.start or
	// game.phase.start log record gave it.
	TraceID string `json:"trace_id"`
	// UpdatedAtMs is the cycle's time, epoch ms on the agent's clock.
	UpdatedAtMs            int64    `json:"updated_at_ms"`
	Enabled                bool     `json:"enabled"`
	Objectives             string   `json:"objectives"`
	InterventionsAllowed   string   `json:"interventions_allowed"`
	InterventionsEnabled   bool     `json:"interventions_enabled"`
	BatteryThreshold       int      `json:"battery_threshold"`
	MaxInterventionsPerMin int      `json:"max_interventions_per_minute"`
	DecisionBackend        string   `json:"decision_backend"`
	// ActConfidenceThreshold is the selected backend's own threshold; nil on rules,
	// which routes nothing.
	ActConfidenceThreshold *float64 `json:"act_confidence_threshold,omitempty"`
	ExperimentsEnabled     bool     `json:"experiments_enabled"`
	LLMEligibleGameKinds   []string `json:"llm_eligible_game_kinds"`
}

// MaxFlagStates bounds the games a FlagBoard keeps per class, shadow and every
// other kind (the newest by cycle time), so shadow games never evict the stage game.
const MaxFlagStates = 20

// FlagBoard holds the newest FlagState per game partition, shared by every Loop.
// Safe for concurrent use; a nil board records nothing.
type FlagBoard struct {
	mu     sync.Mutex
	byGame map[string]FlagState
}

// NewFlagBoard returns an empty board.
func NewFlagBoard() *FlagBoard { return &FlagBoard{byGame: map[string]FlagState{}} }

func (b *FlagBoard) record(s FlagState) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.byGame[s.GameID] = s
	shadow := isShadowKind(s.GameKind)
	n, oldest := 0, ""
	for id, held := range b.byGame {
		if isShadowKind(held.GameKind) != shadow {
			continue
		}
		n++
		if oldest == "" || held.UpdatedAtMs < b.byGame[oldest].UpdatedAtMs {
			oldest = id
		}
	}
	if n > MaxFlagStates {
		delete(b.byGame, oldest)
	}
}

func isShadowKind(kind string) bool { return kind == gamecontext.GameKindShadow }

// List returns the held flag states, newest first.
func (b *FlagBoard) List() []FlagState {
	if b == nil {
		return []FlagState{}
	}
	b.mu.Lock()
	out := make([]FlagState, 0, len(b.byGame))
	for _, s := range b.byGame {
		out = append(out, s)
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAtMs != out[j].UpdatedAtMs {
			return out[i].UpdatedAtMs > out[j].UpdatedAtMs
		}
		return out[i].GameID < out[j].GameID
	})
	return out
}

// SetFlagBoard wires the shared board this partition records its flag state on.
func (l *Loop) SetFlagBoard(b *FlagBoard) { l.flagBoard = b }

// recordFlagState stores this cycle's effective flag set for the partition.
func (l *Loop) recordFlagState(snapshot flags.Snapshot, c gamecontext.GameContext, at time.Time) {
	if l.flagBoard == nil {
		return
	}
	state := newLayerState(snapshot)
	eligible := append([]string{}, snapshot.LLMGate.EligibleGameKinds...)
	backend := requestedBackend(snapshot)
	var threshold *float64
	if thr, ok := snapshot.Decision.ActThreshold(backend); ok {
		threshold = &thr
	}
	l.flagBoard.record(FlagState{
		GameID:                 c.SessionID,
		GameKind:               c.GameKind,
		GameMode:               derefStr(c.Session.GameMode),
		TraceID:                c.GameTraceID,
		UpdatedAtMs:            at.UnixMilli(),
		Enabled:                state.Enabled,
		Objectives:             summarizeObjectives(state.Objectives),
		InterventionsAllowed:   allowedSummary(state.InterventionsAllowed),
		InterventionsEnabled:   snapshot.InterventionsEnabled,
		BatteryThreshold:       state.PolicyBatteryThreshold,
		MaxInterventionsPerMin: state.PolicyMaxPerMinute,
		DecisionBackend:        backend,
		ActConfidenceThreshold: threshold,
		ExperimentsEnabled:     snapshot.ExperimentsEnabled,
		LLMEligibleGameKinds:   eligible,
	})
}
