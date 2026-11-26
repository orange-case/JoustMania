package registry

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
)

func root(t testing.TB) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..")
}

func loadReal(t *testing.T, flagDir string) *Registry {
	t.Helper()
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", flagDir), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	r, err := LoadFile(filepath.Join(root(t), "registry.yaml"), b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// UC: UC-053 BR-001
func TestRegistryLoadsAgainstBothBaselines(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-053/BR-001 registry.yaml validates against services/flagd/"+dir, func(t *testing.T) {
			if n := len(loadReal(t, dir).Policies()); n < 50 {
				t.Fatalf("only %d policies", n)
			}
		})
	}
}

// flags.md §13 row, parsed.
type mdRow struct {
	set, key, kind, bounds string
	scopes                 []string
	persist                bool
	flagsmithOnly          bool // the row's writer column starts with **Flagsmith only** (#2705)
}

var backticked = regexp.MustCompile("`([^`]+)`")

// parseFlagsMD reads the §13 registry tables of docs/specs/flags.md.
func parseFlagsMD(t *testing.T) []mdRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root(t), "..", "..", "docs", "specs", "flags.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "## 13. Mutable-flag registry")
	if start < 0 {
		t.Fatal("flags.md has no §13")
	}
	text = text[start:]
	if end := strings.Index(text[3:], "\n## "); end > 0 {
		text = text[:end+3]
	}
	var rows []mdRow
	section := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "### 13.") {
			section = ""
			// A heading naming one set ("### 13.2 `agent`") sets it; a heading
			// naming several ("### 13.4 `user`, `controller`") means
			// the table carries a Set column.
			if m := backticked.FindAllStringSubmatch(line, -1); len(m) == 1 {
				section = m[0][1]
			}
			continue
		}
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") || strings.HasPrefix(line, "| Key") || strings.HasPrefix(line, "| Set") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		set := section
		if set == "" { // 13.4 / 13.5 tables carry a Set column
			set, cells = cells[0], cells[1:]
		}
		if len(cells) < 5 {
			continue
		}
		keys := backticked.FindAllStringSubmatch(cells[0], -1)
		if len(keys) == 0 {
			continue // "every flag the experiment catalog allows", "the flag of a promoted experiment"
		}
		scopes := strings.Split(cells[2], " or ")
		for _, k := range keys {
			rows = append(rows, mdRow{
				set: set, key: k[1], kind: cells[1], scopes: scopes, bounds: cells[3],
				persist:       strings.HasPrefix(cells[4], "**yes**"),
				flagsmithOnly: len(cells) > 5 && strings.HasPrefix(cells[5], "**Flagsmith only**"),
			})
		}
	}
	return rows
}

var rangeRe = regexp.MustCompile(`^(int )?(-?[0-9.]+) to (-?[0-9.]+)$`)

func checkBounds(t *testing.T, row mdRow, p *Policy) {
	t.Helper()
	b := p.Bounds
	text := row.bounds
	switch {
	case strings.HasPrefix(text, "declared variants"):
		if len(b.Variants) > 0 || b.Range != nil || b.Pattern != nil || b.FreeString || b.Payload != nil {
			t.Errorf("%s: flags.md says declared variants, registry %+v", row.key, b)
		}
	case strings.HasPrefix(text, "range table"):
		if p.MagnitudeLimits() == nil {
			t.Errorf("%s: flags.md says range table, registry.magnitudeLimits has no entry", row.key)
		}
	case strings.HasPrefix(text, "same shape"):
		if !b.Shape {
			t.Errorf("%s: flags.md says same shape", row.key)
		}
	case text == "free string":
		if !b.FreeString {
			t.Errorf("%s: flags.md says free string", row.key)
		}
	case strings.HasPrefix(text, "payload"):
		if b.Payload == nil {
			t.Errorf("%s: flags.md gives a payload schema", row.key)
		}
	case strings.Contains(text, "<"):
		if b.Pattern == nil {
			t.Errorf("%s: flags.md gives a format (%s), registry has no pattern", row.key, text)
		}
	case rangeRe.MatchString(text):
		m := rangeRe.FindStringSubmatch(text)
		lo, _ := strconv.ParseFloat(m[2], 64)
		hi, _ := strconv.ParseFloat(m[3], 64)
		if !reflect.DeepEqual(b.Range, []float64{lo, hi}) || b.Integer != (m[1] != "") {
			t.Errorf("%s: flags.md %q, registry range %v integer %v", row.key, text, b.Range, b.Integer)
		}
	default:
		var want []string
		for _, m := range backticked.FindAllStringSubmatch(text, -1) {
			want = append(want, m[1])
		}
		got := append([]string(nil), b.Variants...)
		sort.Strings(want)
		sort.Strings(got)
		if len(want) == 0 || !reflect.DeepEqual(want, got) {
			t.Errorf("%s: flags.md bounds %q, registry variants %v", row.key, text, b.Variants)
		}
	}
}

// UC: UC-053 BR-001
func TestRegistryMatchesFlagsMD(t *testing.T) {
	t.Run("UC-053/BR-001 registry.yaml and flags.md §13 declare the same flags, kinds, scopes, bounds and persist", func(t *testing.T) {
		reg := loadReal(t, "ci")
		rows := parseFlagsMD(t)
		if len(rows) < 40 {
			t.Fatalf("parsed only %d rows from flags.md §13", len(rows))
		}
		seen := map[string]bool{}
		for _, row := range rows {
			p := reg.Get(row.set, row.key)
			if p == nil {
				t.Errorf("flags.md §13 lists %s/%s; registry.yaml has no row", row.set, row.key)
				continue
			}
			seen[row.set+"/"+row.key] = true
			if p.Kind != row.kind {
				t.Errorf("%s: kind %s in registry, %s in flags.md", row.key, p.Kind, row.kind)
			}
			if !reflect.DeepEqual(p.Scopes, row.scopes) {
				t.Errorf("%s: scopes %v in registry, %v in flags.md", row.key, p.Scopes, row.scopes)
			}
			if p.Persist != row.persist {
				t.Errorf("%s: persist %v in registry, %v in flags.md", row.key, p.Persist, row.persist)
			}
			if p.FlagsmithOnly() != row.flagsmithOnly {
				t.Errorf("%s: Flagsmith only %v in registry.flagsmithOnly, %v in flags.md (writer column **Flagsmith only**)", row.key, p.FlagsmithOnly(), row.flagsmithOnly)
			}
			checkBounds(t, row, p)
		}
		for _, p := range reg.Policies() {
			if !seen[p.Set+"/"+p.Key] {
				t.Errorf("registry.yaml has %s/%s; flags.md §13 does not list it", p.Set, p.Key)
			}
		}
	})
}

func testPolicy(bounds Bounds) (*Policy, *baseline.Flag) {
	return &Policy{Set: "s", Key: "k", Kind: KindState, Scopes: []string{ScopeGame}, Bounds: bounds},
		&baseline.Flag{Variants: map[string]json.RawMessage{"none": json.RawMessage("0"), "short": json.RawMessage("3"), "label": json.RawMessage(`"calm"`)}, DefaultVariant: "none"}
}

// UC: UC-053 A5
func TestCheckValue(t *testing.T) {
	cases := []struct {
		name    string
		bounds  Bounds
		variant string
		value   string
		want    string // variant chosen, "" for a free value, "!reason" for a rejection
	}{
		{"UC-053/A5 declared variant", Bounds{}, "short", "", "short"},
		{"UC-053/A5 undeclared variant", Bounds{}, "long", "", "!" + fserr.OutOfBounds},
		{"UC-053/A5 variant outside the subset", Bounds{Variants: []string{"short"}}, "none", "", "!" + fserr.OutOfBounds},
		{"UC-053/A5 value equal to a declared variant", Bounds{}, "", "3", "short"},
		{"UC-053/A5 free value without a bound", Bounds{}, "", "4", "!" + fserr.OutOfBounds},
		{"UC-053/A5 value in range", Bounds{Range: []float64{1, 10}}, "", "5.5", ""},
		{"UC-053/A5 value below range", Bounds{Range: []float64{1, 10}}, "", "0.5", "!" + fserr.OutOfBounds},
		{"UC-053/A5 integer range rejects a fraction", Bounds{Range: []float64{1, 10}, Integer: true}, "", "5.5", "!" + fserr.OutOfBounds},
		{"UC-053/A5 integer range accepts an int", Bounds{Range: []float64{1, 10}, Integer: true}, "", "7", ""},
		{"UC-053/A5 range rejects a string", Bounds{Range: []float64{1, 10}}, "", `"5"`, "!" + fserr.OutOfBounds},
		{"UC-053/A5 pattern match", Bounds{Pattern: [][]string{{"float 1.0 1.3", "float 1 30", "enum linear|ease"}}}, "", `"1.2:8:ease"`, ""},
		{"UC-053/A5 pattern out of range segment", Bounds{Pattern: [][]string{{"float 1.0 1.3", "float 1 30", "enum linear|ease"}}}, "", `"1.5:8:ease"`, "!" + fserr.OutOfBounds},
		{"UC-053/A5 pattern alternative", Bounds{Pattern: [][]string{{"enum warn|tighten"}, {"enum tighten", "float 1 10", "float 0.1 1.0"}}}, "", `"tighten:5:0.5"`, ""},
		{"UC-053/A5 pattern wrong arity", Bounds{Pattern: [][]string{{"enum warn|tighten"}}}, "", `"warn:1"`, "!" + fserr.OutOfBounds},
		{"UC-053/A5 free string", Bounds{FreeString: true}, "", `"http://fake:8080/v1"`, ""},
		{"UC-053/A5 free string too long", Bounds{FreeString: true}, "", `"` + strings.Repeat("x", MaxFreeString+1) + `"`, "!" + fserr.OutOfBounds},
		{"UC-053/A5 string value equal to a declared variant", Bounds{FreeString: true}, "", `"calm"`, "label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, f := testPolicy(c.bounds)
			var value json.RawMessage
			if c.value != "" {
				value = json.RawMessage(c.value)
			}
			got, err := CheckValue(p, f, c.variant, value)
			if strings.HasPrefix(c.want, "!") {
				if err == nil || err.Reason != c.want[1:] || err.Status != http.StatusUnprocessableEntity {
					t.Fatalf("want rejection %s, got %+v %v", c.want, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection %v", err)
			}
			if got.Variant != c.want {
				t.Fatalf("variant %q, want %q", got.Variant, c.want)
			}
		})
	}
}

// UC: UC-053 A5
func TestCheckValueShape(t *testing.T) {
	p := &Policy{Set: "game", Key: "windows", Kind: KindOverlay, Scopes: []string{ScopeExperiment}, Bounds: Bounds{Shape: true}}
	f := &baseline.Flag{Variants: map[string]json.RawMessage{"default": json.RawMessage(`{"a":4,"b":8}`)}, DefaultVariant: "default"}
	for value, ok := range map[string]bool{
		`{"a":6,"b":11}`:       true,
		`{"b":1,"a":2}`:        true,
		`{"a":6}`:              false,
		`{"a":6,"b":11,"c":1}`: false,
		`{"a":"6","b":11}`:     false,
		`[4,8]`:                false,
		`"x"`:                  false,
	} {
		t.Run("UC-053/A5 shape "+value, func(t *testing.T) {
			_, err := CheckValue(p, f, "", json.RawMessage(value))
			if ok != (err == nil) {
				t.Fatalf("value %s: accepted=%v, want %v (%v)", value, err == nil, ok, err)
			}
		})
	}
}

// UC: UC-053 A4
func TestPayloadCheck(t *testing.T) {
	inGame := func(s string) bool { return s == "MOCK0001" }
	p := &Policy{Key: "controller_effect", Kind: KindEvent, Scopes: []string{ScopeGame},
		Bounds: Bounds{Payload: map[string]string{"serial": "serial_or_empty", "effect": "enum rumble|pulse"}}}
	cases := []struct {
		name    string
		payload map[string]any
		reason  string
	}{
		{"UC-053/A4 valid targeted effect", map[string]any{"serial": "MOCK0001", "effect": "rumble"}, ""},
		{"UC-053/A4 valid broadcast effect", map[string]any{"effect": "pulse"}, ""},
		{"UC-053/A4 unknown field", map[string]any{"effect": "pulse", "extra": "x"}, fserr.OutOfBounds},
		{"UC-053/A4 serial not in game", map[string]any{"serial": "MOCK0002", "effect": "pulse"}, fserr.SerialNotInGame},
		{"UC-053/A4 effect not allowed", map[string]any{"effect": "explode"}, fserr.OutOfBounds},
		{"UC-053/A4 non-string field", map[string]any{"effect": 3}, fserr.OutOfBounds},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := PayloadCheck(p, c.payload, inGame)
			if (err == nil) != (c.reason == "") || (err != nil && err.Reason != c.reason) {
				t.Fatalf("got %v, want %q", err, c.reason)
			}
		})
	}
}

// UC: UC-048 BR-014
func TestPayloadCheckGroupShield(t *testing.T) {
	game := map[string]bool{"A": true, "B": true, "C": true, "D": true, "E": true, "F": true, "G": true}
	inGame := func(s string) bool { return game[s] }
	p := &Policy{Key: "group_shield", Kind: KindEvent, Scopes: []string{ScopeGame},
		Bounds: Bounds{Payload: map[string]string{"serials": "serial_set", "seconds": "float 1 10"}}}
	cases := []struct {
		name    string
		payload map[string]any
		reason  string
	}{
		{"UC-048/BR-014 two serials", map[string]any{"serials": "A,B", "seconds": "5"}, ""},
		{"UC-048/BR-014 six serials", map[string]any{"serials": "A,B,C,D,E,F", "seconds": "2.5"}, ""},
		{"UC-048/BR-014 duplicate serial", map[string]any{"serials": "A,A,B", "seconds": "5"}, fserr.OutOfBounds},
		{"UC-048/BR-014 single serial", map[string]any{"serials": "A", "seconds": "5"}, fserr.OutOfBounds},
		{"UC-048/BR-014 seven serials", map[string]any{"serials": "A,B,C,D,E,F,G", "seconds": "5"}, fserr.OutOfBounds},
		{"UC-048/BR-014 foreign serial", map[string]any{"serials": "A,Z", "seconds": "5"}, fserr.SerialNotInGame},
		{"UC-048/BR-014 whitespace entry", map[string]any{"serials": "A, B", "seconds": "5"}, fserr.SerialNotInGame},
		{"UC-048/BR-014 lower-case entry", map[string]any{"serials": "A,b", "seconds": "5"}, fserr.SerialNotInGame},
		{"UC-048/BR-014 empty set", map[string]any{"serials": "", "seconds": "5"}, fserr.Malformed},
		{"UC-048/BR-014 missing set", map[string]any{"seconds": "5"}, fserr.Malformed},
		{"UC-048/BR-014 empty entry", map[string]any{"serials": "A,,B", "seconds": "5"}, fserr.OutOfBounds},
		{"UC-048/BR-014 seconds below range", map[string]any{"serials": "A,B", "seconds": "0"}, fserr.OutOfBounds},
		{"UC-048/BR-014 seconds above range", map[string]any{"serials": "A,B", "seconds": "11"}, fserr.OutOfBounds},
		{"UC-048/BR-014 seconds not a number", map[string]any{"serials": "A,B", "seconds": "abc"}, fserr.OutOfBounds},
		{"UC-048/BR-014 seconds hex float", map[string]any{"serials": "A,B", "seconds": "0x1p2"}, fserr.OutOfBounds},
		{"UC-048/BR-014 seconds missing", map[string]any{"serials": "A,B"}, fserr.OutOfBounds},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := PayloadCheck(p, c.payload, inGame)
			if (err == nil) != (c.reason == "") || (err != nil && err.Reason != c.reason) {
				t.Fatalf("got %v, want %q", err, c.reason)
			}
		})
	}
}

// UC: UC-048 BR-014
func TestPayloadRulesAreValidated(t *testing.T) {
	for rule, ok := range map[string]bool{
		"serial_set": true, "float 1 10": true, "float 10 1": false, "float 1": false, "float a b": false,
	} {
		if got := validPayloadRule(rule) == nil; got != ok {
			t.Errorf("validPayloadRule(%q) accepted=%v, want %v", rule, got, ok)
		}
	}
}

func TestParseRejectsBadRows(t *testing.T) {
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"unknown flag":           `flags: [{set: agent, key: nope, kind: overlay, scope: [stack]}]`,
		"wrong set":              `flags: [{set: game, key: enabled, kind: overlay, scope: [stack]}]`,
		"unknown kind":           `flags: [{set: agent, key: enabled, kind: weird, scope: [stack]}]`,
		"unknown scope":          `flags: [{set: agent, key: enabled, kind: overlay, scope: [world]}]`,
		"persist on a game flag": `flags: [{set: interventions, key: volume_override, kind: state, scope: [game], persist: true}]`,
		"event without payload":  `flags: [{set: interventions, key: end_game, kind: event, scope: [game]}]`,
		"undeclared variant":     `flags: [{set: agent, key: enabled, kind: overlay, scope: [stack], bounds: {variants: [maybe]}}]`,
		"duplicate row":          `flags: [{set: agent, key: enabled, kind: overlay, scope: [stack]}, {set: agent, key: enabled, kind: overlay, scope: [stack]}]`,
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc), b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A URL a backend sends its API key to must never take a free value: the API
// has no authentication (D11), so a free string would let any LAN client point
// the key at its own host (PR #1417 review, CRITICAL 1).
//
// UC: UC-053 A5 BR-001
func TestCredentialBearingURLsAcceptOnlyDeclaredVariants(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", dir), baseline.DefaultFiles)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := LoadFile(filepath.Join(root(t), "registry.yaml"), b)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range reg.Policies() {
			k := strings.ToLower(p.Key)
			if strings.Contains(k, "url") || strings.Contains(k, "host") || strings.Contains(k, "endpoint") || strings.Contains(k, "key") {
				if p.Bounds.FreeString || p.Bounds.Pattern != nil {
					t.Errorf("UC-053/A5 %s/%s feeds a URL or credential and takes a free value", p.Set, p.Key)
				}
			}
		}
		for _, key := range []string{"decision.jev.base_url", "decision.claude.base_url"} {
			t.Run("UC-053/A5 "+dir+" "+key+" refuses an undeclared URL", func(t *testing.T) {
				p := reg.Get("agent", key)
				if p == nil {
					t.Fatalf("%s is not in the registry", key)
				}
				_, ferr := CheckValue(p, b.Flag("agent", key), "", json.RawMessage(`"https://attacker.example/v1"`))
				if ferr == nil || ferr.Reason != fserr.OutOfBounds || ferr.Status != http.StatusUnprocessableEntity {
					t.Fatalf("want 422 out_of_bounds, got %v", ferr)
				}
			})
		}
	}
}

// UC: UC-015 main
// The operator set is the read-only baseline (flags.md §13.5, §10a): it is loaded with
// eighteen keys in both flag directories (UC-016 added the three display keys, then the
// auto-paging and internet-check keys of #2742, then hold_reset_ms of #2747) and the
// registry holds no row for it.
func TestOperatorSetIsReadOnlyBaseline(t *testing.T) {
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-015/main operator set in services/flagd/"+dir+" has no registry row", func(t *testing.T) {
			b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", dir), baseline.DefaultFiles)
			if err != nil {
				t.Fatal(err)
			}
			set := b.Sets["operator"]
			if set == nil || len(set.Flags) != 18 {
				t.Fatalf("operator set: %+v", set)
			}
			reg := loadReal(t, dir)
			for key := range set.Flags {
				if reg.Get("operator", key) != nil {
					t.Fatalf("operator/%s must not be mutable at runtime", key)
				}
			}
		})
	}
}

// UC: UC-043 A17
func TestRealScopeIsLatched(t *testing.T) {
	t.Run("UC-043/A17 every flag promotable in scope real is latched at game start", func(t *testing.T) {
		for _, dir := range []string{"", "ci"} {
			real := map[string]bool{}
			for _, p := range loadReal(t, dir).Policies() {
				if !p.AllowsScope(ScopeReal) {
					continue
				}
				real[id(p.Set, p.Key)] = true
				if !p.LatchedAtGameStart() {
					t.Errorf("flagd/%s: %s allows scope real but is not latched at game start; a real write would change a running game", dir, id(p.Set, p.Key))
				}
			}
			for key := range latchedAtGameStart {
				if !real[key] {
					t.Errorf("flagd/%s: latchedAtGameStart lists %s, which has no scope real", dir, key)
				}
			}
		}
	})
}

// shield.activation is read at each grant in instant and trigger games
// (UC-048 BR-004), so it is neither latched nor writable in scope real.
//
// UC: UC-043 A17
// UC: UC-048 BR-004
func TestShieldActivationIsReadPerGrant(t *testing.T) {
	t.Run("UC-043/A17 shield.activation is not latched and has no scope real", func(t *testing.T) {
		const key = "game/shield.activation"
		if _, listed := latchedAtGameStart[key]; listed {
			t.Errorf("latchedAtGameStart lists %s, which instant and trigger games read per grant", key)
		}
		for _, dir := range []string{"", "ci"} {
			for _, p := range loadReal(t, dir).Policies() {
				if id(p.Set, p.Key) == key && p.AllowsScope(ScopeReal) {
					t.Errorf("flagd/%s: %s allows scope real", dir, key)
				}
			}
		}
	})
}

// UC: UC-054 BR-005
// UC: UC-053 A12
func TestKillSwitchSetMatchesFlagsMD(t *testing.T) {
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root(t), "..", "..", rel))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(raw), "\r\n", "\n")
	}
	paragraph := func(text, anchor string) string {
		start := strings.Index(text, anchor)
		if start < 0 {
			t.Fatalf("anchor %q is missing", anchor)
		}
		para := text[start:]
		if end := strings.Index(para, "\n\n"); end > 0 {
			para = para[:end]
		}
		return para
	}

	t.Run("UC-054/BR-005 flags.md §13.6 lists exactly the Go kill-switch set", func(t *testing.T) {
		var got []string
		for _, m := range backticked.FindAllStringSubmatch(paragraph(read("docs/specs/flags.md"), "**Kill switches** (UC-054"), -1) {
			if strings.Contains(m[1], "/") && !strings.ContainsAny(m[1], " ?*") {
				got = append(got, m[1])
			}
		}
		sort.Strings(got)
		if want := sortedKeys(killSwitches); !reflect.DeepEqual(got, want) {
			t.Fatalf("flags.md §13.6 names %v, the Go set is %v", got, want)
		}
	})

	// FR-152's agent-limiting settings (C-034), "*" expanded against the registry rows.
	sentence := paragraph(read("docs/OWNERSHIP_MODEL.md"), "Every agent-limiting setting of FR-152 is in `agent`:")
	if j := strings.Index(sentence, ". "); j >= 0 {
		sentence = sentence[:j]
	}
	fr152 := backticked.FindAllStringSubmatch(sentence, -1)
	for _, dir := range []string{"", "ci"} {
		t.Run("UC-054/BR-005 every kill switch has a row and every FR-152 row is one, against flagd/"+dir, func(t *testing.T) {
			reg := loadReal(t, dir)
			for _, name := range sortedKeys(killSwitches) {
				set, key, _ := strings.Cut(name, "/")
				if reg.Get(set, key) == nil {
					t.Errorf("%s: kill switch with no registry row (not a Flagsmith feature)", name)
				}
			}
			n := 0
			for _, m := range fr152[1:] {
				prefix, wild := strings.CutSuffix(m[1], "*")
				for _, p := range reg.Policies() {
					if p.Set == "agent" && (p.Key == m[1] || wild && strings.HasPrefix(p.Key, prefix)) {
						n++
						if !IsKillSwitch(p.Set, p.Key) {
							t.Errorf("agent/%s: FR-152 agent-limiting flag with a registry row is not a kill switch", p.Key)
						}
					}
				}
			}
			if n < 3 {
				t.Fatalf("only %d FR-152 flags with a registry row (anchor drift?)", n)
			}
		})
		t.Run("UC-053/A12 only interventions_allowed applies when the save fails, against flagd/"+dir, func(t *testing.T) {
			var got []string
			for _, p := range loadReal(t, dir).Policies() {
				if p.AppliesWhenPersistFails() {
					got = append(got, id(p.Set, p.Key))
				}
			}
			if want := []string{"agent/interventions_allowed"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("AppliesWhenPersistFails holds for %v, want %v", got, want)
			}
		})
	}
}

// UC: UC-053 BR-001
func TestRegistryYAMLHasNoUnknownKeys(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root(t), "registry.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRegistry(raw, true); err != nil {
		t.Fatalf("registry.yaml has a key the parser does not know (misspelt?): %v", err)
	}
}

// UC: UC-053 BR-001
func TestParseIgnoresUnknownKeys(t *testing.T) {
	b, err := baseline.Load(filepath.Join(root(t), "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse([]byte(`flags: [{set: agent, key: interventions_allowed, kind: overlay, scope: [stack], persist: true, kill_switch: true, bounds: {variants: [none], later: 1}}]`), b)
	if err != nil {
		t.Fatalf("a newer release's key refused: %v", err)
	}
	p := r.Get("agent", "interventions_allowed")
	if p == nil || !p.Persist || !reflect.DeepEqual(p.Bounds.Variants, []string{"none"}) {
		t.Fatalf("known keys lost: %+v", p)
	}
	if _, err := Parse([]byte(`flags: [{set: agent, key: enabled, kind: overlay, scope: stack, extra: 1}]`), b); err == nil {
		t.Fatal("a type error next to an unknown key was accepted")
	}
}

// UC: UC-053 A5
func TestCheckValueShapeCeiling(t *testing.T) {
	p := &Policy{Set: "game", Key: "windows", Kind: KindOverlay, Scopes: []string{ScopeExperiment}, Bounds: Bounds{Shape: true}}
	f := &baseline.Flag{Variants: map[string]json.RawMessage{
		"default": json.RawMessage(`{"a":4,"b":8}`), "calm": json.RawMessage(`{"a":3,"b":30}`)}, DefaultVariant: "default"}
	for value, ok := range map[string]bool{
		`{"a":8,"b":60}`:     true,
		`{"a":0,"b":0}`:      true,
		`{"a":9,"b":11}`:     false,
		`{"a":4,"b":61}`:     false,
		`{"a":-1,"b":11}`:    false,
		`{"a":1e9,"b":11}`:   false,
		`{"a":4,"b":999999}`: false,
	} {
		t.Run("UC-053/A5 shape ceiling "+value, func(t *testing.T) {
			_, err := CheckValue(p, f, "", json.RawMessage(value))
			if ok != (err == nil) {
				t.Fatalf("value %s: accepted=%v, want %v (%v)", value, err == nil, ok, err)
			}
			if err != nil && err.Reason != fserr.OutOfBounds {
				t.Fatalf("reason %s, want out_of_bounds", err.Reason)
			}
		})
	}
}

// UC: UC-053 A5
func TestPatternFloatIsPlainDecimal(t *testing.T) {
	seg := &segment{kind: "float", lo: 0, hi: 10}
	for part, ok := range map[string]bool{"1": true, "2.5": true, "0.25": true, "0x1p0": false, "inf": false, "NaN": false, "1e1": false, "1_0": false, "": false, ".5": true, "5.": true, "+1": true, "-2.5": false, "+.5": true, "-.": false, ".": false, "+": false, "1.5.2": false, "+0x1": false, ".5e1": false} {
		t.Run("UC-053/A5 float segment "+part, func(t *testing.T) {
			if got := seg.match(part); got != ok {
				t.Fatalf("match(%q)=%v, want %v", part, got, ok)
			}
		})
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
