package decision

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
)

// UC: UC-040 BR-044
// operator_brief_uc040_test.go: a claude or openai decide request carries the
// operator's decision.operator_brief, jev never does, the brief changes neither the
// state nor the questions, and agent.decide records only its presence and length
// (#2687, decide-contract.md §3, §12).

func TestRemote_OperatorBrief(t *testing.T) {
	type result struct {
		req      decide.Request
		asked    int
		present  bool
		length   int64
		hasAttrs bool
	}
	run := func(t *testing.T, backend string, brief *string) result {
		t.Helper()
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.9, "A", 0.5, "shield"))
		be.name = backend
		snap := remoteSnapshot(time.Second)
		snap.Decision.Backend = backend
		snap.Decision.OpenAI = flags.RemoteBackendConfig{Model: "gpt-x", Timeout: time.Second}
		snap.Decision.Jev = flags.RemoteBackendConfig{Model: "jev-latest", Timeout: time.Second}
		h := newRemoteHarness(t, snap, be)
		var res result
		if brief != nil {
			h.loop.SetOperatorBriefSource(func(_ context.Context, gameID string) string {
				if gameID != remoteGameID {
					t.Errorf("brief read for %q", gameID)
				}
				res.asked++
				return *brief
			})
		}
		span := h.run(t)
		if len(be.reqs) != 1 {
			t.Fatalf("backend saw %d requests", len(be.reqs))
		}
		res.req = be.reqs[0]
		p, okP := attrValue(span, AttrDecideOperatorBriefPresent)
		l, okL := attrValue(span, AttrDecideOperatorBriefLength)
		res.present, res.length, res.hasAttrs = p.AsBool(), l.AsInt64(), okP || okL
		return res
	}
	brief := "keep the red player alive"

	for _, backend := range []string{flags.DecisionBackendClaude, flags.DecisionBackendOpenAI} {
		t.Run("UC-040/BR-044 "+backend+" gets the brief and the span its presence and length", func(t *testing.T) {
			r := run(t, backend, &brief)
			if r.req.OperatorBrief != brief || r.asked != 1 {
				t.Fatalf("request brief %q (read %d times)", r.req.OperatorBrief, r.asked)
			}
			if !r.present || r.length != int64(len(brief)) {
				t.Fatalf("span present=%v length=%d", r.present, r.length)
			}
		})
	}
	t.Run("UC-040/BR-044 jev never gets the brief and its span has no brief attributes", func(t *testing.T) {
		r := run(t, flags.DecisionBackendJev, &brief)
		if r.req.OperatorBrief != "" || r.asked != 0 || r.hasAttrs {
			t.Fatalf("jev request brief %q, read %d, span attributes %v", r.req.OperatorBrief, r.asked, r.hasAttrs)
		}
	})
	t.Run("UC-040/BR-044 an empty brief or no source leaves the request and the span as before", func(t *testing.T) {
		empty := ""
		for _, b := range []*string{nil, &empty} {
			r := run(t, flags.DecisionBackendClaude, b)
			if r.req.OperatorBrief != "" || r.hasAttrs {
				t.Fatalf("request brief %q, span attributes %v", r.req.OperatorBrief, r.hasAttrs)
			}
		}
	})
	t.Run("UC-040/BR-044 the brief changes neither the state nor the questions", func(t *testing.T) {
		with := run(t, flags.DecisionBackendClaude, &brief)
		without := run(t, flags.DecisionBackendClaude, nil)
		if with.req.State.Digest != without.req.State.Digest || !reflect.DeepEqual(with.req.Questions, without.req.Questions) {
			t.Fatal("the operator brief must only add text a backend renders, never a question, option or range")
		}
	})
}
