package experiment

import (
	"testing"

	"github.com/joustmania/agent/experiment/journal"
)

// armFrom builds an ArmStat by folding the given samples through the real Welford
// recurrence, so Count/Mean/M2 are exactly what the journal would persist.
func armFrom(samples ...float64) *journal.ArmStat {
	var st journal.ArmStat
	for _, s := range samples {
		st.Add(s)
	}
	return &st
}

// summaryWith assembles a journal.Summary with the canonical arms and no pair.
func summaryWith(exp, ctl *journal.ArmStat) journal.Summary {
	return journal.Summary{
		ExperimentID: "exp_test",
		Status:       "running",
		Arms: map[string]*journal.ArmStat{
			ArmExperimental: exp,
			ArmControl:      ctl,
		},
	}
}

// UC: UC-043 main
func TestVerdict_NoCompletedPair_Inconclusive(t *testing.T) {
	v := NewVerdict(1, 0.5)
	for name, s := range map[string]journal.Summary{
		"no PairDiff": summaryWith(armFrom(0.9, 0.9, 0.9), armFrom(0.1, 0.1, 0.1)),
		"empty PairDiff": func() journal.Summary {
			s := summaryWith(armFrom(0.9), armFrom(0.1))
			s.PairDiff = &journal.Welford{}
			return s
		}(),
	} {
		t.Run("UC-043/main "+name, func(t *testing.T) {
			got, ok := v.Evaluate(s)
			if !ok || got.Outcome != OutcomeInconclusive || got.Significant {
				t.Fatalf("verdict = %+v ok=%v, want INCONCLUSIVE: arms alone never decide", got, ok)
			}
		})
	}
}

func TestVerdict_MissingArm_NoVerdict(t *testing.T) {
	v := NewVerdict(5, 0.5)
	for name, arms := range map[string]map[string]*journal.ArmStat{
		"missing control": {ArmExperimental: armFrom(0.8)},
		"nil control":     {ArmExperimental: armFrom(0.8), ArmControl: nil},
	} {
		if _, ok := v.Evaluate(journal.Summary{Arms: arms}); ok {
			t.Fatalf("%s: want ok=false so a prior verdict is kept", name)
		}
	}
}

func TestVerdict_DefaultsOnNonPositiveConfig(t *testing.T) {
	v := NewVerdictWithFloors(0, -1, 0, -1)
	if v.minPairs != DefaultMinPairs || v.effectThreshold != DefaultEffectThreshold ||
		v.minRawEffect != DefaultMinRawEffect || v.sdFloor != DefaultSDFloor {
		t.Fatalf("gates = %d/%v/%v/%v, want the Default* constants", v.minPairs, v.effectThreshold, v.minRawEffect, v.sdFloor)
	}
	d := NewDefaultVerdict()
	if d.minPairs != DefaultMinPairs || d.effectThreshold != DefaultEffectThreshold {
		t.Fatalf("NewDefaultVerdict gates = %d/%v, want the defaults", d.minPairs, d.effectThreshold)
	}
}

// A non-positive live value never loosens a gate.
func TestVerdict_SetThresholdsIgnoreNonPositive(t *testing.T) {
	v := NewDefaultVerdict()
	v.SetThresholds(3)
	v.SetFloatThresholds(0.8, 0.05, 0.001)
	v.SetThresholds(0)
	v.SetFloatThresholds(0, -1, 0)
	if v.minPairs != 3 || v.effectThreshold != 0.8 || v.minRawEffect != 0.05 || v.sdFloor != 0.001 {
		t.Fatalf("gates = %d/%v/%v/%v, want 3/0.8/0.05/0.001", v.minPairs, v.effectThreshold, v.minRawEffect, v.sdFloor)
	}
}

// UC: UC-043 main
func TestVerdict_LiveThresholdsDriveTheVerdict(t *testing.T) {
	v := NewVerdict(3, DefaultEffectThreshold)
	s := pairedSummary(pairDiffFrom(0.10, 0.30, 0.20, 0.25))
	if got, _ := v.Evaluate(s); got.Outcome != CohortOutcomePromote {
		t.Fatalf("default gates: verdict = %+v, want PROMOTE", got)
	}
	v.SetThresholds(5)
	if got, _ := v.Evaluate(s); got.Outcome != OutcomeInconclusive {
		t.Fatalf("min_pairs 5 with 4 pairs: verdict = %+v, want INCONCLUSIVE", got)
	}
	v.SetThresholds(3)
	v.SetFloatThresholds(10, 0, 0)
	if got, _ := v.Evaluate(s); got.Outcome != OutcomeInconclusive {
		t.Fatalf("effect threshold 10: verdict = %+v, want INCONCLUSIVE", got)
	}
}
