package flagsmith

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/joustmania/flag-service/baseline"
)

// repoRoot is the repository root (tests read the baselines and the specs).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func loadBaseline(t *testing.T, dir string) *baseline.Baseline {
	t.Helper()
	b, err := baseline.Load(filepath.Join(repoRoot(t), "services", "flagd", dir), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func variant(b *baseline.Flag, name string) Value {
	return Value{Present: true, Variant: name, Value: b.Variants[name]}
}

var none = Value{}

// UC: UC-054 main step 3 A2 A3 A4
// The merge table of architecture note §6 for a stack key and a game key
// (neither a kill switch): every row, and a refused document value is never
// pushed over.
func TestDecideTable(t *testing.T) {
	a, b, c := val(`"a"`), val(`"b"`), val(`"c"`)
	rows := []struct {
		name    string
		r, d, l Value
		want    action
		res     string
	}{
		{"UC-054/main step 3 R=D=L none", a, a, a, actNone, ""},
		{"UC-054/A2 step 3 R=D, L moved: push", a, a, b, actPush, ""},
		{"UC-054/main step 3 R=L, D moved: apply", a, b, a, actApply, ""},
		{"UC-054/A2 both moved to the same value: adopt", a, b, b, actAdopt, ""},
		{"UC-054/A2 step 2 conflict: Flagsmith wins", a, b, c, actApply, ResolutionFlagsmith},
		{"UC-054/A2 step 2 conflict, document cleared: Flagsmith wins", a, none, c, actApply, ResolutionFlagsmith},
	}
	for _, scope := range []string{ScopeStack, "game:g1"} {
		for _, row := range rows {
			t.Run(scope+" "+row.name, func(t *testing.T) {
				got := decide(mergeIn{R: row.r, HasR: true, D: row.d, L: row.l})
				if got.action != row.want || got.resolution != row.res {
					t.Fatalf("decide = %v/%q, want %v/%q", got.action, got.resolution, row.want, row.res)
				}
			})
		}
	}
	t.Run("UC-054/A4 step 3 the refused document value is skipped, not pushed over", func(t *testing.T) {
		got := decide(mergeIn{R: b, HasR: true, D: b, L: a, Refused: &refusal{d: b, l: a}})
		if got.action != actSkip {
			t.Fatalf("decide = %v, want skip", got.action)
		}
	})
	t.Run("UC-054/A4 step 3 a new document value after a refusal is a deliberate change", func(t *testing.T) {
		got := decide(mergeIn{R: b, HasR: true, D: c, L: a, Refused: &refusal{d: b, l: a}})
		if got.action != actApply || got.conflict() {
			t.Fatalf("decide = %v/%q, want apply without conflict", got.action, got.resolution)
		}
	})
	t.Run("UC-054/A3 no remote: document wins, nothing to apply pushes", func(t *testing.T) {
		if got := decide(mergeIn{D: b, L: a}); got.action != actApply {
			t.Fatalf("no remote, D set: %v, want apply", got.action)
		}
		if got := decide(mergeIn{D: none, L: a}); got.action != actPush {
			t.Fatalf("no remote, D absent: %v, want push", got.action)
		}
		if got := decide(mergeIn{D: a, L: a}); got.action != actAdopt {
			t.Fatalf("no remote, D = L: %v, want adopt", got.action)
		}
	})
}

// UC: UC-054 BR-005
// Kill switches: a conflict takes the more restrictive value; a deliberate
// change applies even when it loosens; with no remote (restart, first
// sighting) any difference is a conflict (#2590 R1).
func TestDecideKillSwitch(t *testing.T) {
	base := loadBaseline(t, "ci")
	enabled := base.Flag("agent", "enabled")
	allowed := base.Flag("agent", "interventions_allowed")
	on, off := variant(enabled, "on"), variant(enabled, "off")
	kill := killRuleOf("agent", "enabled")
	ia := killRuleOf("agent", "interventions_allowed")
	cases := []struct {
		name string
		in   mergeIn
		want action
		res  string
	}{
		{"UC-054/BR-005 L=off vs D=on: local stays, pushed",
			mergeIn{R: none, HasR: true, D: on, L: off, Kill: kill, Base: enabled}, actKeepLocal, ResolutionTightened},
		{"UC-054/BR-005 L=on vs D=off: off applied",
			mergeIn{R: none, HasR: true, D: off, L: on, Kill: kill, Base: enabled}, actApply, ResolutionTightened},
		{"UC-054/BR-005 R=L=off, D=on: a deliberate change loosens",
			mergeIn{R: off, HasR: true, D: on, L: off, Kill: kill, Base: enabled}, actApply, ""},
		{"UC-054/BR-005 no remote, D tighter: applied",
			mergeIn{D: off, L: on, Kill: kill, Base: enabled}, actApply, ResolutionTightened},
		{"UC-054/BR-005 no remote, L tighter: local stays (R1)",
			mergeIn{D: on, L: off, Kill: kill, Base: enabled}, actKeepLocal, ResolutionTightened},
		{"UC-054/BR-005 no remote, D absent (baseline off) vs L=on: baseline applied",
			mergeIn{D: none, L: on, Kill: kill, Base: enabled}, actApply, ResolutionTightened},
		{"UC-054/BR-005 no remote, incomparable: local stays (R1)",
			mergeIn{D: variant(allowed, "full"), L: variant(allowed, "standard_group"), Kill: ia, Base: allowed}, actKeepLocal, ResolutionLocal},
		{"UC-054/BR-005 conflict, incomparable: local stays",
			mergeIn{R: variant(allowed, "none"), HasR: true, D: variant(allowed, "full"), L: variant(allowed, "standard_group"), Kill: ia, Base: allowed}, actKeepLocal, ResolutionLocal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decide(c.in)
			if got.action != c.want || got.resolution != c.res {
				t.Fatalf("decide = %v/%q, want %v/%q", got.action, got.resolution, c.want, c.res)
			}
		})
	}
	t.Run("UC-054/A2 step 2 a non-kill conflict takes the document", func(t *testing.T) {
		got := decide(mergeIn{R: none, HasR: true, D: on, L: off, Base: enabled})
		if got.action != actApply || got.resolution != ResolutionFlagsmith {
			t.Fatalf("decide = %v/%q, want apply/flagsmith", got.action, got.resolution)
		}
	})
}
