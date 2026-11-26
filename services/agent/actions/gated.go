package actions

import (
	"context"
	"log/slog"

	"github.com/joustmania/agent/decision"
)

// InterventionGate resolves the LIVE interventions_enabled safety gate (#1213) at
// USE-TIME. *flags.Flags satisfies it via InterventionsEnabled; tests supply a
// fake. It is read on EVERY Apply (never cached at construction) so flipping the
// flag in flagd starts/stops actuation with no restart, and a flagd outage at the
// moment of an apply FAILS CLOSED (InterventionsEnabled returns the fail-closed
// default false) so the agent never writes an intervention when it cannot
// confirm the gate is on (#1177/#1217).
type InterventionGate interface {
	InterventionsEnabled(ctx context.Context) bool
}

// GatedActionSink wraps the real intervention Writer behind the LIVE
// interventions_enabled flag (#1213). It REPLACES the old startup-time
// AGENT_INTERVENTIONS_ENABLED env gate (which selected the Writer-or-nil sink ONCE
// in main.go and so could never react to a flag flip and failed open to whatever
// sink was chosen at boot). Apply re-evaluates the gate per decision against the
// live Flags: when on it delegates to the real Writer; when off (or flagd
// unreachable) it discards the decision exactly like the loop's default discarding sink, so a fully-gated,
// decided intervention is still recorded/spanned by the loop but never applied.
type GatedActionSink struct {
	gate   InterventionGate
	writer decision.ActionSink
	log    *slog.Logger
}

// NewGatedActionSink wraps writer behind gate. writer is the real sink applied when
// the gate is on (typically *Writer); gate is the live interventions_enabled
// source. log nil → slog.Default().
func NewGatedActionSink(gate InterventionGate, writer decision.ActionSink, log *slog.Logger) *GatedActionSink {
	if log == nil {
		log = slog.Default()
	}
	return &GatedActionSink{gate: gate, writer: writer, log: log}
}

// Apply evaluates the LIVE interventions_enabled gate (#1213) and delegates to the
// real writer only when it is on; otherwise it is a no-op (the decision was already
// recorded/spanned by the loop). Fail-closed: a flagd error inside the gate
// resolves to false, so the agent does NOT write when the control plane is
// unreachable.
func (s *GatedActionSink) Apply(ctx context.Context, d decision.Decision) error {
	_, err := s.ApplyReported(ctx, d)
	return err
}

// ApplyReported is Apply with the outcome reported: applied is true only when the
// LIVE gate was on and the real writer ran. The loop counts
// agent_interventions_applied_total only for applied=true, so a gated no-op is not
// counted as an applied intervention (UC-040 A11, #1681).
func (s *GatedActionSink) ApplyReported(ctx context.Context, d decision.Decision) (applied bool, err error) {
	// UC-045 BR-001: interventions_enabled is the actuation gate of the three.
	if !s.gate.InterventionsEnabled(ctx) {
		s.log.Debug("agent.intervention_gated_off",
			"intervention", d.Intervention, "flag", keyInterventionsEnabledLog)
		return false, nil
	}
	return true, s.writer.Apply(ctx, d)
}

// Compile-time check: the loop reads the gated outcome only through this optional
// interface, so a signature drift would silently count gated no-ops again.
var _ decision.ActionApplier = (*GatedActionSink)(nil)

// keyInterventionsEnabledLog is the flag key surfaced in the gated-off log line.
// Mirrors flags.keyInterventionsEnabled (kept local so actions does not need the
// flags key const exported).
const keyInterventionsEnabledLog = "interventions_enabled"
