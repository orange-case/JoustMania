package experiment

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/joustmania/agent/experiment/journal"
	"github.com/joustmania/agent/patterngame"
)

// holdPin is a two-archetype pin; burst lists which archetypes press the trigger.
func holdPin(t *testing.T, burst ...bool) patterngame.Pin {
	t.Helper()
	pin := testPin(t, "calm", "chaotic")
	for i, b := range burst {
		if b {
			pin.Archetypes[i].TriggerPolicy = patterngame.TriggerPolicy{Kind: patterngame.TriggerPolicyBurst}
		}
	}
	return pin
}

// switchGate is a HoldGate whose answer the test changes between ticks.
type switchGate struct {
	mu     sync.Mutex
	reason string
	err    error
	calls  int
}

func (g *switchGate) set(reason string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reason, g.err = reason, err
}

func (g *switchGate) gate(_ context.Context, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.reason, g.err
}

func startHold(t *testing.T, r *Registry, flagKey string, value any) string {
	t.Helper()
	in := sampleIntent()
	in.FlagKey, in.ExperimentalValue = flagKey, value
	id, err := r.Declare(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func lastOutcomeNote(t *testing.T, r *Registry, id string) string {
	t.Helper()
	outcomes := eventsOfKind(journalEvents(t, r, id), journal.KindOutcome)
	if len(outcomes) == 0 {
		return ""
	}
	return outcomes[len(outcomes)-1].Note
}

// UC: UC-043 A17
func TestDeclareRefusesActivationAwayFromHold(t *testing.T) {
	r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: &patternSpawner{}})
	for _, v := range []any{"instant", "trigger", 1.0} {
		t.Run("UC-043/A17 shield.activation toward "+strings.TrimSpace(fmtAny(v))+" is refused", func(t *testing.T) {
			in := sampleIntent()
			in.FlagKey, in.ExperimentalValue = HoldActivationKey, v
			if _, err := r.Declare(in); err == nil {
				t.Fatal("declared")
			}
		})
	}
	t.Run("UC-043/A17 shield.activation toward hold is declared", func(t *testing.T) {
		in := sampleIntent()
		in.FlagKey, in.ExperimentalValue = HoldActivationKey, HoldValue
		if _, err := r.Declare(in); err != nil {
			t.Fatal(err)
		}
	})
}

func fmtAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "a number"
}

// UC: UC-046 A17
func TestRegistryHoldPinDiscards(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-046/A17 an archetype without a trigger policy discards a hold experiment", func(t *testing.T) {
		spawner := &patternSpawner{}
		r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: spawner,
			Patterns: &queuePinner{pins: []patterngame.Pin{holdPin(t, true, false)}}})
		id := startHold(t, r, HoldActivationKey, HoldValue)
		if n := r.AllocateAndSpawn(ctx); n != 0 || spawner.count() != 0 {
			t.Fatalf("spawned %d", n)
		}
		if st, _ := r.Status(id); st != StatusDiscarded {
			t.Fatalf("status = %s, want discarded", st)
		}
		if note := lastOutcomeNote(t, r, id); !strings.HasPrefix(note, patterngame.DiscardTriggerPolicyMissing) || !strings.Contains(note, "chaotic") {
			t.Fatalf("outcome note %q", note)
		}
	})
	t.Run("UC-046/A17 the same selection runs an experiment on another flag", func(t *testing.T) {
		spawner := &patternSpawner{}
		r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: spawner,
			Patterns: &queuePinner{pins: []patterngame.Pin{holdPin(t, true, false)}}})
		id := startExperiment(t, r)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusRunning || spawner.count() == 0 {
			t.Fatalf("status = %s spawns %d, want running with spawns", st, spawner.count())
		}
	})
	t.Run("UC-043/A17 a hold sub-parameter under a non-hold activation is discarded at its pin", func(t *testing.T) {
		gate := &switchGate{reason: DiscardHoldNotRealDefault}
		spawner := &patternSpawner{}
		r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: spawner, HoldGate: gate.gate,
			Patterns: &queuePinner{pins: []patterngame.Pin{holdPin(t, true, true)}}})
		id := startHold(t, r, "shield.energy_budget_seconds", 20.0)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusDiscarded || spawner.count() != 0 {
			t.Fatalf("status = %s spawns %d", st, spawner.count())
		}
		if note := lastOutcomeNote(t, r, id); !strings.HasPrefix(note, DiscardHoldNotRealDefault) {
			t.Fatalf("outcome note %q", note)
		}
	})
	t.Run("UC-043/A17 a read error leaves the experiment unpinned until the next tick", func(t *testing.T) {
		gate := &switchGate{err: errors.New("flag service down")}
		spawner := &patternSpawner{}
		r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: spawner, HoldGate: gate.gate,
			Patterns: &queuePinner{pins: []patterngame.Pin{holdPin(t, true, true)}}})
		id := startHold(t, r, "shield.energy_budget_seconds", 20.0)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusRunning || spawner.count() != 0 {
			t.Fatalf("status = %s spawns %d, want running, nothing spawned", st, spawner.count())
		}
		gate.set("", nil)
		r.AllocateAndSpawn(ctx)
		if spawner.count() == 0 {
			t.Fatal("no spawn once the gate reads again")
		}
	})
}

// UC: UC-043 A17
func TestRegistryHoldStaleDiscard(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-043/A17 a running sub-parameter experiment ends once activation leaves hold", func(t *testing.T) {
		gate := &switchGate{}
		spawner := &patternSpawner{}
		r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: spawner, HoldGate: gate.gate,
			Patterns: &queuePinner{pins: []patterngame.Pin{holdPin(t, true, true)}}})
		id := startHold(t, r, "shield.energy_budget_seconds", 20.0)
		other := startExperiment(t, r)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusRunning {
			t.Fatalf("status = %s, want running", st)
		}
		gate.set("", errors.New("transient"))
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusRunning {
			t.Fatalf("a read error discarded it: %s", st)
		}
		gate.set(DiscardHoldNotRealDefault, nil)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusDiscarded {
			t.Fatalf("status = %s, want discarded", st)
		}
		if note := lastOutcomeNote(t, r, id); !strings.HasPrefix(note, DiscardHoldNotRealDefault) {
			t.Fatalf("outcome note %q", note)
		}
		if st, _ := r.Status(other); st != StatusRunning {
			t.Fatalf("the non-hold experiment = %s, want running", st)
		}
	})
	t.Run("UC-043/A17 a running shield.activation experiment ends once hold is resolved", func(t *testing.T) {
		gate := &switchGate{}
		r := newTestRegistry(t, RegistryConfig{MaxShadowGames: 2, Spawner: &patternSpawner{}, HoldGate: gate.gate,
			Patterns: &queuePinner{pins: []patterngame.Pin{holdPin(t, true, true)}}})
		id := startHold(t, r, HoldActivationKey, HoldValue)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusRunning {
			t.Fatalf("status = %s, want running", st)
		}
		gate.set(DiscardHoldAlreadyDefault, nil)
		r.AllocateAndSpawn(ctx)
		if st, _ := r.Status(id); st != StatusDiscarded {
			t.Fatalf("status = %s, want discarded", st)
		}
		if note := lastOutcomeNote(t, r, id); !strings.HasPrefix(note, DiscardHoldAlreadyDefault) {
			t.Fatalf("outcome note %q", note)
		}
	})
}

// UC: UC-043 A17
func TestRegistryLaterPromotionOrder(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-043/A17 a later outcome journaled during the promotion follows its own event", func(t *testing.T) {
		promo := &fakePromoter{result: PromotionResult{Outcome: PromotionApplied}}
		var r *Registry
		var id string
		promo.during = func() {
			// The kill switch journals before the promotion returned.
			r.RecordPromotion(id, PromotionResult{Outcome: PromotionReverted, Reason: "kill_switch"})
		}
		spawner := &fakeSpawner{}
		r = newTestRegistry(t, RegistryConfig{
			Root: t.TempDir(), MaxShadowGames: 4, Spawner: spawner,
			Verdict:  fakeVerdict{outcome: CohortOutcomePromote, concludeAtN: 2, alwaysReport: true},
			Promoter: promo, Targeting: &fakeTargeting{},
		})
		id = startHold(t, r, HoldActivationKey, HoldValue)
		r.AllocateAndSpawn(ctx)
		for _, g := range append([]spawnCall(nil), spawner.calls...)[:2] {
			if _, err := r.ConcludeGame(ctx, g.gameID, 0.7, 120); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for _, e := range eventsOfKind(journalEvents(t, r, id), journal.KindPromotion) {
			got = append(got, e.Promotion.Outcome)
		}
		if strings.Join(got, ",") != "applied,reverted" {
			t.Fatalf("promotion events = %v, want [applied reverted]", got)
		}
	})
}

// UC: UC-043 A15
func TestRehydrateWarnsAboutLegacyQueuedRevert(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-043/A15 a revert an older agent queued is not run; Rehydrate logs one WARN", func(t *testing.T) {
		root := t.TempDir()
		spawner := &fakeSpawner{}
		r := newTestRegistry(t, RegistryConfig{
			Root: root, MaxShadowGames: 4, Spawner: spawner,
			Verdict:  fakeVerdict{outcome: CohortOutcomePromote, concludeAtN: 2, alwaysReport: true},
			Promoter: &fakePromoter{result: PromotionResult{Outcome: PromotionApplied, Reason: "revert deferred", RevertPending: true}}, Targeting: &fakeTargeting{},
		})
		id := startHold(t, r, "shield.energy_budget_seconds", 20.0)
		r.AllocateAndSpawn(ctx)
		for _, g := range append([]spawnCall(nil), spawner.calls...)[:2] {
			if _, err := r.ConcludeGame(ctx, g.gameID, 0.7, 120); err != nil {
				t.Fatal(err)
			}
		}
		var logs bytes.Buffer
		r2 := newTestRegistry(t, RegistryConfig{Root: root, MaxShadowGames: 4, Spawner: &fakeSpawner{}, Log: slog.New(slog.NewTextHandler(&logs, nil))})
		if err := r2.Rehydrate([]string{id}); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(logs.String(), "queued revert from an older agent was not run"); n != 1 {
			t.Fatalf("WARNs = %d, want 1:\n%s", n, logs.String())
		}
	})
}
