package decision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/agent/gamecontext"
)

// intervention_id_uc101_test.go covers the agent slice of #2665 (UC-101 BR-003,
// FR-221, NFR-061): the intervention id is created before the write, sent with it,
// stamped on agent.decision as decision.intervention_id and reused by the effect
// sample as intervention.id.

// idSink records the intervention id of every decision the loop handed to it, and
// reports the outcome the test chose (written, gated off, failed).
type idSink struct {
	mu     sync.Mutex
	ids    []string
	gateOn bool
	err    error
}

func (s *idSink) Apply(ctx context.Context, d Decision) error {
	_, err := s.ApplyReported(ctx, d)
	return err
}

func (s *idSink) ApplyReported(_ context.Context, d Decision) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, d.InterventionID)
	if !s.gateOn {
		return false, nil
	}
	return true, s.err
}

func (s *idSink) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

var errWriteRefused = errors.New("flag service refused")

// UC: UC-101 3 BR-003
func TestInterventionID_CreatedBeforeTheWriteAndSentWithIt(t *testing.T) {
	t.Run("UC-101/BR-003 the write carries the id the decision span records", func(t *testing.T) {
		// No context provider: no effect sample is scheduled, the id is still made.
		l, sr, _ := newTestLoop(t)
		sink := &idSink{gateOn: true}
		l.Actions = sink
		l.Rules = fakeRules{out: []Decision{{Intervention: "grant_shield", TargetSerial: "A", Reason: "x"}}}

		l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "game_1"}, testTrigger())

		sent := sink.sent()
		if len(sent) != 1 || sent[0] == "" {
			t.Fatalf("ids sent with the write = %q, want one non-empty id", sent)
		}
		spans := spansByName(sr.Ended(), SpanDecision)
		if len(spans) != 1 {
			t.Fatalf("agent.decision spans = %d, want 1", len(spans))
		}
		v, ok := attrValue(spans[0], AttrDecisionInterventionID)
		if !ok || v.AsString() != sent[0] {
			t.Fatalf("decision.intervention_id = %q (present %v), want the id sent %q", v.AsString(), ok, sent[0])
		}
	})

	t.Run("UC-101/BR-003 each written decision gets its own id", func(t *testing.T) {
		l, _, _ := newTestLoop(t)
		sink := &idSink{gateOn: true}
		l.Actions = sink
		l.Rules = fakeRules{out: []Decision{
			{Intervention: "grant_shield", TargetSerial: "A", Reason: "x"},
			{Intervention: "play_audio_cue", Reason: "y"},
		}}

		l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "game_1"}, testTrigger())

		sent := sink.sent()
		if len(sent) != 2 || sent[0] == "" || sent[1] == "" || sent[0] == sent[1] {
			t.Fatalf("ids sent = %q, want two distinct non-empty ids", sent)
		}
	})
}

// UC: UC-101 A2 BR-003 BR-005
func TestInterventionID_NotWrittenDecisions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sink   *idSink
		wantID bool
	}{
		{"UC-101/A2 a failed write keeps the id it sent", &idSink{gateOn: true, err: errWriteRefused}, true},
		{"UC-101/BR-003 a gated no-op carries no id", &idSink{gateOn: false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, sr, _ := newTestLoop(t)
			l.Actions = tc.sink
			l.Rules = fakeRules{out: []Decision{{Intervention: "grant_shield", TargetSerial: "A", Reason: "x"}}}

			l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "game_1"}, testTrigger())

			sent := tc.sink.sent()
			if len(sent) != 1 || sent[0] == "" {
				t.Fatalf("ids handed to the sink = %q, want one non-empty id (made before the write)", sent)
			}
			spans := spansByName(sr.Ended(), SpanDecision)
			if len(spans) != 1 {
				t.Fatalf("agent.decision spans = %d, want 1", len(spans))
			}
			v, ok := attrValue(spans[0], AttrDecisionInterventionID)
			if tc.wantID && (!ok || v.AsString() != sent[0]) {
				t.Fatalf("decision.intervention_id = %q (present %v), want %q", v.AsString(), ok, sent[0])
			}
			if !tc.wantID && ok && v.AsString() != "" {
				t.Fatalf("decision.intervention_id = %q, want none", v.AsString())
			}
		})
	}

	t.Run("UC-101/BR-005 a blocked decision gets no id and no write", func(t *testing.T) {
		l, sr, _ := newTestLoop(t)
		sink := &idSink{gateOn: true}
		l.Actions = sink
		// end_game is not on newTestLoop's allow-list: the permission gate blocks it.
		l.Rules = fakeRules{out: []Decision{{Intervention: "end_game", Reason: "x"}}}

		l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "game_1"}, testTrigger())

		if sent := sink.sent(); len(sent) != 0 {
			t.Fatalf("blocked decision reached the sink with %q", sent)
		}
		for _, s := range spansByName(sr.Ended(), SpanDecision) {
			if v, ok := attrValue(s, AttrDecisionInterventionID); ok && v.AsString() != "" {
				t.Fatalf("blocked decision.intervention_id = %q, want none", v.AsString())
			}
		}
	})
}

// UC: UC-101 BR-003
func TestInterventionID_EffectSampleReusesTheWriteID(t *testing.T) {
	t.Run("UC-101/BR-003 intervention.id of the effect is the id sent with the write", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_2665"
		provider.set(gameID, durationContext(gameID, 60))

		l, sr, _, sched := effectLoop(t, provider, gameID)
		sink := &idSink{gateOn: true}
		l.Actions = sink
		l.Rules = &baselineRules{out: []Decision{shieldDecision()}, fitness: enduranceBaseline()}

		l.OnEvaluate(context.Background(), durationContext(gameID, 30), EvalTrigger{Signal: "metrics", T0: time.Now()})
		sched.fire()
		l.AwaitEffects()

		sent := sink.sent()
		if len(sent) != 1 || sent[0] == "" {
			t.Fatalf("ids sent = %q, want one non-empty id", sent)
		}
		decs := spansByName(sr.Ended(), SpanDecision)
		effects := spansByName(sr.Ended(), SpanInterventionEffect)
		if len(decs) != 1 || len(effects) != 1 {
			t.Fatalf("decision spans = %d, effect spans = %d, want 1 and 1", len(decs), len(effects))
		}
		if v, _ := attrValue(decs[0], AttrDecisionInterventionID); v.AsString() != sent[0] {
			t.Errorf("decision.intervention_id = %q, want %q", v.AsString(), sent[0])
		}
		if v, _ := attrValue(effects[0], AttrEffectInterventionID); v.AsString() != sent[0] {
			t.Errorf("effect intervention.id = %q, want the id sent with the write %q", v.AsString(), sent[0])
		}
		if v, _ := attrValue(effects[0], AttrDecisionInterventionID); v.AsString() != sent[0] {
			t.Errorf("effect decision.intervention_id = %q, want %q", v.AsString(), sent[0])
		}
	})
}

// UC: UC-101 BR-003
func TestInterventionID_ProbeNoopGetsNoWriteID(t *testing.T) {
	t.Run("UC-101/BR-003 a probe-mode noop writes nothing and gets no id before the write", func(t *testing.T) {
		l, sr, _ := newTestLoop(t)
		sink := &idSink{gateOn: true}
		l.Actions = sink
		l.Rules = fakeRules{out: []Decision{{Intervention: InterventionNoop, Reason: "probe"}}}

		l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "game_1"}, testTrigger())

		for _, id := range sink.sent() {
			if id != "" {
				t.Fatalf("noop reached the sink with id %q, want none", id)
			}
		}
		for _, s := range spansByName(sr.Ended(), SpanDecision) {
			if v, ok := attrValue(s, AttrDecisionInterventionID); ok && v.AsString() != "" {
				t.Fatalf("noop decision.intervention_id = %q, want none (no effect sample in this loop)", v.AsString())
			}
		}
	})
}

// UC: UC-101 BR-003
func TestInterventionID_NotSerialized(t *testing.T) {
	t.Run("UC-101/BR-003 the id never reaches a Decision's JSON (rules golden, decide wire)", func(t *testing.T) {
		raw, err := json.Marshal(Decision{Intervention: "grant_shield", InterventionID: "id-2665"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "id-2665") {
			t.Fatalf("Decision JSON carries the intervention id: %s", raw)
		}
	})
}
