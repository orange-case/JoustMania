package flagsmith

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/registry"
)

// UC: UC-054 BR-005
// The order of every kill switch on the real baselines (services/flagd and
// services/flagd/ci), values normalised as the agent reads them (#2590 P11).
func TestKillSwitchOrder(t *testing.T) {
	for _, dir := range []string{".", "ci"} {
		base := loadBaseline(t, dir)
		flag := func(set, key string) (killRule, func(string) Value) {
			t.Helper()
			r := killRuleOf(set, key)
			b := base.Flag(set, key)
			if r == nil || b == nil {
				t.Fatalf("%s: %s/%s has no rule or no baseline flag", dir, set, key)
			}
			return *r, func(name string) Value {
				if name == "" {
					return Value{}
				}
				if _, ok := b.Variants[name]; ok {
					return variant(b, name)
				}
				return Value{Present: true, Value: json.RawMessage(name)}
			}
		}
		// tighter(a, b) says a is the more restrictive (a local, b remote).
		type row struct {
			set, key, a, b string
			want           string // "a", "b", "tie", "incomparable"
		}
		rows := []row{
			{"agent", "enabled", "off", "on", "a"},
			{"agent", "enabled", "on", "off", "b"},
			{"agent", "enabled", "", "on", "a"}, // absent = baseline default off
			{"agent", "interventions_allowed", "none", "probe", "a"},
			{"agent", "interventions_allowed", "probe", "ambient", "a"},
			{"agent", "interventions_allowed", "ambient", "standard", "a"},
			{"agent", "interventions_allowed", "standard", "full", "a"},
			{"agent", "interventions_allowed", "full", "shadow_experimental", "a"},
			{"agent", "interventions_allowed", "standard", "standard_group", "a"},
			{"agent", "interventions_allowed", "shadow_experimental", "shadow_group", "a"},
			{"agent", "interventions_allowed", "standard_group", "full", "incomparable"},
			{"agent", "interventions_allowed", "full", "standard_group", "incomparable"},
			{"agent", "interventions_allowed", "", "standard", "a"}, // absent = ambient
			{"agent", "interventions_allowed", `"bogus_action"`, "none", "b"},
			{"agent", "code_improvement.mode", "issue", "pr", "a"},
			{"agent", "code_improvement.mode", "autonomous", "pr", "b"},
			{"agent", "code_improvement.mode", `"weird"`, "pr", "a"}, // unknown = issue
			{"agent", "code_improvement.target", "github", "local", "b"},
			{"agent", "code_improvement.target", `"weird"`, "github", "a"},
			{"agent", "code_improvement.min_interval_seconds", "slow", "fast", "a"},
			{"agent", "code_improvement.min_interval_seconds", `0`, "fast", "a"},  // 0 = 300 > 60
			{"agent", "code_improvement.min_interval_seconds", `-5`, "slow", "b"}, // -5 = 300 < 900
			{"agent", "code_improvement.min_interval_seconds", `0`, "default", "tie"},
			{"agent", "llm.max_requests_per_minute", "conservative", "frenzy", "a"},
			{"agent", "llm.max_requests_per_minute", `0`, "conservative", "a"}, // closed
			{"agent", "llm.max_requests_per_minute", `-1`, "conservative", "a"},
			{"agent", "decision.claude.max_requests_per_minute", "stage", "inherit", "a"}, // inherit highest
			{"agent", "decision.openai.max_requests_per_minute", "inherit", "conservative", "b"},
			{"agent", "decision.claude.max_requests_per_minute", `-7`, "conservative", "a"}, // -7 = 0
			{"agent", "experiment_max_games", "default", "unlimited", "a"},
			{"agent", "experiment_max_games", "unlimited", "long", "b"},
			{"agent", "experiment_dynamic_max_concurrent", "one", "wide", "a"},
			{"agent", "experiment_dynamic_max_concurrent", `0`, "default", "tie"}, // ≤ 0 = 3
			{"agent", "experiment_dynamic_max_concurrent", `-1`, "one", "b"},
			{"agent", "experiment_proposer_enabled", "off", "on", "a"},
			{"agent", "interventions_enabled", "on", "off", "b"},
			{"agent", "experiments_enabled", "off", "on", "a"},
			{"agent", "rollout_enabled", "on", "off", "b"},
			{"agent", "interventions_allowed", `"x"`, `{`, "incomparable"},
		}
		for _, r := range rows {
			rule, v := flag(r.set, r.key)
			b := base.Flag(r.set, r.key)
			la, lb := v(r.a), v(r.b)
			ra, _ := resolveValue(b, la)
			rb, _ := resolveValue(b, lb)
			c, ok := rule.compare(ra, rb)
			got := "incomparable"
			switch {
			case !ok:
			case c < 0:
				got = "a"
			case c > 0:
				got = "b"
			default:
				got = "tie"
			}
			if got != r.want {
				t.Errorf("UC-054/BR-005 %s %s/%s %s vs %s: %s, want %s", dir, r.set, r.key, r.a, r.b, got, r.want)
			}
		}
	}
	base := loadBaseline(t, ".")
	rr := killRuleOf("rollout", "remediation_allowed")
	b := base.Flag("rollout", "remediation_allowed")
	if side, ok := rr.tighter(b, variant(b, "off"), variant(b, "on")); !ok || side != SideLocal {
		t.Errorf("UC-054/BR-005 rollout/remediation_allowed off vs on: %v %v, want local", side, ok)
	}
}

// backticked names in s.
var backticked = regexp.MustCompile("`([^`]+)`")

// UC: UC-054 BR-005
// FR-152 sync (#2590 P5, R4): killSwitchFlags equals the flags.md §13.6 table;
// the table holds agent/enabled, the registry kill switches and every
// agent-limiting flag of FR-152 (C-034, code_improvement.* expanded against
// both agent baselines); every rule orders every declared variant.
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
	start := strings.Index(flags, "**Kill switches and their order**")
	if start < 0 {
		t.Fatal("flags.md: the anchor **Kill switches and their order** is missing")
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
		if _, ok := killSwitchFlags[name]; !ok {
			t.Errorf("UC-054/BR-005 flags.md §13.6 lists %s, killSwitchFlags has no rule", name)
		}
	}
	for name := range killSwitchFlags {
		if !table[name] {
			t.Errorf("UC-054/BR-005 killSwitchFlags has %s, flags.md §13.6 does not list it", name)
		}
	}

	// Every rule orders every declared variant (against the default).
	for _, dir := range []string{".", "ci"} {
		base := loadBaseline(t, dir)
		for name, rule := range killSwitchFlags {
			set, key, _ := strings.Cut(name, "/")
			b := base.Flag(set, key)
			if b == nil {
				t.Errorf("%s: %s has no baseline flag", dir, name)
				continue
			}
			for v := range b.Variants {
				if _, ok := rule.tighter(b, variant(b, v), Value{}); !ok && rule.kind != killActions {
					t.Errorf("%s: %s cannot order variant %s", dir, name, v)
				}
				if raw, ok := resolveValue(b, variant(b, v)); !ok || len(raw) == 0 {
					t.Errorf("%s: %s variant %s does not resolve", dir, name, v)
				}
			}
		}
	}
}
