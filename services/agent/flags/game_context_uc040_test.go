package flags

import (
	"context"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
)

// UC: UC-040 BR-044
// game_context_uc040_test.go: per-game agent reads (decision.operator_brief) are
// evaluated in the game's flag context (GameContext).

// ctxBriefEvaluator answers decision.operator_brief with value, recording the
// evaluation context.
type ctxBriefEvaluator struct {
	stubEvaluator
	value string
	seen  []openfeature.EvaluationContext
}

func (p *ctxBriefEvaluator) StringValue(_ context.Context, flag, def string, ec openfeature.EvaluationContext, _ ...openfeature.Option) (string, error) {
	if flag != keyDecisionOperatorBrief {
		return def, nil
	}
	p.seen = append(p.seen, ec)
	return p.value, nil
}

func TestGameContext_OperatorBrief(t *testing.T) {
	t.Run("UC-040/BR-044 read with the game's kind, experiment and arm", func(t *testing.T) {
		ev := &ctxBriefEvaluator{value: "keep the red player alive"}
		f := New(ev, nil)
		f.SetGameAttrs(func(id string) GameAttrs {
			if id != "g-7" {
				t.Errorf("lookup for %q", id)
			}
			return GameAttrs{GameKind: "shadow", ExperimentID: "exp-1", Arm: "b"}
		})
		f.OperatorBrief(context.Background(), "g-7")
		if len(ev.seen) != 1 {
			t.Fatalf("reads = %d", len(ev.seen))
		}
		attrs := ev.seen[0].Attributes()
		for k, want := range map[string]any{"gameId": "g-7", "game_kind": "shadow", "experiment_id": "exp-1", "arm": "b"} {
			if attrs[k] != want {
				t.Errorf("%s = %v, want %v", k, attrs[k], want)
			}
		}
	})
	t.Run("UC-040/BR-044 an unknown game reads as real", func(t *testing.T) {
		ev := &ctxBriefEvaluator{}
		New(ev, nil).OperatorBrief(context.Background(), "g-8")
		if attrs := ev.seen[0].Attributes(); attrs["game_kind"] != "real" || attrs["arm"] != nil {
			t.Errorf("context = %v", attrs)
		}
	})
}
