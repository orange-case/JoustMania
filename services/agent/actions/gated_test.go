package actions

import (
	"context"
	"errors"
	"testing"

	"github.com/joustmania/agent/decision"
)

// staticGate is a constant InterventionGate for the gated-wrapper unit tests.
type staticGate bool

func (g staticGate) InterventionsEnabled(context.Context) bool { return bool(g) }

// recordingSink records whether Apply was called (the real Writer stand-in for the
// intervention-gate tests).
type recordingSink struct {
	applied bool
	err     error
}

func (r *recordingSink) Apply(context.Context, decision.Decision) error {
	r.applied = true
	return r.err
}

// TestGatedActionSink_GateOff_NoApply: with the gate OFF (the fail-closed default,
// and what a flagd outage resolves to) the gated sink does NOT call the wrapped
// writer and returns nil — a decided, fully-gated intervention is never written.
// UC: UC-045 A1 BR-001
func TestGatedActionSink_GateOff_NoApply(t *testing.T) {
	inner := &recordingSink{}
	sink := NewGatedActionSink(staticGate(false), inner, nil)
	if err := sink.Apply(context.Background(), decision.Decision{Intervention: "audio_cue"}); err != nil {
		t.Fatalf("gate-off Apply must be a no-op, got %v", err)
	}
	if inner.applied {
		t.Fatal("gate-off Apply must NOT call the wrapped writer")
	}
}

// TestGatedActionSink_GateOn_Delegates: with the gate ON the gated sink delegates to
// the wrapped writer (and propagates its error).
func TestGatedActionSink_GateOn_Delegates(t *testing.T) {
	sentinel := errors.New("write failed")
	inner := &recordingSink{err: sentinel}
	sink := NewGatedActionSink(staticGate(true), inner, nil)
	if err := sink.Apply(context.Background(), decision.Decision{Intervention: "audio_cue"}); !errors.Is(err, sentinel) {
		t.Fatalf("gate-on Apply must delegate (and propagate the writer error), got %v", err)
	}
	if !inner.applied {
		t.Fatal("gate-on Apply must call the wrapped writer")
	}
}

// TestGatedActionSink_ApplyReportsOutcome: ApplyReported says whether the write
// happened, so the loop counts agent_interventions_applied_total only for a real
// write and never for a gated no-op (#1681).
// UC: UC-040 A11
func TestGatedActionSink_ApplyReportsOutcome(t *testing.T) {
	t.Run("UC-040/A11 gate off reports not applied", func(t *testing.T) {
		inner := &recordingSink{}
		applied, err := NewGatedActionSink(staticGate(false), inner, nil).
			ApplyReported(context.Background(), decision.Decision{Intervention: "audio_cue"})
		if err != nil || applied || inner.applied {
			t.Fatalf("gate off: applied=%v err=%v writer called=%v, want false nil false", applied, err, inner.applied)
		}
	})
	t.Run("UC-040/main gate on reports applied", func(t *testing.T) {
		inner := &recordingSink{}
		applied, err := NewGatedActionSink(staticGate(true), inner, nil).
			ApplyReported(context.Background(), decision.Decision{Intervention: "audio_cue"})
		if err != nil || !applied || !inner.applied {
			t.Fatalf("gate on: applied=%v err=%v writer called=%v, want true nil true", applied, err, inner.applied)
		}
	})
}
