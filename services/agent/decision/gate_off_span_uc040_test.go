package decision

import (
	"context"
	"errors"
	"testing"

	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// UC: UC-040 A11
// A write dropped because `enabled` is watch is recorded as not applied on the
// agent.decision and agent.action spans, with the reason; a written one says
// applied=true (#2093).
func TestGateOff_SpansRecordNotApplied(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gateOn      bool
		wantApplied bool
		wantReason  string
	}{
		{"UC-040/A11 watch: not applied, reason watch", false, false, NotAppliedWatch},
		{"UC-040/main gate on: applied", true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, sr, _ := newTestLoop(t)
			l.Actions = &gatedSink{gateOn: tc.gateOn}
			l.Rules = fakeRules{out: []Decision{{Intervention: "grant_shield", Reason: "x"}}}

			l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "s"}, testTrigger())

			for _, name := range []string{SpanDecision, SpanAction} {
				spans := spansByName(sr.Ended(), name)
				if len(spans) != 1 {
					t.Fatalf("%s spans = %d, want 1", name, len(spans))
				}
				v, ok := attrValue(spans[0], AttrDecisionApplied)
				if !ok || v.AsBool() != tc.wantApplied {
					t.Errorf("%s %s = %v (present %v), want %v", name, AttrDecisionApplied, v.Emit(), ok, tc.wantApplied)
				}
				r, has := attrValue(spans[0], AttrDecisionNotAppliedReason)
				if has != (tc.wantReason != "") || r.AsString() != tc.wantReason {
					t.Errorf("%s %s = %q (present %v), want %q", name, AttrDecisionNotAppliedReason, r.AsString(), has, tc.wantReason)
				}
			}
		})
	}
}

// UC: UC-040 A27
// A write that errors is recorded as not applied with write_failed on both spans.
func TestWriteFailed_SpansRecordNotApplied(t *testing.T) {
	l, sr, _ := newTestLoop(t)
	l.Actions = &failingSink{}
	l.Rules = fakeRules{out: []Decision{{Intervention: "grant_shield", Reason: "x"}}}

	l.OnEvaluate(context.Background(), gamecontext.GameContext{SessionID: "s"}, testTrigger())

	for _, name := range []string{SpanDecision, SpanAction} {
		spans := spansByName(sr.Ended(), name)
		if len(spans) != 1 {
			t.Fatalf("%s spans = %d, want 1", name, len(spans))
		}
		if v, ok := attrValue(spans[0], AttrDecisionApplied); !ok || v.AsBool() {
			t.Errorf("%s %s = %v (present %v), want false", name, AttrDecisionApplied, v.Emit(), ok)
		}
		if r, _ := attrValue(spans[0], AttrDecisionNotAppliedReason); r.AsString() != NotAppliedWriteFailed {
			t.Errorf("%s reason = %q, want %q", name, r.AsString(), NotAppliedWriteFailed)
		}
	}
}

type failingSink struct{}

func (failingSink) Apply(context.Context, Decision) error { return errors.New("flag service refused") }

// UC: UC-035 3
// The flag state carries the `enabled` state, so the panel shows watch and on.
func TestFlagState_CarriesEnabledState(t *testing.T) {
	l, b, fl := boardLoop(t)
	c := modeCtx("game_fs2093", "FFA")
	for _, want := range []flags.EnabledState{flags.EnabledWatch, flags.EnabledOn} {
		fl.snap.EnabledState = want
		l.OnEvaluate(context.Background(), c, testTrigger())
		if got := b.List(); len(got) != 1 || got[0].Enabled != string(want) {
			t.Errorf("enabled = %+v, want %q", got, want)
		}
	}
}
