package experiment

import (
	"fmt"
	"math"
	"sync"

	"github.com/joustmania/agent/experiment/journal"
)

// verdict.go is the CohortVerdict seam: it turns a journal.Summary into a
// promote / discard / inconclusive verdict (UC-043 step 5, FR-063). The only
// statistic is the paired effect d_z = mean(d) / sd(d) over the per-pair fitness
// differences of seed-sharing pairs; the agent's experiments always run in pairs,
// so an experiment without a completed pair is simply inconclusive.

// Default gate thresholds; each equals its flag default and holds until the live
// value arrives via SetThresholds / SetFloatThresholds.
const (
	// DefaultEffectThreshold is the minimum |d_z| for promote/discard (Cohen's
	// "medium" effect; verdict_effect_threshold).
	DefaultEffectThreshold = 0.5

	// DefaultMinPairs is the minimum number of completed pairs for a conclusive
	// verdict (verdict_min_pairs).
	DefaultMinPairs = 5

	// DefaultMinRawEffect is the practical-significance floor on |mean(d)|
	// (verdict_min_raw_effect): on near-deterministic shadow games sd(d)→0 makes
	// d_z explode on a meaningless delta, so the raw difference must clear 2% of the
	// fitness range too.
	DefaultMinRawEffect = 0.02

	// DefaultSDFloor floors sd(d) so d_z stays finite (verdict_sd_floor); the
	// raw-effect floor is the real gate in that regime.
	DefaultSDFloor = 1e-4
)

// Verdict is the default CohortVerdict. Higher fitness is better, so a positive
// d_z means the experimental arm beat control.
type Verdict struct {
	// mu guards the live thresholds: SetThresholds runs on the loop's tick while
	// Evaluate runs under the registry lock from a ConcludeGame goroutine.
	mu              sync.RWMutex
	minPairs        int
	effectThreshold float64
	minRawEffect    float64
	sdFloor         float64
}

// SetThresholds live-updates the min-pairs gate. A non-positive value is ignored,
// so a transient flag read of 0 never makes a thin sample conclusive.
func (v *Verdict) SetThresholds(minPairs int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if minPairs > 0 {
		v.minPairs = minPairs
	}
}

// SetFloatThresholds live-updates the float gates; a non-positive value is ignored
// per gate (it must never loosen one).
func (v *Verdict) SetFloatThresholds(effectThreshold, minRawEffect, sdFloor float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if effectThreshold > 0 {
		v.effectThreshold = effectThreshold
	}
	if minRawEffect > 0 {
		v.minRawEffect = minRawEffect
	}
	if sdFloor > 0 {
		v.sdFloor = sdFloor
	}
}

// NewVerdict builds a Verdict with the default floors. A non-positive argument
// falls back to its default.
func NewVerdict(minPairs int, effectThreshold float64) *Verdict {
	return NewVerdictWithFloors(minPairs, effectThreshold, DefaultMinRawEffect, DefaultSDFloor)
}

// NewVerdictWithFloors is the full constructor. A non-positive argument falls back
// to its default.
func NewVerdictWithFloors(minPairs int, effectThreshold, minRawEffect, sdFloor float64) *Verdict {
	if minPairs <= 0 {
		minPairs = DefaultMinPairs
	}
	if effectThreshold <= 0 {
		effectThreshold = DefaultEffectThreshold
	}
	if minRawEffect <= 0 {
		minRawEffect = DefaultMinRawEffect
	}
	if sdFloor <= 0 {
		sdFloor = DefaultSDFloor
	}
	return &Verdict{
		minPairs:        minPairs,
		effectThreshold: effectThreshold,
		minRawEffect:    minRawEffect,
		sdFloor:         sdFloor,
	}
}

// NewDefaultVerdict builds the loop's verdict from the compiled defaults.
func NewDefaultVerdict() *Verdict {
	return NewVerdictWithFloors(DefaultMinPairs, DefaultEffectThreshold, DefaultMinRawEffect, DefaultSDFloor)
}

// Evaluate computes the current verdict from an experiment's rolling Summary.
//
//   - no completed pair ⇒ INCONCLUSIVE;
//   - pairs < minPairs, or fewer than 2 pairs ⇒ INCONCLUSIVE (under-powered);
//   - |d_z| ≥ threshold but |mean(d)| < minRawEffect ⇒ INCONCLUSIVE;
//   - d_z ≥ threshold ⇒ PROMOTE; d_z ≤ −threshold ⇒ DISCARD; else INCONCLUSIVE.
//
// The bool is false only when the Summary has neither a pair nor both canonical
// arms, so the registry keeps any prior verdict rather than a vacuous one.
func (v *Verdict) Evaluate(s journal.Summary) (journal.Verdict, bool) {
	v.mu.RLock()
	minPairs, effectThreshold, minRawEffect, sdFloor := v.minPairs, v.effectThreshold, v.minRawEffect, v.sdFloor
	v.mu.RUnlock()

	if s.PairDiff == nil || s.PairDiff.Count == 0 {
		exp, expOK := s.Arms[ArmExperimental]
		ctl, ctlOK := s.Arms[ArmControl]
		if !expOK || !ctlOK || exp == nil || ctl == nil {
			return journal.Verdict{}, false
		}
		return journal.Verdict{
			Outcome: OutcomeInconclusive,
			Delta:   exp.Welford.Mean - ctl.Welford.Mean,
			Reason:  "no completed pair",
		}, true
	}
	pd := *s.PairDiff
	meanD := pd.Mean
	inconclusive := func(reason string) (journal.Verdict, bool) {
		return journal.Verdict{Outcome: OutcomeInconclusive, Delta: meanD, Reason: reason}, true
	}

	if pd.Count < minPairs {
		return inconclusive(fmt.Sprintf("paired under-powered: pairs=%d < min_pairs=%d", pd.Count, minPairs))
	}
	// A min_pairs=1 override must never divide by a single pair's undefined SD.
	if pd.Count < 2 {
		return inconclusive("paired effect undefined: need >= 2 pairs for a paired SD")
	}
	sdD := math.Max(math.Sqrt(pd.M2/float64(pd.Count-1)), sdFloor)
	dz := meanD / sdD

	if math.Abs(dz) >= effectThreshold && math.Abs(meanD) < minRawEffect {
		return inconclusive(fmt.Sprintf("paired not practically significant: |mean_d|=%.4f < min_raw_effect=%.4f (d_z=%.3f, pairs=%d)",
			math.Abs(meanD), minRawEffect, dz, pd.Count))
	}
	switch {
	case dz >= effectThreshold:
		return journal.Verdict{
			Outcome:     CohortOutcomePromote,
			Delta:       meanD,
			Significant: true,
			Reason: fmt.Sprintf("paired: experimental better: d_z=%.3f >= threshold=%.2f, |mean_d|=%.4f >= min_raw=%.4f (pairs=%d)",
				dz, effectThreshold, math.Abs(meanD), minRawEffect, pd.Count),
		}, true
	case dz <= -effectThreshold:
		return journal.Verdict{
			Outcome:     CohortOutcomeDiscard,
			Delta:       meanD,
			Significant: true,
			Reason: fmt.Sprintf("paired: experimental worse: d_z=%.3f <= -threshold=%.2f, |mean_d|=%.4f >= min_raw=%.4f (pairs=%d)",
				dz, effectThreshold, math.Abs(meanD), minRawEffect, pd.Count),
		}, true
	default:
		return inconclusive(fmt.Sprintf("paired within noise: |d_z|=%.3f < threshold=%.2f (pairs=%d)",
			math.Abs(dz), effectThreshold, pd.Count))
	}
}
