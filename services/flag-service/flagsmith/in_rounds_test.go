package flagsmith_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/api"
	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/state"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Round rules of the in side (architecture note §4.2, #2590 plan §8): push
// guards, health per refused-key set, the restart rule, kill switches first,
// the write budget, Last-Modified, backoff, 429 and segment overrides.

// UC: UC-054 A4
// A refusal that ends without an apply (the operator's new value is pushed and
// the document shows it) turns the feature healthy again (#2590 P7).
func TestHealthClearsWhenRefusalEnds(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	k := flagsmith.Key{Set: "agent", Flag: "decision.min_interval_ms", Scope: flagsmith.ScopeStack}
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 5)})
	h.round()
	h.waitFor("an unhealthy post", func() bool { return len(h.healthPosts()) == 1 })
	if _, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "decision.min_interval_ms", Value: json.RawMessage("700"), Writer: "operator"}); ferr != nil {
		t.Fatal(ferr)
	}
	h.link.Links().ConfirmPushed(k, flagsmith.Value{Present: true, Value: json.RawMessage("700")})
	h.clock.Advance(2 * time.Minute)
	h.setDoc([]map[string]any{fsState("decision.min_interval_ms", true, 700)})
	h.round()
	h.waitFor("a healthy post", func() bool { return len(h.healthPosts()) == 2 })
	if p := h.healthPosts()[1]; p["status"] != "healthy" {
		t.Fatalf("UC-054/A4 second post %v, want healthy", p)
	}
	if st := h.link.Status(); st.Refused != 0 {
		t.Fatalf("UC-054/A4 refused %d after the refusal ended, want 0", st.Refused)
	}
}

// UC: UC-054 BR-005
// A push guard never blocks a later change made in Flagsmith: when the
// environment updated_at does not move past a confirmed push, a new document
// value is still applied (#2596 finding 1a).
func TestPushGuardLetsNewValueThrough(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	t0 := h.clock.Now()
	k := flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}
	if _, ferr := h.st.Put(state.WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "claude", Writer: "operator"}); ferr != nil {
		t.Fatal(ferr)
	}
	h.link.Links().ConfirmPushed(k, flagsmith.Value{Present: true, Variant: "claude", Value: json.RawMessage(`"claude"`)})
	h.fake.SetDocument(document(t0.Add(-time.Minute), []map[string]any{fsState("decision.backend", true, "openai")}), "")
	h.clock.Advance(time.Hour)
	for range 3 {
		h.round()
	}
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "openai" {
		t.Fatalf("UC-054/BR-005 decision.backend = %+v an hour later, want openai (the guard skips only the replaced value)", v)
	}
}

// UC: UC-054 main step 4
// After the out side pushed the event list, a stale document that still holds
// the payload object appends nothing (#2590 P1, event key).
func TestStaleEventObjectNotAppendedTwice(t *testing.T) {
	h := newHarness(t, harnessOpts{before: func(h *harness) {
		if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0003"}}); ferr != nil {
			t.Fatal(ferr)
		}
	}})
	h.online()
	k := flagsmith.Key{Set: "interventions", Flag: "eliminate_player", Scope: "game:g1"}
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("eliminate_player", true, `{"serial":"MOCK0003"}`)}})
	h.round()
	list, _ := h.local("interventions", "eliminate_player", "game:g1")
	h.link.Links().ConfirmPushed(k, list)
	for range 2 {
		h.round()
	}
	if n := eventCount(h, "game:g1"); n != 1 {
		t.Fatalf("UC-054/BR-004 %d events: the stale payload was appended again after the list push", n)
	}
}

func eventCount(h *harness, scope string) int {
	v, _ := h.local("interventions", "eliminate_player", scope)
	var list []json.RawMessage
	_ = json.Unmarshal(v.Value, &list)
	return len(list)
}

// UC: UC-054 BR-005
// Kill switches go first and a round applies at most 50 writes (#2590 P8):
// with 60 game:serial changes and a kill switch whose set sorts last, the
// kill switch and 49 others apply in the first round, the rest in the next.
func TestKillSwitchFirstAndWriteBudget(t *testing.T) {
	var serials []string
	for i := range 30 {
		serials = append(serials, fmt.Sprintf("MOCK%04d", 100+i))
	}
	remediation := flagsmith.Key{Set: "rollout", Flag: "remediation_allowed", Scope: flagsmith.ScopeStack}
	h := newHarness(t, harnessOpts{before: func(h *harness) {
		if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: serials}); ferr != nil {
			t.Fatal(ferr)
		}
		if _, ferr := h.st.Put(state.WriteRequest{Set: "rollout", Flag: "remediation_allowed", Variant: "on", Writer: "operator"}); ferr != nil {
			t.Fatal(ferr)
		}
		h.fake.SetDocument(document(h.clock.Now().Add(-time.Minute), []map[string]any{fsState("remediation_allowed", true, "on")}), "")
	}})
	h.online()
	if r, ok := h.link.Links().Remote(remediation); !ok || r.Variant != "on" {
		t.Fatalf("setup: remote %+v %v, want on", r, ok)
	}
	var ovs []override
	for _, s := range serials {
		ovs = append(ovs, override{"game:g1:" + s, []map[string]any{
			fsState("player_sensitivity_factor", true, "1.5"),
			fsState("player_handicap_factor", true, "1.5"),
		}})
	}
	h.setDoc([]map[string]any{fsState("remediation_allowed", true, "off")}, ovs...)
	h.round()
	if v, _ := h.local("rollout", "remediation_allowed", flagsmith.ScopeStack); v.Variant != "off" {
		t.Fatalf("UC-054/BR-005 rollout/remediation_allowed = %+v after the first round, want off (kill switches first)", v)
	}
	if n := serialValues(h, serials); n != 49 {
		t.Fatalf("NFR-050 %d game:serial values after the first round, want 49 (50 writes, the kill switch included)", n)
	}
	h.round()
	if n := serialValues(h, serials); n != 60 {
		t.Fatalf("NFR-050 %d game:serial values after the second round, want 60", n)
	}
}

func serialValues(h *harness, serials []string) int {
	changes, _ := h.st.Changes()
	want := map[string]bool{}
	for _, s := range serials {
		want["game:g1:"+s] = true
	}
	n := 0
	for _, c := range changes {
		k, v, ok := flagsmith.ValueOfChange(c)
		if ok && v.Present && want[k.Scope] {
			n++
		}
	}
	return n
}

// UC: UC-054 BR-006
// The restart rule (#2590 P10, P13, R6): the first round after start skips
// the keys the rule cleared; later, a value an API key set on an admin
// setting or on a game identity is not brought back (the local value wins and
// is queued), a person's value is applied, also on a game that registers after
// the restart.
func TestRestartRuleAttribution(t *testing.T) {
	interval := flagsmith.Key{Set: "agent", Flag: "decision.min_interval_ms", Scope: flagsmith.ScopeStack}
	backend := flagsmith.Key{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}
	apiOverride := flagsmith.Key{Set: "interventions", Flag: "global_sensitivity_override", Scope: "game:g7"}
	personOverride := flagsmith.Key{Set: "interventions", Flag: "volume_override", Scope: "game:g7"}
	h := newHarness(t, harnessOpts{before: func(h *harness) {
		h.link.SetRestartReport(flagsmith.RestartAuditLog, map[flagsmith.Key]flagsmith.Author{
			interval: flagsmith.AuthorAPIKey, backend: flagsmith.AuthorAPIKey,
			apiOverride: flagsmith.AuthorAPIKey, personOverride: flagsmith.AuthorPerson,
		})
		h.fake.SetDocument(document(h.clock.Now().Add(-time.Minute),
			[]map[string]any{fsState("decision.min_interval_ms", true, 700), fsState("decision.backend", true, "claude")},
			override{"game:g7", []map[string]any{fsState("global_sensitivity_override", true, 2), fsState("volume_override", true, "0.5")}},
		), "")
	}})
	h.online()
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) == "700" {
		t.Fatal("UC-054/BR-006 the start round applied a key the restart rule cleared (P13: edge lag)")
	}
	h.round()
	if v, _ := h.local("agent", "decision.min_interval_ms", flagsmith.ScopeStack); string(v.Value) != "700" {
		t.Fatalf("UC-054/main step 3 decision.min_interval_ms = %+v after the start round, want 700 (not an admin setting)", v)
	}
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant == "claude" {
		t.Fatal("UC-054/BR-006 decision.backend: an API key's value on an admin setting was brought back after a restart")
	}
	if r, ok := h.link.Links().Remote(backend); !ok || r.Variant != "claude" {
		t.Fatalf("UC-054/BR-006 remote %+v %v, want claude seeded so the local value is pushed", r, ok)
	}
	if _, ferr := h.st.RegisterGame(state.Game{ID: "g7", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0001"}}); ferr != nil {
		t.Fatal(ferr)
	}
	h.round()
	if v, _ := h.local("interventions", "volume_override", "game:g7"); string(v.Value) != "0.5" {
		t.Fatalf("UC-054/BR-006 a person's game override = %+v after the game registered, want 0.5", v)
	}
	if v, _ := h.local("interventions", "global_sensitivity_override", "game:g7"); v.Present {
		t.Fatalf("UC-054/BR-006 an API key's game override was brought back: %+v", v)
	}
	if r, ok := h.link.Links().Remote(apiOverride); !ok || string(r.Value) != "2" {
		t.Fatalf("UC-054/BR-006 remote %+v %v, want 2 seeded so the out side deletes the override", r, ok)
	}
	if st := h.link.Status(); st.Pending != 2 {
		t.Fatalf("UC-054/BR-006 pending %d, want 2 (decision.backend and the API key's override)", st.Pending)
	}
}

// UC: UC-054 A3
// The start reconcile span records the restart rule and how many values it
// clears (telemetry.md §10.4): the API key's values, never a person's, a kill
// switch (BR-005) or an event-flag override (one-shot).
func TestReconcileSpanRecordsRestartRule(t *testing.T) {
	t.Run("UC-054/A3 BR-006 the start reconcile span carries restart_rule and cleared", func(t *testing.T) {
		h := newHarness(t, harnessOpts{before: func(h *harness) {
			h.link.SetRestartReport(flagsmith.RestartAuditLog, map[flagsmith.Key]flagsmith.Author{
				{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}:         flagsmith.AuthorAPIKey,
				{Set: "interventions", Flag: "global_sensitivity_override", Scope: "game:g7"}: flagsmith.AuthorAPIKey,
				{Set: "agent", Flag: "enabled", Scope: flagsmith.ScopeStack}:                  flagsmith.AuthorAPIKey,
				{Set: "interventions", Flag: "audio_cue", Scope: "game:g7"}:                   flagsmith.AuthorAPIKey,
				{Set: "agent", Flag: "decision.min_interval_ms", Scope: flagsmith.ScopeStack}: flagsmith.AuthorPerson,
				{Set: "interventions", Flag: "volume_override", Scope: "game:g7"}:             flagsmith.AuthorUnknown,
			})
		}})
		h.online()
		spans := h.ended(flagsmith.SpanReconcile)
		if len(spans) != 1 {
			t.Fatalf("%d reconcile spans, want 1", len(spans))
		}
		got := map[string]string{}
		for _, kv := range spans[0].Attributes() {
			got[string(kv.Key)] = kv.Value.String()
		}
		if got[flagsmith.AttrTrigger] != flagsmith.TriggerStart {
			t.Fatalf("trigger %q, want start", got[flagsmith.AttrTrigger])
		}
		if got[flagsmith.AttrRestartRule] != flagsmith.RestartAuditLog {
			t.Fatalf("UC-054/A3 %s = %q, want %q", flagsmith.AttrRestartRule, got[flagsmith.AttrRestartRule], flagsmith.RestartAuditLog)
		}
		if got[flagsmith.AttrCleared] != "2" {
			t.Fatalf("UC-054/BR-006 %s = %q, want 2 (the stack and the game value; not the kill switch, the event, a person's)", flagsmith.AttrCleared, got[flagsmith.AttrCleared])
		}
	})
	t.Run("UC-054/A3 BR-006 a reconnect reconcile does not record the restart rule", func(t *testing.T) {
		h := newHarness(t, harnessOpts{before: func(h *harness) {
			h.link.SetRestartReport(flagsmith.RestartAuditLog, map[flagsmith.Key]flagsmith.Author{
				{Set: "agent", Flag: "decision.backend", Scope: flagsmith.ScopeStack}: flagsmith.AuthorAPIKey,
			})
		}})
		h.online()
		h.fake.Fail(http.StatusBadGateway)
		h.round() // the pull fails: the link goes offline and reconnects
		h.fake.Fail(0)
		triggerOf := func(s sdktrace.ReadOnlySpan) string {
			for _, kv := range s.Attributes() {
				if string(kv.Key) == flagsmith.AttrTrigger {
					return kv.Value.String()
				}
			}
			return ""
		}
		h.waitFor("a reconnect reconcile", func() bool {
			for _, s := range h.ended(flagsmith.SpanReconcile) {
				if triggerOf(s) == flagsmith.TriggerReconnect {
					return true
				}
			}
			return false
		})
		for _, s := range h.ended(flagsmith.SpanReconcile) {
			if triggerOf(s) == flagsmith.TriggerStart {
				continue
			}
			for _, kv := range s.Attributes() {
				if k := string(kv.Key); k == flagsmith.AttrCleared || k == flagsmith.AttrRestartRule {
					t.Fatalf("UC-054/BR-006 the %s reconcile span carries %s (telemetry.md §10.4: the start reconcile only)", triggerOf(s), k)
				}
			}
		}
	})
	t.Run("UC-054/A3 without a restart report the span has no restart attributes", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		for _, s := range h.ended(flagsmith.SpanReconcile) {
			for _, kv := range s.Attributes() {
				if k := string(kv.Key); k == flagsmith.AttrCleared || k == flagsmith.AttrRestartRule {
					t.Fatalf("%s set without a restart report", k)
				}
			}
		}
	})
}

// UC: UC-054 main step 2
// Last-Modified (§4.2 step 8, #2590 P6): a body older than its Last-Modified
// header is a stale copy, and a body whose second has not passed may still
// change: both make the next pull unconditional.
func TestLastModifiedForcesFullPull(t *testing.T) {
	lastIMS := func(h *harness) string {
		r := h.docRequests()
		return r[len(r)-1].Header.Get("If-Modified-Since")
	}
	t.Run("UC-054/main step 2 stale body with a fresh header", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		hdr := h.clock.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
		h.fake.SetDocument(document(h.clock.Now().Add(-time.Minute), []map[string]any{fsState("decision.backend", true, "claude")}), hdr)
		h.round() // changed
		h.round() // unchanged, the body older than its header
		h.round()
		if got := lastIMS(h); got != "" {
			t.Fatalf("UC-054/main step 2 pull after a stale body sent If-Modified-Since %q", got)
		}
	})
	t.Run("UC-054/main step 2 the document's second has not passed", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.online()
		h.fake.SetDocument(document(h.clock.Now().Add(time.Hour), []map[string]any{fsState("decision.backend", true, "claude")}), "")
		h.round() // changed
		h.round() // unchanged, updated_at not in the past
		h.round()
		if got := lastIMS(h); got != "" {
			t.Fatalf("UC-054/main step 2 pull in the document's second sent If-Modified-Since %q", got)
		}
	})
	t.Run("UC-054/main step 2 the update time from the header when the body has none", func(t *testing.T) {
		start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) // the harness clock
		upd := start.Add(-90 * time.Second)
		doc := document(start, []map[string]any{fsState("decision.backend", true, "claude")})
		delete(doc, "updated_at")
		h := newHarness(t, harnessOpts{docURL: func(f *flagsmithtest.Fake) string {
			f.Handle("GET /hdr/environment-document/", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Flagsmith-Document-Updated-At", fmt.Sprintf("%d.250000", upd.Unix()))
				flagsmithtest.WriteJSON(w, http.StatusOK, doc)
			})
			return f.Server.URL + "/hdr/"
		}})
		h.online()
		h.round()
		if got, want := h.link.LastModified(), upd.UTC().Format(http.TimeFormat); got != want {
			t.Fatalf("UC-054/main step 2 Last-Modified %q, want %q from X-Flagsmith-Document-Updated-At", got, want)
		}
	})
	t.Run("UC-054/main step 2 a header that is not a float epoch is no update time", func(t *testing.T) {
		start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		doc := document(start, []map[string]any{fsState("decision.backend", true, "claude")})
		delete(doc, "updated_at")
		h := newHarness(t, harnessOpts{docURL: func(f *flagsmithtest.Fake) string {
			f.Handle("GET /hdr/environment-document/", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Flagsmith-Document-Updated-At", start.Add(-90*time.Second).Format(time.RFC3339Nano))
				flagsmithtest.WriteJSON(w, http.StatusOK, doc)
			})
			return f.Server.URL + "/hdr/"
		}})
		h.online()
		h.round()
		if got := h.link.LastModified(); got != "" {
			t.Fatalf("UC-054/main step 2 Last-Modified %q from an RFC 3339 header, want none (the header is a float epoch)", got)
		}
	})
}

// countingWriter counts the pulled writes of one flag.
type countingWriter struct {
	*api.API
	flag string
	n    atomic.Int32
}

func (w *countingWriter) FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error) {
	if req.Flag == w.flag {
		w.n.Add(1)
	}
	return w.API.FlagsmithPut(ctx, req)
}

// UC: UC-054 A5
// A deferred key backs off (1, 2, 4 ... s) and its feature turns unhealthy
// after 5 deferrals in a row; applied later, it turns healthy (#2590 P14).
func TestDeferralBackoffAndHealth(t *testing.T) {
	var cw *countingWriter
	h := newHarness(t, harnessOpts{noStream: true, writer: func(a *api.API, _ *state.State) flagsmith.LocalWriter {
		cw = &countingWriter{API: a, flag: "decision.backend"}
		return cw
	}})
	h.online()
	h.setDoc([]map[string]any{fsState("decision.backend", true, "claude")})
	for i := 1; i <= 4; i++ {
		h.clock.Advance(time.Minute)
		h.round()
		if got := int(cw.n.Load()); got != i {
			t.Fatalf("UC-054/A5 %d write attempts after %d rounds, want %d", got, i, i)
		}
	}
	h.round() // 2 s later: inside the 8 s backoff
	if got := cw.n.Load(); got != 4 {
		t.Fatalf("UC-054/A5 %d write attempts: a round inside the backoff tried again", got)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(h.healthPosts()); n != 0 {
		t.Fatalf("UC-054/A5 %d health posts after 4 deferrals, want 0", n)
	}
	h.clock.Advance(time.Minute)
	h.round()
	h.waitFor("an unhealthy post after 5 deferrals", func() bool { return len(h.healthPosts()) == 1 })
	if p := h.healthPosts()[0]; p["status"] != "unhealthy" || !strings.Contains(string(mustJSON(p["reason"])), "deferred 5 times") {
		t.Fatalf("UC-054/A5 health post %v", p)
	}
	h.sub, _ = h.st.Subscribe("")
	h.clock.Advance(time.Minute)
	h.round()
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant != "claude" {
		t.Fatalf("UC-054/A5 decision.backend = %+v once flagd connected, want claude", v)
	}
	h.waitFor("a healthy post", func() bool { return len(h.healthPosts()) == 2 })
}

// UC: UC-054 main step 2
// A 429 on the document is honoured: the next pull waits for Retry-After
// (#2590 P8).
func TestDocumentRetryAfter(t *testing.T) {
	var limited atomic.Bool
	doc := document(time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC), nil)
	h := newHarness(t, harnessOpts{docURL: func(f *flagsmithtest.Fake) string {
		f.Handle("GET /limited/environment-document/", func(w http.ResponseWriter, _ *http.Request) {
			if limited.Load() {
				w.Header().Set("Retry-After", "30")
				http.Error(w, `{"detail":"slow down"}`, http.StatusTooManyRequests)
				return
			}
			flagsmithtest.WriteJSON(w, http.StatusOK, doc)
		})
		return f.Server.URL + "/limited/"
	}})
	h.online()
	limited.Store(true)
	h.round()
	limited.Store(false)
	_, mark := h.longestSleep(0)
	n := h.pulls()
	h.clock.Advance(2 * time.Second)
	h.link.Wake()
	h.waitFor("the pull after the 429", func() bool { return h.pulls() > n })
	if d, _ := h.longestSleep(mark); d < 25*time.Second {
		t.Fatalf("UC-054/main step 2 the pull after a 429 waited %s, want the Retry-After (30 s)", d)
	}
}

// UC: UC-054 main step 4
// A document event list that is a prefix of the local list is a stale value:
// nothing is appended and nothing is refused (#2590 P9).
func TestEventPrefixIsNoOp(t *testing.T) {
	h := newHarness(t, harnessOpts{before: func(h *harness) {
		if _, ferr := h.st.RegisterGame(state.Game{ID: "g1", CoordinatorInstance: "c1", Kind: "shadow", Serials: []string{"MOCK0003", "MOCK0004"}}); ferr != nil {
			t.Fatal(ferr)
		}
	}})
	h.online()
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("eliminate_player", true, `{"serial":"MOCK0003"}`)}})
	h.round()
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("eliminate_player", true, `{"serial":"MOCK0004"}`)}})
	h.round()
	v, _ := h.local("interventions", "eliminate_player", "game:g1")
	var list []json.RawMessage
	if err := json.Unmarshal(v.Value, &list); err != nil || len(list) != 2 {
		t.Fatalf("setup: event list %s, want 2 events", v.Value)
	}
	prefix, _ := json.Marshal(list[:1])
	h.setDoc(nil, override{"game:g1", []map[string]any{fsState("eliminate_player", true, string(prefix))}})
	h.round()
	if n := eventCount(h, "game:g1"); n != 2 {
		t.Fatalf("UC-054/main step 4 %d events after a prefix document, want 2", n)
	}
	if st := h.link.Status(); st.Refused != 0 {
		t.Fatalf("UC-054/main step 4 refused %d after a prefix document, want 0 (a no-op)", st.Refused)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(h.healthPosts()); n != 0 {
		t.Fatalf("UC-054/main step 4 %d health posts after a prefix document, want 0", n)
	}
}

// UC: UC-054 A4
// A segment override on a synced feature is not applied: one unhealthy post
// for the feature; removed, the feature turns healthy (#2590 P15).
func TestSegmentOverrideIsUnhealthy(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	seg := fsState("decision.backend", true, "claude")
	seg["feature_segment"] = map[string]any{"segment": 7, "priority": 1}
	h.setDoc([]map[string]any{seg})
	h.round()
	h.waitFor("an unhealthy post", func() bool { return len(h.healthPosts()) == 1 })
	if p := h.healthPosts()[0]; p["feature"] != "decision.backend" || p["status"] != "unhealthy" ||
		!strings.Contains(string(mustJSON(p["reason"])), "segment overrides are not applied") {
		t.Fatalf("UC-054/A4 health post %v", p)
	}
	if v, _ := h.local("agent", "decision.backend", flagsmith.ScopeStack); v.Variant == "claude" {
		t.Fatal("UC-054/A4 a segment override was applied")
	}
	h.round()
	time.Sleep(50 * time.Millisecond)
	if n := len(h.healthPosts()); n != 1 {
		t.Fatalf("UC-054/A4 %d health posts, want 1 (one per transition)", n)
	}
	h.setDoc(nil)
	h.round()
	h.waitFor("a healthy post", func() bool { return len(h.healthPosts()) == 2 })
}
