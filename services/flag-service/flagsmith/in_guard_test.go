package flagsmith_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/api"
	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/state"
)

// Push guards, refusal ends, pruning and slot checks of the in side (#2596
// delta review findings 1 to 3).

var (
	maxGamesKey      = flagsmith.Key{Set: "agent", Flag: "experiment_max_games", Scope: flagsmith.ScopeStack}
	minIntervalKey   = flagsmith.Key{Set: "agent", Flag: "decision.min_interval_ms", Scope: flagsmith.ScopeStack}
	volumeOverrideG1 = flagsmith.Key{Set: "interventions", Flag: "volume_override", Scope: "game:g1"}
)

func num(n string) flagsmith.Value { return flagsmith.Value{Present: true, Value: json.RawMessage(n)} }

// operatorWriter lets the operator write 10 to experiment_max_games between
// the round's snapshot and its pulled write, once (the pulled write is then
// 412 slot_changed: a deferral that leaves the remote value unknown). With
// confirm, the out side's push of 10 is confirmed while the round still runs.
type operatorWriter struct {
	*api.API
	st      *state.State
	confirm bool
	link    *flagsmith.Link
	done    atomic.Bool
}

func (w *operatorWriter) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	if req.Flag != "experiment_max_games" || w.done.Swap(true) {
		return w.API.FlagsmithPut(ctx, req)
	}
	if _, ferr := w.st.Put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("10"), Writer: "operator"}); ferr != nil {
		panic(ferr)
	}
	res, ferr := w.API.FlagsmithPut(ctx, req)
	if w.confirm {
		w.link.Links().ConfirmPushed(maxGamesKey, num("10"))
	}
	return res, ferr
}

// maxGamesRace starts a harness where the operator holds 500, the document
// 100 (tighter: applied at start) and the operator's 10 races the apply.
func maxGamesRace(t *testing.T, confirm bool) *harness {
	t.Helper()
	ow := &operatorWriter{confirm: confirm}
	h := newHarness(t, harnessOpts{
		writer: func(a *api.API, st *state.State) flagsmith.LocalWriter {
			ow.API, ow.st = a, st
			return ow
		},
		before: func(hh *harness) {
			ow.link = hh.link
			if _, ferr := hh.st.Put(state.WriteRequest{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("500"), Writer: "operator"}); ferr != nil {
				t.Fatal(ferr)
			}
			hh.fake.SetDocument(document(hh.clock.Now().Add(-time.Minute), []map[string]any{fsState("experiment_max_games", true, 100)}), "")
		}})
	h.online()
	if !ow.done.Load() {
		t.Fatal("setup: the start round did not apply the tighter document value")
	}
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "10" {
		t.Fatalf("setup: local %+v, want the operator's 10", v)
	}
	return h
}

// UC: UC-054 BR-005
// A push confirmed after a deferred first apply (the remote value unknown) is
// guarded against the document that round read: the stale document (100)
// never loosens the operator's confirmed 10 (#2596 delta finding 1).
func TestGuardAfterDeferredApply(t *testing.T) {
	h := maxGamesRace(t, false)
	if r, ok := h.link.Links().Remote(maxGamesKey); ok {
		t.Fatalf("setup: remote %+v known, want unknown after the deferred apply", r)
	}
	h.link.Links().ConfirmPushed(maxGamesKey, num("10"))
	h.clock.Advance(time.Minute)
	for range 2 {
		h.round()
	}
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "10" {
		t.Fatalf("UC-054/BR-005 experiment_max_games = %+v: a stale document (100) loosened the confirmed 10", v)
	}
}

// UC: UC-054 BR-005
// A push confirmed after a deferred apply is also guarded against the remote
// value known before that apply: a stale copy without an update time (no
// body updated_at, no header) that still shows it is not applied.
func TestGuardKeepsRemoteBeforeDeferredApply(t *testing.T) {
	ow := &operatorWriter{}
	ow.done.Store(true)
	h := newHarness(t, harnessOpts{writer: func(a *api.API, st *state.State) flagsmith.LocalWriter {
		ow.API, ow.st = a, st
		return ow
	}})
	h.online()
	setDoc := func(v int) {
		d := document(h.clock.Now(), []map[string]any{fsState("experiment_max_games", true, v)})
		delete(d, "updated_at")
		h.fake.SetDocument(d, "")
	}
	setDoc(100)
	h.round()
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "100" {
		t.Fatalf("setup: local %+v, want 100", v)
	}
	ow.done.Store(false)
	setDoc(50)
	h.round()
	if r, ok := h.link.Links().Remote(maxGamesKey); !ok || string(r.Value) != "100" {
		t.Fatalf("setup: remote %+v %v after the deferred apply, want 100 restored", r, ok)
	}
	h.link.Links().ConfirmPushed(maxGamesKey, num("10"))
	h.clock.Advance(time.Minute)
	setDoc(100)
	for range 2 {
		h.round()
	}
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "10" {
		t.Fatalf("UC-054/BR-005 experiment_max_games = %+v: a stale copy (100) loosened the confirmed 10", v)
	}
}

// UC: UC-054 BR-005
// The same push confirmed while the round runs: the document value that round
// read is guarded too.
func TestGuardPushConfirmedDuringRound(t *testing.T) {
	h := maxGamesRace(t, true)
	h.clock.Advance(time.Minute)
	for range 2 {
		h.round()
	}
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "10" {
		t.Fatalf("UC-054/BR-005 experiment_max_games = %+v: a stale document (100) loosened the push confirmed mid-round", v)
	}
}

// UC: UC-054 BR-005
// A document older than one already read (a second cache or region still
// serving the pre-push copy) is refused: the round writes nothing, the pull
// counts as error with reason stale_document and the next pull is in full
// (#2596 delta finding 3).
func TestOlderDocumentRefused(t *testing.T) {
	var old time.Time
	h := newHarness(t, harnessOpts{before: func(hh *harness) {
		if _, ferr := hh.st.Put(state.WriteRequest{Set: "agent", Flag: "enabled", Variant: "on", Writer: "operator"}); ferr != nil {
			t.Fatal(ferr)
		}
		old = hh.clock.Now().Add(-time.Minute)
		hh.fake.SetDocument(document(old, []map[string]any{fsState("enabled", true, "on")}), "")
	}})
	h.online()
	if _, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "enabled", Variant: "off", Writer: "operator"}); ferr != nil {
		t.Fatal(ferr)
	}
	h.link.Links().ConfirmPushed(enabledKey, offValue)
	h.round()
	h.fake.SetDocument(document(h.clock.Now(), []map[string]any{fsState("enabled", true, "off")}), "")
	h.round()
	h.round() // unchanged: the next pull is conditional again
	h.fake.SetDocument(document(old, []map[string]any{fsState("enabled", true, "on")}), "")
	h.round()
	lastIMS := func() string {
		reqs := h.docRequests()
		return reqs[len(reqs)-1].Header.Get("If-Modified-Since")
	}
	if lastIMS() == "" {
		t.Fatal("setup: the pull of the older copy was unconditional, want a conditional pull")
	}
	if v, _ := h.local("agent", "enabled", flagsmith.ScopeStack); v.Variant != "off" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %+v: an older document copy loosened the kill switch", v)
	}
	pulls := h.ended(flagsmith.SpanPull)
	last := pulls[len(pulls)-1]
	attrs := map[string]string{}
	for _, a := range last.Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if attrs[flagsmith.AttrResult] != flagsmith.ResultError || attrs[flagsmith.AttrReason] != flagsmith.ReasonStaleDocument {
		t.Fatalf("telemetry §10.4 pull span %v, want result error, reason stale_document", attrs)
	}
	if n := h.counter("flagservice_flagsmith_pulls_total", "result", flagsmith.ResultError); n != 1 {
		t.Fatalf("telemetry §10.4 %d pulls with result error, want 1", n)
	}
	h.round()
	if ims := lastIMS(); ims != "" {
		t.Fatalf("UC-054/2 the pull after a stale document sent If-Modified-Since %q, want a full pull", ims)
	}
}

// healthOf returns the statuses posted for feature, in order.
func healthOf(h *harness, feature string) []any {
	var out []any
	for _, p := range h.healthPosts() {
		if p["feature"] == feature {
			out = append(out, p["status"])
		}
	}
	return out
}

// UC: UC-054 A4
// A feature unhealthy after 5 deferrals turns healthy when the deferral ends
// without an apply: by a push (the document no longer holds a value, the
// local one is sent) and by keeping the local value (a kill switch whose
// document value is now looser) (#2596 delta finding 2, E and K).
func TestDeferralEndsByPushOrKeepLocal(t *testing.T) {
	h := newHarness(t, harnessOpts{noStream: true})
	h.online()
	sub, _ := h.st.Subscribe("")
	for _, w := range []state.WriteRequest{
		{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("700"), Writer: "operator"},
		{Set: "agent", Flag: "experiment_max_games", Value: json.RawMessage("500"), Writer: "operator"},
	} {
		if _, ferr := h.st.Put(w); ferr != nil {
			t.Fatal(ferr)
		}
	}
	sub.Close()
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 900), fsState("experiment_max_games", true, 100)})
	for range 5 {
		h.clock.Advance(time.Minute)
		h.round()
	}
	h.waitFor("two unhealthy posts", func() bool {
		return len(healthOf(h, "decision.min_interval_ms")) == 1 && len(healthOf(h, "experiment_max_games")) == 1
	})
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", false, nil), fsState("experiment_max_games", true, 1000)})
	h.clock.Advance(time.Minute)
	h.round()
	h.waitFor("two healthy posts", func() bool {
		return len(healthOf(h, "decision.min_interval_ms")) == 2 && len(healthOf(h, "experiment_max_games")) == 2
	})
	for _, f := range []string{"decision.min_interval_ms", "experiment_max_games"} {
		if s := healthOf(h, f); s[1] != flagsmith.HealthHealthy {
			t.Fatalf("UC-054/A4 %s posts %v, want unhealthy then healthy", f, s)
		}
	}
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "500" {
		t.Fatalf("UC-054/BR-005 experiment_max_games = %+v, want the tighter local 500 kept", v)
	}
}

// UC: UC-054 A4
// A refused key that leaves the round (the identity forgotten and its
// override deleted in Flagsmith) is pruned: its feature turns healthy (#2596
// delta finding 2, F).
func TestRefusedKeyLeavingRoundIsPruned(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	sens := h.base.Flag("game", "sensitivity")
	h.setDoc(nil, override{"real", []map[string]any{fsState("sensitivity", true, sens.VariantNames()[0])}})
	h.round()
	h.waitFor("an unhealthy post", func() bool { return len(healthOf(h, "sensitivity")) == 1 })
	h.link.Links().ForgetIdentity("real")
	h.setDoc(nil)
	h.round()
	h.waitFor("a healthy post", func() bool { return len(healthOf(h, "sensitivity")) == 2 })
	if s := healthOf(h, "sensitivity"); s[1] != flagsmith.HealthHealthy {
		t.Fatalf("UC-054/A4 posts %v, want unhealthy then healthy", s)
	}
}

// UC: UC-054 BR-004
// A local write that lands while the document is in flight is not replaced
// in the push queue by the older value the round's snapshot read (#2596
// finding 6; delta finding 2, G).
func TestOfferKeepsNewerLocalValue(t *testing.T) {
	var (
		mu    sync.Mutex
		doc   any
		race  atomic.Bool
		hooks func()
	)
	h := newHarness(t, harnessOpts{docURL: func(f *flagsmithtest.Fake) string {
		f.Handle("GET /racy/environment-document/", func(w http.ResponseWriter, _ *http.Request) {
			if race.Swap(false) {
				hooks()
			}
			mu.Lock()
			d := doc
			mu.Unlock()
			if d == nil {
				d = document(time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC), nil)
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, d)
		})
		return f.Server.URL + "/racy/"
	}})
	h.online()
	setDoc := func(v int) {
		mu.Lock()
		doc = document(h.clock.Now().Add(-time.Minute), []map[string]any{fsState("decision.min_interval_ms", true, v)})
		mu.Unlock()
	}
	setDoc(700)
	h.round()
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) != "700" {
		t.Fatalf("setup: local %+v, want 700", v)
	}
	if _, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("800"), Writer: "operator"}); ferr != nil {
		t.Fatal(ferr)
	}
	// While the document is in flight the operator writes 900; the out side's
	// observer queues it (here the test, the out side is idle).
	hooks = func() {
		ch, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("900"), Writer: "operator"})
		if ferr != nil {
			panic(ferr)
		}
		h.link.Links().Offer(minIntervalKey, num("900"), h.clock.Now(), ch.SnapshotVersion)
	}
	race.Store(true)
	h.round()
	k, v, _, _, ok := h.link.Links().Next()
	if !ok || k != minIntervalKey || string(v.Value) != "900" {
		t.Fatalf("UC-054/BR-004 pending %v %+v %v, want 900 (the older 800 must not replace it)", k, v, ok)
	}
}

// slotRacer writes the operator's 0.7 on game g1 between the round's slot
// read and the pulled write, once.
type slotRacer struct {
	*api.API
	st   *state.State
	done atomic.Bool
}

func (w *slotRacer) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	if req.Flag == "volume_override" && !w.done.Swap(true) {
		if _, ferr := w.st.Put(state.WriteRequest{Set: "interventions", Flag: "volume_override", Scope: state.Scope{GameID: "g1"}, Value: json.RawMessage("0.7"), Writer: "operator"}); ferr != nil {
			panic(ferr)
		}
	}
	return w.API.FlagsmithPut(ctx, req)
}

// UC: UC-054 3
// A pulled write to an empty game slot (not in the round's snapshot) is
// conditional too: a local write that lands first wins, the pulled one is
// deferred (#2590 P2; #2596 finding 5, delta finding 2, H).
func TestEmptyGameSlotWriteIsConditional(t *testing.T) {
	h := newHarness(t, harnessOpts{
		writer: func(a *api.API, st *state.State) flagsmith.LocalWriter { return &slotRacer{API: a, st: st} },
		before: func(hh *harness) {
			if _, ferr := hh.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0003"}}); ferr != nil {
				t.Fatal(ferr)
			}
		}})
	h.online()
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("volume_override", true, "0.5")}})
	h.round()
	if v, _ := h.local("interventions", "volume_override", "game:g1"); string(v.Value) != "0.7" {
		t.Fatalf("UC-054/3 volume_override on g1 = %+v, want the operator's 0.7 (the pulled write must be 412 slot_changed)", v)
	}
	if _, ok := h.link.Links().Remote(volumeOverrideG1); ok {
		t.Fatal("UC-054/A5 the remote value is known after a deferred apply, want it restored to unknown")
	}
}

// docSwitch serves the environment document itself; when armed, the NEXT
// document request runs fn (a push confirmed while the round is in flight)
// and answers with status (an error) or else with alt.
type docSwitch struct {
	mu     sync.Mutex
	body   []byte
	armed  bool
	fn     func()
	status int
	alt    []byte
}

func (d *docSwitch) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	armed, fn, status, alt, body := d.armed, d.fn, d.status, d.alt, d.body
	d.armed = false
	d.mu.Unlock()
	if armed {
		fn()
		if status != 0 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"detail":"x"}`, status)
			return
		}
		body = alt
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// currentDoc is the document the fake serves now.
func currentDoc(t *testing.T, h *harness) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.fake.URL()+"environment-document/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Environment-Key", "ser.test")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("setup: document %d %s", res.StatusCode, b)
	}
	return b
}

// pushDuringRound runs the deferred-first-apply race of maxGamesRace (remote
// unknown, last document 100) with the push of 10 confirmed while the next
// round's fetch is in flight; that fetch fails with status, or (status 0)
// returns alt, or the current document when alt is nil. The rounds after it
// read the pre-push document (100) again.
func pushDuringRound(t *testing.T, status int, alt func(h *harness) []byte) {
	t.Helper()
	h := maxGamesRace(t, false)
	if _, ok := h.link.Links().Remote(maxGamesKey); ok {
		t.Fatal("setup: remote known, want unknown after the deferred first apply")
	}
	ds := &docSwitch{body: currentDoc(t, h), status: status}
	ds.alt = ds.body
	if alt != nil {
		ds.alt = alt(h)
	}
	ds.fn = func() { h.link.Links().ConfirmPushed(maxGamesKey, num("10")) }
	ds.armed = true
	h.fake.Handle("GET "+flagsmithtest.APIPath+"environment-document/", ds.ServeHTTP)
	h.clock.Advance(time.Minute)
	for range 4 {
		h.round()
	}
	if v, _ := h.local("agent", "experiment_max_games", flagsmith.ScopeStack); string(v.Value) != "10" {
		t.Fatalf("UC-054/BR-005 experiment_max_games = %s: the pre-push document (100) loosened the push of 10 confirmed during a round that read no document", v.Value)
	}
}

// UC: UC-054 BR-005
// A push confirmed during a round that read no document (a 429, an older
// document refused as stale) replaced the last document's value too: after a
// deferred first apply the remote is unknown, so only the last document can
// guard it (#2596 delta 2 finding 1).
func TestGuardPushDuringRoundWithoutDocument(t *testing.T) {
	t.Run("UC-054/BR-005 429", func(t *testing.T) {
		pushDuringRound(t, http.StatusTooManyRequests, nil)
	})
	t.Run("UC-054/BR-005 older document (stale_document)", func(t *testing.T) {
		pushDuringRound(t, 0, func(h *harness) []byte {
			b, err := json.Marshal(document(h.clock.Now().Add(-time.Hour), []map[string]any{fsState("experiment_max_games", true, 100)}))
			if err != nil {
				t.Fatal(err)
			}
			return b
		})
	})
	t.Run("UC-054/BR-005 control: a round that read the document", func(t *testing.T) {
		pushDuringRound(t, 0, nil)
	})
}

// UC: UC-054 2
// A garbled update time (not a finite epoch, beyond year 9999, or more than a
// day ahead of the newest time read and the local clock) is no update time:
// it never pins the newest time, so a later document is not refused as older
// (#2596 delta 2 finding 3).
func TestGarbledUpdateTimeIgnored(t *testing.T) {
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // the harness clock
	for _, hdr := range []string{"NaN", "1e300", "1e11", fmt.Sprintf("%d", start.AddDate(1, 0, 0).Unix())} {
		t.Run("UC-054/2 header "+hdr, func(t *testing.T) {
			var mu sync.Mutex
			header, backend := hdr, "claude"
			h := newHarness(t, harnessOpts{docURL: func(f *flagsmithtest.Fake) string {
				f.Handle("GET /hdr/environment-document/", func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					hv, bv := header, backend
					mu.Unlock()
					doc := document(start, []map[string]any{fsState("decision.backend", true, bv)})
					delete(doc, "updated_at")
					w.Header().Set("X-Flagsmith-Document-Updated-At", hv)
					flagsmithtest.WriteJSON(w, http.StatusOK, doc)
				})
				return f.Server.URL + "/hdr/"
			}})
			h.online()
			h.round()
			if got := h.link.LastModified(); got != "" {
				t.Fatalf("UC-054/2 Last-Modified %q from the garbled header %q, want none", got, hdr)
			}
			mu.Lock()
			header, backend = fmt.Sprintf("%d", start.Add(-30*time.Second).Unix()), "openai"
			mu.Unlock()
			h.round()
			if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "openai" {
				t.Fatalf("UC-054/2 decision.backend = %+v after the garbled header %q: the later document was refused as older", v, hdr)
			}
		})
	}
}
