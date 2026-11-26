package decision

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// levercooldown_uc040_test.go covers the lever cooldown (UC-040 A53, BR-046;
// decide-contract §4.5 step 3a; #2641).

func globalDifficulty(value string) Decision {
	return Decision{Intervention: InterventionAdjustGlobalDifficulty, Value: value,
		ObjectiveServed: ObjectiveBalanced, Objectives: balancedFocused()}
}

// alternate runs one cycle every step for total, deciding make(i) in cycle i, and
// returns the outcome of every cycle.
func alternate(l *Loop, clock *time.Time, step, total time.Duration, make func(i int) Decision) []string {
	var out []string
	for i := 0; time.Duration(i)*step < total; i++ {
		if i > 0 {
			*clock = clock.Add(step)
		}
		l.Rules = fakeRules{out: []Decision{make(i)}}
		out = append(out, outcomeSummary(runCycle(l, budgetCtx()))...)
	}
	return out
}

func difficultyEvery3s(i int) Decision {
	if i%2 == 0 {
		return globalDifficulty("1.25")
	}
	return globalDifficulty("1.625")
}

// UC: UC-040 A53 BR-046 A35
func TestLeverCooldown_AlternatingMagnitudesWriteOncePerWindow(t *testing.T) {
	t.Run("UC-040/A53 BR-046 alternating 1.25 / 1.625 every 3 s writes the lever once per 20 s window", func(t *testing.T) {
		l, sink, clock, _ := appliedLoop(t)
		l.SetEffectWindow(20 * time.Second)
		got := alternate(l, clock, 3*time.Second, 60*time.Second, difficultyEvery3s)

		const iv = "adjust_global_difficulty/"
		want := map[int]string{0: "dispatched", 1: "lever_cooldown", 2: "already_applied", 7: "dispatched", 8: "lever_cooldown", 14: "dispatched"}
		for i, w := range want {
			if got[i] != iv+":"+w {
				t.Fatalf("cycle %d (t=%ds) = %s, want %s; all %v", i, 3*i, got[i], w, got)
			}
		}
		if n := writesOf(sink, InterventionAdjustGlobalDifficulty, ""); n != 3 {
			t.Fatalf("writes = %d, want 3 (t=0, 21, 42 s); outcomes %v", n, got)
		}
	})
	t.Run("UC-040/A53 the window is the effect window resolved at the check", func(t *testing.T) {
		l, sink, clock, _ := appliedLoop(t)
		l.SetEffectWindowSource(func() time.Duration { return 5 * time.Second })
		_ = alternate(l, clock, 3*time.Second, 12*time.Second, difficultyEvery3s)
		// t=0 1.25 written, t=3 1.625 cooling, t=6 1.25 already applied, t=9 1.625 written.
		if n := writesOf(sink, InterventionAdjustGlobalDifficulty, ""); n != 2 {
			t.Fatalf("writes = %d, want 2 with a 5 s window", n)
		}
	})
	t.Run("UC-040/A53 BR-014 a cooling lever charges no budget and does not end admission", func(t *testing.T) {
		l, sink, clock, _ := appliedLoop(t, globalDifficulty("1.25"))
		_ = runCycle(l, budgetCtx())
		before := limiterEntries(l, l.nowOrDefault()())
		*clock = clock.Add(3 * time.Second)
		l.Rules = fakeRules{out: []Decision{globalDifficulty("1.625"), budgetShield("S")}}
		got := outcomeSummary(runCycle(l, budgetCtx()))
		if !equalStrings(got, []string{"adjust_global_difficulty/:lever_cooldown", "grant_shield/S:dispatched"}) {
			t.Fatalf("outcomes = %v", got)
		}
		if after := limiterEntries(l, l.nowOrDefault()()); after != before+1 {
			t.Fatalf("limiter entries %d -> %d, want only the shield charged", before, after)
		}
		if n := writesOf(sink, InterventionGrantShield, "S"); n != 1 {
			t.Fatalf("shield writes = %d, want 1", n)
		}
	})
}

// UC: UC-040 A53 BR-046
func TestLeverCooldown_OnlyGameWideLevers(t *testing.T) {
	t.Run("UC-040/BR-046 a per-player lever may change every cycle", func(t *testing.T) {
		l, sink, clock, _ := appliedLoop(t)
		_ = alternate(l, clock, 3*time.Second, 12*time.Second, func(i int) Decision {
			return sens("WEAK", []string{"0.8", "0.6"}[i%2])
		})
		if n := writesOf(sink, InterventionAdjustPlayerSensitivity, "WEAK"); n != 4 {
			t.Fatalf("writes = %d, want 4", n)
		}
	})
	t.Run("UC-040/A53 every BR-046 lever cools down", func(t *testing.T) {
		for _, iv := range []string{InterventionAdjustGlobalDifficulty, InterventionAdjustGlobalSensitivity,
			InterventionAdjustMusicTempo, InterventionAdjustVolume, InterventionSetPacingProfile} {
			if !cooldownLevers[iv] {
				t.Errorf("%s has no cooldown", iv)
			}
		}
		for _, iv := range []string{InterventionAdjustPlayerSensitivity, InterventionSetPlayerHandicap,
			InterventionRampTempo, InterventionAutoRubberband, InterventionGrantShield} {
			if cooldownLevers[iv] {
				t.Errorf("%s must not cool down", iv)
			}
		}
	})
	t.Run("UC-040/A53 a loop without suppression has no cooldown", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100)
		_ = alternate(l, clock, 3*time.Second, 12*time.Second, difficultyEvery3s)
		if n := writesOf(sink, InterventionAdjustGlobalDifficulty, ""); n != 4 {
			t.Fatalf("writes = %d, want 4", n)
		}
	})
}

// UC: UC-040 A53 A27
func TestLeverCooldown_OnlyAnAppliedWriteStartsAWindow(t *testing.T) {
	t.Run("UC-040/A53 A27 a failed write starts no window", func(t *testing.T) {
		l, sink, clock, _ := appliedLoop(t, globalDifficulty("1.25"))
		l.Actions = errSink{}
		_ = runCycle(l, budgetCtx())
		l.Actions = sink
		*clock = clock.Add(3 * time.Second)
		l.Rules = fakeRules{out: []Decision{globalDifficulty("1.625")}}
		if got := outcomeSummary(runCycle(l, budgetCtx())); !equalStrings(got, []string{"adjust_global_difficulty/:dispatched"}) {
			t.Fatalf("outcomes = %v, want dispatched after a failed write", got)
		}
		if n := writesOf(sink, InterventionAdjustGlobalDifficulty, ""); n != 1 {
			t.Fatalf("writes = %d, want 1", n)
		}
	})
}

// UC: UC-040 A53 A21
func TestLeverCooldown_FirstInfoThenQuiet(t *testing.T) {
	t.Run("UC-040/A53 the first refusal per write logs info with spans; repeats only count", func(t *testing.T) {
		l, _, clock, decisionSpans := appliedLoop(t, globalDifficulty("1.25"))
		logs := &syncBuffer{}
		l.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		_ = runCycle(l, budgetCtx())
		for _, v := range []string{"1.625", "1.5", "1.625"} {
			*clock = clock.Add(2 * time.Second)
			l.Rules = fakeRules{out: []Decision{globalDifficulty(v)}}
			_ = runCycle(l, budgetCtx())
		}
		if n := decisionSpans(); n != 2 {
			t.Fatalf("agent.decision spans = %d, want 2 (the write and the first refusal)", n)
		}
		if c := logs.count("level=INFO msg=agent.decision_lever_cooldown "); c != 1 {
			t.Fatalf("info lines = %d, want 1", c)
		}
		if c := logs.count("level=DEBUG msg=agent.decision_lever_cooldown_repeat"); c != 2 {
			t.Fatalf("repeat lines = %d, want 2", c)
		}
		if c := logs.count("agent.decision_blocked"); c != 0 {
			t.Fatalf("lever_cooldown must not log the blocked warning (%d lines)", c)
		}
	})
}

// UC: UC-040 A53
func TestLeverCooldown_DecideStateNamesCoolingLevers(t *testing.T) {
	t.Run("UC-040/A53 step 4 the decide state's session line names the cooling lever", func(t *testing.T) {
		l, _, clock, _ := appliedLoop(t, globalDifficulty("1.25"))
		c := budgetCtx()
		if req := l.buildDecideRequest(c, nil, false); len(req.State.LeverCooldown) != 0 {
			t.Fatalf("before a write: lever_cooldown = %v", req.State.LeverCooldown)
		}
		_ = runCycle(l, c)
		*clock = clock.Add(5 * time.Second)
		req := l.buildDecideRequest(c, nil, false)
		if !equalStrings(req.State.LeverCooldown, []string{InterventionAdjustGlobalDifficulty}) {
			t.Fatalf("lever_cooldown = %v", req.State.LeverCooldown)
		}
		if !strings.Contains(req.State.Narrative, " lever_cooldown=adjust_global_difficulty") {
			t.Fatalf("narrative has no lever_cooldown token:\n%s", req.State.Narrative)
		}
		*clock = clock.Add(DefaultEffectWindow)
		if req := l.buildDecideRequest(c, nil, false); len(req.State.LeverCooldown) != 0 {
			t.Fatalf("after the window: lever_cooldown = %v", req.State.LeverCooldown)
		}
	})
}
