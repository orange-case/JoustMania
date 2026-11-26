package decision

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/gamecontext"
)

// shieldgate_uc040_test.go covers the shield gate (UC-040 A31, A33, A37; decide-contract
// §4.5 step 4): a targeted grant_shield is refused from the coordinator's
// game_player_shield_gate gauges before the rate limit, the views feed the decide state
// and the rules, and missing gauges refuse nothing.

func runCycle(l *Loop, c gamecontext.GameContext) LayerState {
	return l.OnEvaluate(context.Background(), c, testTrigger())
}

// ffaRealCtx is an active real Free-for-All game with players WEAK and S.
func ffaRealCtx() gamecontext.GameContext {
	c := budgetCtx()
	c.GameKind = gamecontext.GameKindReal
	mode := "FFA"
	c.Session.GameMode = &mode
	return c
}

// withGate returns c with serial's shield gate set as the coordinator would export it.
func withGate(c gamecontext.GameContext, serial string, g gamecontext.ShieldGate) gamecontext.GameContext {
	gates := make(map[string]gamecontext.ShieldGate, len(c.Session.ShieldGate)+1)
	for k, v := range c.Session.ShieldGate {
		gates[k] = v
	}
	gates[serial] = g
	c.Session.ShieldGate = gates
	return c
}

// limitLoop is budgetLoop with the shield limits set in its snapshot.
func limitLoop(t *testing.T, budget, maxShields, cooldownSeconds int, out ...Decision) (*Loop, *capturingSink, *time.Time, func() int) {
	t.Helper()
	l, sink, clock, spans := budgetLoop(t, budget, out...)
	snap := l.Flags.Evaluate(context.Background())
	snap.Policy.MaxShieldsPerPlayerPerGame = maxShields
	snap.Policy.ShieldCooldownSeconds = cooldownSeconds
	l.Flags = fakeFlags{snap: snap}
	return l, sink, clock, spans
}

// limiterEntries counts the budget entries in the window at now (pruned first: the
// repeat check of every refusal prunes too).
func limiterEntries(l *Loop, now time.Time) int {
	l.limiter.mu.Lock()
	defer l.limiter.mu.Unlock()
	l.limiter.prune(now)
	return len(l.limiter.entries)
}

func shieldWritesTo(sink *capturingSink, serial string) int {
	n := 0
	for _, d := range sink.decisions() {
		if d.Intervention == InterventionGrantShield && d.TargetSerial == serial {
			n++
		}
	}
	return n
}

func chargedIn(t *testing.T, st decide.State, serial string) bool {
	t.Helper()
	return playerIn(t, st, serial).ShieldCharged
}

// UC: UC-040 A31 A33 BR-020 BR-022
func TestShieldGate_Refusals(t *testing.T) {
	now := time.Unix(50_000, 0)
	for _, tc := range []struct {
		name string
		gate gamecontext.ShieldGate
		max  int
		want BlockReason
	}{
		{"UC-040/A31 a held charge is already_charged", gamecontext.ShieldGate{Charged: true, Grants: 5}, 3, ReasonAlreadyCharged},
		{"UC-040/A33 grants at the cap", gamecontext.ShieldGate{Grants: 3}, 3, ReasonShieldCapReached},
		{"UC-040/A33 the cap 0 is unlimited", gamecontext.ShieldGate{Grants: 30}, 0, ""},
		{"UC-040/A33 before the cooldown end", gamecontext.ShieldGate{Grants: 1, CooldownUntil: now.Add(time.Second)}, 3, ReasonShieldCooldown},
		{"UC-040/A33 at the cooldown end", gamecontext.ShieldGate{Grants: 1, CooldownUntil: now}, 3, ""},
		{"UC-040/A33 the cap before the cooldown", gamecontext.ShieldGate{Grants: 3, CooldownUntil: now.Add(time.Second)}, 3, ReasonShieldCapReached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := withGate(ffaRealCtx(), "WEAK", tc.gate)
			if got := shieldGateRefusal(c, "WEAK", shieldLimits{maxShields: tc.max}, now); got != tc.want {
				t.Fatalf("refusal = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("UC-040/A31 no gauge refuses nothing", func(t *testing.T) {
		if got := shieldGateRefusal(ffaRealCtx(), "WEAK", shieldLimits{maxShields: 1}, now); got != "" {
			t.Fatalf("refusal = %q, want none", got)
		}
	})
	t.Run("UC-040/A37 a charge gauge in a hold game is not already_charged", func(t *testing.T) {
		c := withGate(withHold(ffaRealCtx()), "WEAK", gamecontext.ShieldGate{Charged: true})
		if got := shieldGateRefusal(c, "WEAK", shieldLimits{}, now); got != "" {
			t.Fatalf("refusal = %q, want none", got)
		}
	})
}

// UC: UC-040 A31 BR-020
func TestShieldGate_BlockedBeforeTheRateLimit(t *testing.T) {
	t.Run("UC-040/A31 a charged target is already_charged, not written and charges no budget", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100, budgetShield("WEAK"))
		c := withGate(ffaRealCtx(), "WEAK", gamecontext.ShieldGate{Charged: true})
		expectOutcome(t, runCycle(l, c), "grant_shield/WEAK:already_charged")
		if n := len(sink.decisions()); n != 0 {
			t.Fatalf("writes = %d, want 0", n)
		}
		if n := limiterEntries(l, *clock); n != 0 {
			t.Fatalf("budget entries = %d, want 0", n)
		}
	})
	t.Run("UC-040/A33 a capped target is shield_cap_reached", func(t *testing.T) {
		l, sink, _, _ := limitLoop(t, 100, 2, 0, budgetShield("WEAK"))
		c := withGate(ffaRealCtx(), "WEAK", gamecontext.ShieldGate{Grants: 2})
		expectOutcome(t, runCycle(l, c), "grant_shield/WEAK:shield_cap_reached")
		if n := shieldWritesTo(sink, "WEAK"); n != 0 {
			t.Fatalf("writes = %d, want 0", n)
		}
	})
	t.Run("UC-040/A33 a cooling-down target is shield_cooldown until the end the coordinator exported", func(t *testing.T) {
		l, sink, clock, _ := limitLoop(t, 100, 3, 15, budgetShield("WEAK"))
		c := withGate(ffaRealCtx(), "WEAK", gamecontext.ShieldGate{Grants: 1, CooldownUntil: clock.Add(5 * time.Second)})
		expectOutcome(t, runCycle(l, c), "grant_shield/WEAK:shield_cooldown")
		*clock = clock.Add(5 * time.Second)
		expectOutcome(t, runCycle(l, c), "grant_shield/WEAK:dispatched")
		if n := shieldWritesTo(sink, "WEAK"); n != 1 {
			t.Fatalf("writes = %d, want 1", n)
		}
	})
	t.Run("UC-040/A31 a grant the gauges allow is written again: the coordinator stays authoritative", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100, budgetShield("WEAK"))
		expectOutcome(t, runCycle(l, ffaRealCtx()), "grant_shield/WEAK:dispatched")
		*clock = clock.Add(time.Second)
		expectOutcome(t, runCycle(l, ffaRealCtx()), "grant_shield/WEAK:dispatched")
		if n := shieldWritesTo(sink, "WEAK"); n != 2 {
			t.Fatalf("writes = %d, want 2 (no agent-side ledger)", n)
		}
	})
}

// UC: UC-040 A31 A33
func TestShieldGate_Views(t *testing.T) {
	t.Run("UC-040/A31 the state marks a charged player shield_charged, a capped one shield_limited", func(t *testing.T) {
		l, _, _, _ := limitLoop(t, 100, 1, 0)
		_ = runCycle(l, ffaRealCtx()) // the cycle publishes its limits to the views
		c := withGate(withGate(ffaRealCtx(), "WEAK", gamecontext.ShieldGate{Charged: true}), "S", gamecontext.ShieldGate{Grants: 1})
		st := l.buildDecideRequest(c, allInterventions(), false).State
		if !chargedIn(t, st, "WEAK") || playerIn(t, st, "WEAK").ShieldLimited {
			t.Fatal("WEAK must be shield_charged, not shield_limited")
		}
		if s := playerIn(t, st, "S"); !s.ShieldLimited || s.ShieldCharged {
			t.Fatalf("S limited=%v charged=%v, want limited only", s.ShieldLimited, s.ShieldCharged)
		}
	})
	t.Run("UC-040/A41 ShieldGrants reads the coordinator's count", func(t *testing.T) {
		l, _, _, _ := budgetLoop(t, 100)
		c := withGate(withGate(ffaRealCtx(), "WEAK", gamecontext.ShieldGate{Grants: 2}), "S", gamecontext.ShieldGate{})
		if g := l.ShieldGrants(c); g["WEAK"] != 2 || len(g) != 1 {
			t.Fatalf("ShieldGrants = %v, want WEAK 2 only", g)
		}
	})
	t.Run("UC-040/A31 without gauges both views are empty", func(t *testing.T) {
		l, _, _, _ := limitLoop(t, 100, 1, 0)
		_ = runCycle(l, ffaRealCtx())
		if charged, noShield := l.ShieldSerials(ffaRealCtx()); charged != nil || noShield != nil {
			t.Fatalf("views = %v, %v, want none", charged, noShield)
		}
	})
}

// fakeCoordinator stands in for the coordinator's shield gate in multi-cycle tests:
// sync applies the grant_shield writes the sink saw since the last call at now (each
// target counts one shield, its cooldown runs from now) and returns c with the gauges
// the coordinator would export.
type fakeCoordinator struct {
	cooldown time.Duration
	seen     int
	gates    map[string]gamecontext.ShieldGate
	groups   int
}

func (f *fakeCoordinator) sync(c gamecontext.GameContext, sink *capturingSink, now time.Time) gamecontext.GameContext {
	if f.gates == nil {
		f.gates = make(map[string]gamecontext.ShieldGate)
	}
	ds := sink.decisions()
	for _, d := range ds[f.seen:] {
		var targets []string
		switch d.Intervention {
		case InterventionGrantShield:
			targets = []string{d.TargetSerial}
		case InterventionGrantGroupShield:
			targets = d.TargetSerials
			f.groups++
		}
		seconds := float64(DefaultShieldSeconds)
		if v, err := strconv.ParseFloat(d.Value, 64); err == nil {
			seconds = v
		}
		for _, s := range targets {
			g := f.gates[s]
			g.Grants++
			if f.cooldown > 0 {
				g.CooldownUntil = now.Add(time.Duration(seconds*float64(time.Second)) + f.cooldown)
			}
			f.gates[s] = g
		}
	}
	f.seen = len(ds)
	for s, g := range f.gates {
		c = withGate(c, s, g)
	}
	if f.groups > 0 {
		n := f.groups
		c.Session.ShieldGroupGrants = &n
	}
	return c
}

// blockingSink holds the first write until release is closed.
type blockingSink struct {
	capturingSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSink) Apply(ctx context.Context, d Decision) error {
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if first {
		<-s.release
	}
	return s.capturingSink.Apply(ctx, d)
}

// UC: UC-040 A31
func TestShieldGate_RemoteAnswerForAChargedPlayer(t *testing.T) {
	t.Run("UC-040/A31 a remote answer landing for a charged player is blocked already_charged", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.95, "A", 0.5, "x"))
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		c := withGate(remoteContext(), "A", gamecontext.ShieldGate{Charged: true})
		h.provider.set(remoteGameID, c)

		_ = h.loop.OnEvaluate(context.Background(), c, testTrigger())
		h.loop.AwaitInflight()
		ds := spansByName(h.sr.Ended(), SpanDecide)
		if len(ds) != 1 {
			t.Fatalf("invoke_agent spans = %d, want 1", len(ds))
		}
		if v, ok := attrValue(ds[0], AttrDecideBlocked); !ok || !v.AsBool() {
			t.Fatal("decide.blocked must be true")
		}
		if g := strAttr(t, ds[0], AttrDecideBlockReason); g != string(ReasonAlreadyCharged) {
			t.Fatalf("decide.block_reason = %q, want already_charged", g)
		}
		if n := len(h.sink.decisions()); n != 0 {
			t.Fatalf("writes = %d, want 0", n)
		}
	})
}
