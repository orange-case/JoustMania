package actions

import (
	"context"
	"log/slog"

	"github.com/joustmania/agent/decision"
)

// InterventionGate resolves the live `enabled` state at use time: true only when
// it is act. *flags.Flags satisfies it via Applying; tests supply a fake. It is
// read on every Apply, never cached, so a flag flip takes effect with no restart,
// and a flagd outage fails closed (off), so the agent never writes when it cannot
// confirm `enabled` is act.
type InterventionGate interface {
	Applying(ctx context.Context) bool
}

// GatedActionSink wraps the real intervention Writer behind the live `enabled`
// state. When it is act, Apply delegates to the Writer; in watch (or off, or with
// flagd unreachable) it discards the decision like the loop's default sink, so a
// decided intervention is still recorded and spanned by the loop but never applied.
type GatedActionSink struct {
	gate   InterventionGate
	writer decision.ActionSink
	log    *slog.Logger
}

// NewGatedActionSink wraps writer behind gate. writer is the real sink applied when
// `enabled` is act (typically *Writer). log nil → slog.Default().
func NewGatedActionSink(gate InterventionGate, writer decision.ActionSink, log *slog.Logger) *GatedActionSink {
	if log == nil {
		log = slog.Default()
	}
	return &GatedActionSink{gate: gate, writer: writer, log: log}
}

// Apply delegates to the real writer only while `enabled` is act; otherwise it is a
// no-op (the loop already recorded and spanned the decision).
func (s *GatedActionSink) Apply(ctx context.Context, d decision.Decision) error {
	_, err := s.ApplyReported(ctx, d)
	return err
}

// ApplyReported is Apply with the outcome reported: applied is true only when
// `enabled` was act and the real writer ran. The loop counts
// agent_interventions_applied_total only for applied=true, so a gated no-op is not
// counted as an applied intervention (UC-040 A11).
func (s *GatedActionSink) ApplyReported(ctx context.Context, d decision.Decision) (applied bool, err error) {
	if !s.gate.Applying(ctx) {
		s.log.Debug("agent.intervention_gated_off",
			"intervention", d.Intervention, "flag", keyEnabledLog)
		return false, nil
	}
	return true, s.writer.Apply(ctx, d)
}

// Compile-time check: the loop reads the gated outcome only through this optional
// interface, so a signature drift would silently count gated no-ops again.
var _ decision.ActionApplier = (*GatedActionSink)(nil)

// keyEnabledLog is the flag key surfaced in the gated-off log line.
const keyEnabledLog = "enabled"
