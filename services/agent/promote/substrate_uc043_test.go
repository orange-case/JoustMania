package promote

// substrate_uc043_test.go: the shadow substrate validity bar on autonomous
// promotion (UC-043 A23, FR-152 g, FR-203, #2541). An autonomous real-default
// write is made only for an objective the operator attested for the running
// release; every other objective is a recorded no-op. Issue and PR modes, the
// reverts and the kill switch are never gated.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func substrateEvidence(objective string) Evidence {
	ev := testEvidence()
	ev.Objective = objective
	return ev
}

// barPromoter builds a Promoter with a real-default writer and the given bar
// (nil: SetSubstrateBar is never called).
func barPromoter(t *testing.T, bar map[string]bool) (*Promoter, *recordingRealDefault, *recordingReverter) {
	t.Helper()
	realDef, rev := &recordingRealDefault{}, &recordingReverter{}
	p := NewPromoter(&recordingGitHub{}, realDef, rev, nil, discardLogger())
	if bar != nil {
		p.SetSubstrateBar(bar)
	}
	return p, realDef, rev
}

func wantSubstrateRefusal(t *testing.T, outcome Outcome, reason, objective string) {
	t.Helper()
	if outcome != OutcomeDiscarded {
		t.Fatalf("outcome = %q (%s), want discarded", outcome, reason)
	}
	if want := ReasonSubstrateNotValidated + " objective=" + objective; !strings.HasPrefix(reason, want) {
		t.Fatalf("reason = %q, want prefix %q", reason, want)
	}
}

// UC: UC-043 A23
func TestUC043_A23_SubstrateBar(t *testing.T) {
	t.Run("UC-043/A23 T1 no bar set refuses the autonomous write (fail closed)", func(t *testing.T) {
		p, realDef, _ := barPromoter(t, nil)
		res := p.Promote(context.Background(), promoteProposal, promoteVerdict, autoOn, substrateEvidence("balanced"))
		wantSubstrateRefusal(t, res.Outcome, res.Reason, "balanced")
		if len(realDef.calls) != 0 {
			t.Fatalf("SetRealDefault calls = %d, want 0", len(realDef.calls))
		}
	})
	t.Run("UC-043/A23 T2 only an attested objective is written", func(t *testing.T) {
		p, realDef, _ := barPromoter(t, map[string]bool{"balanced": true})
		res := p.Promote(context.Background(), promoteProposal, promoteVerdict, autoOn, substrateEvidence("endurance"))
		wantSubstrateRefusal(t, res.Outcome, res.Reason, "endurance")
		if len(realDef.calls) != 0 {
			t.Fatalf("SetRealDefault calls = %d for endurance, want 0", len(realDef.calls))
		}
		res = p.Promote(context.Background(), promoteProposal, promoteVerdict, autoOn, substrateEvidence("balanced"))
		if res.Outcome != OutcomeApplied || len(realDef.calls) != 1 {
			t.Fatalf("balanced: outcome = %q (%s) calls %d, want applied once", res.Outcome, res.Reason, len(realDef.calls))
		}
	})
	t.Run("UC-043/A23 an empty objective is never validated", func(t *testing.T) {
		p, realDef, _ := barPromoter(t, map[string]bool{"balanced": true, "": true})
		res := p.Promote(context.Background(), promoteProposal, promoteVerdict, autoOn, substrateEvidence(""))
		wantSubstrateRefusal(t, res.Outcome, res.Reason, "")
		if len(realDef.calls) != 0 {
			t.Fatal("an empty objective wrote the real default")
		}
	})
	t.Run("UC-043/A23 T3 a below-bar hold key is discarded, never written", func(t *testing.T) {
		p, realDef, _ := barPromoter(t, map[string]bool{"balanced": true})
		ev := holdEvidence()
		ev.Objective = "endurance"
		res := p.Promote(context.Background(), holdProposal("exp-t3"), promoteVerdict, autoOn, ev)
		wantSubstrateRefusal(t, res.Outcome, res.Reason, "endurance")
		if len(realDef.calls) != 0 {
			t.Fatal("a below-bar hold key wrote the real default")
		}
	})
	t.Run("UC-043/A23 the bar is set once", func(t *testing.T) {
		p, _, _ := barPromoter(t, map[string]bool{"balanced": true})
		p.SetSubstrateBar(map[string]bool{"endurance": true})
		if !p.SubstrateValidated("balanced") || p.SubstrateValidated("endurance") {
			t.Fatal("a second SetSubstrateBar changed the bar")
		}
	})
}

// UC: UC-043 A23
func TestUC043_A23_SeamForwardsTheObjective(t *testing.T) {
	for _, tc := range []struct {
		objective string
		applied   bool
	}{
		{"endurance", true},
		{"balanced", false},
		{"", false},
	} {
		t.Run("UC-043/A23 T5 intent objective "+tc.objective+" through ExperimentPromoter", func(t *testing.T) {
			p, realDef, _ := barPromoter(t, map[string]bool{"endurance": true})
			a := NewExperimentPromoter(p, fixedConfig(autoOn))
			view := promoteView()
			view.Intent.Objective = tc.objective
			res := a.Promote(context.Background(), view)
			if tc.applied {
				if res.Outcome != string(OutcomeApplied) || len(realDef.calls) != 1 {
					t.Fatalf("outcome = %q (%s) calls %d, want applied once", res.Outcome, res.Reason, len(realDef.calls))
				}
				return
			}
			wantSubstrateRefusal(t, Outcome(res.Outcome), res.Reason, tc.objective)
			if len(realDef.calls) != 0 {
				t.Fatalf("SetRealDefault calls = %d, want 0", len(realDef.calls))
			}
		})
	}
}

// UC: UC-043 A23
// UC: UC-043 BR-003
func TestUC043_A23_IssueModeUnaffected(t *testing.T) {
	t.Run("UC-043/A23 G1 issue mode opens the issue under a closed bar", func(t *testing.T) {
		gh := &recordingGitHub{issueURL: "https://example.invalid/issues/1"}
		p := NewPromoter(gh, &recordingRealDefault{}, nil, nil, discardLogger())
		res := p.Promote(context.Background(), promoteProposal, promoteVerdict, Config{Mode: ModeIssue, Enabled: true}, substrateEvidence("endurance"))
		if res.Outcome != OutcomeApplied || len(gh.issues) != 1 {
			t.Fatalf("outcome = %q (%s) issues %d, want applied once", res.Outcome, res.Reason, len(gh.issues))
		}
	})
}

// UC: UC-043 A23
// UC: UC-043 A1
func TestUC043_A23_RevertsNeverGated(t *testing.T) {
	t.Run("UC-043/A23 G2 the kill switch reverts under a closed bar", func(t *testing.T) {
		rev := &listingReverter{flags: []string{promoteProposal.FlagKey}}
		p := NewPromoter(nil, &recordingRealDefault{}, rev, nil, discardLogger())
		got := p.RevertOnKillSwitch(context.Background())
		if len(got) != 1 || got[0].Outcome != OutcomeReverted || rev.calls != 1 {
			t.Fatalf("kill switch = %+v reverts %d, want one reverted", got, rev.calls)
		}
	})
}

// UC: UC-043 A23
func TestUC043_A23_ParseValidatedObjectives(t *testing.T) {
	const release = "0.14.0"
	for _, tc := range []struct {
		name, env, version string
		want               []string
		warns              int
	}{
		{"unset", "", release, nil, 0},
		{"only separators", " , ,", release, nil, 0},
		{"real release tag", "balanced@0.14.0", release, []string{"balanced"}, 0},
		{"two entries", "balanced@0.14.0,endurance@0.14.0", release, []string{"balanced", "endurance"}, 0},
		{"trimmed and case-folded", " Balanced @ 0.14.0 ", release, []string{"balanced"}, 0},
		{"trailing comma adds nothing", "balanced@0.14.0,", release, []string{"balanced"}, 0},
		{"semicolon is not a separator", "balanced@0.14.0;endurance@0.14.0", release, nil, 1},
		{"v-prefixed entry", "balanced@v0.14.0", release, nil, 1},
		{"older release", "balanced@0.13.0", release, nil, 1},
		{"no release", "balanced", release, nil, 1},
		{"empty objective", "@0.14.0", release, nil, 1},
		{"chaos has no fitness", "chaos@0.14.0", release, nil, 1},
		{"typo", "endurence@0.14.0", release, nil, 1},
		{"latest even when it matches", "balanced@latest", "latest", nil, 1},
		{"main even when it matches", "balanced@main", "main", nil, 1},
		{"pr tag even when it matches", "balanced@pr-123", "pr-123", nil, 1},
		{"dev even when it matches", "balanced@0.0.0-dev", "0.0.0-dev", nil, 1},
		{"v-prefixed version", "balanced@v0.14.0", "v0.14.0", nil, 1},
		{"commit sha", "balanced@1c39ef1b6a1c39ef1b6a1c39ef1b6a1c39ef1b6a", "1c39ef1b6a1c39ef1b6a1c39ef1b6a1c39ef1b6a", nil, 1},
		{"empty version", "balanced@0.14.0", "", nil, 1},
	} {
		t.Run("UC-043/A23 T6 "+tc.name, func(t *testing.T) {
			got, warns := ParseValidatedObjectives(tc.env, tc.version)
			if list := ValidatedObjectiveList(got); !reflect.DeepEqual(list, append([]string{}, tc.want...)) {
				t.Errorf("validated = %v, want %v", list, tc.want)
			}
			if got[""] {
				t.Error("the empty objective was validated")
			}
			if len(warns) != tc.warns {
				t.Errorf("warnings = %q, want %d", warns, tc.warns)
			}
		})
	}
}

// TestUC043_A23_SingleRealDefaultWriteSite pins the claim the backstop rests on:
// every reference to SetRealDefault in the agent's production code (a call, or a
// method value such as `write := pr.realDef.SetRealDefault`) is inside
// writeTracked, and writeTracked is referenced only by promoteAutonomous and
// drainWrites. Function bodies and package-level var values are both walked.
//
// UC: UC-043 A23
func TestUC043_A23_SingleRealDefaultWriteSite(t *testing.T) {
	refs := map[string][]string{} // selector -> enclosing "file:decl"
	root := ".."                  // services/agent
	collect := func(path, owner string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && (sel.Sel.Name == "SetRealDefault" || sel.Sel.Name == "writeTracked") {
				refs[sel.Sel.Name] = append(refs[sel.Sel.Name], filepath.Base(path)+":"+owner)
			}
			return true
		})
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					collect(path, decl.Name.Name, decl.Body)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, v := range vs.Values {
							collect(path, "var", v)
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("UC-043/A23 SetRealDefault is called only in writeTracked", func(t *testing.T) {
		if want := []string{"killswitch.go:writeTracked"}; !reflect.DeepEqual(refs["SetRealDefault"], want) {
			t.Fatalf("SetRealDefault references = %v, want %v", refs["SetRealDefault"], want)
		}
	})
	t.Run("UC-043/A23 writeTracked is called only by promoteAutonomous", func(t *testing.T) {
		got := append([]string(nil), refs["writeTracked"]...)
		want := []string{"actions.go:promoteAutonomous"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("writeTracked references = %v, want %v", got, want)
		}
	})
}
