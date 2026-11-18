package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// UC: UC-040 BR-044
// operator_brief_wiring_uc040_test.go: main.go wires decision.operator_brief into
// every per-game decision loop (#2687). Without the call the feature is silently
// off (every unit test of the loop injects its own source), so this pins the call
// site: loop.SetOperatorBriefSource(c.agentFlags.OperatorBrief).
func TestOperatorBriefSourceIsWired(t *testing.T) {
	t.Run("UC-040/BR-044 main.go passes Flags.OperatorBrief to SetOperatorBriefSource", func(t *testing.T) {
		f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fn.Sel.Name != "SetOperatorBriefSource" {
				return true
			}
			if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "OperatorBrief" {
				found = true
			}
			return true
		})
		if !found {
			t.Fatal("main.go must call SetOperatorBriefSource(<flags>.OperatorBrief) on the decision loop")
		}
	})
}
