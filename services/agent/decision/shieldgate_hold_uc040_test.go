package decision

import (
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// shieldgate_hold_uc040_test.go covers the shield gate in a hold game (UC-040 A37
// step 1, BR-022, BR-024; decide-contract §4.5 step 4): no already_charged;
// shield_budget_exhausted and shield_pool_full from the coordinator's gauges before
// the cap, the cooldown and the rate limit; skipped for an active shield; fail open
// without the gauges; the rules' no-shield filter drops such players; repeats are quiet.

type holdPlayer struct {
	energy, budget *float64
	active         bool
}

// holdCtx is ffaRealCtx latched to hold (cap 5 s, budget 15 s, minimum burst 0.3 s)
// with the given pools for WEAK and S.
func holdCtx(weak, s holdPlayer) gamecontext.GameContext {
	c := ffaRealCtx()
	mode := gamecontext.ShieldActivationHold
	c.Session.ShieldActivation = &mode
	c.Session.ShieldSettings = map[string]float64{
		gamecontext.ShieldSettingEnergyMax:    5,
		gamecontext.ShieldSettingEnergyBudget: 15,
		gamecontext.ShieldSettingMinBurst:     0.3,
	}
	for serial, hp := range map[string]holdPlayer{"WEAK": weak, "S": s} {
		p := *c.Players[serial]
		p.ShieldEnergy, p.ShieldBudgetRemaining = hp.energy, hp.budget
		active := hp.active
		p.ShieldActive = &active
		c.Players[serial] = &p
	}
	return c
}

// withHold latches c to hold without pools.
func withHold(c gamecontext.GameContext) gamecontext.GameContext {
	mode := gamecontext.ShieldActivationHold
	c.Session.ShieldActivation = &mode
	return c
}

// UC: UC-040 A37 BR-022 BR-024
func TestHoldLedger_Refusals(t *testing.T) {
	roomy := holdPlayer{energy: fptr(1), budget: fptr(12)}
	for _, tc := range []struct {
		name string
		weak holdPlayer
		want string
	}{
		{"UC-040/A37 a pool within 0.5 s of the cap is shield_pool_full", holdPlayer{energy: fptr(4.6), budget: fptr(12)}, "shield_pool_full"},
		{"UC-040/A37 full is min(cap, budget left)", holdPlayer{energy: fptr(1.6), budget: fptr(2)}, "shield_pool_full"},
		{"UC-040/A37 a budget left below the minimum burst is shield_budget_exhausted", holdPlayer{energy: fptr(0), budget: fptr(0.2)}, "shield_budget_exhausted"},
		{"UC-040/A37 the budget is checked before the pool", holdPlayer{energy: fptr(5), budget: fptr(0.1)}, "shield_budget_exhausted"},
		{"UC-040/A37 an active shield skips the pool check", holdPlayer{energy: fptr(5), budget: fptr(12), active: true}, "dispatched"},
		{"UC-040/A37 an active shield keeps the budget check", holdPlayer{energy: fptr(5), budget: fptr(0.1), active: true}, "shield_budget_exhausted"},
		{"UC-040/A37 missing gauges fail open", holdPlayer{}, "dispatched"},
		{"UC-040/A36 room in the pool is a grant", roomy, "dispatched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, sink, _, _ := budgetLoop(t, 100, budgetShield("WEAK"))
			got := outcomeSummary(runCycle(l, holdCtx(tc.weak, roomy)))
			if want := []string{"grant_shield/WEAK:" + tc.want}; !equalStrings(got, want) {
				t.Fatalf("outcomes = %v, want %v", got, want)
			}
			if tc.want != "dispatched" && shieldWritesTo(sink, "WEAK") != 0 {
				t.Fatal("a refused grant was written")
			}
		})
	}
}

// UC: UC-040 A37 BR-022
func TestHoldLedger_NoAlreadyChargedAndNoBudgetCharge(t *testing.T) {
	t.Run("UC-040/A37 a second grant is not already_charged in a hold game, and a refusal draws no budget", func(t *testing.T) {
		shieldCost := int(interventionCost(InterventionGrantShield))
		l, _, clock, _ := budgetLoop(t, 2*shieldCost, budgetShield("WEAK"))
		roomy := holdPlayer{energy: fptr(0), budget: fptr(15)}
		if got := outcomeSummary(runCycle(l, holdCtx(roomy, roomy))); !equalStrings(got, []string{"grant_shield/WEAK:dispatched"}) {
			t.Fatalf("first grant = %v, want dispatched", got)
		}
		*clock = clock.Add(5 * time.Second)
		full := holdPlayer{energy: fptr(5), budget: fptr(15)}
		if got := outcomeSummary(runCycle(l, holdCtx(full, roomy))); !equalStrings(got, []string{"grant_shield/WEAK:shield_pool_full"}) {
			t.Fatalf("grant into a full pool = %v, want shield_pool_full", got)
		}
		*clock = clock.Add(time.Second)
		if got := outcomeSummary(runCycle(l, holdCtx(roomy, roomy))); !equalStrings(got, []string{"grant_shield/WEAK:dispatched"}) {
			t.Fatalf("grant after the pool drained = %v, want dispatched (no already_charged, the refusal drew no budget)", got)
		}
	})
}

// UC: UC-040 A37 A21
func TestHoldLedger_RepeatRefusalQuiet(t *testing.T) {
	t.Run("UC-040/A37 only the first shield_pool_full per entry is emitted; repeats log at debug", func(t *testing.T) {
		l, _, clock, decisionSpans := budgetLoop(t, 100, budgetShield("WEAK"))
		logs := &syncBuffer{}
		l.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		full := holdCtx(holdPlayer{energy: fptr(5), budget: fptr(10)}, holdPlayer{})
		const refusals = 3
		for i := 0; i < refusals; i++ {
			*clock = clock.Add(2 * time.Second)
			_ = runCycle(l, full)
		}
		if n := decisionSpans(); n != 1 {
			t.Fatalf("agent.decision spans = %d, want 1 (the first refusal)", n)
		}
		if n := logs.count("level=DEBUG msg=agent.decision_shield_pool_full_repeat"); n != refusals-1 {
			t.Errorf("repeat debug lines = %d, want %d", n, refusals-1)
		}
	})
}

// UC: UC-040 A37 BR-022
func TestHoldLedger_RulesSkipRefusedPlayers(t *testing.T) {
	t.Run("UC-040/A37 R4 does not propose a shield to a player whose pool is full", func(t *testing.T) {
		snap := rulesSnapshot()
		snap.Objectives = balancedFocused()
		snap.Policy = flags.Policy{MaxInterventionsPerMinute: 50}
		l, _ := decideRecordingLoop(t, snap)
		clock := time.Unix(50_000, 0)
		l.now = func() time.Time { return clock }
		sink := &capturingSink{}
		l.Actions = sink
		rules := newObjectiveRules(balancedRulesConfig(), balancedRulesConfig(), balancedRulesConfig(),
			func() time.Time { return clock }, rand.New(rand.NewPCG(1, 2)))
		rules.SetShieldSource(l.ShieldSerials)
		l.Rules = rules

		c := chargeRulesCtx()
		mode := gamecontext.ShieldActivationHold
		c.Session.ShieldActivation = &mode
		c.Session.ShieldSettings = map[string]float64{gamecontext.ShieldSettingEnergyMax: 5, gamecontext.ShieldSettingMinBurst: 0.3}
		for serial, p := range c.Players {
			cp := *p
			cp.ShieldEnergy, cp.ShieldBudgetRemaining = fptr(5), fptr(10)
			c.Players[serial] = &cp
		}
		for i := 0; i < 5; i++ {
			for _, o := range runCycle(l, c).Outcomes {
				if o.Intervention == InterventionGrantShield {
					t.Fatalf("cycle %d: the rules proposed a shield into a full pool (%s/%v)", i, o.TargetSerial, o.BlockReason)
				}
			}
			clock = clock.Add(5 * time.Second)
		}
	})
}
