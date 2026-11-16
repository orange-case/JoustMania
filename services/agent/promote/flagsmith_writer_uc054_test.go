package promote

import (
	"context"
	"testing"
)

// A value the Flagsmith link applied carries writer "flagsmith" (UC-054
// BR-009): the agent never takes it for a promotion of its own, so it never
// lists it for the kill-switch revert and never reads it back as its overlay.
// UC: UC-054 BR-009
func TestFlagsmithOverlayIsNeverAPromotion(t *testing.T) {
	ctx := context.Background()
	svc := newPromotionService(t)
	putOverlay(t, svc, "flagsmith", 2.0)
	w := writerForTest(t, svc, t.TempDir())

	t.Run("UC-054/BR-009 a flagsmith real overlay is not listed as promoted", func(t *testing.T) {
		got, err := w.PromotedFlags(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("PromotedFlags = %v, want none: the overlay is the operator's from Flagsmith", got)
		}
	})
	t.Run("UC-054/BR-009 a flagsmith real overlay is not read back as the promotion's value", func(t *testing.T) {
		if _, ok, err := w.PromotionOverlay(ctx, promotedFlag); err != nil || ok {
			t.Fatalf("PromotionOverlay = (ok %v, %v), want not the promotion's", ok, err)
		}
	})
}
