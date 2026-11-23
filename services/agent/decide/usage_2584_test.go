package decide

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

// UC: UC-040 main
func TestUsage_Accumulates(t *testing.T) {
	t.Run("UC-040/main sums reported counts only", func(t *testing.T) {
		u := &Usage{}
		u.Add(CallUsage{InputTokens: Tokens(10), FinishReasons: []string{"stop"}})
		u.Add(CallUsage{InputTokens: Tokens(5), OutputTokens: Tokens(0), FinishReasons: []string{"error", "stop", ""}})
		got := u.Totals()
		if got.Calls != 2 || got.InputTokens != Tokens(15) || got.OutputTokens != Tokens(0) {
			t.Errorf("totals = %+v", got)
		}
		if got.CacheReadInputTokens.OK || got.CacheWriteInputTokens.OK || got.ReasoningOutputTokens.OK {
			t.Errorf("unreported counts must stay unset: %+v", got)
		}
		// Positional, no de-duplication; "" is skipped.
		if !reflect.DeepEqual(got.FinishReasons, []string{"stop", "error", "stop"}) {
			t.Errorf("finish reasons = %v, want [stop error stop]", got.FinishReasons)
		}
	})

	t.Run("UC-040/main concurrent calls", func(t *testing.T) {
		u := &Usage{}
		ctx := WithUsage(context.Background(), u)
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				RecordCall(ctx, CallUsage{OutputTokens: Tokens(2)})
			}()
		}
		wg.Wait()
		if got := u.Totals(); got.Calls != 50 || got.OutputTokens != Tokens(100) {
			t.Errorf("totals = %+v, want 50 calls, 100 output tokens", got)
		}
	})

	t.Run("UC-040/main no accumulator: no-op", func(t *testing.T) {
		RecordCall(context.Background(), CallUsage{InputTokens: Tokens(1)})
	})
}
