package decision

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// group_uc040_test.go covers the agent half of the group shield (#2467 PR 4; UC-040
// UC-048 A13, UC-048 A14, UC-048 BR-014; decide-contract §2, §4.3 to §4.5, §7.1): the lever is offered
// only while the group gate is open, a group answer resolves into its set, the
// permission chain shields the whole set or nobody, the group count and the cost
// are bounded, and the landing drops a group whose target left.

// groupPolicy is an open gate: enabled, one group per game, up to 4 targets.
func groupPolicy() flags.Policy {
	return flags.Policy{
		MaxInterventionsPerMinute: 100,
		GroupGrantEnabled:         true,
		GroupGrantMaxPerGame:      1,
		GroupGrantMaxTargets:      4,
	}
}

// groupCtx is an active game of kind with the given alive serials.
func groupCtx(kind string, alive ...string) gamecontext.GameContext {
	players := map[string]testPlayer{}
	for _, s := range alive {
		players[s] = testPlayer{active: true}
	}
	c := gameCtx(40, nil, players)
	c.GameKind = kind
	return c
}

func groupDecision(serials ...string) Decision {
	return Decision{Intervention: InterventionGrantGroupShield, TargetSerials: serials, Objectives: balancedFocused()}
}

func groupSnapshot(p flags.Policy) flags.Snapshot {
	s := rulesSnapshot()
	s.Policy = p
	s.Objectives = balancedFocused()
	return s
}

// UC: UC-048 A13 BR-014
func TestGroupLever_OfferedOnlyWhileTheGateIsOpen(t *testing.T) {
	allowed := []string{InterventionGrantShield, InterventionGrantGroupShield}
	cases := []struct {
		name   string
		policy func(*flags.Policy)
		ctx    gamecontext.GameContext
		want   bool
	}{
		{"UC-048/A13 BR-014 shadow game, gate open: offered", func(*flags.Policy) {}, groupCtx("shadow", "A", "B", "C"), true},
		{"UC-048/A13 BR-014 enabled off: not offered", func(p *flags.Policy) { p.GroupGrantEnabled = false }, groupCtx("shadow", "A", "B", "C"), false},
		{"UC-048/A13 BR-014 max_per_game 0: not offered", func(p *flags.Policy) { p.GroupGrantMaxPerGame = 0 }, groupCtx("shadow", "A", "B", "C"), false},
		{"UC-048/A13 BR-014 max_targets unreadable (0): not offered", func(p *flags.Policy) { p.GroupGrantMaxTargets = 0 }, groupCtx("shadow", "A", "B", "C"), false},
		{"UC-048/A13 BR-014 real game, real_games off: not offered", func(*flags.Policy) {}, groupCtx("real", "A", "B", "C"), false},
		{"UC-048/A13 BR-014 unknown kind counts as real", func(*flags.Policy) {}, groupCtx("", "A", "B", "C"), false},
		{"UC-048/A13 BR-014 real game, real_games on: offered", func(p *flags.Policy) { p.GroupGrantRealGames = true }, groupCtx("real", "A", "B", "C"), true},
		{"UC-048/A13 BR-014 one alive player: not offered", func(*flags.Policy) {}, groupCtx("shadow", "A"), false},
		{"UC-048/A13 BR-014 more alive than max_targets: not offered", func(p *flags.Policy) { p.GroupGrantMaxTargets = 2 }, groupCtx("shadow", "A", "B", "C"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _, _, _ := budgetLoop(t, 100)
			p := groupPolicy()
			tc.policy(&p)
			snap := groupSnapshot(p)
			snap.InterventionsAllowed = allowed
			got := slices.Contains(l.offeredInterventions(tc.ctx, snap), InterventionGrantGroupShield)
			if got != tc.want {
				t.Fatalf("offered = %v, want %v", got, tc.want)
			}
			// The request built from the offered list carries group options exactly then.
			req := l.buildDecideRequest(tc.ctx, l.offeredInterventions(tc.ctx, snap), false)
			var target decide.Question
			for _, q := range req.Questions {
				if q.ID == decide.QuestionFFATarget {
					target = q
				}
			}
			if has := slices.ContainsFunc(target.Options, decide.IsGroupTarget); has != tc.want {
				t.Fatalf("group target options = %v, want offered=%v", target.Options, tc.want)
			}
		})
	}
	t.Run("UC-048/A13 BR-014 the game's group count reached: not offered", func(t *testing.T) {
		l, _, _, _ := budgetLoop(t, 100)
		snap := groupSnapshot(groupPolicy())
		snap.InterventionsAllowed = allowed
		c := groupCtx("shadow", "A", "B", "C")
		one := 1
		c.Session.ShieldGroupGrants = &one // the coordinator applied the game's one group
		if slices.Contains(l.offeredInterventions(c, snap), InterventionGrantGroupShield) {
			t.Fatal("a game at max_per_game must not be offered another group")
		}
	})
	t.Run("UC-048/A13 BR-014 the rules path drops a closed lever too", func(t *testing.T) {
		l, _, _, _ := budgetLoop(t, 100)
		snap := rulesSnapshot() // zero Policy: the gate is closed (the rules golden)
		got := l.withoutClosedGroupLever(snap.Policy, groupCtx("shadow", "A", "B"), snap.InterventionsAllowed)
		if slices.Contains(got, InterventionGrantGroupShield) || len(got) != len(snap.InterventionsAllowed)-1 {
			t.Fatalf("rules allow-list = %v, want every lever but the group lever", got)
		}
	})
}

// UC: UC-048 A13 BR-014
func TestGroupLever_MapAnswers(t *testing.T) {
	st := decide.BuildStateWith(groupCtx("shadow", "A", "B", "C"), decide.Annotations{})
	req := decide.Request{State: st}
	answer := func(iv, target string) decide.Response {
		return decide.Response{Backend: "claude", Answers: []decide.Answer{
			{QuestionID: decide.QuestionFFAIntervention, Choice: iv},
			{QuestionID: decide.QuestionFFATarget, Choice: target},
		}}
	}
	t.Run("UC-048/A13 group:all resolves to every alive player", func(t *testing.T) {
		d, outcome := mapAnswers(req, answer(InterventionGrantGroupShield, decide.TargetGroupAll))
		if outcome != DecideOutcomeAct || !slices.Equal(d.TargetSerials, []string{"A", "B", "C"}) || d.TargetSerial != "" {
			t.Fatalf("decision = %+v (%s), want set A,B,C and no single target", d, outcome)
		}
	})
	t.Run("UC-048/A13 group:all_except:B resolves to the others", func(t *testing.T) {
		d, outcome := mapAnswers(req, answer(InterventionGrantGroupShield, decide.TargetGroupAllExceptPrefix+"B"))
		if outcome != DecideOutcomeAct || !slices.Equal(d.TargetSerials, []string{"A", "C"}) {
			t.Fatalf("decision = %+v (%s), want set A,C", d, outcome)
		}
	})
	// An answer that does not fit the group lever is an invalid answer (rules fallback).
	full := req
	full.Questions = []decide.Question{
		{ID: decide.QuestionFFAIntervention, Kind: decide.KindChoice, Options: []string{decide.OptionNoop,
			InterventionGrantGroupShield, InterventionGrantShield, InterventionAdjustMusicTempo}},
		{ID: decide.QuestionFFATarget, Kind: decide.KindChoice, Options: []string{decide.TargetNone, "A", "B", "C",
			decide.TargetGroupAll, decide.TargetGroupAllExceptPrefix + "Z"}},
	}
	certain := func(iv, target string) decide.Response {
		return decide.Response{Answers: []decide.Answer{
			{QuestionID: decide.QuestionFFAIntervention, Choice: iv, Probabilities: map[string]float64{iv: 1}, Confidence: 1},
			{QuestionID: decide.QuestionFFATarget, Choice: target, Probabilities: map[string]float64{target: 1}, Confidence: 1},
		}}
	}
	for _, tc := range []struct{ name, iv, target string }{
		{"UC-048/A14 group lever with none", InterventionGrantGroupShield, decide.TargetNone},
		{"UC-048/A14 group lever with a serial", InterventionGrantGroupShield, "A"},
		{"UC-048/A14 group lever excluding a player who is not alive", InterventionGrantGroupShield, decide.TargetGroupAllExceptPrefix + "Z"},
		{"UC-048/A14 group option on a single grant", InterventionGrantShield, decide.TargetGroupAll},
		{"UC-048/A14 group option on a session lever", InterventionAdjustMusicTempo, decide.TargetGroupAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := decide.Validate(full, certain(tc.iv, tc.target)); !errors.Is(err, decide.ErrInvalid) {
				t.Fatalf("Validate = %v, want an invalid answer", err)
			}
		})
	}
}

// UC: UC-048 A13 A14 BR-014 BR-007
func TestGroupLever_PermissionChainIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-048/A13 BR-014 an open gate writes one group with its set at cost 1", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100)
		snap := groupSnapshot(groupPolicy())
		var st LayerState
		l.runDecision(ctx, snap, groupCtx("shadow", "A", "B", "C"), groupDecision("A", "B", "C"), *clock, &st)
		if got := outcomeSummary(st); !equalStrings(got, []string{"grant_group_shield/:dispatched"}) {
			t.Fatalf("outcomes = %v, want dispatched", got)
		}
		got := sink.decisions()
		if len(got) != 1 || !slices.Equal(got[0].TargetSerials, []string{"A", "B", "C"}) {
			t.Fatalf("writes = %+v, want one group write with A,B,C", got)
		}
		if used := l.limiter.used(); used != 1 {
			t.Fatalf("budget used = %v, want 1 (one group costs one medium unit)", used)
		}
	})
	t.Run("UC-048/A14 BR-014 one capped target refuses the group and reserves nothing", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100)
		p := groupPolicy()
		p.MaxShieldsPerPlayerPerGame = 1
		snap := groupSnapshot(p)
		c := withGate(groupCtx("shadow", "A", "B", "C"), "B", gamecontext.ShieldGate{Grants: 1})
		var st LayerState
		l.runDecision(ctx, snap, c, groupDecision("A", "B", "C"), *clock, &st)
		if got := outcomeSummary(st); !equalStrings(got, []string{"grant_group_shield/:shield_cap_reached"}) {
			t.Fatalf("outcomes = %v, want shield_cap_reached", got)
		}
		if n := len(sink.decisions()); n != 0 {
			t.Fatalf("writes = %d, want none", n)
		}
	})
	t.Run("UC-048/A14 BR-014 max_per_game: the second group is refused group_cap_reached", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100)
		snap := groupSnapshot(groupPolicy())
		c := groupCtx("shadow", "A", "B", "C")
		one := 1
		c.Session.ShieldGroupGrants = &one
		var st LayerState
		l.runDecision(ctx, snap, c, groupDecision("A", "B"), *clock, &st)
		if got := outcomeSummary(st); !equalStrings(got, []string{"grant_group_shield/:group_cap_reached"}) {
			t.Fatalf("outcomes = %v, want group_cap_reached", got)
		}
		if n := len(sink.decisions()); n != 0 {
			t.Fatalf("writes = %d, want none", n)
		}
	})
	for _, tc := range []struct {
		name   string
		policy func(*flags.Policy)
		ctx    gamecontext.GameContext
		d      Decision
		reason BlockReason
	}{
		{"UC-048/A14 BR-014 gate off at the decision", func(p *flags.Policy) { p.GroupGrantEnabled = false }, groupCtx("shadow", "A", "B"), groupDecision("A", "B"), ReasonGroupDisabled},
		{"UC-048/A14 BR-014 real game without real_games", func(*flags.Policy) {}, groupCtx("real", "A", "B"), groupDecision("A", "B"), ReasonGroupRealGame},
		{"UC-048/A14 BR-014 one target", func(*flags.Policy) {}, groupCtx("shadow", "A", "B"), groupDecision("A"), ReasonGroupBadSet},
		{"UC-048/A14 BR-014 a repeated target", func(*flags.Policy) {}, groupCtx("shadow", "A", "B"), groupDecision("A", "A"), ReasonGroupBadSet},
		{"UC-048/A14 BR-014 a target that is not alive", func(*flags.Policy) {}, groupCtx("shadow", "A", "B"), groupDecision("A", "Z"), ReasonGroupBadSet},
		{"UC-048/A14 BR-014 more targets than max_targets", func(p *flags.Policy) { p.GroupGrantMaxTargets = 2 }, groupCtx("shadow", "A", "B", "C"), groupDecision("A", "B", "C"), ReasonGroupBadSet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, sink, clock, _ := budgetLoop(t, 100)
			p := groupPolicy()
			tc.policy(&p)
			var st LayerState
			l.runDecision(ctx, groupSnapshot(p), tc.ctx, tc.d, *clock, &st)
			if got := outcomeSummary(st); !equalStrings(got, []string{"grant_group_shield/:" + string(tc.reason)}) {
				t.Fatalf("outcomes = %v, want %s", got, tc.reason)
			}
			if len(sink.decisions()) != 0 || l.limiter.used() != 0 {
				t.Fatal("a refused group must write nothing and charge no budget")
			}
		})
	}
	t.Run("UC-048/A14 BR-014 a low-battery target blocks the group", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100)
		p := groupPolicy()
		p.BatteryThreshold = 20
		c := groupCtx("shadow", "A", "B")
		c.Players["B"].BatteryPct = fptr(5)
		var st LayerState
		l.runDecision(ctx, groupSnapshot(p), c, groupDecision("A", "B"), *clock, &st)
		if got := outcomeSummary(st); !equalStrings(got, []string{"grant_group_shield/:battery_threshold"}) || len(sink.decisions()) != 0 {
			t.Fatalf("outcomes = %v, want battery_threshold and no write", got)
		}
	})
	t.Run("UC-048/A14 BR-014 the allow-list stays the outer gate", func(t *testing.T) {
		l, _, clock, _ := budgetLoop(t, 100)
		snap := groupSnapshot(groupPolicy())
		snap.InterventionsAllowed = []string{InterventionGrantShield}
		var st LayerState
		l.runDecision(ctx, snap, groupCtx("shadow", "A", "B"), groupDecision("A", "B"), *clock, &st)
		if got := outcomeSummary(st); !equalStrings(got, []string{"grant_group_shield/:not_allowed"}) {
			t.Fatalf("outcomes = %v, want not_allowed", got)
		}
	})
	t.Run("UC-048/A13 BR-012 the group is stamped with the configured length", func(t *testing.T) {
		l, sink, clock, _ := budgetLoop(t, 100)
		l.SetShieldSecondsSource(func(context.Context) float64 { return 7 })
		var st LayerState
		l.runDecision(ctx, groupSnapshot(groupPolicy()), groupCtx("shadow", "A", "B"), groupDecision("A", "B"), *clock, &st)
		got := sink.decisions()
		if len(got) != 1 || got[0].Value != "7" {
			t.Fatalf("writes = %+v, want value 7", got)
		}
	})
}

// UC: UC-048 A14 BR-014
func TestGroupLever_RemoteLanding(t *testing.T) {
	groupRemoteSnapshot := func() flags.Snapshot {
		s := remoteSnapshot(time.Hour)
		s.InterventionsAllowed = append(s.InterventionsAllowed, InterventionGrantGroupShield)
		s.Policy = groupPolicy()
		s.Policy.GroupGrantRealGames = true // remoteContext is a real game
		return s
	}
	t.Run("UC-048/A13 BR-014 a group answer is written with its set", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer(InterventionGrantGroupShield, 0.95, decide.TargetGroupAll, 0, "x"))
		h, _ := fireAndMutateSnapshot(t, be, groupRemoteSnapshot(), time.Hour, func(remoteHarness) {})
		got := h.sink.decisions()
		if len(got) != 1 || !slices.Equal(got[0].TargetSerials, []string{"A", "B"}) {
			t.Fatalf("writes = %+v, want one group write to A,B", got)
		}
		if !slices.Contains(be.reqs[0].Questions[1].Options, decide.TargetGroupAll) {
			t.Fatalf("request target options = %v, want group:all offered", be.reqs[0].Questions[1].Options)
		}
	})
	t.Run("UC-048/A14 BR-014 a target gone at landing discards the group", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer(InterventionGrantGroupShield, 0.95, decide.TargetGroupAll, 0, "x"))
		h, span := fireAndMutateSnapshot(t, be, groupRemoteSnapshot(), time.Hour, func(h remoteHarness) {
			c := remoteContext()
			c.Players["B"].Active = boolPtr(false)
			h.provider.set(remoteGameID, c)
		})
		assertDiscarded(t, h, span, DiscardTargetGone)
	})
	t.Run("UC-048/A14 BR-014 the gate closed at landing blocks the group", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer(InterventionGrantGroupShield, 0.95, decide.TargetGroupAll, 0, "x"))
		h, _ := fireAndMutateSnapshot(t, be, groupRemoteSnapshot(), time.Hour, func(h remoteHarness) {
			h.flags.mutate(func(s *flags.Snapshot) { s.Policy.GroupGrantEnabled = false })
		})
		if got := h.sink.decisions(); len(got) != 0 {
			t.Fatalf("writes = %+v, want none once the gate closed", got)
		}
	})
}
