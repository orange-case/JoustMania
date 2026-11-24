package registry

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
)

// The writer-list table (flags.md §13 "Writer list") is Go, not a registry.yaml
// attribute: released images parse the checkout's registry.yaml strictly
// (#2403). flags.md §13 names the same rows and labels; each row is a stack
// overlay without persist, outside interventions.
//
// UC: UC-053 A14
func TestWriterListsMatchFlagsMD(t *testing.T) {
	t.Run("UC-053/A14 flags.md §13 lists exactly the Go writer-list table", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(root(t), "..", "..", "docs", "specs", "flags.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		const marker = "**Writer list (`writers`"
		start := strings.Index(text, marker)
		if start < 0 {
			t.Fatalf("flags.md has no %q paragraph", marker)
		}
		para := text[start:]
		if end := strings.Index(para, "\n\n"); end > 0 {
			para = para[:end]
		}
		const lead = "single source of truth, kept in sync by a test:"
		i := strings.Index(para, lead)
		if i < 0 {
			t.Fatalf("the writer-list paragraph has no %q sentence", lead)
		}
		tail := para[i+len(lead):]
		if end := strings.Index(tail, " ("); end > 0 {
			tail = tail[:end]
		}
		// "`set/key` → `a`, `b`; `set/key2` → `c`"
		got := map[string][]string{}
		row := ""
		for _, m := range backticked.FindAllStringSubmatch(tail, -1) {
			if strings.Contains(m[1], "/") {
				row = m[1]
				got[row] = nil
				continue
			}
			if row == "" {
				t.Fatalf("label %q before any set/key in %q", m[1], tail)
			}
			got[row] = append(got[row], m[1])
		}
		if want := WriterLists(); len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("flags.md writer-list paragraph names %v, the Go table is %v", got, want)
		}
	})
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-053/A14 every writer-list row is a stack overlay row without persist against flagd/"+dir, func(t *testing.T) {
			reg := loadReal(t, dir)
			lists := WriterLists()
			for name, want := range lists {
				set, key, _ := strings.Cut(name, "/")
				p := reg.Get(set, key)
				if p == nil {
					t.Errorf("%s: writer-list row with no registry row", name)
					continue
				}
				if !reflect.DeepEqual(p.Writers, want) {
					t.Errorf("%s: Writers %q, want %q", name, p.Writers, want)
				}
				if p.Set == "interventions" || p.Kind != KindOverlay || !reflect.DeepEqual(p.Scopes, []string{ScopeStack}) || p.Persist {
					t.Errorf("%s: %+v, want an overlay of scope [stack] without persist outside interventions", name, p)
				}
			}
			for _, p := range reg.Policies() {
				if _, ok := lists[id(p.Set, p.Key)]; p.HasWriterList() && !ok {
					t.Errorf("%s/%s: has a writer list but is not in the table", p.Set, p.Key)
				}
			}
		})
	}
	t.Run("UC-053/A14 goal.profile is written by flagsmith and menu", func(t *testing.T) {
		p := loadReal(t, "ci").Get("agent", "goal.profile")
		if !reflect.DeepEqual(p.Writers, []string{FlagsmithWriter, "menu"}) || p.FlagsmithOnly() {
			t.Fatalf("goal.profile Writers %q FlagsmithOnly %v, want [flagsmith menu], false", p.Writers, p.FlagsmithOnly())
		}
	})
	t.Run("UC-053/A14 a row without a list has none and takes any writer", func(t *testing.T) {
		reg := loadReal(t, "ci")
		for _, k := range []string{"objectives", "enabled", "decision.backend"} {
			p := reg.Get("agent", k)
			if p.HasWriterList() || p.FlagsmithOnly() || p.WriterAllowed("anyone") != nil || p.WriterAllowed("") != nil {
				t.Errorf("agent/%s: %+v restricts its writers", k, p)
			}
		}
		var nilPolicy *Policy
		if nilPolicy.HasWriterList() || nilPolicy.FlagsmithOnly() || nilPolicy.WriterAllowed("x") != nil {
			t.Error("a nil policy (read-only baseline) restricts its writers")
		}
	})
}

// WriterAllowed answers per list: flagsmith_only for exactly [flagsmith],
// writer_not_allowed for any other list, nil for a label in the list.
//
// UC: UC-053 A14
func TestWriterAllowedPerList(t *testing.T) {
	only := &Policy{Set: "agent", Key: "x", Writers: []string{FlagsmithWriter}}
	two := &Policy{Set: "agent", Key: "y", Writers: []string{FlagsmithWriter, "menu"}}
	other := &Policy{Set: "agent", Key: "z", Writers: []string{"menu"}}
	cases := []struct {
		name   string
		p      *Policy
		writer string
		reason string
	}{
		{"[flagsmith] takes flagsmith", only, FlagsmithWriter, ""},
		{"[flagsmith] refuses menu with flagsmith_only", only, "menu", fserr.FlagsmithOnly},
		{"[flagsmith] refuses an empty label with flagsmith_only", only, "", fserr.FlagsmithOnly},
		{"[flagsmith, menu] takes flagsmith", two, FlagsmithWriter, ""},
		{"[flagsmith, menu] takes menu", two, "menu", ""},
		{"[flagsmith, menu] refuses agent with writer_not_allowed", two, "agent", fserr.WriterNotAllowed},
		{"[flagsmith, menu] refuses an empty label with writer_not_allowed", two, "", fserr.WriterNotAllowed},
		{"[flagsmith, menu] refuses a label differing in case", two, "Menu", fserr.WriterNotAllowed},
		{"[menu] refuses flagsmith with writer_not_allowed", other, FlagsmithWriter, fserr.WriterNotAllowed},
	}
	for _, c := range cases {
		t.Run("UC-053/A14 "+c.name, func(t *testing.T) {
			err := c.p.WriterAllowed(c.writer)
			if c.reason == "" {
				if err != nil {
					t.Fatalf("%v, want allowed", err)
				}
				return
			}
			if err == nil || err.Status != http.StatusForbidden || err.Reason != c.reason {
				t.Fatalf("%v, want 403 %s", err, c.reason)
			}
			for _, w := range c.p.Writers {
				if !strings.Contains(err.Detail, w) {
					t.Errorf("detail %q does not name %q", err.Detail, w)
				}
			}
		})
	}
	t.Run("UC-053/A14 a writer reset clears a list row only for a label in it, never for flagsmith", func(t *testing.T) {
		for _, c := range []struct {
			p      *Policy
			writer string
			want   bool
		}{
			{two, "menu", true}, {two, FlagsmithWriter, false}, {two, "stage", false},
			{only, FlagsmithWriter, false}, {only, "menu", false},
			{&Policy{}, FlagsmithWriter, true}, {&Policy{}, "stage", true},
		} {
			if got := c.p.ClearedByWriterReset(c.writer); got != c.want {
				t.Errorf("%q ClearedByWriterReset(%q) = %v, want %v", c.p.Writers, c.writer, got, c.want)
			}
		}
	})
	t.Run("UC-053/A14 the GET policy serves writers only on a list row", func(t *testing.T) {
		b, _ := json.Marshal(two)
		if !strings.Contains(string(b), `"writers":["flagsmith","menu"]`) {
			t.Errorf("policy JSON %s has no writers", b)
		}
		b, _ = json.Marshal(&Policy{Set: "agent", Key: "objectives"})
		if strings.Contains(string(b), "writers") {
			t.Errorf("policy JSON %s of a row without a list names writers", b)
		}
	})
}

// A writer-list row that is not a stack overlay without persist, or whose list
// has an empty or repeated label, is refused at start, like any other bad row.
//
// UC: UC-053 A14
func TestParseRejectsBadWriterListRows(t *testing.T) {
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"interventions/volume_override", "agent/interventions_allowed", "game/sensitivity", "controller/bluetooth_backend"}
	for _, name := range names {
		writerLists[name] = []string{FlagsmithWriter}
	}
	saved := writerLists["agent/goal.profile"]
	t.Cleanup(func() {
		for _, name := range names {
			delete(writerLists, name)
		}
		writerLists["agent/goal.profile"] = saved
	})
	bad := map[string]string{
		"interventions row": `flags: [{set: interventions, key: volume_override, kind: state, scope: [game], bounds: {range: [0.3, 1.0]}}]`,
		"persist row":       `flags: [{set: agent, key: interventions_allowed, kind: overlay, scope: [stack], persist: true}]`,
		"wider scope":       `flags: [{set: game, key: sensitivity, kind: overlay, scope: [stack, experiment+arm], description: "x"}]`,
		"fraction scope":    `flags: [{set: controller, key: bluetooth_backend, kind: overlay, scope: [stack, fraction], bounds: {fraction: [1, 100]}}]`,
	}
	for name, doc := range bad {
		t.Run("UC-053/A14 "+name+" is refused", func(t *testing.T) {
			if _, err := Parse([]byte(doc), b); err == nil || !strings.Contains(err.Error(), "writer list") {
				t.Errorf("%s: %v, want a writer-list refusal", name, err)
			}
		})
	}
	goal := `flags: [{set: agent, key: goal.profile, kind: overlay, scope: [stack]}]`
	for name, list := range map[string][]string{"an empty label": {"menu", ""}, "a repeated label": {"menu", "menu"}} {
		t.Run("UC-053/A14 "+name+" is refused", func(t *testing.T) {
			writerLists["agent/goal.profile"] = list
			defer func() { writerLists["agent/goal.profile"] = saved }()
			if _, err := Parse([]byte(goal), b); err == nil || !strings.Contains(err.Error(), "empty or repeated") {
				t.Errorf("%q: %v, want a refusal", list, err)
			}
		})
	}
	t.Run("UC-053/A14 a stack overlay without persist is accepted", func(t *testing.T) {
		if _, err := Parse([]byte(goal), b); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("UC-053/A14 a policy's list is a copy of the table", func(t *testing.T) {
		reg, err := Parse([]byte(goal), b)
		if err != nil {
			t.Fatal(err)
		}
		reg.Get("agent", "goal.profile").Writers[0] = "x"
		if writerLists["agent/goal.profile"][0] != FlagsmithWriter {
			t.Fatal("changing a policy's Writers changed the table")
		}
	})
}
