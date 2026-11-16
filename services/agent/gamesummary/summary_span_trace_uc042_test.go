package gamesummary

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/gamecontext"
)

// UC: UC-042 main step 3
func TestCoordinator_SummarySpanInGameTrace(t *testing.T) {
	const (
		gameTrace  = "4bf92f3577b34da6a3ce929d0e0e4736"
		gameSpanID = "051581bf3cb55c13"
	)
	t0 := time.Unix(10_000, 0)
	game := func(id string) gamecontext.GameContext {
		return gamecontext.GameContext{
			SessionID: id, GameKind: "real", GameTraceID: gameTrace, GameTraceSpanID: gameSpanID,
			Session:  gamecontext.SessionSignals{GameActive: bptr(false)},
			Timeline: []gamecontext.TimelineEvent{{At: t0, Kind: gamecontext.EventPhase, Detail: "start"}},
		}
	}

	t.Run("UC-042/main step 3 the summary span is a child of the game root span", func(t *testing.T) {
		c, sr, _ := recordingCoordinator(t)
		c.OnGameEnd(game("game_tr"))
		spans := spansByName(sr.Ended(), SpanGameSummary)
		if len(spans) != 1 {
			t.Fatalf("agent.game.summary spans = %d, want 1", len(spans))
		}
		s := spans[0]
		if got := s.SpanContext().TraceID().String(); got != gameTrace {
			t.Errorf("summary trace_id = %s, want the game trace %s", got, gameTrace)
		}
		if got := s.Parent().SpanID().String(); got != gameSpanID {
			t.Errorf("summary parent = %s, want the game root span %s", got, gameSpanID)
		}
		if len(s.Links()) != 1 {
			t.Errorf("summary keeps its game Link, got %d links", len(s.Links()))
		}
	})

	t.Run("UC-042/main step 3 an experiment parent still wins", func(t *testing.T) {
		c, sr, _ := recordingCoordinator(t)
		expTID, _ := trace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		expSID, _ := trace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
		c.SetExperimentParent(func(string) (trace.SpanContext, bool) {
			return trace.NewSpanContext(trace.SpanContextConfig{TraceID: expTID, SpanID: expSID, TraceFlags: trace.FlagsSampled, Remote: true}), true
		})
		g := game("game_exp")
		g.ExperimentID = "exp_1"
		c.OnGameEnd(g)
		s := spansByName(sr.Ended(), SpanGameSummary)[0]
		if s.Parent().SpanID() != expSID {
			t.Errorf("experiment-bound summary parent = %s, want the experiment span", s.Parent().SpanID())
		}
	})

	t.Run("UC-042/main step 3 the span lists the game's interventions", func(t *testing.T) {
		c, sr, _ := recordingCoordinator(t)
		c.SetInterventionLookup(func(id string) AgentInterventions {
			if id != "game_iv" {
				return AgentInterventions{}
			}
			return AgentInterventions{
				Entries: []AgentIntervention{
					{At: t0.Add(12340 * time.Millisecond), Action: "player_sensitivity_factor", Target: "SER1", TargetName: "Ann", Value: "0.8", Outcome: "applied", Backend: "rules"},
					{At: t0.Add(30 * time.Second), Action: "grant_shield", Target: "SER2", Outcome: "blocked", Backend: "rules"},
					{At: t0.Add(45 * time.Second), Action: "global_difficulty_factor", Value: "1.2", Outcome: "held", Backend: "claude"},
				},
				Held: 1, Noop: 2, Backends: []string{"claude", "rules"},
			}
		})
		c.OnGameEnd(game("game_iv"))
		s := spansByName(sr.Ended(), SpanGameSummary)[0]
		const want = `[{"offset_s":12.3,"action":"player_sensitivity_factor","target":"SER1","name":"Ann","value":"0.8","outcome":"applied","backend":"rules"},` +
			`{"offset_s":30,"action":"grant_shield","target":"SER2","outcome":"blocked","backend":"rules"},` +
			`{"offset_s":45,"action":"global_difficulty_factor","value":"1.2","outcome":"held","backend":"claude"}]`
		if v, _ := spanAttr(s, attrInterventions); v.AsString() != want {
			t.Errorf("%s =\n %s\nwant\n %s", attrInterventions, v.AsString(), want)
		}
		if v, _ := spanAttr(s, attrInterventionsHeld); v.AsInt64() != 1 {
			t.Errorf("held = %v, want 1", v.AsInt64())
		}
		if v, _ := spanAttr(s, attrInterventionsNoop); v.AsInt64() != 2 {
			t.Errorf("noop = %v, want 2", v.AsInt64())
		}
		if v, _ := spanAttr(s, attrDecideBackend); v.AsString() != "claude,rules" {
			t.Errorf("decide backend = %q, want claude,rules", v.AsString())
		}
		if _, ok := spanAttr(s, attrInterventionsTruncated); ok {
			t.Error("nothing dropped: no truncated attribute")
		}
	})

	t.Run("UC-042/main step 3 a game without interventions carries no list", func(t *testing.T) {
		c, sr, _ := recordingCoordinator(t)
		c.SetInterventionLookup(func(string) AgentInterventions { return AgentInterventions{} })
		c.OnGameEnd(game("game_none"))
		s := spansByName(sr.Ended(), SpanGameSummary)[0]
		for _, k := range []string{attrInterventions, attrInterventionsHeld, attrInterventionsNoop, attrDecideBackend} {
			if _, ok := spanAttr(s, k); ok {
				t.Errorf("%s set without any intervention", k)
			}
		}
	})
}
