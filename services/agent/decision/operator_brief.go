package decision

import (
	"context"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/joustmania/agent/flags"
)

// The operator brief (#2687, decide-contract.md §3 and §12, UC-040 BR-044,
// FR-096): decision.operator_brief, the operator's free-text wish, rendered into
// the claude prompt only. It adds no lever, no option and no bound.
const (
	// AttrDecideOperatorBriefPresent is true on a remote invoke_agent span whose
	// request carried an operator brief; absent otherwise.
	AttrDecideOperatorBriefPresent = "decide.operator_brief.present"
	// AttrDecideOperatorBriefLength is that brief's length in characters (runes),
	// after decide.NormalizeOperatorBrief. The text itself is never a decide.*
	// attribute: only the GenAI content capture records it.
	AttrDecideOperatorBriefLength = "decide.operator_brief.length"
)

// OperatorBriefSource returns the cleaned decision.operator_brief of one game ("" =
// none). main.go wires flags.Flags.OperatorBrief, which gives "" on any error.
type OperatorBriefSource func(ctx context.Context, gameID string) string

// SetOperatorBriefSource injects the operator-brief source of remote decide
// requests. nil (the default and every older test) never sets
// Request.OperatorBrief, so the prompts stay byte-identical. Set at construction,
// before the loop serves.
func (l *Loop) SetOperatorBriefSource(src OperatorBriefSource) { l.operatorBrief = src }

// operatorBriefFor is the brief a request to backend carries: the source's value
// for claude, "" for every other backend (jev's gate criteria are fixed
// and calibrated, decide-contract.md §3) and without a source.
func (l *Loop) operatorBriefFor(ctx context.Context, backend, gameID string) string {
	if l.operatorBrief == nil || backend != flags.DecisionBackendClaude {
		return ""
	}
	return l.operatorBrief(ctx, gameID)
}

// operatorBriefAttributes are the invoke_agent attributes of a request's brief:
// none without one.
func operatorBriefAttributes(brief string) []attribute.KeyValue {
	if brief == "" {
		return nil
	}
	return []attribute.KeyValue{
		attribute.Bool(AttrDecideOperatorBriefPresent, true),
		attribute.Int(AttrDecideOperatorBriefLength, utf8.RuneCountInString(brief)),
	}
}
