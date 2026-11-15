package flagsmith_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/joustmania/flag-service/api"
	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
	"github.com/joustmania/flag-service/telemetry"
)

// Journeys of the in side (UC-054) with the real state, the real in-process
// writer (api.API) and a fake Flagsmith. The out side is #2589's: here its
// stub sends nothing, so "queued for push" is the link's pending count.

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fsState is one feature state of a document.
func fsState(name string, enabled bool, value any) map[string]any {
	raw, _ := json.Marshal(value)
	return map[string]any{
		"feature":             map[string]any{"id": 1, "name": name, "type": "STANDARD"},
		"enabled":             enabled,
		"feature_segment":     nil,
		"feature_state_value": json.RawMessage(raw),
	}
}

type override struct {
	identifier string
	states     []map[string]any
}

// document builds an environment document.
func document(updated time.Time, states []map[string]any, overrides ...override) map[string]any {
	ios := []map[string]any{}
	for _, o := range overrides {
		ios = append(ios, map[string]any{"identifier": o.identifier, "identity_uuid": "u-" + o.identifier, "identity_features": o.states})
	}
	if states == nil {
		states = []map[string]any{}
	}
	return map[string]any{
		"name":                                 "stage",
		"updated_at":                           updated.UTC().Format(time.RFC3339Nano),
		"use_identity_overrides_in_local_eval": true,
		"feature_states":                       states,
		"identity_overrides":                   ios,
	}
}

type harness struct {
	t      *testing.T
	fake   *flagsmithtest.Fake
	base   *baseline.Baseline
	st     *state.State
	api    *api.API
	link   *flagsmith.Link
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	clock  *clock
	cancel context.CancelFunc
	done   chan struct{}
	sub    *state.Subscription

	sleepMu sync.Mutex
	sleeps  []time.Duration // every wait the link asked for
}

type harnessOpts struct {
	noStream bool
	realtime func(*flagsmithtest.Fake) string
	// writer wraps the in-process writer (nil: api.API).
	writer func(*api.API, *state.State) flagsmith.LocalWriter
	// before runs before the link starts.
	before func(h *harness)
	doc    any
	// docURL is FLAGSMITH_DOCUMENT_URL (nil: the fake's API base).
	docURL func(*flagsmithtest.Fake) string
}

func newHarness(t *testing.T, o harnessOpts) *harness {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _, err := state.New(base, reg, state.Options{Epoch: "e1", Served: base.Order, StateFile: filepath.Join(t.TempDir(), "state.json"), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, base: base, st: st, clock: &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}}
	if !o.noStream {
		h.sub, _ = st.Subscribe("")
	}
	h.spans = tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans)).Tracer("t")
	h.reader = sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader)).Meter("t")
	metrics, err := telemetry.NewMetrics(meter, st.Stats)
	if err != nil {
		t.Fatal(err)
	}
	links := &telemetry.Links{}
	h.api = &api.API{State: st, Metrics: metrics, Tracer: tracer, Links: links, Logger: logger}
	h.fake = flagsmithtest.New(t)
	h.fake.Handle("POST /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	doc := o.doc
	if doc == nil {
		doc = document(h.clock.Now().Add(-time.Minute), nil)
	}
	h.fake.SetDocument(doc, "")
	cfg := flagsmith.Config{
		Enabled: true, APIURL: h.fake.URL(), DocumentURL: h.fake.URL(),
		ServerKey: "ser.test", AdminKey: "admin", EnvironmentKey: "envkey", ProjectID: h.fake.ProjectID,
		Poll: time.Hour, PushRate: 5, Timeout: 2 * time.Second,
		HealthWebhookURL: h.fake.Server.URL + "/health",
	}
	if o.docURL != nil {
		cfg.DocumentURL = o.docURL(h.fake)
	}
	if o.realtime != nil {
		cfg.RealtimeURL = o.realtime(h.fake)
	}
	var w flagsmith.LocalWriter = h.api
	if o.writer != nil {
		w = o.writer(h.api, st)
	}
	link, err := flagsmith.New(cfg, flagsmith.Deps{
		State: st, Writer: w, Tracer: tracer, Meter: meter, Logger: logger, Links: links,
		Now: h.clock.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.sleepMu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.sleepMu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(d, 20*time.Millisecond)):
				return nil
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.link = link
	flagsmith.UseInSideOnly(link)
	st.SetObserver(link)
	if o.before != nil {
		o.before(h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		defer close(h.done)
		link.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
		if h.sub != nil {
			h.sub.Close()
		}
	})
	return h
}

// longestSleep is the longest wait the link asked for since mark (an index
// into the recorded waits) and the new mark.
func (h *harness) longestSleep(mark int) (time.Duration, int) {
	h.sleepMu.Lock()
	defer h.sleepMu.Unlock()
	var m time.Duration
	for _, d := range h.sleeps[min(mark, len(h.sleeps)):] {
		m = max(m, d)
	}
	return m, len(h.sleeps)
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) online() {
	h.t.Helper()
	h.waitFor("link online after the start reconcile", func() bool {
		return h.link.Mode() == flagsmith.ModeOnline && h.pulls() >= 1
	})
}

// ended returns the ended spans named name.
func (h *harness) ended(name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range h.spans.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func (h *harness) pulls() int { return len(h.ended(flagsmith.SpanPull)) }

// round advances the clock 2 s (past a first deferral) and runs one pull.
func (h *harness) round() {
	h.t.Helper()
	n := h.pulls()
	h.clock.Advance(2 * time.Second)
	h.link.Wake()
	h.waitFor("a pull round", func() bool { return h.pulls() > n })
}

// setDoc sets the document, updated one minute before now.
func (h *harness) setDoc(states []map[string]any, overrides ...override) {
	h.fake.SetDocument(document(h.clock.Now().Add(-time.Minute), states, overrides...), "")
}

// local returns the stack's value of a key and whether the slot was persisted.
func (h *harness) local(set, flag, scope string) (flagsmith.Value, bool) {
	changes, _ := h.st.Changes()
	for _, c := range changes {
		k, v, ok := flagsmith.ValueOfChange(c)
		if ok && k == (flagsmith.Key{Set: set, Flag: flag, Scope: scope}) {
			return v, c.Persisted
		}
	}
	return flagsmith.Value{}, false
}

func (h *harness) writeSpans(flag string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range h.ended(telemetry.SpanWrite) {
		for _, a := range s.Attributes() {
			if string(a.Key) == "feature_flag.key" && a.Value.AsString() == flag {
				out = append(out, s)
			}
		}
	}
	return out
}

// healthPosts returns the decoded feature-health posts.
func (h *harness) healthPosts() []map[string]any {
	var out []map[string]any
	for _, r := range h.fake.RequestsTo(http.MethodPost, "/health") {
		var m map[string]any
		_ = json.Unmarshal(r.Body, &m)
		out = append(out, m)
	}
	return out
}

func (h *harness) docRequests() []flagsmithtest.Request {
	return h.fake.RequestsTo(http.MethodGet, flagsmithtest.APIPath+"environment-document/")
}

// histogramCount returns the number of samples of a histogram metric.
func (h *harness) histogramCount(name string) uint64 {
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatal(err)
	}
	var n uint64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if hist, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
				for _, dp := range hist.DataPoints {
					n += dp.Count
				}
			}
		}
	}
	return n
}

// counter returns the sum of a counter metric with the attribute k=v.
func (h *harness) counter(name, k, v string) int64 {
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		h.t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != name {
				continue
			}
			for _, dp := range sum.DataPoints {
				if got, ok := dp.Attributes.Value(attribute.Key(k)); ok && got.AsString() == v {
					n += dp.Value
				}
			}
		}
	}
	return n
}

var (
	enabledKey = flagsmith.Key{Set: "agent", Flag: "enabled", Scope: flagsmith.ScopeStack}
	offValue   = flagsmith.Value{Present: true, Variant: "off", Value: json.RawMessage("false")}
)

// UC: UC-054 3
// A change made in Flagsmith is applied as an in-process write labelled
// flagsmith, persisted for an admin setting, a child of the pull span; the
// same document again writes nothing (no echo, BR-004); the change delay is
// recorded (NFR-058).
func TestPullAppliesChange(t *testing.T) {
	var echo echoProbe
	h := newHarness(t, harnessOpts{before: func(h *harness) {
		echo.link = h.link
		h.st.SetObserver(&echo)
	}})
	h.online()
	h.setDoc([]map[string]any{fsState("decision.backend", true, "claude")})
	h.round()
	v, persisted := h.local("agent", "decision.backend", flagsmith.ScopeStack)
	if v.Variant != "claude" || !persisted {
		t.Fatalf("UC-054/5 decision.backend = %+v persisted %v, want claude persisted", v, persisted)
	}
	ws := h.writeSpans("decision.backend")
	if len(ws) != 1 {
		t.Fatalf("UC-054/5 %d write spans, want 1", len(ws))
	}
	var writer string
	for _, a := range ws[0].Attributes() {
		if a.Key == "flagservice.writer" {
			writer = a.Value.AsString()
		}
	}
	if writer != flagsmith.WriterLabel {
		t.Fatalf("UC-054/BR-001 writer %q, want flagsmith", writer)
	}
	pulls := h.ended(flagsmith.SpanPull)
	if ws[0].Parent().SpanID() != pulls[len(pulls)-1].SpanContext().SpanID() {
		t.Fatal("UC-054/7 the write span is not a child of flagservice.flagsmith.pull")
	}
	if st := h.link.Status(); st.Pending != 0 || st.OutOfSync != 0 {
		t.Fatalf("UC-054/7 status %+v, want nothing pending, in sync", st)
	}
	if n := h.histogramCount(flagsmith.MetricChangeDelay); n != 1 {
		t.Fatalf("NFR-058 change delay samples %d, want 1", n)
	}
	if r, ok := echo.remoteAtWrite(); !ok || r.Variant != "claude" {
		t.Fatalf("UC-054/BR-004 remote %+v when the out side saw the write, want claude set before it (no echo)", r)
	}
	h.round()
	if len(h.writeSpans("decision.backend")) != 1 {
		t.Fatal("UC-054/BR-004 the same document was applied twice")
	}
}

// echoProbe is the state observer in front of the link: it records the
// link's remote value of decision.backend when the flagsmith write reaches
// the observer, which is when the out side would compare it (BR-004).
type echoProbe struct {
	link   *flagsmith.Link
	mu     sync.Mutex
	remote *flagsmith.Value
}

func (e *echoProbe) StateChanged(c state.Change) {
	if c.Flag == "decision.backend" && c.Writer == flagsmith.WriterLabel {
		r, _ := e.link.Links().Remote(flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack})
		e.mu.Lock()
		e.remote = &r
		e.mu.Unlock()
	}
	e.link.StateChanged(c)
}

func (e *echoProbe) remoteAtWrite() (flagsmith.Value, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.remote == nil {
		return flagsmith.Value{}, false
	}
	return *e.remote, true
}

// racingWriter writes agent/enabled=off locally (as an operator's HTTP write
// would) just before the pulled write of agent/enabled lands.
type racingWriter struct {
	*api.API
	st   *state.State
	once sync.Once
}

func (w *racingWriter) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	if req.Set == "agent" && req.Flag == "enabled" {
		w.once.Do(func() {
			if _, ferr := w.st.Put(state.WriteRequest{Set: "agent", Flag: "enabled", Variant: "off", Writer: "operator"}); ferr != nil {
				panic(ferr)
			}
		})
	}
	return w.API.FlagsmithPut(ctx, req)
}

// UC: UC-054 BR-005
// Compare-and-set (#2590 P2, test 6/6b): R = L = baseline (empty slot), a
// person turns agent/enabled on in Flagsmith while the operator turns it off
// locally between the snapshot and the write: the pulled write is refused as
// stale (412 slot_changed), remote is restored, and the next round sees the
// conflict and keeps the tighter local off, queued for push.
func TestPulledWriteIsConditional(t *testing.T) {
	h := newHarness(t, harnessOpts{writer: func(a *api.API, st *state.State) flagsmith.LocalWriter {
		return &racingWriter{API: a, st: st}
	}})
	h.online()
	h.setDoc([]map[string]any{fsState("enabled", true, "on")})
	h.round()
	if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %+v after the race, want off (the local write wins)", v)
	}
	if r, ok := h.link.Links().Remote(enabledKey); ok && r.Present {
		t.Fatalf("UC-054/A5 remote %+v after a stale write, want restored", r)
	}
	h.round()
	if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %+v after the conflict, want off (tightened)", v)
	}
	if st := h.link.Status(); st.Pending != 1 {
		t.Fatalf("UC-054/A2.1 pending %d, want the local off queued for push", st.Pending)
	}
	if n := h.counter(flagsmith.MetricConflicts, "resolution", flagsmith.ResolutionTightened); n != 1 {
		t.Fatalf("UC-054/A2.1 tightened conflicts %d, want 1", n)
	}
}

// UC: UC-054 A5
// No flagd stream: the change is not applied and not recorded as seen
// (Last-Modified does not advance); once flagd connects it is applied.
func TestNoStreamDefers(t *testing.T) {
	h := newHarness(t, harnessOpts{noStream: true})
	h.online()
	lm := h.link.LastModified()
	h.clock.Advance(10 * time.Second) // a newer document: its Last-Modified differs
	h.setDoc([]map[string]any{fsState("decision.backend", true, "claude")})
	h.round()
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Present {
		t.Fatalf("UC-054/A5.1 applied without a stream: %+v", v)
	}
	if got := h.link.LastModified(); got != lm {
		t.Fatalf("UC-054/A5.1 Last-Modified moved %q -> %q on a deferral", lm, got)
	}
	if _, ok := h.link.Links().Remote(flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}); ok {
		if r, _ := h.link.Links().Remote(flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}); r.Present {
			t.Fatalf("UC-054/A5.1 remote %+v, want not seen", r)
		}
	}
	h.sub, _ = h.st.Subscribe("")
	h.round()
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
		t.Fatalf("UC-054/A5.2 decision.backend = %+v once flagd connected, want claude", v)
	}
}

// UC: UC-054 A4
// A refused change keeps the local value, marks the feature unhealthy once,
// is not retried and not pushed over; the next valid value is applied and the
// feature turns healthy.
func TestRefusedChangeIsUnhealthy(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	before, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack)
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 5)})
	h.round()
	h.waitFor("an unhealthy post", func() bool { return len(h.healthPosts()) == 1 })
	p := h.healthPosts()[0]
	if p["feature"] != "decision.min_interval_ms" || p["status"] != "unhealthy" || p["environment"] != "stage" ||
		!strings.Contains(string(mustJSON(p["reason"])), fserr.OutOfBounds) {
		t.Fatalf("UC-054/A4.2 health post %v", p)
	}
	if after, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); !after.Equal(before) {
		t.Fatalf("UC-054/A4.1 local value changed to %+v", after)
	}
	if st := h.link.Status(); st.Refused != 1 {
		t.Fatalf("UC-054/A4.1 refused %d, want 1", st.Refused)
	}
	writes := len(h.writeSpans("decision.min_interval_ms"))
	h.round()
	if got := len(h.writeSpans("decision.min_interval_ms")); got != writes {
		t.Fatalf("UC-054/A4.3 the refused value was tried again (%d -> %d writes)", writes, got)
	}
	if st := h.link.Status(); st.Pending != 0 {
		t.Fatalf("UC-054/A4.3 pending %d: the local value would be pushed over the refused one", st.Pending)
	}
	if n := len(h.healthPosts()); n != 1 {
		t.Fatalf("UC-054/A4.2 %d health posts, want 1 (posted on the transition only)", n)
	}
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 500)})
	h.round()
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) != "500" {
		t.Fatalf("UC-054/A4.3 the next valid value was not applied: %+v", v)
	}
	h.waitFor("a healthy post", func() bool { return len(h.healthPosts()) == 2 })
	if p := h.healthPosts()[1]; p["status"] != "healthy" {
		t.Fatalf("FR-213 second post %v, want healthy", p)
	}
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// UC: UC-054 BR-003
// An override on identity real is shown only: changed in Flagsmith it is
// refused with 422 flagsmith_scope_read_only and the feature turns unhealthy.
func TestReadOnlyScopeRefused(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	sens := h.base.Flag("game", "sensitivity")
	name := sens.VariantNames()[0]
	h.setDoc(nil, override{"real", []map[string]any{fsState("sensitivity", true, name)}})
	h.round()
	h.waitFor("an unhealthy post", func() bool { return len(h.healthPosts()) == 1 })
	if p := h.healthPosts()[0]; p["feature"] != "sensitivity" || !strings.Contains(string(mustJSON(p["reason"])), fserr.FlagsmithScopeReadOnly) {
		t.Fatalf("UC-054/BR-003 health post %v", p)
	}
	if v, _ := h.local("game", "sensitivity", "real"); v.Present {
		t.Fatalf("UC-054/BR-003 the real overlay was written: %+v", v)
	}
}

// UC: UC-054 A1
// Without use_identity_overrides_in_local_eval the link stays offline with
// reason identity_overrides_disabled.
func TestIdentityOverridesDisabled(t *testing.T) {
	doc := document(time.Now(), nil)
	doc["use_identity_overrides_in_local_eval"] = false
	h := newHarness(t, harnessOpts{doc: doc})
	h.waitFor("link offline", func() bool {
		st := h.link.Status()
		return st.Link == flagsmith.ModeOffline && st.OfflineReason == flagsmith.ReasonIdentityOverridesDisabled
	})
}

// UC: UC-054 4
// An event payload set on game:<id> is appended once; the same document, and
// a document holding the pushed list, append nothing (§4.3).
func TestEventAppendedOnce(t *testing.T) {
	h := newHarness(t, harnessOpts{before: func(h *harness) {
		if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0003"}}); ferr != nil {
			t.Fatal(ferr)
		}
	}})
	h.online()
	events := func() []json.RawMessage {
		v, _ := h.local("interventions", "eliminate_player", "game:g1")
		var list []json.RawMessage
		_ = json.Unmarshal(v.Value, &list)
		return list
	}
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("eliminate_player", true, `{"serial":"MOCK0003"}`)}})
	h.round()
	if n := len(events()); n != 1 {
		t.Fatalf("UC-054/4 %d events after one payload, want 1", n)
	}
	h.round()
	if n := len(events()); n != 1 {
		t.Fatalf("UC-054/BR-004 %d events after the same document, want 1 (no second append)", n)
	}
	list, _ := h.local("interventions", "eliminate_player", "game:g1")
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("eliminate_player", true, string(list.Value))}})
	h.round()
	if n := len(events()); n != 1 {
		t.Fatalf("UC-054/BR-004 %d events after the pushed list came back, want 1", n)
	}
	if st := h.link.Status(); st.Refused != 0 {
		t.Fatalf("UC-054/4 refused %d, want 0", st.Refused)
	}
}

// UC: UC-054 2
// Conditional pulls (§4.2 steps 1 and 8): after a 200 with a changed document
// the next pull has no If-Modified-Since; an unchanged one makes the next
// conditional; every 10th pull is unconditional; while a game is live every
// pull is unconditional (#2590 R2).
func TestConditionalPulls(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	ims := func(i int) string { return h.docRequests()[i].Header.Get("If-Modified-Since") }
	h.setDoc([]map[string]any{fsState("decision.backend", true, "claude")})
	h.round() // changed
	h.round()
	n := len(h.docRequests())
	if got := ims(n - 1); got != "" {
		t.Fatalf("UC-054/2 pull after a changed document sent If-Modified-Since %q", got)
	}
	h.round()
	if got := ims(len(h.docRequests()) - 1); got == "" {
		t.Fatal("UC-054/2 pull after an unchanged document is not conditional")
	}
	unconditional := 0
	for range 10 {
		h.round()
		if ims(len(h.docRequests())-1) == "" {
			unconditional++
		}
	}
	if unconditional != 1 {
		t.Fatalf("UC-054/2 %d unconditional pulls in 10 unchanged rounds, want 1", unconditional)
	}
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0001"}}); ferr != nil {
		t.Fatal(ferr)
	}
	for range 3 {
		h.round()
		if got := ims(len(h.docRequests()) - 1); got != "" {
			t.Fatalf("UC-054/2 a pull while a game is live sent If-Modified-Since %q", got)
		}
	}
}

// UC: UC-054 2
// The realtime stream wakes the poller (trigger realtime); a stream that
// cannot open leaves the link online and polling.
func TestRealtimeWakes(t *testing.T) {
	t.Run("UC-054/2 an SSE event pulls at once", func(t *testing.T) {
		h := newHarness(t, harnessOpts{realtime: func(f *flagsmithtest.Fake) string { return f.Server.URL + "/" }})
		h.online()
		h.waitFor("the SSE stream open", func() bool { return len(h.fake.RequestsTo(http.MethodGet, "/sse/environments/envkey/stream")) == 1 })
		n := h.pulls()
		h.fake.SendEvent(`{"updated_at":1}`)
		h.waitFor("a realtime pull", func() bool { return h.pulls() > n })
		last := h.ended(flagsmith.SpanPull)[n]
		for _, a := range last.Attributes() {
			if a.Key == flagsmith.AttrTrigger && a.Value.AsString() != flagsmith.TriggerRealtime {
				t.Fatalf("UC-054/2 trigger %q, want realtime", a.Value.AsString())
			}
		}
	})
	t.Run("UC-054/2 a stream that answers 404 leaves polling on", func(t *testing.T) {
		h := newHarness(t, harnessOpts{realtime: func(f *flagsmithtest.Fake) string { return f.Server.URL + "/nope/" }})
		h.online()
		h.waitFor("the stream tried", func() bool { return len(h.fake.RequestsTo(http.MethodGet, "/nope/")) >= 1 })
		h.round()
		if h.link.Mode() != flagsmith.ModeOnline {
			t.Fatalf("UC-054/2 link %s after a failed stream, want online", h.link.Mode())
		}
	})
}

// UC: UC-054 BR-005
// A stale document never reverts a confirmed push (#2590 P1): agent/enabled
// was on everywhere, the operator turned it off and the push was confirmed
// while the document (still on) was in flight, or the document is older than
// the confirmation: off stays.
func TestStaleDocumentNeverReverts(t *testing.T) {
	setup := func(t *testing.T, docUpdated func(now time.Time) time.Time) *harness {
		h := newHarness(t, harnessOpts{before: func(hh *harness) {
			if _, ferr := hh.st.Put(state.WriteRequest{Set: "agent", Flag: "enabled", Variant: "on", Writer: "operator"}); ferr != nil {
				t.Fatal(ferr)
			}
			hh.fake.SetDocument(document(docUpdated(hh.clock.Now()), []map[string]any{fsState("enabled", true, "on")}), "")
		}})
		h.online()
		if r, ok := h.link.Links().Remote(enabledKey); !ok || r.Variant != "on" {
			t.Fatalf("setup: remote %+v %v, want on", r, ok)
		}
		if _, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "enabled", Variant: "off", Writer: "operator"}); ferr != nil {
			t.Fatal(ferr)
		}
		return h
	}
	t.Run("UC-054/BR-005 push confirmed while the document was in flight", func(t *testing.T) {
		// The document claims to be newer than the confirmation: only the
		// generation snapshot catches it.
		h := setup(t, func(now time.Time) time.Time { return now.Add(time.Hour) })
		h.fake.Delay(300 * time.Millisecond)
		n := h.pulls()
		h.clock.Advance(2 * time.Second)
		h.link.Wake()
		h.waitFor("the document request", func() bool { return len(h.docRequests()) >= 3 })
		h.link.Links().ConfirmPushed(enabledKey, offValue)
		h.waitFor("the round", func() bool { return h.pulls() > n })
		if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %+v: a stale document reverted the confirmed push", v)
		}
	})
	t.Run("UC-054/BR-005 document older than the confirmed push", func(t *testing.T) {
		h := setup(t, func(now time.Time) time.Time { return now.Add(-time.Minute) })
		h.link.Links().ConfirmPushed(enabledKey, offValue)
		h.round()
		if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %+v: a stale document reverted the confirmed push", v)
		}
	})
	t.Run("UC-054/BR-005 stale document stamped ahead of the local clock (skew)", func(t *testing.T) {
		// Flagsmith's clock is an hour ahead: a time comparison would take
		// the stale on for a change made after the push.
		h := setup(t, func(now time.Time) time.Time { return now.Add(time.Hour) })
		h.link.Links().ConfirmPushed(enabledKey, offValue)
		for range 3 {
			h.round()
		}
		if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %+v: a stale document loosened the kill switch", v)
		}
	})
	t.Run("UC-054/BR-005 once the document shows the push, a later change applies", func(t *testing.T) {
		h := setup(t, func(now time.Time) time.Time { return now.Add(-time.Minute) })
		h.link.Links().ConfirmPushed(enabledKey, offValue)
		h.round()
		h.setDoc([]map[string]any{fsState("enabled", true, "off")})
		h.round()
		h.setDoc([]map[string]any{fsState("enabled", true, "on")})
		h.round()
		if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "on" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %+v: a deliberate on after the push was not applied", v)
		}
	})
	t.Run("UC-054/BR-005 the guard expires after ten poll intervals", func(t *testing.T) {
		h := setup(t, func(now time.Time) time.Time { return now.Add(-time.Minute) })
		h.link.Links().ConfirmPushed(enabledKey, offValue)
		h.round()
		h.clock.Advance(9 * time.Hour) // poll interval 1 h: the guard lives 10 h
		h.round()
		if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %+v inside the guard", v)
		}
		h.clock.Advance(2 * time.Hour)
		h.round()
		if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "on" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %+v: the guard outlived its bound", v)
		}
	})
}

// UC: UC-054 BR-003
// An override on a game that is not registered is neither applied nor
// refused; once the game registers it is applied (#2590 R5).
func TestOverrideWaitsForGame(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	h.setDoc(nil, override{"game:g9", []map[string]any{fsState("global_sensitivity_override", true, 2)}})
	h.round()
	if v, _ := h.local("interventions", "global_sensitivity_override", "game:g9"); v.Present {
		t.Fatalf("UC-054/BR-003 applied before the game registered: %+v", v)
	}
	if st := h.link.Status(); st.Refused != 0 || len(h.writeSpans("global_sensitivity_override")) != 0 {
		t.Fatalf("UC-054/BR-003 status %+v: an unregistered game's override was tried", st)
	}
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g9", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0001"}}); ferr != nil {
		t.Fatal(ferr)
	}
	h.round()
	if v, _ := h.local("interventions", "global_sensitivity_override", "game:g9"); string(v.Value) != "2" {
		t.Fatalf("UC-054/4 override = %+v after the game registered, want 2", v)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(h.healthPosts()); n != 0 {
		t.Fatalf("UC-054/BR-003 %d health posts, want 0", n)
	}
}
