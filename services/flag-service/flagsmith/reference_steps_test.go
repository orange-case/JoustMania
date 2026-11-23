package flagsmith

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
)

// The reference step's pacing, re-read and failures (FR-227, architecture
// note §5.2; maintainer decisions D2, D4 and D6 of 2026-11-23, #2623).

// flagDefault is an environment default in the flag API's form.
func flagDefault(enabled bool, typ, value string) flagsmithtest.EnvironmentDefault {
	raw, _ := json.Marshal(map[string]string{"type": typ, "value": value})
	return flagsmithtest.EnvironmentDefault{Enabled: enabled, Value: raw}
}

// offFeatures makes the start see the named features, every one off without a
// value in the document; ids are 1, 2, ... in order. Every other registry flag
// is a feature already that no document shows (never written).
func (e *startEnv) offFeatures(names ...string) {
	var states []map[string]any
	e.admin.mu.Lock()
	e.admin.features = nil
	named := map[string]bool{}
	for i, n := range names {
		named[n] = true
		e.admin.features = append(e.admin.features, fsFeature{ID: i + 1, Name: n})
		states = append(states, fstate(n, false, nil))
	}
	for i, p := range e.reg.Policies() {
		if !named[p.Key] {
			e.admin.features = append(e.admin.features, fsFeature{ID: 2000 + i, Name: p.Key})
		}
	}
	e.admin.mu.Unlock()
	e.admin.f.SetDocument(testDocument(states, nil), "")
}

// referenceLogLines counts the log lines of the reference step.
func referenceLogLines(logs string) int {
	return strings.Count(logs, `msg="flagsmith reference`)
}

const reReadPath = flagsmithtest.APIPath + "environments/envkey123/featurestates/"

// UC: UC-054 A3
func TestReferenceStepPacing(t *testing.T) {
	t.Run("UC-054/A3 FR-227 every re-read and write is paced at PUSH_RATE and a 429 waits its Retry-After", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		e.offFeatures("decision.backend", "experiment_tick_seconds", "decision.min_interval_ms")
		var mu sync.Mutex
		var at []time.Time
		limited := false
		stamp := func() {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, e.clock.Now())
		}
		f.Handle("GET /api/v1/environments/{env}/featurestates/{$}", func(w http.ResponseWriter, _ *http.Request) {
			stamp()
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		})
		f.Handle("PATCH /api/__future__/environments/{env}/features/{fid}/{$}", func(w http.ResponseWriter, _ *http.Request) {
			stamp()
			mu.Lock()
			first := !limited
			limited = true
			mu.Unlock()
			if first {
				w.Header().Set("Retry-After", "4")
				flagsmithtest.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"detail": "throttled"})
				return
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{})
		})
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		// re-read 1, PATCH 1 (429), PATCH 1 again, then re-read and PATCH for 2 and 3.
		if len(at) != 7 {
			t.Fatalf("%d reference requests, want 7 (the 429 retried once)", len(at))
		}
		interval := time.Second / DefaultPushRate
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < interval {
				t.Fatalf("requests %d and %d %v apart, want at least %v (FLAGSMITH_PUSH_RATE %d/s)", i-1, i, gap, interval, DefaultPushRate)
			}
		}
		if gap := at[2].Sub(at[1]); gap < 4*time.Second {
			t.Fatalf("the retry of the 429 came %v after it, want its Retry-After of 4 s", gap)
		}
	})
}

// UC: UC-054 A3
func TestReferenceReReadBeforeWrite(t *testing.T) {
	t.Run("UC-054/A3 FR-227 a kill switch off in the document but on in the re-read is not written", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		e.offFeatures("experiment_max_games")
		f.Options.SetDefault(1, flagDefault(true, "integer", "20")) // turned on after the document was built
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := referencePatches(f)[1]; n != 0 {
			t.Fatalf("experiment_max_games written %d times, want never (on in the authoritative state)", n)
		}
		if got, enabled, _ := storedDefault(t, f, 1); !enabled || got != "20" {
			t.Fatalf("experiment_max_games enabled=%v %q, want the person's enabled=true 20 untouched", enabled, got)
		}
		if n := len(f.RequestsTo(http.MethodGet, reReadPath)); n != 1 {
			t.Fatalf("%d re-reads, want 1", n)
		}
	})
	t.Run("UC-054/A3 FR-227 a kill switch value overwritten under the off toggle is logged at INFO with its old value", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		e.offFeatures("experiment_max_games", "experiment_tick_seconds")
		f.Options.SetDefault(1, flagDefault(false, "integer", "20"))
		f.Options.SetDefault(2, flagDefault(false, "integer", "7"))
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		logs := e.logs.String()
		if !strings.Contains(logs, `level=INFO msg="flagsmith reference overwrote a kill switch value" flag_set=agent flag=experiment_max_games old_value=20`) {
			t.Fatalf("log %q, want the INFO naming experiment_max_games and its old value 20", logs)
		}
		if strings.Contains(logs, "flag=experiment_tick_seconds old_value") {
			t.Fatalf("log %q, want no kill switch line for experiment_tick_seconds (no kill switch)", logs)
		}
	})
}

// UC: UC-054 A3
func TestReferenceEnvironmentRefusal(t *testing.T) {
	t.Run("UC-054/A3 FR-227 the first change-request refusal stops the step with one WARN", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		e.offFeatures("decision.backend", "experiment_tick_seconds", "decision.min_interval_ms")
		f.Handle("PATCH /api/__future__/environments/{env}/features/{fid}/{$}", func(w http.ResponseWriter, _ *http.Request) {
			flagsmithtest.WriteJSON(w, http.StatusConflict, map[string]any{"detail": "change_requests_enabled"})
		})
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatalf("start %v, want it to go on past the refusal", err)
		}
		if n := len(f.RequestsTo(http.MethodPatch, "/api/__future__/")); n != 1 {
			t.Fatalf("%d reference writes for 3 features, want 1 (the step stops at the first)", n)
		}
		logs := e.logs.String()
		if n := referenceLogLines(logs); n != 1 || !strings.Contains(logs, "level=WARN") {
			t.Fatalf("%d reference log lines in %q, want one WARN", n, logs)
		}
	})
}

// UC: UC-054 A3
func TestReferenceVariantsRefused(t *testing.T) {
	t.Run("UC-054/A3 FR-227 options changed since the feature list: one relist, then the write goes through", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		e.offFeatures("rollout_enabled")
		f.Options.Add(1, "on")
		f.Options.Add(1, "off")
		var once sync.Once
		f.Handle("GET /api/v1/environments/{env}/featurestates/{$}", func(w http.ResponseWriter, _ *http.Request) {
			once.Do(func() { f.Options.Add(1, "added by a person") }) // after the feature list
			flagsmithtest.WriteJSON(w, http.StatusOK, map[string]any{"results": []any{}})
		})
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := referencePatches(f)[1]; n != 2 {
			t.Fatalf("rollout_enabled written %d times, want 2 (refused, relisted, sent again)", n)
		}
		if n := len(f.RequestsTo(http.MethodGet, flagsmithtest.APIPath+"projects/42/features/1/mv-options/")); n != 1 {
			t.Fatalf("%d option listings, want 1", n)
		}
		got, enabled, variants := storedDefault(t, f, 1)
		if enabled || got != "off" || len(variants) != 3 {
			t.Fatalf("rollout_enabled enabled=%v %q weights %+v, want enabled=false off with all three options named", enabled, got, variants)
		}
	})
	t.Run("UC-054/A3 FR-227 a second variants refusal is logged for the feature and the step goes on", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		e.offFeatures("rollout_enabled", "experiment_tick_seconds")
		f.Handle("PATCH /api/__future__/environments/{env}/features/1/{$}", func(w http.ResponseWriter, _ *http.Request) {
			flagsmithtest.WriteJSON(w, http.StatusBadRequest, map[string]any{"environment_default": map[string]any{"variants": []string{"Must include all feature's variants."}}})
		})
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := referencePatches(f)[1]; n != 2 {
			t.Fatalf("rollout_enabled written %d times, want 2 (one relist, no third try)", n)
		}
		if got, enabled, _ := storedDefault(t, f, 2); enabled || got != "default" {
			t.Fatalf("experiment_tick_seconds %q enabled=%v, want its reference written after the refusal", got, enabled)
		}
		if !strings.Contains(e.logs.String(), `msg="flagsmith reference not written" flag_set=agent flag=rollout_enabled`) {
			t.Fatalf("log %q, want a WARN naming rollout_enabled", e.logs.String())
		}
	})
}

// UC: UC-054 A3
func TestReferenceDocumentReadFailure(t *testing.T) {
	// serveDocument answers the n-th document read with status(n) (200: the document).
	serveDocument := func(e *startEnv, status func(n int) int) {
		var mu sync.Mutex
		n := 0
		e.admin.f.Handle("GET /api/v1/environment-document/", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			n++
			st := status(n)
			mu.Unlock()
			if st != http.StatusOK {
				flagsmithtest.WriteJSON(w, st, map[string]any{"detail": "no"})
				return
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, testDocument([]map[string]any{fstate("decision.backend", false, nil)}, nil))
		})
	}
	t.Run("UC-054/A3 FR-227 a transient document read failure fails the start and the re-run writes", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		e.offFeatures("decision.backend")
		// The ping (1) answers; the step's read fails on every attempt (2-4).
		serveDocument(e, func(n int) int {
			if n >= 2 && n <= 4 {
				return http.StatusServiceUnavailable
			}
			return http.StatusOK
		})
		if err := e.s.Start(e.ctx); err == nil {
			t.Fatal("start succeeded, want it to fail on a 503 document read (retried with backoff)")
		}
		if n := referencePatches(f)[1]; n != 0 {
			t.Fatalf("%d writes after a failed read, want 0", n)
		}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if got, enabled, _ := storedDefault(t, f, 1); enabled || got != "rules" {
			t.Fatalf("decision.backend %q enabled=%v after the re-run, want the reference rules", got, enabled)
		}
	})
	t.Run("UC-054/A3 FR-227 a 4xx document read skips the references of the process with one WARN", func(t *testing.T) {
		e := newStartEnv(t)
		f := e.admin.f
		f.ServeFlagAPI()
		e.offFeatures("decision.backend")
		serveDocument(e, func(n int) int {
			if n == 2 {
				return http.StatusForbidden
			}
			return http.StatusOK
		})
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatalf("start %v, want it to go on without references", err)
		}
		if n := len(f.RequestsTo(http.MethodPatch, "/api/__future__/")); n != 0 {
			t.Fatalf("%d writes, want 0", n)
		}
		if n := referenceLogLines(e.logs.String()); n != 1 || !strings.Contains(e.logs.String(), "this process writes no reference value") {
			t.Fatalf("log %q, want one WARN that no reference is written", e.logs.String())
		}
	})
}

// UC: UC-054 A3
func TestEnvironmentDefaultPick(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage(`{"results":[
		{"id":1,"enabled":false,"identity":null,"feature_segment":null,"feature_state_value":"old","live_from":"2026-01-01T00:00:00Z"},
		{"id":2,"enabled":true,"identity":null,"feature_segment":null,"feature_state_value":"live","live_from":"2026-11-01T00:00:00.123456Z","multivariate_feature_state_values":[{"multivariate_feature_option":7,"percentage_allocation":100}]},
		{"id":3,"enabled":false,"identity":null,"feature_segment":null,"feature_state_value":"scheduled","live_from":"2026-12-01T00:00:00Z"},
		{"id":4,"enabled":false,"identity":9,"feature_segment":null,"feature_state_value":"identity"},
		{"id":5,"enabled":false,"identity":null,"feature_segment":3,"feature_state_value":"segment"}]}`)
	t.Run("UC-054/A3 FR-227 the newest live environment default is the state (v2 versioning)", func(t *testing.T) {
		fs, ok := environmentDefault(listedItems[listedState](raw), now)
		if !ok || !fs.Enabled || fsText(fs.FeatureStateValue) != "live" || len(fs.MultivariateValues) != 1 || fs.MultivariateValues[0].PercentageAllocation != 100 {
			t.Fatalf("picked %+v ok=%v, want the live one (id 2) with its weight", fs, ok)
		}
	})
	t.Run("UC-054/A3 FR-227 a plain list with no environment default lists none", func(t *testing.T) {
		if _, ok := environmentDefault(listedItems[listedState](json.RawMessage(`[{"id":4,"identity":9}]`)), now); ok {
			t.Fatal("picked an identity override, want none")
		}
	})
}
