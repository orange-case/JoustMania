package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// --- fixtures ---------------------------------------------------------------

// startClock is the start's clock: Sleep records the wait and moves the clock;
// the stale identity grace waits for release (or the end of the test), and,
// when backoff is set, the link's first backoff (BackoffMin) waits for it.
type startClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	release chan struct{}
	backoff chan struct{}
	// hook runs once at the next Sleep (one-shot).
	hook func()
}

func (c *startClock) onNextSleep(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = f
}

func newStartClock() *startClock {
	return &startClock{now: time.Date(2026, 11, 15, 18, 0, 0, 0, time.UTC), release: make(chan struct{})}
}

func (c *startClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *startClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	gate := c.release
	if d == BackoffMin {
		gate = c.backoff
	} else if d != StaleIdentityGrace {
		gate = nil
	}
	c.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	hook := c.hook
	c.hook = nil
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

func (c *startClock) got() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// fakeAdmin serves the Admin API endpoints of the start sequence on a
// flagsmithtest.Fake with the state and failures the start tests script
// (the fake's default start-up routes serve every other link test).
type fakeAdmin struct {
	f *flagsmithtest.Fake

	mu         sync.Mutex
	env        map[string]any
	envStatus  int
	features   []fsFeature
	tagPages   [][]fsTag
	tags429    int   // the first tag listings answer 429 Retry-After 3
	edgeStatus []int // statuses of the next edge checks (page_size=1)
	listStatus []int // statuses of the next cleanup listings
	listFail   bool  // every cleanup listing answers 500
	identities [][]edgeIdentity
	onDelete   func(uuid string)
	delStatus  map[string][]int                             // uuid -> statuses of its next deletes
	create     func(name string, attempt int) (int, string) // status, body; 0: created
	attempts   map[string]int
	posts      []map[string]any
	tagPosts   []map[string]any
	afterList  func() // runs once the feature list was written (#2713)
	deletes    []string
	nextID     int
}

const (
	testProjectID = 42
	testEnvID     = 7
)

func newFakeAdmin(t *testing.T) *fakeAdmin {
	t.Helper()
	a := &fakeAdmin{
		f:         flagsmithtest.New(t),
		env:       map[string]any{"id": testEnvID, "name": "stage", "project": testProjectID, "use_v2_feature_versioning": false, "use_identity_overrides_in_local_eval": true},
		attempts:  map[string]int{},
		delStatus: map[string][]int{},
		nextID:    1000,
	}
	p := flagsmithtest.APIPath
	a.f.Handle("GET "+p+"environments/{key}/{$}", a.environment)
	a.f.Handle("GET "+p+"environments/{key}/edge-identities/{$}", a.edgeList)
	a.f.Handle("DELETE "+p+"environments/{key}/edge-identities/{uuid}/{$}", a.edgeDelete)
	a.f.Handle("GET "+p+"projects/{pid}/features/{$}", a.featureList)
	a.f.Handle("POST "+p+"projects/{pid}/features/{$}", a.featureCreate)
	a.f.Handle("GET "+p+"projects/{pid}/tags/{$}", a.tagList)
	a.f.Handle("POST "+p+"projects/{pid}/tags/{$}", a.tagCreate)
	// The fake's mv-options routes serve this admin's features (#2709).
	a.f.Options.Exists = a.hasFeature
	return a
}

func pageOf(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func (a *fakeAdmin) environment(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.envStatus != 0 {
		flagsmithtest.WriteJSON(w, a.envStatus, map[string]any{"detail": "no"})
		return
	}
	flagsmithtest.WriteJSON(w, 200, a.env)
}

func (a *fakeAdmin) edgeList(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if r.URL.Query().Get("page_size") == "1" {
		st := 200
		if len(a.edgeStatus) > 0 {
			st, a.edgeStatus = a.edgeStatus[0], a.edgeStatus[1:]
		}
		a.mu.Unlock()
		if st != 200 {
			flagsmithtest.WriteJSON(w, st, map[string]any{"detail": "Dynamo DB is not enabled for this project"})
			return
		}
		flagsmithtest.WriteJSON(w, 200, map[string]any{"results": []any{}, "last_evaluated_key": nil})
		return
	}
	st := 200
	if len(a.listStatus) > 0 {
		st, a.listStatus = a.listStatus[0], a.listStatus[1:]
	}
	if a.listFail {
		st = 500
	}
	if st != 200 {
		a.mu.Unlock()
		flagsmithtest.WriteJSON(w, st, map[string]any{"detail": "list"})
		return
	}
	i := 0
	if k := r.URL.Query().Get("last_evaluated_key"); k != "" {
		i, _ = strconv.Atoi(strings.TrimPrefix(k, "p"))
	}
	var page []edgeIdentity
	if i < len(a.identities) {
		page = a.identities[i]
	}
	var next any
	if i+1 < len(a.identities) {
		next = "p" + strconv.Itoa(i+1)
	}
	a.mu.Unlock()
	flagsmithtest.WriteJSON(w, 200, map[string]any{"results": page, "last_evaluated_key": next})
}

func (a *fakeAdmin) edgeDelete(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	a.mu.Lock()
	if q := a.delStatus[uuid]; len(q) > 0 {
		a.delStatus[uuid] = q[1:]
		a.mu.Unlock()
		flagsmithtest.WriteJSON(w, q[0], map[string]any{"detail": "delete"})
		return
	}
	a.deletes = append(a.deletes, uuid)
	// A deleted identity is no longer listed.
	for i, page := range a.identities {
		kept := page[:0:0]
		for _, id := range page {
			if id.IdentityUUID != uuid {
				kept = append(kept, id)
			}
		}
		a.identities[i] = kept
	}
	onDelete := a.onDelete
	a.mu.Unlock()
	if onDelete != nil {
		onDelete(uuid)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *fakeAdmin) featureList(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	features := append([]fsFeature(nil), a.features...)
	after := a.afterList
	a.mu.Unlock()
	if after != nil {
		defer after()
	}
	// The options as the Admin API answers them (without the key field on a
	// Flagsmith before #7698, #2713).
	results := make([]map[string]any, 0, len(features))
	for _, f := range features {
		results = append(results, map[string]any{"id": f.ID, "name": f.Name, "is_archived": f.IsArchived, "multivariate_options": a.f.Options.Wire(f.ID)})
	}
	flagsmithtest.WriteJSON(w, 200, map[string]any{"count": len(features), "next": nil, "results": results})
}

func (a *fakeAdmin) hasFeature(id int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.features {
		if f.ID == id {
			return true
		}
	}
	return false
}

func (a *fakeAdmin) featureCreate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	name, _ := body["name"].(string)
	a.mu.Lock()
	a.attempts[name]++
	attempt := a.attempts[name]
	create := a.create
	a.mu.Unlock()
	if create != nil {
		if st, msg := create(name, attempt); st != 0 {
			flagsmithtest.WriteJSON(w, st, map[string]any{"name": []string{msg}})
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.posts = append(a.posts, body)
	a.nextID++
	f := fsFeature{ID: a.nextID, Name: name}
	a.features = append(a.features, f)
	flagsmithtest.WriteJSON(w, 201, f)
}

func (a *fakeAdmin) tagList(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tags429 > 0 {
		a.tags429--
		w.Header().Set("Retry-After", "3")
		flagsmithtest.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"detail": "throttled"})
		return
	}
	n := pageOf(r)
	var results []fsTag
	if n <= len(a.tagPages) {
		results = a.tagPages[n-1]
	}
	var next any
	if n < len(a.tagPages) {
		next = "more"
	}
	flagsmithtest.WriteJSON(w, 200, map[string]any{"next": next, "results": results})
}

func (a *fakeAdmin) tagCreate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tagPosts = append(a.tagPosts, body)
	a.nextID++
	flagsmithtest.WriteJSON(w, 201, map[string]any{"id": a.nextID, "label": body["label"]})
}

func (a *fakeAdmin) postNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, p := range a.posts {
		out = append(out, p["name"].(string))
	}
	return out
}

func fstate(name string, enabled bool, value any) map[string]any {
	return map[string]any{"feature": map[string]any{"id": 1, "name": name, "type": "STANDARD"}, "enabled": enabled, "feature_segment": nil, "feature_state_value": value}
}

func identityOverride(identifier, uuid string, states ...map[string]any) map[string]any {
	return map[string]any{"identifier": identifier, "identity_uuid": uuid, "identity_features": states}
}

func testDocument(states []map[string]any, overrides []map[string]any) map[string]any {
	if states == nil {
		states = []map[string]any{}
	}
	if overrides == nil {
		overrides = []map[string]any{}
	}
	return map[string]any{"name": "stage", "use_identity_overrides_in_local_eval": true, "feature_states": states, "identity_overrides": overrides}
}

// startEnv is a link over the CI baseline and the real registry, with the
// fake Admin API and a fake clock.
type startEnv struct {
	l     *Link
	s     *startup
	admin *fakeAdmin
	clock *startClock
	st    *state.State
	reg   *registry.Registry
	logs  *syncBuffer
	ctx   context.Context
}

type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func newStartEnv(t *testing.T) *startEnv { return newStartEnvHTTP(t, nil) }

// newStartEnvHTTP is newStartEnv with Deps.HTTPClient set (nil: the default).
func newStartEnvHTTP(t *testing.T, hc *http.Client) *startEnv {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..")
	base, err := baseline.Load(filepath.Join(root, "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.LoadFile(filepath.Join(root, "registry.yaml"), base)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := state.New(base, reg, state.Options{Epoch: "e1", Served: base.Order, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	a := newFakeAdmin(t)
	clk := newStartClock()
	logs := &syncBuffer{}
	cfg := testConfig(a.f)
	l, err := New(cfg, Deps{State: st, Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: clk.Now, Sleep: clk.Sleep, HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	s := l.start.(*startup)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if done := s.cleanupDone(); done != nil {
			<-done
		}
		if done := s.optionsDone(); done != nil {
			<-done
		}
	})
	return &startEnv{l: l, s: s, admin: a, clock: clk, st: st, reg: reg, logs: logs, ctx: ctx}
}

// waitStatus polls Status until one snapshot satisfies ok.
func waitStatus(t *testing.T, l *Link, ok func(Status) bool, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := l.Status()
		if ok(st) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v, want %s", st, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// allFeaturesExist makes every registry flag an existing feature.
func (e *startEnv) allFeaturesExist() {
	e.admin.mu.Lock()
	defer e.admin.mu.Unlock()
	for i, p := range e.reg.Policies() {
		e.admin.features = append(e.admin.features, fsFeature{ID: i + 1, Name: p.Key})
	}
}

func adminRequests(f *flagsmithtest.Fake) int {
	n := 0
	for _, r := range f.Requests() {
		if !strings.HasSuffix(r.Path, "environment-document/") {
			n++
		}
	}
	return n
}

var (
	kBackend = Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
	kCapture = Key{Set: "agent", Flag: "decision.capture_content", Scope: ScopeStack}
	kTick    = Key{Set: "agent", Flag: "experiment_tick_seconds", Scope: ScopeStack}
)

// --- tests -------------------------------------------------------------------

// UC: UC-054 A3 BR-007
func TestStartPingAndChecks(t *testing.T) {
	t.Run("UC-054/A3 an unreachable document fails Start before any Admin call", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.f.Fail(http.StatusBadGateway)
		err := e.s.Start(e.ctx)
		if Reason(err) != "http_502" {
			t.Fatalf("Start %v, want http_502", err)
		}
		if n := adminRequests(e.admin.f); n != 0 {
			t.Fatalf("%d Admin requests before the ping succeeded, want 0", n)
		}
	})
	t.Run("UC-054/A3 a project mismatch keeps the link offline", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.env["project"] = 99
		if err := e.s.Start(e.ctx); Reason(err) != ReasonProjectMismatch {
			t.Fatalf("Start %v, want project_mismatch", err)
		}
	})
	t.Run("UC-054/A3 identity overrides off in local evaluation keep the link offline", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.env["use_identity_overrides_in_local_eval"] = false
		if err := e.s.Start(e.ctx); Reason(err) != ReasonIdentityOverridesDisabled {
			t.Fatalf("Start %v, want identity_overrides_disabled", err)
		}
	})
	t.Run("UC-054/A3 a project without edge identities keeps the link offline (edge_required) before any write", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.edgeStatus = []int{400}
		if err := e.s.Start(e.ctx); Reason(err) != ReasonEdgeRequired {
			t.Fatalf("Start %v, want edge_required", err)
		}
		// The check is a read right after the ids: no feature or tag is
		// created (project-level writes) in a project that cannot link.
		if n := len(e.admin.f.RequestsTo(http.MethodPost, "")); n != 0 {
			t.Fatalf("%d POSTs before edge_required, want 0 (posts %v)", n, e.admin.postNames())
		}
		if e.s.cleanupDone() != nil {
			t.Fatal("the identity cleanup was scheduled by a failed start")
		}
	})
	t.Run("UC-054/A3 through Run: offline with the reason, online once Start passes", func(t *testing.T) {
		e := newStartEnv(t)
		e.allFeaturesExist()
		e.admin.env["project"] = 99
		// The real start with test doubles for the in and out sides
		// (link_test.go): this case does not depend on #2589 / #2590.
		e.l.in, e.l.out = &scriptedIn{}, &idleOut{}
		// The first backoff holds until the project is fixed, so the link
		// stays offline while the test reads it; one snapshot must show both
		// the mode and the reason.
		backoff := make(chan struct{})
		e.clock.mu.Lock()
		e.clock.backoff = backoff
		e.clock.mu.Unlock()
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		done := runLink(ctx, e.l)
		waitStatus(t, e.l, func(st Status) bool {
			return st.Link == ModeOffline && st.OfflineReason == ReasonProjectMismatch
		}, "offline project_mismatch")
		e.admin.mu.Lock()
		e.admin.env["project"] = testProjectID
		e.admin.mu.Unlock()
		close(backoff)
		waitMode(t, e.l, ModeOnline)
		if st := e.l.Status(); st.Environment != "stage" {
			t.Fatalf("status %+v, want environment stage", st)
		}
		cancel()
		waitDone(t, done)
	})
}

// UC: UC-054 A3 BR-002
func TestStartCreatesFeatures(t *testing.T) {
	t.Run("UC-054/A3 BR-002 only missing features are created, tagged, never with initial_value", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "decision.capture_content"}, {ID: 2, Name: "experiment_tick_seconds"}}
		page1 := make([]fsTag, 10)
		for i := range page1 {
			page1[i] = fsTag{ID: 100 + i, Label: fmt.Sprintf("other%d", i)}
		}
		e.admin.tagPages = [][]fsTag{page1, {{ID: 77, Label: "agent"}}}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		posts := e.admin.postNames()
		if want := len(e.reg.Policies()) - 2; len(posts) != want {
			t.Fatalf("%d features created, want %d (registry rows minus the 2 existing)", len(posts), want)
		}
		for _, name := range posts {
			if name == "decision.capture_content" || name == "experiment_tick_seconds" {
				t.Fatalf("existing feature %s created again", name)
			}
			if e.reg.Get(e.st.Baseline().SetOf(name), name) == nil {
				t.Fatalf("%s has no registry row (read-only baseline flags are not features)", name)
			}
		}
		e.admin.mu.Lock()
		for _, body := range e.admin.posts {
			if _, has := body["initial_value"]; has {
				t.Fatalf("create sent initial_value: %v", body)
			}
			if body["type"] != "STANDARD" || body["default_enabled"] != false {
				t.Fatalf("create body %v", body)
			}
			name := body["name"].(string)
			p := e.reg.Get(e.st.Baseline().SetOf(name), name)
			if body["description"] != p.Description {
				t.Fatalf("%s description %v, want the registry's %q", name, body["description"], p.Description)
			}
			tags, _ := body["tags"].([]any)
			if p.Set == "agent" && (len(tags) != 1 || tags[0] != float64(77)) {
				t.Fatalf("%s tags %v, want the existing agent tag 77 (page 2)", name, tags)
			}
		}
		for _, tp := range e.admin.tagPosts {
			if tp["label"] == "agent" {
				t.Fatal("the agent tag was created again (it is on the second tag page)")
			}
		}
		e.admin.mu.Unlock()
		ids := e.l.IDs()
		ids.Lock()
		defer ids.Unlock()
		if len(ids.FeatureID) != len(e.reg.Policies()) || ids.FeatureID["decision.capture_content"] != 1 {
			t.Fatalf("FeatureID has %d entries (capture_content %d), want every registry flag", len(ids.FeatureID), ids.FeatureID["decision.capture_content"])
		}
		if ids.ProjectID != testProjectID || ids.EnvironmentID != testEnvID || ids.EnvironmentName != "stage" {
			t.Fatalf("ids %d %d %q", ids.ProjectID, ids.EnvironmentID, ids.EnvironmentName)
		}
	})
	t.Run("UC-054/A3 a case-different or archived feature is refused, never created or used", func(t *testing.T) {
		e := newStartEnv(t)
		e.allFeaturesExist()
		e.admin.mu.Lock()
		for i, f := range e.admin.features {
			switch f.Name {
			case "decision.backend":
				e.admin.features[i].Name = "Decision.Backend"
			case "rollout_enabled":
				e.admin.features[i].IsArchived = true
			}
		}
		e.admin.mu.Unlock()
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if posts := e.admin.postNames(); len(posts) != 0 {
			t.Fatalf("created %v, want nothing", posts)
		}
		if r, side := e.l.Links().Refusal(kBackend); r != ReasonFeatureNameCase || side != SideRemote {
			t.Fatalf("decision.backend refusal %q %q, want feature_name_case", r, side)
		}
		if r, _ := e.l.Links().Refusal(Key{Set: "agent", Flag: "rollout_enabled", Scope: ScopeStack}); r != ReasonFeatureArchived {
			t.Fatalf("rollout_enabled refusal %q, want feature_archived", r)
		}
		ids := e.l.IDs()
		ids.Lock()
		defer ids.Unlock()
		if _, has := ids.FeatureID["decision.backend"]; has {
			t.Fatal("the case-different feature's id is used")
		}
		if _, has := ids.FeatureID["rollout_enabled"]; has {
			t.Fatal("the archived feature's id is used")
		}
	})
	t.Run("UC-054/A3 create errors: exists re-lists, 400 refuses, 5xx thrice refuses, the start goes on", func(t *testing.T) {
		e := newStartEnv(t)
		e.allFeaturesExist()
		e.admin.mu.Lock()
		keep := e.admin.features[:0]
		for _, f := range e.admin.features {
			switch f.Name {
			case "decision.backend", "decision.capture_content", "experiment_tick_seconds", "enabled":
				continue
			}
			keep = append(keep, f)
		}
		e.admin.features = keep
		e.admin.create = func(name string, _ int) (int, string) {
			switch name {
			case "decision.backend":
				// A parallel start created it meanwhile.
				e.admin.mu.Lock()
				e.admin.features = append(e.admin.features, fsFeature{ID: 555, Name: name})
				e.admin.mu.Unlock()
				return 400, "Feature with that name already exists for this project."
			case "decision.capture_content":
				return 400, "Feature name must match the project's regex"
			case "experiment_tick_seconds":
				return 503, "busy"
			}
			return 0, ""
		}
		e.admin.mu.Unlock()
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatalf("Start %v: per-feature errors must not fail the start", err)
		}
		ids := e.l.IDs()
		ids.Lock()
		backendID, enabledID := ids.FeatureID["decision.backend"], ids.FeatureID["enabled"]
		ids.Unlock()
		if backendID != 555 || enabledID == 0 {
			t.Fatalf("ids backend %d enabled %d, want the re-listed 555 and a created id", backendID, enabledID)
		}
		if r, _ := e.l.Links().Refusal(kCapture); r != "feature_create_400" {
			t.Fatalf("capture_content refusal %q, want feature_create_400", r)
		}
		if r, _ := e.l.Links().Refusal(kTick); r != ReasonFeatureCreateFailed {
			t.Fatalf("tick refusal %q, want feature_create_failed", r)
		}
		e.admin.mu.Lock()
		tries := e.admin.attempts["experiment_tick_seconds"]
		e.admin.mu.Unlock()
		if tries != transientAttempts {
			t.Fatalf("5xx create tried %d times, want %d", tries, transientAttempts)
		}
	})
	t.Run("UC-054/A3 a re-run after a failed start creates only what is still missing and clears its refusals", func(t *testing.T) {
		ca := &cancelAfter{}
		e := newStartEnvHTTP(t, &http.Client{Transport: ca})
		e.admin.f.SetDocument(testDocument([]map[string]any{fstate("experiment_tick_seconds", true, 10)}, nil), "")
		e.admin.create = func(name string, attempt int) (int, string) {
			if name == "experiment_tick_seconds" && attempt <= transientAttempts {
				return 503, "busy"
			}
			return 0, ""
		}
		// The first run is cancelled right after its last feature create was
		// answered: the features step ended, the start did not.
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		ca.arm(cancel, isFeatureCreate, len(e.reg.Policies())-1+transientAttempts)
		if err := e.s.Start(ctx); err == nil {
			t.Fatal("Start passed although it was cancelled")
		}
		first := len(e.admin.postNames())
		if first != len(e.reg.Policies())-1 {
			t.Fatalf("first start created %d, want %d (all but the failing one)", first, len(e.reg.Policies())-1)
		}
		if r, _ := e.l.Links().Refusal(kTick); r != ReasonFeatureCreateFailed {
			t.Fatalf("tick refusal %q after the first run, want feature_create_failed", r)
		}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if posts := e.admin.postNames(); len(posts) != first+1 || posts[first] != "experiment_tick_seconds" {
			t.Fatalf("re-run created %v, want only experiment_tick_seconds", posts[first:])
		}
		if r, side := e.l.Links().Refusal(kTick); r != "" {
			t.Fatalf("tick still refused %q %q after the re-run created it", r, side)
		}
		if _, _, refused := e.l.Links().Counts(); refused != 0 {
			t.Fatalf("%d keys counted refused after the re-run, want 0", refused)
		}
	})
	t.Run("UC-054/A3 a re-run that finds a fixed feature clears its refusal", func(t *testing.T) {
		ca := &cancelAfter{}
		e := newStartEnvHTTP(t, &http.Client{Transport: ca})
		e.allFeaturesExist()
		e.admin.mu.Lock()
		for i, f := range e.admin.features {
			if f.Name == "decision.backend" {
				e.admin.features[i].IsArchived = true
			}
		}
		e.admin.mu.Unlock()
		e.admin.f.SetDocument(testDocument([]map[string]any{fstate("experiment_tick_seconds", true, 10)}, nil), "")
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		ca.arm(cancel, isFeatureList, 1) // the features step's only call
		if err := e.s.Start(ctx); err == nil {
			t.Fatal("Start passed although it was cancelled")
		}
		if r, _ := e.l.Links().Refusal(kBackend); r != ReasonFeatureArchived {
			t.Fatalf("backend refusal %q, want feature_archived", r)
		}
		// A person unarchives the feature before the re-run.
		e.admin.mu.Lock()
		for i, f := range e.admin.features {
			if f.Name == "decision.backend" {
				e.admin.features[i].IsArchived = false
			}
		}
		e.admin.mu.Unlock()
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if r, _ := e.l.Links().Refusal(kBackend); r != "" {
			t.Fatalf("backend still refused %q after the re-run found it usable", r)
		}
	})
	t.Run("UC-054/A3 a local refusal survives a re-run: only remote start refusals are cleared", func(t *testing.T) {
		e := newStartEnv(t)
		e.allFeaturesExist()
		e.l.Links().SetInSync(kBackend, false) // creates the entry Refuse needs
		e.l.Links().SetInSync(kTick, false)
		e.l.Links().Refuse(kBackend, SideLocal, ReasonFeatureNameCase)
		e.l.Links().Refuse(kTick, SideRemote, ReasonFeatureNameCase)
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if r, side := e.l.Links().Refusal(kBackend); r != ReasonFeatureNameCase || side != SideLocal {
			t.Fatalf("local refusal now %q %q, want it kept by the re-run", r, side)
		}
		if r, _ := e.l.Links().Refusal(kTick); r != "" {
			t.Fatalf("remote start refusal %q survived the re-run that found the feature", r)
		}
	})
}

// UC: UC-054 A3 BR-006
// Flagsmith wins at a restart (FR-215, #2621): the start sequence reads no
// authorship and seeds no last state, whoever made the document's values, so
// the start reconcile applies them all (in_start_test.go). It only remembers
// the identity uuids, in its final step (O7).
func TestStartSeedsNothing(t *testing.T) {
	doc := testDocument(
		[]map[string]any{
			fstate("decision.backend", true, "claude"),  // persist: yes
			fstate("experiment_tick_seconds", true, 10), // a plain flag
			fstate("enabled", true, "on"),               // a kill switch
		},
		[]map[string]any{
			identityOverride("game:g1", "u-g1", fstate("pacing_profile", true, "calm")),
		},
	)
	// The audit log names an API key for every value: the restart rule of
	// #2587 read it, cleared those values and seeded their remote state.
	apiKeyAudit := func(a *fakeAdmin) {
		a.f.Handle("GET "+flagsmithtest.APIPath+"projects/{pid}/audit/{$}", func(w http.ResponseWriter, _ *http.Request) {
			flagsmithtest.WriteJSON(w, 200, map[string]any{"next": nil, "results": []any{
				map[string]any{"log": "Flag state updated for feature: decision.backend", "is_system_event": false, "author": nil},
				map[string]any{"log": "Flag state updated for feature: experiment_tick_seconds", "is_system_event": false, "author": nil},
				map[string]any{"log": "Flag state updated for feature: enabled", "is_system_event": false, "author": nil},
				map[string]any{"log": "Feature override created for feature 'pacing_profile' and identity 'game:g1'", "is_system_event": false, "author": nil},
			}})
		})
	}
	uuidOf := func(e *startEnv, identifier string) string {
		ids := e.l.IDs()
		ids.Lock()
		defer ids.Unlock()
		return ids.IdentityUUID[identifier]
	}

	t.Run("UC-054/A3 BR-006 FR-215 no authorship, no seeded remote, whoever made the values; identity uuids remembered", func(t *testing.T) {
		e := newStartEnv(t)
		e.allFeaturesExist()
		apiKeyAudit(e.admin)
		e.admin.f.SetDocument(doc, "")
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if n := len(e.admin.f.RequestsTo(http.MethodGet, flagsmithtest.APIPath+"projects/42/audit/")); n != 0 {
			t.Fatalf("UC-054/A3 the start read the audit log %d times, want 0", n)
		}
		for _, k := range e.l.Links().Keys() {
			if v, ok := e.l.Links().Remote(k); ok {
				t.Fatalf("UC-054/BR-006 %v seeded with %+v at start, want no last state (Flagsmith wins)", k, v)
			}
		}
		if u := uuidOf(e, "game:g1"); u != "u-g1" {
			t.Fatalf("identity uuid of game:g1 %q, want u-g1", u)
		}
	})
	t.Run("UC-054/A3 the final step runs only for a start that ended (O7, deterministic)", func(t *testing.T) {
		ca := &cancelAfter{}
		e := newStartEnvHTTP(t, &http.Client{Transport: ca})
		e.allFeaturesExist()
		e.admin.f.SetDocument(doc, "")
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()
		ca.arm(cancel, isFeatureList, 1) // answered, then cancelled
		if err := e.s.Start(ctx); err == nil {
			t.Fatal("Start passed although it was cancelled after its last Admin call")
		}
		if u := uuidOf(e, "game:g1"); u != "" {
			t.Fatalf("identity uuid %q remembered by a start that did not end", u)
		}
		if e.s.cleanupDone() != nil {
			t.Fatal("the stale identity cleanup was scheduled by a start that did not end")
		}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if u := uuidOf(e, "game:g1"); u != "u-g1" {
			t.Fatalf("identity uuid of game:g1 %q after the re-run, want u-g1", u)
		}
	})
}

// cancelAfter answers every request for real; once armed, it reads the whole
// response of the n-th request match accepts, cancels the start's context and
// only then hands that (complete) response back: answered, then cancelled.
type cancelAfter struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	match  func(*http.Request) bool
	n      int
}

func (c *cancelAfter) arm(cancel context.CancelFunc, match func(*http.Request) bool, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel, c.match, c.n = cancel, match, n
}

func (c *cancelAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	c.mu.Lock()
	fire := false
	if err == nil && c.match != nil && c.match(req) {
		c.n--
		fire = c.n == 0
	}
	cancel := c.cancel
	c.mu.Unlock()
	if !fire {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	cancel()
	return resp, nil
}

func isFeatureList(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/42/features/")
}

func isFeatureCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/42/features/")
}

// UC: UC-054 A3
func TestStartStaleIdentities(t *testing.T) {
	t.Run("UC-054/A3 start never deletes; after the grace only unregistered game identities go (O1)", func(t *testing.T) {
		e := newStartEnv(t)
		e.st.SetObserver(e.l)
		e.allFeaturesExist()
		e.admin.identities = [][]edgeIdentity{
			{{IdentityUUID: "u1", Identifier: "game:g1"}, {IdentityUUID: "u2", Identifier: "game:g1:S1"}, {IdentityUUID: "u3", Identifier: "game:dead"}},
			{{IdentityUUID: "u4", Identifier: "game:dead:S9"}, {IdentityUUID: "u5", Identifier: "game:g1:S2"}, {IdentityUUID: "u6", Identifier: "game:late"}, {IdentityUUID: "u7", Identifier: "game:"}},
		}
		// Game "late" registers after its identity was found stale and before
		// its delete is sent (the paced wait right after the delete of u5).
		e.admin.onDelete = func(uuid string) {
			if uuid != "u5" {
				return
			}
			e.clock.onNextSleep(func() {
				if _, err := e.st.RegisterGame(state.Game{ID: "late", CoordinatorInstance: "c1", Kind: "real"}); err != nil {
					t.Error(err)
				}
			})
		}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		// The background option sync (#2709) sleeps on the same clock: let it
		// end first, so the one-shot hook above fires at the cleanup's paced
		// wait and not at an option request's.
		if done := e.s.optionsDone(); done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("option sync did not end")
			}
		}
		// Nothing is deleted at start nor before the grace ends, while no game
		// is registered yet.
		time.Sleep(200 * time.Millisecond)
		if n := len(e.admin.f.RequestsTo(http.MethodDelete, "")); n != 0 {
			t.Fatalf("%d deletes before the grace, want 0", n)
		}
		// The coordinator re-registers its live game after the start.
		if _, err := e.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "real", Serials: []string{"S1"}}); err != nil {
			t.Fatal(err)
		}
		e.l.Links().SetRemote(Key{Set: "interventions", Flag: "pacing_profile", Scope: "game:dead"}, Value{Present: true})
		close(e.clock.release)
		select {
		case <-e.s.cleanupDone():
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup did not end")
		}
		e.admin.mu.Lock()
		got := strings.Join(e.admin.deletes, ",")
		e.admin.mu.Unlock()
		if got != "u3,u4,u5,u7" {
			t.Fatalf("deleted %s, want u3,u4,u5,u7 (game:dead, game:dead:S9, the dropped serial, the malformed id)", got)
		}
		ids := e.l.IDs()
		ids.Lock()
		kept := ids.IdentityUUID["game:g1"] == "u1" && ids.IdentityUUID["game:g1:S1"] == "u2" && ids.IdentityUUID["game:late"] == "u6"
		_, deadKept := ids.IdentityUUID["game:dead"]
		ids.Unlock()
		if !kept || deadKept {
			t.Fatal("identity uuids: live ones not kept or a deleted one kept")
		}
		if len(e.l.Links().Keys()) != 0 {
			t.Fatalf("keys of a deleted identity left: %v", e.l.Links().Keys())
		}
	})
}

// waitCleanup waits for the scheduled cleanup to end.
func (e *startEnv) waitCleanup(t *testing.T) {
	t.Helper()
	select {
	case <-e.s.cleanupDone():
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not end")
	}
}

func (e *startEnv) deleted() string {
	e.admin.mu.Lock()
	defer e.admin.mu.Unlock()
	return strings.Join(e.admin.deletes, ",")
}

func (e *startEnv) sleepsOf(d time.Duration) int {
	n := 0
	for _, s := range e.clock.got() {
		if s == d {
			n++
		}
	}
	return n
}

// UC: UC-054 A3
func TestStartStaleIdentityTrigger(t *testing.T) {
	t.Run("UC-054/A3 after the grace the cleanup still waits for the first registration (whichever is later)", func(t *testing.T) {
		e := newStartEnv(t)
		e.st.SetObserver(e.l)
		e.allFeaturesExist()
		e.admin.identities = [][]edgeIdentity{{{IdentityUUID: "u1", Identifier: "game:g1"}, {IdentityUUID: "u3", Identifier: "game:dead"}}}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		close(e.clock.release)
		time.Sleep(200 * time.Millisecond)
		select {
		case <-e.s.cleanupDone():
			t.Fatal("cleanup ended before any game registered")
		default:
		}
		if n := len(e.admin.f.RequestsTo(http.MethodDelete, "")); n != 0 {
			t.Fatalf("%d deletes before the first registration, want 0 (every live game would be deleted)", n)
		}
		if _, err := e.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "real"}); err != nil {
			t.Fatal(err)
		}
		e.waitCleanup(t)
		if got := e.deleted(); got != "u3" {
			t.Fatalf("deleted %s, want u3", got)
		}
	})
}

// UC: UC-054 A3
func TestStartStaleIdentityRetry(t *testing.T) {
	t.Run("UC-054/A3 a listing error or a 5xx delete is retried every 60 s", func(t *testing.T) {
		e := newStartEnv(t)
		e.st.SetObserver(e.l)
		e.allFeaturesExist()
		e.admin.identities = [][]edgeIdentity{{{IdentityUUID: "u3", Identifier: "game:dead"}, {IdentityUUID: "u4", Identifier: "game:dead2"}}}
		// Pass 1: the listing fails (every transient attempt); pass 2: the
		// delete of u4 fails (every attempt); pass 3 deletes it.
		e.admin.listStatus = []int{500, 500, 500}
		e.admin.delStatus["u4"] = []int{503, 503, 503}
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "real"}); err != nil {
			t.Fatal(err)
		}
		close(e.clock.release)
		e.waitCleanup(t)
		if got := e.deleted(); got != "u3,u4" {
			t.Fatalf("deleted %s, want u3,u4", got)
		}
		if n := e.sleepsOf(staleCleanupRetryEvery); n != 2 {
			t.Fatalf("%d retry waits of %s, want 2", n, staleCleanupRetryEvery)
		}
	})
	t.Run("UC-054/A3 a cleanup failing every time stops after 10 retries with a warning", func(t *testing.T) {
		e := newStartEnv(t)
		e.st.SetObserver(e.l)
		e.allFeaturesExist()
		e.admin.listFail = true
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "real"}); err != nil {
			t.Fatal(err)
		}
		close(e.clock.release)
		e.waitCleanup(t)
		if n := e.sleepsOf(staleCleanupRetryEvery); n != staleCleanupRetries {
			t.Fatalf("%d retry waits, want %d", n, staleCleanupRetries)
		}
		lists := 0
		for _, r := range e.admin.f.RequestsTo(http.MethodGet, flagsmithtest.APIPath+"environments/envkey123/edge-identities/") {
			if strings.Contains(r.Query, "page_size=100") && strings.Contains(r.Query, "q=game") {
				lists++
			}
		}
		if want := (staleCleanupRetries + 1) * transientAttempts; lists != want {
			t.Fatalf("%d listing requests, want %d (one pass and %d retries, %d attempts each)", lists, want, staleCleanupRetries, transientAttempts)
		}
		if !strings.Contains(e.logs.String(), "flagsmith stale identities left") {
			t.Fatalf("no warning after the last retry:\n%s", e.logs.String())
		}
	})
}

// UC: UC-054 A3
func TestStartPacing(t *testing.T) {
	t.Run("UC-054/A3 a 429 waits its Retry-After; calls are paced at PUSH_RATE; no key in the logs", func(t *testing.T) {
		e := newStartEnv(t)
		e.allFeaturesExist()
		e.admin.mu.Lock()
		e.admin.features = e.admin.features[1:] // one feature to create
		e.admin.tags429 = 1
		e.admin.mu.Unlock()
		e.admin.f.SetDocument(testDocument([]map[string]any{fstate("experiment_tick_seconds", true, 10)}, nil), "")
		if err := e.s.Start(e.ctx); err != nil {
			t.Fatal(err)
		}
		sleeps := e.clock.got()
		var three, paced int
		interval := time.Second / DefaultPushRate
		for _, d := range sleeps {
			switch d {
			case 3 * time.Second:
				three++
			case interval:
				paced++
			}
		}
		if three != 1 {
			t.Fatalf("sleeps %v, want one Retry-After wait of 3 s", sleeps)
		}
		calls := adminRequests(e.admin.f)
		if paced < calls-2 {
			t.Fatalf("%d paced waits for %d Admin calls, want one per call at %s", paced, calls, interval)
		}
		logs := e.logs.String()
		for _, secret := range []string{"secretserverkey", "secretadminkey", "envkey123"} {
			if strings.Contains(logs, secret) {
				t.Fatalf("log names a key: %s", logs)
			}
		}
	})
}
