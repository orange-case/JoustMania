package flags

import (
	"context"
	"errors"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
)

// UC: UC-040 BR-044
// operator_brief_uc040_test.go: decision.operator_brief is read per game, cleaned,
// and "" on any failure (#2687, decide-contract.md §3).

type briefEvaluator struct {
	stubEvaluator
	value string
	err   error
	reads int
}

func (b *briefEvaluator) StringValue(_ context.Context, flag, def string, _ openfeature.EvaluationContext, _ ...openfeature.Option) (string, error) {
	if flag != keyDecisionOperatorBrief {
		return def, nil
	}
	b.reads++
	return b.value, b.err
}

func TestOperatorBrief_Read(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		err         error
		want        string
	}{
		{"UC-040/BR-044 a brief is read and cleaned", "  keep <red> alive ", nil, "keep ‹red› alive"},
		{"UC-040/BR-044 the off variant is no brief", "", nil, ""},
		{"UC-040/BR-044 a read error is no brief", "keep red alive", errors.New("PROVIDER_NOT_READY"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := &briefEvaluator{value: tc.value, err: tc.err}
			if got := New(ev, nil).OperatorBrief(context.Background(), "g-1"); got != tc.want {
				t.Fatalf("OperatorBrief = %q, want %q", got, tc.want)
			}
			if ev.reads != 1 {
				t.Fatalf("reads = %d, want 1 (read per request)", ev.reads)
			}
		})
	}
	t.Run("UC-040/BR-044 a nil wrapper or client is no brief", func(t *testing.T) {
		var f *Flags
		if got := f.OperatorBrief(context.Background(), "g-1"); got != "" {
			t.Fatalf("nil Flags = %q", got)
		}
		if got := New(nil, nil).OperatorBrief(context.Background(), "g-1"); got != "" {
			t.Fatalf("nil client = %q", got)
		}
	})
}
