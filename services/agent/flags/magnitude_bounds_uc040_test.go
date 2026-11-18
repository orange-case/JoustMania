package flags

import (
	"context"
	"errors"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// magnitude_bounds_uc040_test.go: decision.magnitude_bounds is read per game and
// parsed against the hard limits; any read failure gives the code default ranges
// (#2696, decide-contract.md §2.1).

func TestMagnitudeBounds_Read(t *testing.T) {
	t.Run("UC-040/BR-045 an operator value is read and applied", func(t *testing.T) {
		ev := stubEvaluator{objects: map[string]any{keyDecisionMagnitudeBounds: map[string]any{
			"adjust_player_sensitivity": map[string]any{"min": 0.2, "max": 1.8},
		}}}
		mt := New(ev, nil).MagnitudeBounds(context.Background(), "g-1")
		if b, _ := mt.Lookup("adjust_player_sensitivity"); b.Min != 0.2 || mt.Source() != decide.MagnitudeSourceFlag {
			t.Fatalf("got %+v source %s", b, mt.Source())
		}
	})
	t.Run("UC-040/BR-045 an out-of-limit entry falls back and is named", func(t *testing.T) {
		ev := stubEvaluator{objects: map[string]any{keyDecisionMagnitudeBounds: map[string]any{
			"adjust_player_sensitivity": map[string]any{"min": 0.1, "max": 1.8},
		}}}
		mt := New(ev, nil).MagnitudeBounds(context.Background(), "g-1")
		if mt.Source() != decide.MagnitudeSourceDefault || mt.InvalidString() != "adjust_player_sensitivity" {
			t.Fatalf("source %s invalid %q", mt.Source(), mt.InvalidString())
		}
	})
	t.Run("UC-040/BR-045 a read error, a missing flag, a nil wrapper or client give the code defaults", func(t *testing.T) {
		errEv := stubEvaluator{errs: map[string]error{keyDecisionMagnitudeBounds: errors.New("PROVIDER_NOT_READY")},
			objects: map[string]any{keyDecisionMagnitudeBounds: map[string]any{"grant_shield": map[string]any{"min": 4, "max": 8}}}}
		var nilFlags *Flags
		for name, mt := range map[string]*decide.MagnitudeTable{
			"error":   New(errEv, nil).MagnitudeBounds(context.Background(), "g-1"),
			"missing": New(stubEvaluator{}, nil).MagnitudeBounds(context.Background(), "g-1"),
			"nil":     nilFlags.MagnitudeBounds(context.Background(), "g-1"),
			"client":  New(nil, nil).MagnitudeBounds(context.Background(), "g-1"),
		} {
			if mt.Source() != decide.MagnitudeSourceDefault || mt.InvalidString() != "" {
				t.Errorf("%s: source %s invalid %q", name, mt.Source(), mt.InvalidString())
			}
		}
	})
}
