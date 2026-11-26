package flags

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func defaultDecisionBackend() DecisionBackend {
	timeout := DefaultDecisionTimeoutMs * time.Millisecond
	return DecisionBackend{
		Backend:           DefaultDecisionBackend,
		CaptureContent:    DefaultDecisionCaptureContent,
		MinInterval:       DefaultDecisionMinIntervalMs * time.Millisecond,
		MinGapAfterAnswer: DefaultDecisionMinGapAfterAnswerMs * time.Millisecond,
		MaxStateAge:       DefaultDecisionMaxStateAgeMs * time.Millisecond,
		Claude: RemoteBackendConfig{BaseURL: DefaultDecisionClaudeBaseURL, Model: DefaultDecisionClaudeModel,
			Timeout:                DefaultDecisionClaudeTimeoutMs * time.Millisecond,
			ActConfidenceThreshold: DefaultDecisionClaudeActConfidenceThreshold,
			MaxRequestsPerMinute:   DefaultDecisionClaudeMaxRequestsPerMinute, HasMaxRequestsPerMinute: true},
		Jev: RemoteBackendConfig{BaseURL: DefaultDecisionJevBaseURL, Model: DefaultDecisionJevModel, Timeout: timeout,
			ActConfidenceThreshold: DefaultDecisionJevActConfidenceThreshold},
	}
}

func TestDecisionBackend_DefaultsWhenMissing(t *testing.T) {
	got := New(stubEvaluator{}, nil).Evaluate(context.Background()).Decision
	if got != defaultDecisionBackend() {
		t.Fatalf("Decision = %+v, want defaults %+v", got, defaultDecisionBackend())
	}
	if !got.IsRules() {
		t.Fatal("default backend must be rules")
	}
}

// TestDecisionBackend_DefaultTimeoutsPerBackend pins the NFR-010 default
// remote-call timeouts as literal values (the other tests compare against the
// constants, so a changed constant would pass them): Claude 5000 ms, Jev
// 2500 ms. TestDecisionBackend_FlagFilesMatchDefaults holds the flag files
// to the same values.
// NFR: NFR-010
// UC: UC-040 main
func TestDecisionBackend_DefaultTimeoutsPerBackend(t *testing.T) {
	got := New(stubEvaluator{}, nil).Evaluate(context.Background()).Decision
	for name, c := range map[string]struct{ got, want time.Duration }{
		"claude": {got.Claude.Timeout, 5000 * time.Millisecond},
		"jev":    {got.Jev.Timeout, 2500 * time.Millisecond},
	} {
		t.Run("NFR-010 "+name+" default timeout", func(t *testing.T) {
			if c.got != c.want {
				t.Fatalf("decision.%s.timeout_ms default = %v, want %v", name, c.got, c.want)
			}
		})
	}
}

// UC: UC-044 main
func TestDecisionBackend_FlagdShape(t *testing.T) {
	stub := stubEvaluator{
		booleans: map[string]bool{keyDecisionCaptureContent: true},
		strings: map[string]string{
			keyDecisionBackend:       " Claude ",
			keyDecisionClaudeModel:   "claude-sonnet-5-5",
			keyDecisionClaudeBaseURL: "http://127.0.0.1:1",
			keyDecisionJevBaseURL:    "https://jev.test",
			keyDecisionJevModel:      "jev-1",
		},
		ints: map[string]int64{
			keyDecisionClaudeTimeoutMs:         1500,
			keyDecisionJevTimeoutMs:            2000,
			keyDecisionMinIntervalMs:           1500,
			keyDecisionMinGapAfterAnswerMs:     3000,
			keyDecisionMaxStateAgeMs:           6000,
			keyDecisionClaudeMaxRequestsPerMin: 12,
		},
		floats: map[string]float64{keyDecisionClaudeActConfidenceThreshold: 0.95, keyDecisionJevActConfidenceThreshold: 0.9},
	}
	got := New(stub, nil).DecisionBackend(context.Background())
	want := DecisionBackend{
		Backend:           DecisionBackendClaude,
		CaptureContent:    true,
		MinInterval:       1500 * time.Millisecond,
		MinGapAfterAnswer: 3 * time.Second,
		MaxStateAge:       6 * time.Second,
		Claude: RemoteBackendConfig{BaseURL: "http://127.0.0.1:1", Model: "claude-sonnet-5-5", Timeout: 1500 * time.Millisecond,
			ActConfidenceThreshold: 0.95, MaxRequestsPerMinute: 12, HasMaxRequestsPerMinute: true},
		Jev: RemoteBackendConfig{BaseURL: "https://jev.test", Model: "jev-1", Timeout: 2 * time.Second,
			ActConfidenceThreshold: 0.9},
	}
	if got != want {
		t.Fatalf("DecisionBackend = %+v, want %+v", got, want)
	}
	if got.IsRules() {
		t.Fatal("claude is not rules")
	}
	if cfg, ok := got.Remote(DecisionBackendJev); !ok || cfg.Model != "jev-1" {
		t.Fatalf("Remote(jev) = %+v %v", cfg, ok)
	}
	if _, ok := got.Remote(DecisionBackendRules); ok {
		t.Fatal("rules is not a remote backend")
	}
}

// TestDecisionBackend_ClaudeBaseURLReadPerCall: decision.claude.base_url is read
// on every DecisionBackend call (use time, not startup), so a flag flip applies
// to the next cycle without a restart (#1900).
// UC: UC-044 A3
func TestDecisionBackend_ClaudeBaseURLReadPerCall(t *testing.T) {
	stub := stubEvaluator{strings: map[string]string{keyDecisionClaudeBaseURL: "http://127.0.0.1:1"}}
	f := New(stub, nil)
	if got := f.DecisionBackend(context.Background()).Claude.BaseURL; got != "http://127.0.0.1:1" {
		t.Fatalf("first read = %q", got)
	}
	stub.strings[keyDecisionClaudeBaseURL] = "https://proxy.example.test"
	if got := f.DecisionBackend(context.Background()).Claude.BaseURL; got != "https://proxy.example.test" {
		t.Fatalf("second read = %q, want the flipped value", got)
	}
}

func TestDecisionBackend_BadValuesFallBack(t *testing.T) {
	stub := stubEvaluator{
		strings: map[string]string{keyDecisionBackend: "   "},
		ints: map[string]int64{keyDecisionClaudeTimeoutMs: 0, keyDecisionJevTimeoutMs: -5, keyDecisionMinIntervalMs: -1,
			keyDecisionMinGapAfterAnswerMs: -1, keyDecisionMaxStateAgeMs: -1},
		floats: map[string]float64{keyDecisionClaudeActConfidenceThreshold: 1.5, keyDecisionJevActConfidenceThreshold: -1},
	}
	got := New(stub, nil).DecisionBackend(context.Background())
	if got != defaultDecisionBackend() {
		t.Fatalf("Decision = %+v, want defaults %+v", got, defaultDecisionBackend())
	}
}

// TestDecisionBackend_ActThresholdPerBackend covers decision.<backend>.act_confidence_threshold
// (#1287): one flag per backend, no shared or inherited value; NaN or a value
// outside [0,1] gives that flag's default; rules and unknown names have none.
// Spec: flags.md, decide-contract.md §5, FR-047.
// UC: UC-044 A3
func TestDecisionBackend_ActThresholdPerBackend(t *testing.T) {
	cases := []struct {
		name            string
		value           float64
		wantJev, wantCl float64
	}{
		{"gate", 0.6, 0.6, 0.6},
		{"zero", 0, 0, 0},
		{"one", 1, 1, 1},
		{"former inherit sentinel is out of range", -1, 0.6, 0.8},
		{"above one", 1.5, 0.6, 0.8},
		{"negative", -0.5, 0.6, 0.8},
		{"NaN", math.NaN(), 0.6, 0.8},
	}
	for _, tc := range cases {
		t.Run("UC-044/A3 "+tc.name, func(t *testing.T) {
			stub := stubEvaluator{floats: map[string]float64{
				keyDecisionClaudeActConfidenceThreshold: tc.value, keyDecisionJevActConfidenceThreshold: tc.value}}
			d := New(stub, nil).DecisionBackend(context.Background())
			if v, ok := d.ActThreshold(DecisionBackendJev); !ok || v != tc.wantJev {
				t.Fatalf("ActThreshold(jev) = %v,%v, want %v", v, ok, tc.wantJev)
			}
			if v, ok := d.ActThreshold(DecisionBackendClaude); !ok || v != tc.wantCl {
				t.Fatalf("ActThreshold(claude) = %v,%v, want %v", v, ok, tc.wantCl)
			}
			for _, b := range []string{DecisionBackendRules, "bogus"} {
				if _, ok := d.ActThreshold(b); ok {
					t.Fatalf("ActThreshold(%s) has a threshold, want none", b)
				}
			}
		})
	}
}

// TestDecisionBackend_BackendMaxRequestsPerMinute: claude's own decide cap (#2519, FR-152 (c)). `inherit` has no cap of its own, `stage` is 30, a
// missing flag or a flagd error is the Go default 30 (the base file's `stage`),
// and 0 or a bad negative value is a cap of 0, which closes the backend. jev has
// no flag and never a cap of its own.
// UC: UC-070 BR-003
// NFR: NFR-011
func TestDecisionBackend_BackendMaxRequestsPerMinute(t *testing.T) {
	cases := []struct {
		name    string
		ints    map[string]int64
		errs    map[string]error
		wantCap int
		wantHas bool
	}{
		{"UC-070/BR-003 inherit sentinel has no cap", map[string]int64{keyDecisionClaudeMaxRequestsPerMin: -1}, nil, 0, false},
		{"UC-070/BR-003 stage is 30", map[string]int64{keyDecisionClaudeMaxRequestsPerMin: 30}, nil, 30, true},
		{"UC-070/BR-003 missing flag is the default 30", nil, nil, DefaultDecisionClaudeMaxRequestsPerMinute, true},
		{"UC-070/BR-003 flagd error is the default 30", nil, map[string]error{
			keyDecisionClaudeMaxRequestsPerMin: errors.New("flagd down")},
			DefaultDecisionClaudeMaxRequestsPerMinute, true},
		{"UC-070/BR-003 zero closes the backend", map[string]int64{keyDecisionClaudeMaxRequestsPerMin: 0}, nil, 0, true},
		{"UC-070/BR-003 bad negative closes the backend", map[string]int64{keyDecisionClaudeMaxRequestsPerMin: -7}, nil, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := New(stubEvaluator{ints: tc.ints, errs: tc.errs}, nil).DecisionBackend(context.Background())
			for name, cfg := range map[string]RemoteBackendConfig{DecisionBackendClaude: d.Claude} {
				if cfg.MaxRequestsPerMinute != tc.wantCap || cfg.HasMaxRequestsPerMinute != tc.wantHas {
					t.Errorf("%s cap = %d,%v, want %d,%v", name, cfg.MaxRequestsPerMinute, cfg.HasMaxRequestsPerMinute, tc.wantCap, tc.wantHas)
				}
			}
			if d.Jev.HasMaxRequestsPerMinute {
				t.Errorf("jev has a cap of its own (%d), want none (global budget only)", d.Jev.MaxRequestsPerMinute)
			}
		})
	}
	if (DecisionBackend{}).Claude.HasMaxRequestsPerMinute {
		t.Fatal("a zero-value snapshot must have no backend cap")
	}
}

func TestDecisionBackend_JevThresholdDefaultActsAtGate(t *testing.T) {
	d := New(stubEvaluator{}, nil).DecisionBackend(context.Background())
	if v, _ := d.ActThreshold(DecisionBackendJev); v != 0.6 {
		t.Fatalf("default ActThreshold(jev) = %v, want 0.6", v)
	}
	if v, _ := d.ActThreshold(DecisionBackendClaude); v != 0.8 {
		t.Fatalf("default ActThreshold(claude) = %v, want 0.8", v)
	}
}

func TestDecisionBackend_DefaultsOnError(t *testing.T) {
	boom := errors.New("flagd down")
	stub := stubEvaluator{errs: map[string]error{}}
	for _, k := range []string{
		keyDecisionBackend, keyDecisionClaudeActConfidenceThreshold, keyDecisionCaptureContent,
		keyDecisionClaudeModel, keyDecisionClaudeTimeoutMs, keyDecisionClaudeBaseURL, keyDecisionJevBaseURL,
		keyDecisionJevModel, keyDecisionJevTimeoutMs, keyDecisionMinIntervalMs,
		keyDecisionJevActConfidenceThreshold,
	} {
		stub.errs[k] = boom
	}
	if got := New(stub, nil).DecisionBackend(context.Background()); got != defaultDecisionBackend() {
		t.Fatalf("Decision = %+v, want defaults", got)
	}
}

func TestDecisionBackend_ZeroValueIsRules(t *testing.T) {
	if !(DecisionBackend{}).IsRules() {
		t.Fatal("a zero-value DecisionBackend (hand-built snapshot) must count as rules")
	}
}

// TestDecisionBackend_FlagFilesMatchDefaults keeps services/flagd/agent.json and
// ci/agent.json in sync with the Go defaults (the fail-safe values when flagd is
// unreachable). capture_content is the one intended difference: on in ci/.
func TestDecisionBackend_FlagFilesMatchDefaults(t *testing.T) {
	want := map[string]any{
		keyDecisionBackend:                      DefaultDecisionBackend,
		keyDecisionClaudeActConfidenceThreshold: DefaultDecisionClaudeActConfidenceThreshold,
		keyDecisionClaudeModel:                  DefaultDecisionClaudeModel,
		keyDecisionClaudeTimeoutMs:              float64(DefaultDecisionClaudeTimeoutMs),
		keyDecisionClaudeBaseURL:                DefaultDecisionClaudeBaseURL,
		keyDecisionJevBaseURL:                   DefaultDecisionJevBaseURL,
		keyDecisionJevModel:                     DefaultDecisionJevModel,
		keyDecisionJevTimeoutMs:                 float64(DefaultDecisionTimeoutMs),
		keyDecisionMinIntervalMs:                float64(DefaultDecisionMinIntervalMs),
		keyDecisionJevActConfidenceThreshold:    DefaultDecisionJevActConfidenceThreshold,
	}
	for path, capture := range map[string]bool{
		filepath.Join("..", "..", "flagd", "agent.json"):       DefaultDecisionCaptureContent,
		filepath.Join("..", "..", "flagd", "ci", "agent.json"): true,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Flags map[string]struct {
				Variants       map[string]any `json:"variants"`
				DefaultVariant string         `json:"defaultVariant"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		expect := map[string]any{keyDecisionCaptureContent: capture}
		for k, v := range want {
			expect[k] = v
		}
		for key, w := range expect {
			f, ok := doc.Flags[key]
			if !ok {
				t.Errorf("%s: flag %s missing", path, key)
				continue
			}
			if got := f.Variants[f.DefaultVariant]; got != w {
				t.Errorf("%s: %s default = %v, want %v", path, key, got, w)
			}
		}
	}
}

// TestDecisionBackend_JevBlackholeVariant pins the talk's degrade-beat variant
// (#743): decision.jev.base_url ships a non-default "blackhole" variant in both
// flag files that points at TEST-NET-1 (RFC 5737, never routed) over https, so
// the TLS handshake fails before any request header (the bearer key) is sent.
// The default stays DefaultDecisionJevBaseURL.
// UC: UC-070 BR-004
func TestDecisionBackend_JevBlackholeVariant(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "flagd", "agent.json"),
		filepath.Join("..", "..", "flagd", "ci", "agent.json"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Flags map[string]struct {
				Variants       map[string]any `json:"variants"`
				DefaultVariant string         `json:"defaultVariant"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		f := doc.Flags[keyDecisionJevBaseURL]
		if got := f.Variants["blackhole"]; got != "https://192.0.2.1/v1" {
			t.Errorf("%s: %s blackhole = %v, want https://192.0.2.1/v1", path, keyDecisionJevBaseURL, got)
		}
		if f.DefaultVariant == "blackhole" {
			t.Errorf("%s: %s must not default to blackhole", path, keyDecisionJevBaseURL)
		}
		if got := f.Variants[f.DefaultVariant]; got != DefaultDecisionJevBaseURL {
			t.Errorf("%s: %s default = %v, want %s", path, keyDecisionJevBaseURL, got, DefaultDecisionJevBaseURL)
		}
	}
}

// TestDecisionBackend_StageBudgetVariant pins the presenter's live switch for the
// global remote-call budget (#1231): llm.max_requests_per_minute has a "stage"
// variant of 30 in both flag files, while the default stays
// DefaultLLMMaxRequestsPerMinute.
// UC: UC-070 BR-003
func TestDecisionBackend_StageBudgetVariant(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "flagd", "agent.json"),
		filepath.Join("..", "..", "flagd", "ci", "agent.json"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Flags map[string]struct {
				Variants       map[string]any `json:"variants"`
				DefaultVariant string         `json:"defaultVariant"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// One threshold per backend and no shared one to inherit (#2805).
		if _, ok := doc.Flags[keyDecisionJevActConfidenceThreshold].Variants["inherit"]; ok {
			t.Errorf("%s: %s has an inherit variant", path, keyDecisionJevActConfidenceThreshold)
		}
		if _, ok := doc.Flags["decision.act_confidence_threshold"]; ok {
			t.Errorf("%s: the shared decision.act_confidence_threshold still exists", path)
		}
		f := doc.Flags[keyLLMMaxRequestsPerMin]
		if got := f.Variants["stage"]; got != float64(30) {
			t.Errorf("%s: %s stage = %v, want 30", path, keyLLMMaxRequestsPerMin, got)
		}
		if got := f.Variants[f.DefaultVariant]; got != float64(DefaultLLMMaxRequestsPerMinute) {
			t.Errorf("%s: %s default = %v, want %d", path, keyLLMMaxRequestsPerMin, got, DefaultLLMMaxRequestsPerMinute)
		}
	}
}

// UC: UC-040 main
// Step 3 defaults: 3 s between remote calls of a game, 6 calls a minute, real games only.
func TestDecisionGate_Step3Defaults(t *testing.T) {
	snap := New(stubEvaluator{}, nil).Evaluate(context.Background())
	if snap.Decision.MinInterval != 3*time.Second {
		t.Errorf("decision.min_interval_ms default = %v, want 3s", snap.Decision.MinInterval)
	}
	if snap.LLMGate.MaxRequestsPerMinute != 6 {
		t.Errorf("llm.max_requests_per_minute default = %d, want 6", snap.LLMGate.MaxRequestsPerMinute)
	}
	if len(snap.LLMGate.EligibleGameKinds) != 1 || snap.LLMGate.EligibleGameKinds[0] != "real" {
		t.Errorf("llm.eligible_game_kinds default = %v, want [real] (real_only)", snap.LLMGate.EligibleGameKinds)
	}
}

// TestDecisionBackend_CadenceFlagDefaults pins the #2123 cadence flags: both flag
// files ship decision.min_gap_after_answer_ms = 1000 and
// decision.max_state_age_ms = 10000 as their defaults, matching the compiled
// fallbacks, and the stage baseline (scripts/talk-baseline.json) runs claude with
// the "patient" timeout (8000 ms), which the state-age bound stays above.
// UC: UC-040 A4
// UC: UC-070 BR-003
func TestDecisionBackend_CadenceFlagDefaults(t *testing.T) {
	type flagDoc struct {
		Flags map[string]struct {
			Variants       map[string]any `json:"variants"`
			DefaultVariant string         `json:"defaultVariant"`
		} `json:"flags"`
	}
	for _, path := range []string{
		filepath.Join("..", "..", "flagd", "agent.json"),
		filepath.Join("..", "..", "flagd", "ci", "agent.json"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc flagDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for key, want := range map[string]int{
			keyDecisionMinGapAfterAnswerMs: DefaultDecisionMinGapAfterAnswerMs,
			keyDecisionMaxStateAgeMs:       DefaultDecisionMaxStateAgeMs,
		} {
			f, ok := doc.Flags[key]
			if !ok {
				t.Errorf("%s: %s missing", path, key)
				continue
			}
			if got := f.Variants[f.DefaultVariant]; got != float64(want) {
				t.Errorf("%s: %s default = %v, want %d", path, key, got, want)
			}
			if got := f.Variants["off"]; got != float64(0) {
				t.Errorf("%s: %s off = %v, want 0", path, key, got)
			}
		}
		patient := doc.Flags[keyDecisionClaudeTimeoutMs].Variants["patient"]
		if patient != float64(8000) {
			t.Errorf("%s: %s patient = %v, want 8000", path, keyDecisionClaudeTimeoutMs, patient)
		}
		if patient.(float64) >= float64(DefaultDecisionMaxStateAgeMs) {
			t.Errorf("%s: patient timeout %v must stay below the default state-age bound %d", path, patient, DefaultDecisionMaxStateAgeMs)
		}
	}
	snap := New(stubEvaluator{}, nil).Evaluate(context.Background())
	if snap.Decision.MinGapAfterAnswer != time.Second || snap.Decision.MaxStateAge != 10*time.Second {
		t.Errorf("compiled defaults gap=%v age=%v, want 1s and 10s", snap.Decision.MinGapAfterAnswer, snap.Decision.MaxStateAge)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "talk-baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var baseline struct {
		Checked map[string]string `json:"checked"`
	}
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	if got := baseline.Checked[keyDecisionClaudeTimeoutMs]; got != "patient" {
		t.Errorf("talk baseline %s = %q, want patient (#2123)", keyDecisionClaudeTimeoutMs, got)
	}
	// Jev (~0.3 s answers) at a 1.5 s interval would ask ~40/min and fill the stage
	// budget of 30 the retro shares: the stage keeps the 3000 ms default.
	if got := baseline.Checked[keyDecisionMinIntervalMs]; got != "default" {
		t.Errorf("talk baseline %s = %q, want default (#2123)", keyDecisionMinIntervalMs, got)
	}
}
