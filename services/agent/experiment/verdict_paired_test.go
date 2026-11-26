package experiment

import (
	"math"
	"testing"

	"github.com/joustmania/agent/experiment/journal"
)

// verdict_paired_test.go covers the Common-Random-Numbers paired-difference verdict
// path (#1004): d_z = mean(d)/sd(d) over Summary.PairDiff, the min-pairs gate, the
// fallback to the two-arm Cohen's d when no pairs exist, and the CRN precision
// gain over the two-arm path on the same data.

// pairDiffFrom folds the given per-pair differences d_i through the real Welford
// recurrence, producing the PairDiff accumulator the journal would persist.
func pairDiffFrom(diffs ...float64) *journal.Welford {
	w := &journal.Welford{}
	for _, d := range diffs {
		w.Add(d)
	}
	return w
}

// pairedSummary assembles a Summary carrying ONLY a PairDiff (the canonical arms are
// left empty so a regression to the two-arm path would be visible as ok=false).
func pairedSummary(pd *journal.Welford) journal.Summary {
	return journal.Summary{
		ExperimentID: "exp_paired",
		Status:       "running",
		Arms:         map[string]*journal.ArmStat{},
		PairDiff:     pd,
	}
}

// UC: UC-043 main
func TestPairedVerdict_PromotesOnPositiveDz(t *testing.T) {
	v := NewVerdict(5, 0.5)
	// 6 pairs, mean(d)=0.30, small spread ⇒ large d_z ⇒ PROMOTE.
	pd := pairDiffFrom(0.28, 0.32, 0.29, 0.31, 0.30, 0.30)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != CohortOutcomePromote || !got.Significant {
		t.Fatalf("verdict = %+v, want PROMOTE significant", got)
	}
	if math.Abs(got.Delta-pd.Mean) > 1e-9 {
		t.Errorf("Delta = %v, want mean(d)=%v", got.Delta, pd.Mean)
	}
}

// UC: UC-043 A3
func TestPairedVerdict_DiscardsOnNegativeDz(t *testing.T) {
	v := NewVerdict(5, 0.5)
	pd := pairDiffFrom(-0.28, -0.32, -0.29, -0.31, -0.30, -0.30)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != CohortOutcomeDiscard || !got.Significant {
		t.Fatalf("verdict = %+v, want DISCARD significant", got)
	}
}

// UC: UC-043 main
func TestPairedVerdict_MinPairsGate(t *testing.T) {
	v := NewVerdict(5, 0.5)
	// Only 3 pairs (< min-pairs 5), even with a huge clean effect ⇒ INCONCLUSIVE.
	pd := pairDiffFrom(0.50, 0.50, 0.50)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != OutcomeInconclusive || got.Significant {
		t.Fatalf("verdict = %+v, want under-powered INCONCLUSIVE", got)
	}
}

func TestPairedVerdict_NoOpFlagWithinNoise(t *testing.T) {
	v := NewVerdict(5, 0.5)
	// A no-op flag: per-pair deltas tightly centered on 0 (CRN cancels shared
	// variance). |d_z| is small ⇒ within-noise INCONCLUSIVE (not a false positive).
	pd := pairDiffFrom(0.01, -0.01, 0.02, -0.02, 0.00, 0.01, -0.01)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != OutcomeInconclusive || got.Significant {
		t.Fatalf("verdict = %+v, want within-noise INCONCLUSIVE", got)
	}
}

// TestPairedVerdict_ZeroSpreadMeaningfulDelta_SDFloor (#1042): every pair identical
// (zero spread) but with a MEANINGFUL per-pair delta (0.20, well above the raw-effect
// floor). The minimum-SD floor caps the denominator so d_z is large-but-finite (not
// NaN/Inf), and because the raw effect clears the practical floor this is a legitimate
// PROMOTE — the SD floor turns the old "undefined ⇒ inconclusive" bail-out into a sane
// conclusion when the delta actually matters.
// UC: UC-043 main
func TestPairedVerdict_ZeroSpreadMeaningfulDelta_SDFloor(t *testing.T) {
	v := NewVerdict(5, 0.5)
	pd := pairDiffFrom(0.20, 0.20, 0.20, 0.20, 0.20, 0.20)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != CohortOutcomePromote || !got.Significant {
		t.Fatalf("verdict = %+v, want PROMOTE (meaningful delta, SD floored)", got)
	}
}

// TestPairedVerdict_TinyConsistentDelta_NotPractical (#1042) is the EXACT dry-run
// scenario: a tiny per-pair delta (~ -0.0005) that is perfectly consistent across
// pairs. The SD collapses toward 0 so the standardized d_z is enormous (would be the
// dry-run's d_z=-34), but |mean_d| is FAR below the practical-significance floor →
// INCONCLUSIVE, NOT discard. This is the core bug fix.
// UC: UC-043 main
func TestPairedVerdict_TinyConsistentDelta_NotPractical(t *testing.T) {
	v := NewVerdict(5, 0.5)
	// exp ~0.0496..0.0506 vs ctl ~0.0500..0.0510 ⇒ d_i ≈ -0.0004..-0.0006, tiny + consistent.
	pd := pairDiffFrom(-0.0004, -0.0005, -0.0006, -0.0005, -0.0004, -0.0005)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != OutcomeInconclusive || got.Significant {
		t.Fatalf("verdict = %+v, want INCONCLUSIVE (tiny delta below raw-effect floor); the dry-run bug would DISCARD here", got)
	}
}

// TestPairedVerdict_NoOpFlag_ZeroDelta_Inconclusive (#1042): a true no-op flag with
// IDENTICAL fitness per pair ⇒ mean_d == 0 and zero spread. SD floored, but mean_d=0
// is below the raw-effect floor ⇒ INCONCLUSIVE (never a false promote/discard).
func TestPairedVerdict_NoOpFlag_ZeroDelta_Inconclusive(t *testing.T) {
	v := NewVerdict(5, 0.5)
	pd := pairDiffFrom(0.0, 0.0, 0.0, 0.0, 0.0, 0.0)
	got, ok := v.Evaluate(pairedSummary(pd))
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if got.Outcome != OutcomeInconclusive || got.Significant {
		t.Fatalf("verdict = %+v, want no-op INCONCLUSIVE", got)
	}
}

// UC-043 step 5: once seed-matched pairs exist, the verdict is the paired d_z
// gated only by verdict_min_pairs. verdict_min_n (here far above the pair
// count) does not hold back a conclusive paired verdict.
//
// UC: UC-043 main
func TestPairedVerdict_GatedByMinPairsNotMinN(t *testing.T) {
	const minN, minPairs = 50, 5
	v := NewVerdict(minPairs, 0.5)
	diffs := []float64{0.28, 0.32, 0.29, 0.31, 0.30}

	t.Run("UC-043/main one pair short of verdict_min_pairs is inconclusive", func(t *testing.T) {
		got, ok := v.Evaluate(pairedSummary(pairDiffFrom(diffs[:minPairs-1]...)))
		if !ok {
			t.Fatal("expected ok=true")
		}
		if got.Outcome != OutcomeInconclusive || got.Significant {
			t.Fatalf("verdict = %+v, want under-powered INCONCLUSIVE", got)
		}
	})

	t.Run("UC-043/main exactly verdict_min_pairs promotes although every arm is below verdict_min_n", func(t *testing.T) {
		s := pairedSummary(pairDiffFrom(diffs...))
		// Arms carry far fewer samples than verdict_min_n; the paired path ignores them.
		exp, ctl := &journal.Welford{}, &journal.Welford{}
		for i := 0; i < minPairs; i++ {
			exp.Add(0.8 + float64(i)*0.01)
			ctl.Add(0.5 + float64(i)*0.01)
		}
		s.Arms[ArmExperimental] = &journal.ArmStat{Welford: *exp}
		s.Arms[ArmControl] = &journal.ArmStat{Welford: *ctl}
		got, ok := v.Evaluate(s)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if got.Outcome != CohortOutcomePromote || !got.Significant {
			t.Fatalf("verdict = %+v, want PROMOTE significant at %d pairs (min N %d not applied)", got, minPairs, minN)
		}
	})
}
