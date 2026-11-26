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
// Flagsmith wins a conflict for a kill switch like for any flag (#2621): no
// "more restrictive" merge, neither on a live conflict nor at a restart.
func TestDecideKillSwitch(t *testing.T) {
	base := loadBaseline(t, "ci")
	enabled := base.Flag("agent", "enabled")
	act, off := variant(enabled, "act"), variant(enabled, "off")
	cases := []struct {
		name string
		in   mergeIn
		want action
		res  string
	}{
		{"UC-054/BR-005 live conflict, L=off vs D=act: Flagsmith wins (loosens)",
			mergeIn{R: none, HasR: true, D: act, L: off}, actApply, ResolutionFlagsmith},
		{"UC-054/BR-005 live conflict, L=act vs D=off: Flagsmith wins",
			mergeIn{R: none, HasR: true, D: off, L: act}, actApply, ResolutionFlagsmith},
		{"UC-054/BR-005 R=L=off, D=act: a deliberate change loosens",
			mergeIn{R: off, HasR: true, D: act, L: off}, actApply, ""},
		{"UC-054/A3 no remote (restart), L tighter: Flagsmith wins",
			mergeIn{D: act, L: off}, actApply, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decide(c.in)
			if got.action != c.want || got.resolution != c.res {
				t.Fatalf("decide = %v/%q, want %v/%q", got.action, got.resolution, c.want, c.res)
			}
		})
	}
}

// UC: UC-054 A1 A2 BR-007
// A stack-scope local change made while the link was not connected, or a
// stack value Flagsmith refused, is discarded (#2621): Flagsmith's value is
// applied; with no Flagsmith value it goes back to the baseline, an admin
// setting too.
func TestDecideDiscard(t *testing.T) {
	a, b := val(`"a"`), val(`"b"`)
	cases := []struct {
		name string
		in   mergeIn
		want action
		res  string
	}{
		{"UC-054/A2 R=D, offline change: Flagsmith value applied",
			mergeIn{R: a, HasR: true, D: a, L: b, Discard: true}, actApply, ResolutionFlagsmith},
		{"UC-054/A2 R=D absent, offline overlay: back to the baseline",
			mergeIn{R: none, HasR: true, D: none, L: b, Discard: true}, actApply, ResolutionFlagsmith},
		{"UC-054/A2 no remote, D absent, offline overlay: back to the baseline",
			mergeIn{D: none, L: b, Discard: true}, actApply, ""},
		{"UC-054/A3 no remote, D absent, not discarded (persisted admin value at boot): sent",
			mergeIn{D: none, L: b}, actPush, ""},
		{"UC-055/main R=D, online local change (no Discard): pushed",
			mergeIn{R: a, HasR: true, D: a, L: b}, actPush, ""},
		{"UC-054/A2 R=D=L: nothing to discard",
			mergeIn{R: a, HasR: true, D: a, L: a, Discard: true}, actNone, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decide(c.in)
			if got.action != c.want || got.resolution != c.res {
				t.Fatalf("decide = %v/%q, want %v/%q", got.action, got.resolution, c.want, c.res)
			}
		})
	}
}
