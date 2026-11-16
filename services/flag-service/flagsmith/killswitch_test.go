package flagsmith

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/registry"
)

// backticked names in s.
var backticked = regexp.MustCompile("`([^`]+)`")

// UC: UC-054 BR-005
// FR-152 sync (#2590 P5, R4): killSwitches equals the flags.md §13.6 table
// the table holds agent/enabled, the registry kill switches and every
// agent-limiting flag of FR-152 (C-034, code_improvement.* expanded against
// both agent baselines). Since #2621 the set only orders a round (no merge rule).
func TestKillSwitchTableInSync(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(raw), "\r\n", "\n")
	}

	// The §13.6 table.
	flags := read("docs/specs/flags.md")
	start := strings.Index(flags, "**Kill switches**")
	if start < 0 {
		t.Fatal("flags.md: the anchor **Kill switches** is missing")
	}
	table := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(flags[start:], "\n") {
		if strings.HasPrefix(line, "|") {
			inTable = true
			cells := strings.Split(line, "|")
			if len(cells) < 3 || strings.HasPrefix(strings.TrimSpace(cells[1]), "---") {
				continue
			}
			for _, m := range backticked.FindAllStringSubmatch(cells[1], -1) {
				table[m[1]] = true
			}
			continue
		}
		if inTable {
			break
		}
	}
	if len(table) == 0 {
		t.Fatal("flags.md §13.6: no kill-switch table rows found")
	}

	// FR-152's agent-limiting flags (C-034).
	reqs := read("docs/requirements.md")
	const anchor = "Every agent-limiting setting of FR-152 is in `agent`:"
	i := strings.Index(reqs, anchor)
	if i < 0 {
		t.Fatalf("requirements.md: the anchor %q is missing", anchor)
	}
	sentence := reqs[i+len(anchor):]
	if j := strings.Index(sentence, ". "); j >= 0 {
		sentence = sentence[:j]
	}
	expand := func(dir string) []string {
		base := loadBaseline(t, dir)
		var out []string
		for _, m := range backticked.FindAllStringSubmatch(sentence, -1) {
			name := m[1]
			if prefix, ok := strings.CutSuffix(name, "*"); ok {
				for key := range base.Sets["agent"].Flags {
					if strings.HasPrefix(key, prefix) {
						out = append(out, "agent/"+key)
					}
				}
				continue
			}
			out = append(out, "agent/"+name)
		}
		sort.Strings(out)
		return out
	}
	fr152, ciFR152 := expand("."), expand("ci")
	if strings.Join(fr152, ",") != strings.Join(ciFR152, ",") {
		t.Fatalf("FR-152 expands differently on the two baselines:\n%v\n%v", fr152, ciFR152)
	}
	if len(fr152) < 5 {
		t.Fatalf("FR-152 list too short (anchor drift?): %v", fr152)
	}
	must := append(append([]string{"agent/enabled"}, registry.KillSwitches()...), fr152...)
	for _, name := range must {
		if !table[name] {
			t.Errorf("UC-054/BR-005 %s (agent/enabled, registry kill switch or FR-152) is not in the flags.md §13.6 table", name)
		}
	}
	for name := range table {
		if _, ok := killSwitches[name]; !ok {
			t.Errorf("UC-054/BR-005 flags.md §13.6 lists %s, killSwitches lacks it", name)
		}
	}
	for name := range killSwitches {
		if !table[name] {
			t.Errorf("UC-054/BR-005 killSwitches has %s, flags.md §13.6 does not list it", name)
		}
	}
}
