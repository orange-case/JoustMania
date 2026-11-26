package flags

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// game_test.go covers the agent's read-only game domain and its per-game evaluation
// context (GameContext).

func gameFlags(game Evaluator, log *slog.Logger) *Flags {
	f := New(stubEvaluator{strings: map[string]string{"shield.activation": "trigger"}}, log)
	f.game = game
	return f
}

// gameTrigger reads shield.activation from the game domain in the game's context: a
// probe of the domain's sync, which no agent decision reads any more.
func gameTrigger(ctx context.Context, f *Flags, gameID string) bool {
	mode, err := f.game.StringValue(ctx, "shield.activation", "instant", f.GameContext(gameID))
	return err == nil && strings.ToLower(strings.TrimSpace(mode)) == "trigger"
}

// UC: UC-040 BR-036
func TestGameContext_CoordinatorSubset(t *testing.T) {
	t.Run("UC-040/BR-036 a real game reads with game_kind=real and gameId, no targetingKey or game_mode", func(t *testing.T) {
		ec := gameFlags(nil, nil).GameContext("g-42")
		attrs := ec.Attributes()
		if ec.TargetingKey() != "" || len(attrs) != 2 || attrs["game_kind"] != "real" || attrs["gameId"] != "g-42" {
			t.Fatalf("context = key %q attrs %v, want game_kind=real and gameId only", ec.TargetingKey(), attrs)
		}
	})
}

// UC: UC-043 A17
func TestGameContext_ExperimentArm(t *testing.T) {
	t.Run("UC-043/A17 a shadow arm game reads with its kind, experiment_id and arm", func(t *testing.T) {
		f := gameFlags(nil, nil)
		f.SetGameAttrs(func(gameID string) GameAttrs {
			if gameID != "g-7" {
				t.Fatalf("lookup for %q", gameID)
			}
			return GameAttrs{GameKind: "shadow", ExperimentID: "exp_1", Arm: "experimental"}
		})
		attrs := f.GameContext("g-7").Attributes()
		if len(attrs) != 4 || attrs["game_kind"] != "shadow" || attrs["experiment_id"] != "exp_1" ||
			attrs["arm"] != "experimental" || attrs["gameId"] != "g-7" {
			t.Fatalf("attrs %v, want shadow arm context", attrs)
		}
	})
	t.Run("UC-043/A17 an unseen kind reads as real", func(t *testing.T) {
		f := gameFlags(nil, nil)
		f.SetGameAttrs(func(string) GameAttrs { return GameAttrs{} })
		if attrs := f.GameContext("g-8").Attributes(); len(attrs) != 2 || attrs["game_kind"] != "real" {
			t.Fatalf("attrs %v, want game_kind=real and gameId", attrs)
		}
	})
}

// UC: UC-040 A31
// UC: UC-045 A3
func TestSetupFlagd_GameDomain(t *testing.T) {
	t.Run("UC-040/A31 the game set is its own domain: default before its sync, served after", func(t *testing.T) {
		agentSrc, gameSrc := newFakeFlagdSource("on"), newFakeFlagdSource("on")
		agentSrc.set(true, "on")
		ctx, cancel := context.WithCancel(context.Background())
		f, _, shutdown, err := setupFlagd(ctx, fakeFactory(agentSrc), fakeFactory(gameSrc), nil, testSyncTiming, nil)
		if err != nil {
			t.Fatalf("setupFlagd: %v", err)
		}
		t.Cleanup(func() { cancel(); shutdown() })
		read := func() bool { return gameTrigger(context.Background(), f, "g-1") }
		// The agent domain syncs, the game domain does not yet: the game read fails open.
		waitBool(t, "enabled (agent domain)", func() bool { return f.Evaluate(context.Background()).Enabled }, true, 5*time.Second)
		if read() {
			t.Fatal("shield.activation read trigger before the game domain synced")
		}
		gameSrc.set(true, "on")
		waitBool(t, "shield.activation (game domain)", read, true, 5*time.Second)
		if expiry, err := f.game.FloatValue(context.Background(), "shield.charge_expiry_seconds", 20, f.GameContext("g-1")); err != nil || expiry != 10 {
			t.Fatalf("expiry = %v (%v), want 10 from the game set", expiry, err)
		}
		gameSrc.set(true, "off")
		waitBool(t, "shield.activation flipped to instant", read, false, 5*time.Second)
	})
}
