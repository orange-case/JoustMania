package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
	"github.com/joustmania/flag-service/telemetry"
)

// The flags the tests push; each gets a Flagsmith feature id.
var testFlags = []struct{ set, key string }{
	{"agent", "decision.backend"},
	{"agent", "decision.min_interval_ms"},
	{"agent", "decision.act_confidence_threshold"},
	{"agent", "llm.eligible_game_kinds"},
	{"agent", "decision.prompt_profile"},
	{"interventions", "music_tempo_override"},
	{"interventions", "global_sensitivity_override"},
	{"interventions", "player_sensitivity_factor"},
	{"interventions", "audio_cue"},
	{"game", "windows"},
}

// testClock is the link's clock and sleep: a Sleep moves the clock, so a test
// reads the waits without waiting.
type testClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *testClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// outEnv is a link wired to the fake Flagsmith, with the real registry and
// baseline, a fake clock and recorded spans and metrics.
type outEnv struct {
	t      *testing.T
	fake   *flagsmithtest.Fake
	link   *Link
	out    *out
	clk    *testClock
	rec    *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	links  *telemetry.Links
	mu     sync.Mutex
	on     map[string]http.HandlerFunc
	// hook, when set, answers a request first (true: answered).
	hook func(w http.ResponseWriter, r *http.Request) bool
	ver  uint64
}

func loadBaseReg(t *testing.T) (*baseline.Baseline, *registry.Registry) {
	t.Helper()
	base, err := baseline.Load(filepath.Join("..", "..", "flagd", "ci"), baseline.DefaultFiles)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.LoadFile(filepath.Join("..", "registry.yaml"), base)
	if err != nil {
		t.Fatal(err)
	}
	return base, reg
}

func newOutEnv(t *testing.T) *outEnv {
	t.Helper()
	base, reg := loadBaseReg(t)
	return newOutEnvWith(t, Deps{Registry: reg, Baseline: base})
}

func newOutEnvWith(t *testing.T, d Deps) *outEnv {
	t.Helper()
	fake := flagsmithtest.New(t)
	e := &outEnv{t: t, fake: fake, clk: &testClock{now: time.Unix(1_700_000_000, 0)}, on: map[string]http.HandlerFunc{}}
	e.rec = tracetest.NewSpanRecorder()
	e.reader = sdkmetric.NewManualReader()
	e.links = &telemetry.Links{}
	d.Logger = quietLogger()
	d.Now, d.Sleep = e.clk.Now, e.clk.Sleep
	d.Tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(e.rec)).Tracer("t")
	d.Meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(e.reader)).Meter("t")
	d.Links = e.links
	cfg := Config{
		Enabled:        true,
		APIURL:         fake.URL(),
		DocumentURL:    fake.URL(),
		EnvironmentKey: "envkey",
		AdminKey:       "adm",
		ServerKey:      "ser",
		ProjectID:      1,
		PushRate:       5,
		Timeout:        2 * time.Second,
	}
	l, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	e.link = l
	l.connected.Store(true) // the out side alone: every change is made online
	e.out = l.out.(*out)
	for i, f := range testFlags {
		l.IDs().FeatureID[f.key] = 100 + i
	}
	fake.Handle("/api/", e.route)
	return e
}

// fid is the feature id of a test flag.
func (e *outEnv) fid(key string) int { return e.link.IDs().FeatureID[key] }

func (e *outEnv) setOn(method, path string, h http.HandlerFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.on[method+" "+path] = h
}

func (e *outEnv) futurePath(key string) string {
	return "/api/__future__/environments/envkey/features/" + strconv.Itoa(e.fid(key)) + "/"
}

const apiEnv = "/api/v1/environments/envkey/"

func uuidOf(identifier string) string { return "uuid_" + strings.ReplaceAll(identifier, ":", "_") }

// route is the fake Admin API: every call succeeds unless a test set a
// handler for its method and path.
func (e *outEnv) route(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	h := e.on[r.Method+" "+r.URL.Path]
	hook := e.hook
	e.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	if hook != nil && hook(w, r) {
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/api/__future__/"):
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	case p == apiEnv+"edge-identities-featurestates" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case p == apiEnv+"edge-identities-featurestates":
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	case p == apiEnv+"edge-identities/" && r.Method == http.MethodPost:
		var b struct {
			Identifier string `json:"identifier"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		flagsmithtest.WriteJSON(w, http.StatusCreated, json.RawMessage(`{"identity_uuid":"`+uuidOf(b.Identifier)+`"}`))
	case p == apiEnv+"edge-identities/" && r.Method == http.MethodGet:
		q := strings.Trim(r.URL.Query().Get("q"), `"`)
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{"results":[{"identity_uuid":"`+uuidOf(q)+`","identifier":"`+q+`"}]}`))
	case strings.HasPrefix(p, apiEnv+"edge-identities/") && (strings.HasSuffix(p, "/list-traits/") || strings.HasSuffix(p, "/edge-featurestates/")) && r.Method == http.MethodGet:
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`[]`))
	case strings.HasPrefix(p, apiEnv+"edge-identities/") && strings.HasSuffix(p, "/update-traits/") && r.Method == http.MethodPut:
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	case strings.HasPrefix(p, apiEnv+"edge-identities/") && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case p == apiEnv+"featurestates/" && r.Method == http.MethodGet:
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{"results":[{"id":`+r.URL.Query().Get("feature")+`0}]}`))
	case strings.HasPrefix(p, apiEnv+"featurestates/") && r.Method == http.MethodPut:
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	default:
		http.NotFound(w, r)
	}
}

// change builds an observer Change the way the state reports it.
func (e *outEnv) change(kind state.ChangeKind, set, flag, scopeName string, sc state.Scope, variant, raw string) state.Change {
	e.ver++
	c := state.Change{Kind: kind, Set: set, Flag: flag, Scope: sc, ScopeName: scopeName, DeclaredVariant: variant, Writer: "menu", At: e.clk.Now(), Version: e.ver}
	if raw != "" {
		c.Value = json.RawMessage(raw)
	}
	return c
}

// stack builds the change of a stack value.
func (e *outEnv) stack(flag, variant, raw string) state.Change {
	set := "agent"
	return e.change(state.ChangeValue, set, flag, registry.ScopeStack, state.Scope{}, variant, raw)
}

func gameOf(id, kind string, serials ...string) *state.Game {
	return &state.Game{ID: id, Kind: kind, CoordinatorInstance: "c1", Serials: serials}
}

func (e *outEnv) registered(g, prev *state.Game) state.Change {
	e.ver++
	return state.Change{Kind: state.ChangeGameRegistered, ScopeName: registry.ScopeGame, Scope: state.Scope{GameID: g.ID}, Game: g, Prev: prev, At: e.clk.Now(), Version: e.ver}
}

func (e *outEnv) ended(g *state.Game) state.Change {
	e.ver++
	return state.Change{Kind: state.ChangeGameEnded, ScopeName: registry.ScopeGame, Scope: state.Scope{GameID: g.ID}, Game: g, At: e.clk.Now(), Version: e.ver}
}

// idle: nothing is pending and no identity or controller work is left.
func (e *outEnv) idle() bool {
	pending, _, _ := e.link.Links().Counts()
	_, more := e.out.ids.Peek()
	return pending == 0 && !more && !e.link.ctrl.busy()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// runner is a running out.Run.
type runner struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error
}

func (e *outEnv) start() *runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{t: e.t, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- e.out.Run(ctx) }()
	e.t.Cleanup(cancel)
	return r
}

// stop ends the Run and returns its error.
func (r *runner) stop() error {
	r.t.Helper()
	r.cancel()
	return r.wait()
}

// wait returns the error the Run ended with by itself.
func (r *runner) wait() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		r.t.Fatal("out.Run did not return")
		return nil
	}
}

// drain runs the out side until it is idle, then stops it.
func (e *outEnv) drain() {
	e.t.Helper()
	r := e.start()
	waitFor(e.t, "the out side to go idle", e.idle)
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		e.t.Fatalf("Run: %v, want context canceled", err)
	}
}

func (e *outEnv) bodies(method, path string) []string {
	var out []string
	for _, r := range e.fake.RequestsTo(method, path) {
		out = append(out, string(r.Body))
	}
	return out
}

func (e *outEnv) count(method, path string) int { return len(e.fake.RequestsTo(method, path)) }

func (e *outEnv) pushSpans() []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range e.rec.Ended() {
		if s.Name() == SpanPush {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// pushes sums flagservice_flagsmith_pushes_total over the data points whose
// reason label is reason.
func (e *outEnv) pushes(reason string) int64 {
	e.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(context.Background(), &rm); err != nil {
		e.t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != MetricPushes || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, found := dp.Attributes.Value("reason"); found && v.AsString() == reason {
					n += dp.Value
				}
			}
		}
	}
	return n
}

// UC: UC-055 BR-001
// NFR: NFR-057
// A local write never waits for Flagsmith: with a Flagsmith that answers
// slowly, every write of the real state returns at once, and the latest value
// waits as one pending entry.
func TestChangedNeverBlocks(t *testing.T) {
	base, reg := loadBaseReg(t)
	st, _, err := state.New(base, reg, state.Options{Epoch: "e1", Served: base.Order, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	e := newOutEnvWith(t, Deps{State: st})
	st.SetObserver(e.link)
	sub, serr := st.Subscribe("")
	if serr != nil {
		t.Fatal(serr)
	}
	t.Cleanup(sub.Close)
	e.fake.Delay(300 * time.Millisecond)
	r := e.start()
	var worst time.Duration
	for i := 0; i < 200; i++ {
		raw := json.RawMessage(strconv.Itoa(1000 + i))
		began := time.Now()
		if _, ferr := st.Put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: raw, Writer: "menu"}); ferr != nil {
			t.Fatalf("UC-055/BR-001 write %d: %v", i, ferr)
		}
		if d := time.Since(began); d > worst {
			worst = d
		}
	}
	if worst > 100*time.Millisecond {
		t.Fatalf("UC-055/BR-001 slowest write %v with Flagsmith answering in 300 ms", worst)
	}
	if pending, _, _ := e.link.Links().Counts(); pending != 1 {
		t.Fatalf("UC-055/BR-001 pending %d, want 1 (the latest value of one key)", pending)
	}
	_ = r.stop()
}

// UC: UC-055 BR-001
// Changes to one key that arrive while a request is in flight are merged: the
// next request carries the latest value.
func TestMergeLatestWins(t *testing.T) {
	e := newOutEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.setOn(http.MethodPatch, e.futurePath("decision.backend"), func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	})
	e.link.StateChanged(e.stack("decision.backend", "rules", `"rules"`))
	r := e.start()
	<-started
	e.link.StateChanged(e.stack("decision.backend", "claude", `"claude"`))
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	close(release)
	waitFor(t, "the out side to go idle", e.idle)
	_ = r.stop()
	got := e.bodies(http.MethodPatch, e.futurePath("decision.backend"))
	if len(got) != 2 {
		t.Fatalf("UC-055/BR-001 %d requests, want 2 (v1, then the merged v3): %v", len(got), got)
	}
	if !strings.Contains(got[1], `"value":"jev"`) {
		t.Fatalf("UC-055/BR-001 second request %s, want the latest value jev", got[1])
	}
	spans := e.pushSpans()
	if len(spans) != 2 {
		t.Fatalf("UC-055/BR-001 %d push spans, want 2", len(spans))
	}
	if n, _ := strconv.Atoi(spanAttr(spans[1], AttrMerged)); n < 2 {
		t.Fatalf("UC-055/BR-001 second push span merged=%s, want at least 2", spanAttr(spans[1], AttrMerged))
	}
}

// UC: UC-055 BR-001
// The sender keeps to FLAGSMITH_PUSH_RATE: 10 keys at 5 per second are 200 ms
// apart.
func TestPushRate(t *testing.T) {
	e := newOutEnv(t)
	for i := 0; i < 10; i++ {
		sc := state.Scope{GameID: fmt.Sprintf("g%d", i)}
		e.link.StateChanged(e.change(state.ChangeValue, "interventions", "music_tempo_override", registry.ScopeGame, sc, "", "1.25"))
	}
	e.drain()
	if n := e.count(http.MethodPut, apiEnv+"edge-identities-featurestates"); n != 10 {
		t.Fatalf("UC-055/BR-001 %d requests, want 10", n)
	}
	slept := e.clk.Slept()
	var total time.Duration
	for _, d := range slept {
		if d != 200*time.Millisecond {
			t.Fatalf("UC-055/BR-001 slept %v, want 200ms steps: %v", d, slept)
		}
		total += d
	}
	if total < 1800*time.Millisecond {
		t.Fatalf("UC-055/BR-001 slept %v in total for 10 requests at 5/s, want at least 1.8s", total)
	}
}

// UC: UC-055 A4
// A 429 is retried after the wait Flagsmith asks for, and the same key is
// confirmed afterwards.
func TestRetryAfter(t *testing.T) {
	e := newOutEnv(t)
	var calls atomic.Int32
	e.setOn(http.MethodPatch, e.futurePath("decision.backend"), func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			flagsmithtest.WriteJSON(w, http.StatusTooManyRequests, json.RawMessage(`{"detail":"throttled"}`))
			return
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	})
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	e.drain()
	found := false
	for _, d := range e.clk.Slept() {
		found = found || d == 7*time.Second
	}
	if !found {
		t.Fatalf("UC-055/A4 slept %v, want a 7s wait", e.clk.Slept())
	}
	k := Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
	if _, ok := e.link.Links().Remote(k); !ok {
		t.Fatal("UC-055/A4 the value was not confirmed after the retry")
	}
	if n := e.pushes(ReasonRateLimited); n != 1 {
		t.Fatalf("UC-055/A4 pushes_total{reason=rate_limited} = %d, want 1", n)
	}
}

// UC: UC-055 A4
// A refusal is recorded with its reason, marks the key out of sync and is not
// retried until the next local change of that key.
func TestRefusalOutOfSync(t *testing.T) {
	e := newOutEnv(t)
	var calls atomic.Int32
	e.setOn(http.MethodPatch, e.futurePath("decision.backend"), func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			flagsmithtest.WriteJSON(w, http.StatusConflict, json.RawMessage(`{"detail":"change requests enabled"}`))
			return
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	})
	e.setOn(http.MethodPatch, e.futurePath("decision.min_interval_ms"), func(w http.ResponseWriter, _ *http.Request) {
		flagsmithtest.WriteJSON(w, http.StatusNotFound, json.RawMessage(`{"detail":"Not found."}`))
	})
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	e.link.StateChanged(e.stack("decision.min_interval_ms", "", `2500`))
	e.drain()
	pending, outOfSync, refused := e.link.Links().Counts()
	if pending != 0 || outOfSync != 2 || refused != 2 {
		t.Fatalf("UC-055/A4 pending %d out_of_sync %d refused %d, want 0, 2, 2", pending, outOfSync, refused)
	}
	kb := Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
	km := Key{Set: "agent", Flag: "decision.min_interval_ms", Scope: ScopeStack}
	if r, side := e.link.Links().Refusal(kb); r != ReasonChangeRequestRequired || side != SideRemote {
		t.Fatalf("UC-055/A4 refusal %q %q, want change_request_required on the remote side", r, side)
	}
	if r, _ := e.link.Links().Refusal(km); r != ReasonNotFound {
		t.Fatalf("UC-055/A4 refusal %q, want not_found", r)
	}
	// Not retried on a wake.
	r := e.start()
	e.out.poke()
	time.Sleep(50 * time.Millisecond)
	_ = r.stop()
	if n := e.count(http.MethodPatch, "/api/__future__/"); n != 2 {
		t.Fatalf("UC-055/A4 %d requests after a wake, want 2 (a refusal is not retried)", n)
	}
	// The next local change of the key sends again and clears the refusal.
	e.link.StateChanged(e.stack("decision.backend", "claude", `"claude"`))
	e.drain()
	if n := e.count(http.MethodPatch, "/api/__future__/"); n != 3 {
		t.Fatalf("UC-055/A4 %d requests after a new change, want 3", n)
	}
	if reason, _ := e.link.Links().Refusal(kb); reason != "" {
		t.Fatalf("UC-055/A4 refusal %q after an accepted push, want none", reason)
	}
}

// UC: UC-055 main step 5
// Flagsmith's confirmation is the last state seen from it: the pushed value is
// the remote value, nothing is pending and the key is in sync, so the change
// never comes back as a new change from Flagsmith.
func TestNoEcho(t *testing.T) {
	e := newOutEnv(t)
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	e.drain()
	k := Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
	got, ok := e.link.Links().Remote(k)
	if !ok || !got.Equal(Value{Present: true, Variant: "jev", Value: json.RawMessage(`"jev"`)}) {
		t.Fatalf("UC-055/main remote %+v %v, want the pushed value", got, ok)
	}
	if pending, outOfSync, refused := e.link.Links().Counts(); pending != 0 || outOfSync != 0 || refused != 0 {
		t.Fatalf("UC-055/main pending %d out_of_sync %d refused %d, want all 0", pending, outOfSync, refused)
	}
}

// UC: UC-055 A3
// A link that drops keeps the unsent value: Run ends with a link-down error,
// the value is still pending, and the next Run sends it.
func TestLinkDownKeepsPending(t *testing.T) {
	e := newOutEnv(t)
	e.fake.Fail(http.StatusServiceUnavailable)
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	r := e.start()
	err := r.wait()
	if !IsLinkDown(err) {
		t.Fatalf("UC-055/A3 Run returned %v, want a link-down error", err)
	}
	if pending, _, _ := e.link.Links().Counts(); pending != 1 {
		t.Fatalf("UC-055/A3 pending %d after the link dropped, want 1", pending)
	}
	e.fake.Fail(0)
	e.drain()
	if n := e.count(http.MethodPatch, e.futurePath("decision.backend")); n != 2 {
		t.Fatalf("UC-055/A3 %d requests, want 2 (the failed one and the retry)", n)
	}
}

// UC: UC-055 A1
// A game and each player are listed when the game registers; a player that
// joins later is listed alone; an identity Flagsmith has already is success.
func TestGameStartListsIdentities(t *testing.T) {
	e := newOutEnv(t)
	e.setOn(http.MethodPost, apiEnv+"edge-identities/", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Identifier string `json:"identifier"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Identifier == "game:g1:B" {
			flagsmithtest.WriteJSON(w, http.StatusBadRequest, json.RawMessage(`{"identifier":["Identity with this identifier already exists."]}`))
			return
		}
		flagsmithtest.WriteJSON(w, http.StatusCreated, json.RawMessage(`{"identity_uuid":"`+uuidOf(b.Identifier)+`"}`))
	})
	first := gameOf("g1", "shadow", "A", "B")
	e.link.StateChanged(e.registered(first, nil))
	e.drain()
	posts := e.bodies(http.MethodPost, apiEnv+"edge-identities/")
	want := []string{`{"identifier":"game:g1"}`, `{"identifier":"game:g1:A"}`, `{"identifier":"game:g1:B"}`}
	if strings.Join(posts, "|") != strings.Join(want, "|") {
		t.Fatalf("UC-055/A1 identity requests %v, want %v", posts, want)
	}
	traits := e.bodies(http.MethodPut, apiEnv+"edge-identities/"+uuidOf("game:g1")+"/update-traits/")
	if len(traits) != 2 || traits[0] != `{"trait_key":"game_kind","trait_value":"shadow"}` || traits[1] != `{"trait_key":"coordinator_instance","trait_value":"c1"}` {
		t.Fatalf("UC-055/A1 game traits %v", traits)
	}
	if n := e.count(http.MethodPut, apiEnv+"edge-identities/uuid_game_g1_A"); n != 0 {
		t.Fatalf("UC-055/A1 %d trait requests for a player, want none", n)
	}
	if pending, _, refused := e.link.Links().Counts(); pending != 0 || refused != 0 {
		t.Fatalf("UC-055/A1 pending %d refused %d, want 0", pending, refused)
	}
	// A third player joins: one identity request, not four.
	e.link.StateChanged(e.registered(gameOf("g1", "shadow", "A", "B", "C"), first))
	e.drain()
	posts = e.bodies(http.MethodPost, apiEnv+"edge-identities/")
	if len(posts) != 4 || posts[3] != `{"identifier":"game:g1:C"}` {
		t.Fatalf("UC-055/A1 identity requests after a join %v, want one more, for game:g1:C", posts)
	}
}

// UC: UC-055 A2 BR-003
// A game that ends removes itself and its players from Flagsmith, drops the
// values it had not sent, and leaves a game whose id starts the same alone.
func TestGameEndDeletesIdentities(t *testing.T) {
	for _, known := range []bool{true, false} {
		name := "the uuids are known"
		if !known {
			name = "the uuids are found by an exact search"
		}
		t.Run("UC-055/A2 "+name, func(t *testing.T) {
			e := newOutEnv(t)
			g1, g10 := gameOf("g1", "shadow", "A"), gameOf("g10", "shadow", "Z")
			e.link.StateChanged(e.registered(g1, nil))
			e.link.StateChanged(e.registered(g10, nil))
			e.drain()
			if !known {
				for _, id := range []string{"game:g1", "game:g1:A"} {
					e.link.forgetIdentityUUID(id)
				}
			}
			before := len(e.fake.Requests())
			sc := state.Scope{GameID: "g1"}
			e.link.StateChanged(e.change(state.ChangeValue, "interventions", "music_tempo_override", registry.ScopeGame, sc, "", "1.25"))
			e.link.StateChanged(e.ended(g1))
			e.drain()
			if n := e.count(http.MethodPut, apiEnv+"edge-identities-featurestates"); n != 0 {
				t.Fatalf("UC-055/A2 %d override requests for a game that ended, want 0", n)
			}
			for _, id := range []string{"game:g1", "game:g1:A"} {
				if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf(id)+"/"); n != 1 {
					t.Fatalf("UC-055/A2 %d deletes of %s, want 1", n, id)
				}
			}
			for _, id := range []string{"game:g10", "game:g10:Z"} {
				if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf(id)+"/"); n != 0 {
					t.Fatalf("UC-055/A2 %d deletes of %s, want 0", n, id)
				}
			}
			if !known {
				gets := 0
				for _, r := range e.fake.Requests()[before:] {
					if r.Method == http.MethodGet && strings.Contains(r.Query, "q=%22game%3Ag1%22") {
						gets++
					}
				}
				if gets != 1 {
					t.Fatalf("UC-055/A2 %d exact searches for game:g1, want 1", gets)
				}
			}
			if pending, _, _ := e.link.Links().Counts(); pending != 0 {
				t.Fatalf("UC-055/A2 pending %d, want 0: the game's values go with it", pending)
			}
		})
	}
}

// UC: UC-055 A2 BR-003
// A game the expiry sweep drops because its end was never reported (UC-053
// A11) leaves Flagsmith like a game whose end was reported: the real state
// reports the end, the out side deletes the game and its players, and a game
// still seen is left alone.
func TestExpiredGameDeletesIdentities(t *testing.T) {
	base, reg := loadBaseReg(t)
	clk := &testClock{now: time.Unix(1_700_000_000, 0)}
	st, _, err := state.New(base, reg, state.Options{Epoch: "e1", Served: base.Order, Now: clk.Now, MaxGameAge: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	e := newOutEnvWith(t, Deps{State: st})
	st.SetObserver(e.link)
	sub, serr := st.Subscribe("")
	if serr != nil {
		t.Fatal(serr)
	}
	t.Cleanup(sub.Close)
	advance := func(d time.Duration) { _ = clk.Sleep(context.Background(), d) }
	if _, ferr := st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"A"}}); ferr != nil {
		t.Fatal(ferr)
	}
	e.drain()
	advance(30 * time.Minute)
	if _, ferr := st.RegisterGame(state.Game{ID: "g2", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"Z"}}); ferr != nil {
		t.Fatal(ferr)
	}
	e.drain()
	advance(31 * time.Minute)
	t.Run("UC-055/A2 a game dropped by the expiry sweep is removed from Flagsmith", func(t *testing.T) {
		ends := st.ExpireGames()
		if len(ends) != 1 || ends[0].GameID != "g1" || ends[0].Reason != state.EndReasonExpired {
			t.Fatalf("UC-055/A2 expired %+v, want g1 only", ends)
		}
		e.drain()
		for _, id := range []string{"game:g1", "game:g1:A"} {
			if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf(id)+"/"); n != 1 {
				t.Fatalf("UC-055/A2 %d deletes of %s, want 1", n, id)
			}
		}
		for _, id := range []string{"game:g2", "game:g2:Z"} {
			if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf(id)+"/"); n != 0 {
				t.Fatalf("UC-055/A2 %d deletes of %s, a game still seen, want 0", n, id)
			}
		}
	})
}

// UC: UC-055 BR-003
// A game that registers and ends before anything was sent costs no request.
func TestRegisterThenEndBeforeSend(t *testing.T) {
	e := newOutEnv(t)
	g := gameOf("g1", "shadow", "A", "B")
	e.link.StateChanged(e.registered(g, nil))
	e.link.StateChanged(e.change(state.ChangeValue, "interventions", "music_tempo_override", registry.ScopeGame, state.Scope{GameID: "g1"}, "", "1.25"))
	e.link.StateChanged(e.ended(g))
	e.drain()
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("UC-055/BR-003 %d requests for a game that ended before the first send, want 0: %v", n, e.fake.Requests())
	}
}

// UC: UC-055 BR-002
// A one-shot event list is shown in Flagsmith as its latest value: appends
// that arrive before the send are merged into one override carrying the whole
// latest list, and a list that empties removes the override.
func TestEventListShownAsLatestValue(t *testing.T) {
	e := newOutEnv(t)
	e.link.StateChanged(e.registered(gameOf("g1", "real", "A"), nil))
	e.drain()
	events := func(raw string) state.Change {
		c := e.change(state.ChangeEvents, "interventions", "audio_cue", registry.ScopeGame, state.Scope{GameID: "g1"}, "", "")
		c.Events = json.RawMessage(raw)
		return c
	}
	first := `[{"sound":"start"}]`
	latest := `[{"sound":"start"},{"sound":"end"}]`
	e.link.StateChanged(events(first))
	e.link.StateChanged(events(latest))
	e.drain()
	puts := e.bodies(http.MethodPut, apiEnv+"edge-identities-featurestates")
	if len(puts) != 1 {
		t.Fatalf("UC-055/BR-002 %d override requests for two appends before the send, want 1: %v", len(puts), puts)
	}
	var body struct {
		Identifier string          `json:"identifier"`
		Value      json.RawMessage `json:"feature_state_value"`
	}
	if err := json.Unmarshal([]byte(puts[0]), &body); err != nil {
		t.Fatal(err)
	}
	got := string(body.Value)
	var s string
	if json.Unmarshal(body.Value, &s) == nil {
		got = s
	}
	if body.Identifier != "game:g1" || got != latest {
		t.Fatalf("UC-055/BR-002 override %s = %s, want game:g1 = the latest list %s", body.Identifier, got, latest)
	}
	e.link.StateChanged(events(`[]`))
	e.drain()
	if n := e.count(http.MethodDelete, apiEnv+"edge-identities-featurestates"); n != 1 {
		t.Fatalf("UC-055/BR-002 %d override deletes after the list emptied, want 1", n)
	}
	if n := e.count(http.MethodPut, apiEnv+"edge-identities-featurestates"); n != 1 {
		t.Fatalf("UC-055/BR-002 %d override puts after the list emptied, want still 1 (no empty list shown)", n)
	}
}

// UC: UC-055 main step 3 BR-002
// The push span names the local writer (the writer is recorded on the send,
// not stored in Flagsmith) and links the write span that queued the value.
func TestSpanWriterLabel(t *testing.T) {
	e := newOutEnv(t)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	c := e.stack("decision.backend", "jev", `"jev"`)
	e.links.Remember(c.Version, sc, e.clk.Now())
	e.link.StateChanged(c)
	e.drain()
	spans := e.pushSpans()
	if len(spans) != 1 {
		t.Fatalf("UC-055/BR-002 %d push spans, want 1", len(spans))
	}
	s := spans[0]
	for key, want := range map[string]string{AttrWriter: "menu", AttrOp: OpValue, AttrFlagKey: "decision.backend", AttrFlagSet: "agent", AttrScope: ScopeStack, AttrResult: ResultOK} {
		if got := spanAttr(s, key); got != want {
			t.Fatalf("UC-055/BR-002 span attribute %s = %q, want %q", key, got, want)
		}
	}
	if links := s.Links(); len(links) != 1 || links[0].SpanContext.TraceID() != sc.TraceID() {
		t.Fatalf("UC-055/main step 3 span links %v, want one to the write span", links)
	}
}

// UC: UC-055 BR-003
// A game that ends while the create request of its identity is on the wire
// still gets the identity deleted: the entry turns into a delete, it is not
// dropped.
func TestGameEndsWhileCreateInFlight(t *testing.T) {
	e := newOutEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.setOn(http.MethodPost, apiEnv+"edge-identities/", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		flagsmithtest.WriteJSON(w, http.StatusCreated, json.RawMessage(`{"identity_uuid":"`+uuidOf("game:g1")+`"}`))
	})
	g := gameOf("g1", "real")
	e.link.StateChanged(e.registered(g, nil))
	r := e.start()
	<-started
	e.link.StateChanged(e.ended(g))
	close(release)
	waitFor(t, "the out side to go idle", e.idle)
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf("game:g1")+"/"); n != 1 {
		t.Fatalf("UC-055/BR-003 %d deletes of the identity created while the game ended, want 1", n)
	}
}

// UC: UC-055 A2
// A create that fails with the link down is not on the wire any more: a game
// that ends afterwards drops the entry and queues no delete for an identity
// that was never created.
func TestCreateFailsLinkDownThenGameEnds(t *testing.T) {
	e := newOutEnv(t)
	e.fake.Fail(http.StatusServiceUnavailable)
	g := gameOf("g1", "real")
	e.link.StateChanged(e.registered(g, nil))
	r := e.start()
	if err := r.wait(); !IsLinkDown(err) {
		t.Fatalf("UC-055/A2 Run returned %v, want a link-down error", err)
	}
	e.link.StateChanged(e.ended(g))
	e.fake.Fail(0)
	e.drain()
	if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf("game:g1")+"/"); n != 0 {
		t.Fatalf("UC-055/A2 %d deletes of an identity that was never created, want 0", n)
	}
	if n := e.count(http.MethodPost, apiEnv+"edge-identities/"); n != 1 {
		t.Fatalf("UC-055/A2 %d creates, want only the failed one", n)
	}
}

// UC: UC-055 A4
// A refusal drops the value that was pushed, not a newer one merged while the
// request was in flight: the newer value is pushed next.
func TestRefusalKeepsNewerValue(t *testing.T) {
	e := newOutEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.setOn(http.MethodPatch, e.futurePath("decision.backend"), func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			flagsmithtest.WriteJSON(w, http.StatusBadRequest, json.RawMessage(`{"detail":"bad"}`))
			return
		}
		flagsmithtest.WriteJSON(w, http.StatusOK, json.RawMessage(`{}`))
	})
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	r := e.start()
	<-started
	e.link.StateChanged(e.stack("decision.backend", "claude", `"claude"`))
	close(release)
	waitFor(t, "the out side to go idle", e.idle)
	_ = r.stop()
	bodies := e.bodies(http.MethodPatch, e.futurePath("decision.backend"))
	if len(bodies) != 2 || !strings.Contains(bodies[1], "claude") {
		t.Fatalf("UC-055/A4 PATCH bodies %v, want the refused jev then the newer claude", bodies)
	}
	if pending, _, _ := e.link.Links().Counts(); pending != 0 {
		t.Fatalf("UC-055/A4 pending %d, want 0", pending)
	}
}

// UC: UC-055 A4
// NFR: NFR-057
// A refusal without a request (feature id unresolved, value that cannot be
// encoded) is counted and traced like any refused push, with a reason of
// telemetry.md §10.4.
func TestRefusalWithoutRequestRecorded(t *testing.T) {
	e := newOutEnv(t)
	kn := Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
	kb := Key{Set: "agent", Flag: "decision.min_interval_ms", Scope: ScopeStack}
	delete(e.link.IDs().FeatureID, "decision.backend")
	e.link.Links().Offer(kn, Value{Present: true, Variant: "jev", Value: json.RawMessage(`"jev"`)}, e.clk.Now(), 1)
	e.link.Links().Offer(kb, Value{Present: true}, e.clk.Now(), 2)
	e.drain()
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("UC-055/A4 %d requests, want 0", n)
	}
	for _, reason := range []string{ReasonNotFound, ReasonBadValue} {
		if n := e.pushes(reason); n != 1 {
			t.Fatalf("UC-055/A4 %d pushes_total points with reason %q, want 1", n, reason)
		}
	}
	if n := len(e.pushSpans()); n != 2 {
		t.Fatalf("UC-055/A4 %d push spans, want 2", n)
	}
	if r, _ := e.link.Links().Refusal(kb); r != ReasonBadValue {
		t.Fatalf("UC-055/A4 refusal %q, want bad_value", r)
	}
}

// UC: UC-055 A2
// A player that left the game loses its keys with the identity: no override
// request runs after the identity delete.
func TestRemovedSerialKeysForgotten(t *testing.T) {
	e := newOutEnv(t)
	both, onlyA := gameOf("g1", "shadow", "A", "B"), gameOf("g1", "shadow", "A")
	e.link.StateChanged(e.registered(both, nil))
	e.drain()
	kb := Key{Set: "interventions", Flag: "player_sensitivity_factor", Scope: "game:g1:B"}
	e.link.Links().Offer(kb, Value{Writer: "menu"}, e.clk.Now(), 9)
	e.link.StateChanged(e.registered(onlyA, both))
	e.drain()
	if n := e.count(http.MethodDelete, apiEnv+"edge-identities-featurestates"); n != 0 {
		t.Fatalf("UC-055/A2 %d override deletes for a removed player, want 0", n)
	}
	if n := e.count(http.MethodDelete, apiEnv+"edge-identities/"+uuidOf("game:g1:B")+"/"); n != 1 {
		t.Fatalf("UC-055/A2 %d identity deletes of the removed player, want 1", n)
	}
	for _, k := range e.link.Links().Keys() {
		if k.Scope == "game:g1:B" {
			t.Fatalf("UC-055/A2 key %v of the removed player still known", k)
		}
	}
}

// UC: UC-055 A4
// A key whose push keeps failing does not stay first in the queue: after the
// link comes back, the other keys are pushed before it is tried again.
func TestFailingKeyDoesNotBlockQueue(t *testing.T) {
	e := newOutEnv(t)
	e.setOn(http.MethodPatch, e.futurePath("decision.backend"), func(w http.ResponseWriter, _ *http.Request) {
		flagsmithtest.WriteJSON(w, http.StatusInternalServerError, json.RawMessage(`{}`))
	})
	e.link.StateChanged(e.stack("decision.backend", "jev", `"jev"`))
	e.link.StateChanged(e.stack("decision.min_interval_ms", "", `2500`))
	r := e.start()
	if err := r.wait(); err == nil {
		t.Fatal("UC-055/A4 Run ended without an error, want the link down on the 5xx")
	}
	r = e.start()
	waitFor(t, "the second key to be pushed", func() bool {
		return e.count(http.MethodPatch, e.futurePath("decision.min_interval_ms")) == 1
	})
	_ = r.stop()
}
