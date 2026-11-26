package decision

import (
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
)

// Per-backend act-confidence threshold (#1287): one flag per backend,
// decision.<backend>.act_confidence_threshold (jev 0.6: its confidence is the
// calibrated gate p_yes; claude 0.8), no shared or inherited one. These tests use
// the flag defaults unless they say otherwise.
// Spec: decide-contract.md §5 and §12, UC-040 step 9 and A13, FR-047.

// nearThresholdConfidence is the Jev gate's p_yes for a near-threshold state.
const nearThresholdConfidence = 0.72

// defaultThresholdSnapshot is remoteSnapshot with the decision backend set to name
// and both thresholds at their flag defaults.
func defaultThresholdSnapshot(name string, timeout time.Duration) flags.Snapshot {
	s := remoteSnapshot(timeout)
	s.Decision.Backend = name
	s.Decision.Claude.ActConfidenceThreshold = flags.DefaultDecisionClaudeActConfidenceThreshold
	s.Decision.Jev = flags.RemoteBackendConfig{
		Model: flags.DefaultDecisionJevModel, Timeout: timeout,
		ActConfidenceThreshold: flags.DefaultDecisionJevActConfidenceThreshold,
	}
	return s
}

func jevBackend(conf float64) *fakeDecideBackend {
	be := newFakeDecideBackend(ffaAnswer("grant_shield", conf, "A", 0.5, "near threshold"))
	be.name = flags.DecisionBackendJev
	return be
}

// assertThreshold checks the routed threshold and that the evaluation event names
// the backend's own flag.
func assertThreshold(t *testing.T, span sdktrace.ReadOnlySpan, want float64, backend string) {
	t.Helper()
	if v, _ := attrValue(span, AttrDecideActThreshold); v.AsFloat64() != want {
		t.Errorf("%s = %v, want %v", AttrDecideActThreshold, v.AsFloat64(), want)
	}
	if _, ok := attrValue(span, "decide.act_confidence_threshold.source"); ok {
		t.Error("the retired decide.act_confidence_threshold.source is still set")
	}
	wantFlag := "decision." + backend + ".act_confidence_threshold"
	for _, ev := range span.Events() {
		if ev.Name != evaluationEventName {
			continue
		}
		for _, kv := range ev.Attributes {
			if string(kv.Key) == "gen_ai.evaluation.explanation" {
				if !strings.Contains(kv.Value.AsString(), "threshold "+fmtFloat(want)+" ("+wantFlag+")") {
					t.Errorf("explanation = %q, want threshold %s from %s", kv.Value.AsString(), fmtFloat(want), wantFlag)
				}
				return
			}
		}
	}
	t.Error("no gen_ai.evaluation.result explanation")
}

// UC: UC-040 main A13
func TestThreshold_JevActsAtGateByDefault(t *testing.T) {
	h := newRemoteHarness(t, defaultThresholdSnapshot(flags.DecisionBackendJev, time.Second), jevBackend(nearThresholdConfidence))
	span := h.run(t)
	if g := strAttr(t, span, AttrDecideOutcome); g != DecideOutcomeAct {
		t.Fatalf("outcome = %q, want act (0.72 >= jev 0.6)", g)
	}
	if len(h.sink.decisions()) != 1 {
		t.Fatalf("applied = %+v, want one decision", h.sink.decisions())
	}
	assertThreshold(t, span, flags.DefaultDecisionJevActConfidenceThreshold, flags.DecisionBackendJev)
	assertEvaluation(t, span, evaluationLabelPass, nearThresholdConfidence)
}

// UC: UC-040 A5
func TestThreshold_ClaudeHeldAtItsOwnByDefault(t *testing.T) {
	be := newFakeDecideBackend(ffaAnswer("grant_shield", nearThresholdConfidence, "A", 0.5, "x"))
	h := newRemoteHarness(t, defaultThresholdSnapshot(flags.DecisionBackendClaude, time.Second), be)
	span := h.run(t)
	if g := strAttr(t, span, AttrDecideOutcome); g != DecideOutcomeHeld {
		t.Fatalf("outcome = %q, want held (0.72 < claude 0.8; the jev threshold must not apply)", g)
	}
	if len(h.sink.decisions()) != 0 {
		t.Fatal("held answer applied")
	}
	assertThreshold(t, span, flags.DefaultDecisionClaudeActConfidenceThreshold, flags.DecisionBackendClaude)
	assertEvaluation(t, span, evaluationLabelFail, nearThresholdConfidence)
}

// TestThreshold_RequestCarriesFireTimeThreshold: the answering backend's threshold
// rides on the request, so jev's gate opens at the bar routing applies (§3.1).
// UC: UC-040 A13
func TestThreshold_RequestCarriesFireTimeThreshold(t *testing.T) {
	var got float64
	be := newFakeDecideBackend(func(req decide.Request) (decide.Response, error) {
		got = req.ActThreshold
		return ffaAnswer("grant_shield", nearThresholdConfidence, "A", 0.5, "x")(req)
	})
	be.name = flags.DecisionBackendJev
	h := newRemoteHarness(t, defaultThresholdSnapshot(flags.DecisionBackendJev, time.Second), be)
	h.run(t)
	if got != flags.DefaultDecisionJevActConfidenceThreshold {
		t.Fatalf("Request.ActThreshold = %v, want jev's %v", got, flags.DefaultDecisionJevActConfidenceThreshold)
	}
}

// UC: UC-040 A13
func TestThreshold_ClaudeFlipDoesNotMoveJev(t *testing.T) {
	// Raising claude's bar must not move jev's, and vice versa.
	snap := defaultThresholdSnapshot(flags.DecisionBackendJev, time.Second)
	snap.Decision.Claude.ActConfidenceThreshold = 0.95
	h := newRemoteHarness(t, snap, jevBackend(nearThresholdConfidence))
	span := h.run(t)
	if g := strAttr(t, span, AttrDecideOutcome); g != DecideOutcomeAct {
		t.Fatalf("outcome = %q, want act under jev's own 0.6", g)
	}
	assertThreshold(t, span, flags.DefaultDecisionJevActConfidenceThreshold, flags.DecisionBackendJev)
}

// UC: UC-040 A5 A13
// UC: UC-044 A3
func TestThreshold_JevRaisedMidFlightAppliesAtLanding(t *testing.T) {
	be := jevBackend(nearThresholdConfidence)
	h, span := fireAndMutateSnapshot(t, be, defaultThresholdSnapshot(flags.DecisionBackendJev, time.Hour), time.Hour,
		func(h remoteHarness) {
			h.flags.mutate(func(s *flags.Snapshot) { s.Decision.Jev.ActConfidenceThreshold = 0.95 })
		})
	if g := strAttr(t, span, AttrDecideOutcome); g != DecideOutcomeHeld {
		t.Fatalf("outcome = %q, want held under the live jev strict threshold", g)
	}
	if len(h.sink.decisions()) != 0 {
		t.Fatal("held answer applied")
	}
	assertThreshold(t, span, 0.95, flags.DecisionBackendJev)
}
