package decision

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/joustmania/agent/flags"
)

// Lever cooldown: the rules filter, the re-assert after the value TTL and the hold
// against a concurrent write (UC-040 A53, BR-046; #2641 review of #2746).

// UC: UC-040 A53 BR-046
func TestLeverCooldown_RulesSkipCoolingLevers(t *testing.T) {
	sc := func(d Decision) scoredCandidate { return scoredCandidate{candidate: candidate{decision: d}} }
	key := appliedKey(tempoDecision(""))
	t.Run("UC-040/A53 step 5 a top-ranked cooling lever takes no slot; the next candidate ranks", func(t *testing.T) {
		ranked := []scoredCandidate{sc(tempoDecision("1.0")), sc(globalDifficulty("1.25")), sc(budgetShield("S"))}
		cooling := map[string]coolingLever{key: {value: "1.3", reassert: true},
			appliedKey(globalDifficulty("")): {reassert: false}}
		got := applyCoolingPolicy(ranked, cooling)
		if len(got) != 1 || got[0].decision.Intervention != InterventionGrantShield {
			t.Fatalf("kept %v, want only the shield", got)
		}
	})
	t.Run("UC-040/BR-046 re-asserting the last written value is kept", func(t *testing.T) {
		got := applyCoolingPolicy([]scoredCandidate{sc(tempoDecision("1.30"))}, map[string]coolingLever{key: {value: "1.3", reassert: true}})
		if len(got) != 1 {
			t.Fatalf("kept %d, want the re-assert", len(got))
		}
		got = applyCoolingPolicy([]scoredCandidate{sc(tempoDecision("1.3"))}, map[string]coolingLever{key: {value: "1.3"}})
		if len(got) != 0 {
			t.Fatal("a busy lever must drop even its last value")
		}
	})
	for name, w := range tempoMixWeights {
		t.Run("UC-040/A53 step 5 loop: the rules never propose a cooling lever, "+name, func(t *testing.T) {
			snap := rulesSnapshot()
			snap.Objectives = w
			snap.Policy = flags.Policy{MaxInterventionsPerMinute: 20}
			snap.InterventionsAllowed = []string{InterventionAdjustMusicTempo, InterventionGrantShield, InterventionSendControllerEffect}
			l, _ := decideRecordingLoop(t, snap)
			clock := time.Unix(50_000, 0)
			l.now = func() time.Time { return clock }
			l.Actions = &capturingSink{}
			l.SetSuppressReapplied(true)
			cfg := DefaultStaticConfig()
			cfg.ObjectiveWeights = w
			rules := newObjectiveRules(cfg, cfg, cfg, func() time.Time { return clock }, rand.New(rand.NewPCG(1, 2)))
			rules.SetAppliedSource(l.AppliedValues)
			rules.SetCoolingSource(l.CoolingValues)
			l.Rules = rules
			for dur := 61.0; dur < 120; dur++ {
				for _, o := range runCycle(l, tempoMixCtx(dur)).Outcomes {
					if o.BlockReason == ReasonLeverCooldown {
						t.Fatalf("at %.0f s the rules proposed cooling %s", dur, o.Intervention)
					}
				}
				clock = clock.Add(time.Second)
			}
		})
	}
}

// UC: UC-040 A53 BR-046 A35
func TestLeverCooldown_ReassertAfterTTL(t *testing.T) {
	t.Run("UC-040/BR-046 with a 40 s window the last value is written again after the 30 s TTL", func(t *testing.T) {
		l, sink, clock, _ := appliedLoop(t, globalDifficulty("1.25"))
		l.SetEffectWindowSource(func() time.Duration { return 40 * time.Second })
		_ = runCycle(l, budgetCtx())
		*clock = clock.Add(appliedLedgerTTL)
		expectOutcome(t, runCycle(l, budgetCtx()), "adjust_global_difficulty/:dispatched")
		if n := writesOf(sink, InterventionAdjustGlobalDifficulty, ""); n != 2 {
			t.Fatalf("writes = %d, want 2", n)
		}
		*clock = clock.Add(time.Second)
		l.Rules = fakeRules{out: []Decision{globalDifficulty("1.625")}}
		expectOutcome(t, runCycle(l, budgetCtx()), "adjust_global_difficulty/:lever_cooldown")
	})
}

// UC: UC-040 A53
func TestLeverCooldown_BusyLever(t *testing.T) {
	t.Run("UC-040/A53 a write in flight refuses another value for the lever", func(t *testing.T) {
		var al appliedLedger
		now := time.Unix(1_000, 0)
		f := al.begin(globalDifficulty("1.25"))
		key := appliedKey(globalDifficulty(""))
		if cooling, first, _ := al.cooling(key, "1.625", now, time.Second); !cooling || !first {
			t.Fatalf("cooling = %v / %v while a write is in flight", cooling, first)
		}
		f.end(false, nil, now) // gated: nothing written
		if cooling, _, hold := al.cooling(key, "1.625", now, time.Second); cooling {
			t.Fatal("a gated write starts no window")
		} else {
			hold.release()
		}
	})
	t.Run("UC-040/A53 a lever held by a deciding decision is cooling until released", func(t *testing.T) {
		var al appliedLedger
		now := time.Unix(1_000, 0)
		key := appliedKey(globalDifficulty(""))
		_, _, hold := al.cooling(key, "1.25", now, time.Second)
		if cooling, _, _ := al.cooling(key, "1.625", now, time.Second); !cooling {
			t.Fatal("a held lever must cool")
		}
		hold.release()
		hold.release()
		if cooling, _, h := al.cooling(key, "1.625", now, time.Second); cooling || al.reserved[key] != 1 {
			t.Fatalf("after release: cooling %v, reserved %d", cooling, al.reserved[key])
		} else {
			h.release()
		}
	})
	t.Run("UC-040/A53 two concurrent decisions for one lever write once", func(t *testing.T) {
		l, _, clock, _ := appliedLoop(t)
		sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
		l.Actions = sink
		snap := l.Flags.Evaluate(context.Background())
		c := budgetCtx()
		done := make(chan struct{})
		go func() {
			defer close(done)
			var st LayerState
			l.runDecision(context.Background(), snap, c, globalDifficulty("1.25"), *clock, &st)
		}()
		<-sink.entered
		var st LayerState
		l.runDecision(context.Background(), snap, c, globalDifficulty("1.625"), *clock, &st)
		close(sink.release)
		<-done
		if got := outcomeSummary(st); !equalStrings(got, []string{"adjust_global_difficulty/:lever_cooldown"}) {
			t.Fatalf("concurrent outcomes = %v, want lever_cooldown", got)
		}
		if n := writesOf(&sink.capturingSink, InterventionAdjustGlobalDifficulty, ""); n != 1 {
			t.Fatalf("writes = %d, want 1", n)
		}
	})
}
