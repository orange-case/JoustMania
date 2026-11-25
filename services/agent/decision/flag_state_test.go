package decision

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// flag_state_test.go covers the FlagBoard behind GET /experiments/state: every
// evaluated cycle records the partition's effective flag set, before the kill switch,
// and no span carries it.

func modeCtx(gameID, mode string) gamecontext.GameContext {
	c := gamecontext.GameContext{SessionID: gameID, GameKind: gamecontext.GameKindReal}
	c.Session.GameMode = &mode
	return c
}

func boardLoop(t *testing.T) (*Loop, *FlagBoard, *settableFlags) {
	t.Helper()
	l, _, fl := newTestLoop(t)
	b := NewFlagBoard()
	l.SetFlagBoard(b)
	return l, b, fl
}

// UC: UC-035 3
func TestFlagState_NewestCycleWins(t *testing.T) {
	t.Run("UC-035/3 a flag change shows on the next cycle", func(t *testing.T) {
		l, b, fl := boardLoop(t)
		c := modeCtx("game_fs01", "FFA")
		l.OnEvaluate(context.Background(), c, testTrigger())
		fl.snap.Policy.BatteryThreshold = 3
		l.OnEvaluate(context.Background(), modeCtx("game_fs01", "Teams"), testTrigger())
		got := b.List()
		if len(got) != 1 {
			t.Fatalf("flag states = %d, want 1 per game", len(got))
		}
		if got[0].BatteryThreshold != 3 || got[0].GameMode != "Teams" {
			t.Errorf("flag state = %+v, want battery 3 and mode Teams", got[0])
		}
	})
}

// UC: UC-035 3
func TestFlagState_Fields(t *testing.T) {
	l, b, fl := boardLoop(t)
	at := time.UnixMilli(1_700_000_000_123)
	l.now = func() time.Time { return at }
	fl.snap.Objectives = map[string]float64{"endurance": 0.7, "balanced": 0.3}
	fl.snap.InterventionsAllowed = []string{"grant_shield", "noop"}
	fl.snap.InterventionsEnabled = true
	fl.snap.ExperimentsEnabled = true
	fl.snap.LLMGate.EligibleGameKinds = []string{"real", "shadow"}
	fl.snap.Policy = flags.Policy{BatteryThreshold: 2, MaxInterventionsPerMinute: 6}
	fl.snap.Decision = flags.DecisionBackend{Backend: flags.DecisionBackendJev, ActConfidenceThreshold: 0.6}
	c := gameCycleCtx("game_fs02")
	mode := "FFA"
	c.Session.GameMode = &mode
	l.OnEvaluate(context.Background(), c, testTrigger())

	want := FlagState{
		GameID:                 "game_fs02",
		GameKind:               c.GameKind,
		GameMode:               "FFA",
		TraceID:                c.GameTraceID,
		UpdatedAtMs:            at.UnixMilli(),
		Enabled:                true,
		Objectives:             "balanced=0.3,endurance=0.7",
		InterventionsAllowed:   "grant_shield,noop",
		InterventionsEnabled:   true,
		BatteryThreshold:       2,
		MaxInterventionsPerMin: 6,
		DecisionBackend:        flags.DecisionBackendJev,
		ActConfidenceThreshold: 0.6,
		ExperimentsEnabled:     true,
		LLMEligibleGameKinds:   []string{"real", "shadow"},
	}
	got := b.List()
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("flag state = %+v, want %+v", got, want)
	}
}

// UC: UC-045 BR-001
func TestFlagState_RecordedWhenKillSwitchOff(t *testing.T) {
	l, b, fl := boardLoop(t)
	fl.snap.Enabled = false
	l.OnEvaluate(context.Background(), modeCtx("game_fs03", "FFA"), testTrigger())
	got := b.List()
	if len(got) != 1 || got[0].Enabled {
		t.Fatalf("UC-045/BR-001 flag state with the kill switch off = %+v, want one with enabled=false", got)
	}
}

// UC: UC-035 3
func TestFlagBoard_KeepsTheNewestGames(t *testing.T) {
	b := NewFlagBoard()
	for i := 0; i < MaxFlagStates+3; i++ {
		b.record(FlagState{GameID: fmt.Sprintf("game_%02d", i), UpdatedAtMs: int64(1000 + i)})
	}
	got := b.List()
	if len(got) != MaxFlagStates {
		t.Fatalf("flag states = %d, want %d", len(got), MaxFlagStates)
	}
	if got[0].GameID != fmt.Sprintf("game_%02d", MaxFlagStates+2) || got[len(got)-1].GameID != "game_03" {
		t.Errorf("kept %s..%s, want the newest %d games newest first", got[0].GameID, got[len(got)-1].GameID, MaxFlagStates)
	}
	var nilBoard *FlagBoard
	nilBoard.record(FlagState{GameID: "x"}) // nil-safe
	if len(nilBoard.List()) != 0 {
		t.Error("a nil board lists nothing")
	}
}
