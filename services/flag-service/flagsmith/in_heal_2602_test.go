package flagsmith_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith"
	"github.com/joustmania/flag-service/state"
)

// The stale-document self-heal (architecture note §4.2 step 6) while a cache
// or region keeps serving the copy stamped with the too-new update time
// (#2602, probes of the #2596 delta-4 review). staleWedge (in_guard_test.go)
// reads that copy first (2100, agent/enabled on) and then serves a current
// document with enabled off; the harness polls every hour, so the refusal
// times out after 10 h.

var tooNew = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// serveTooNew serves the too-new copy again (enabled on).
func serveTooNew(h *harness) {
	h.fake.SetDocument(document(tooNew, []map[string]any{fsState("enabled", true, "on")}), "")
}

// serveCurrent serves a current document (enabled off), stamped now.
func serveCurrent(h *harness) {
	h.fake.SetDocument(document(h.clock.Now(), []map[string]any{fsState("enabled", true, "off")}), "")
}

func enabledVariant(h *harness) string {
	v, _ := h.local("agent", "enabled", flagsmith.ScopeStack)
	return v.Variant
}

// UC: UC-054 BR-005
// Probe 1, "the heal never fires": the too-new copy served once every 4 h is
// not newer than the newest time, so it no longer ends the run of refusals;
// the current document is accepted once the run has lasted 10 h.
func TestTooNewCopyDoesNotEndRefusalRun(t *testing.T) {
	h := staleWedge(t)
	h.round()
	wantStaleRefusal(t, h, "first refusal")
	for i := 1; i <= 2; i++ {
		h.clock.Advance(4 * time.Hour)
		serveTooNew(h)
		h.round()
		serveCurrent(h)
		h.round()
		wantStaleRefusal(t, h, "refusals for about "+[]string{"", "4", "8"}[i]+" h with the too-new copy in between")
	}
	h.clock.Advance(4 * time.Hour)
	serveTooNew(h)
	h.round()
	serveCurrent(h)
	h.round()
	if a := h.lastPull(); a[flagsmith.AttrReason] == flagsmith.ReasonStaleDocument {
		t.Fatalf("UC-054/BR-005 pull span %v after 12 h of refusals with the too-new copy served every 4 h: the copy keeps ending the run, the heal never fires", a)
	}
	if got := enabledVariant(h); got != "off" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %q after the heal, want off (the operator's kill in Flagsmith)", got)
	}
}

// UC: UC-054 BR-005
// Probe 2, "kill switches flap": after the heal the too-new copy is refused
// as stale_document, also after another 10 h of it alone (never accepted by
// the timeout), so the kill applied by the heal stays. A document strictly
// newer than the abandoned time is accepted normally.
func TestAbandonedCopyRefusedAfterHeal(t *testing.T) {
	h := staleWedge(t)
	h.round()
	wantStaleRefusal(t, h, "first refusal")
	h.clock.Advance(10 * time.Hour)
	h.round()
	if got := enabledVariant(h); got != "off" {
		t.Fatalf("setup: agent/enabled = %q, want off after the heal", got)
	}
	for _, wait := range []time.Duration{time.Hour, 11 * time.Hour} {
		h.clock.Advance(wait)
		serveTooNew(h)
		h.round()
		if a := h.lastPull(); a[flagsmith.AttrReason] != flagsmith.ReasonStaleDocument {
			t.Fatalf("UC-054/BR-005 pull span %v: the abandoned too-new copy was accepted %v after the heal, want stale_document", a, wait)
		}
		if got := enabledVariant(h); got != "off" {
			t.Fatalf("UC-054/BR-005 agent/enabled = %q: the abandoned too-new copy undid the kill (flap)", got)
		}
	}
	h.fake.SetDocument(document(tooNew.Add(time.Second), []map[string]any{fsState("enabled", true, "on")}), "")
	h.round()
	if got := enabledVariant(h); got != "on" {
		t.Fatalf("UC-054/main step 2 agent/enabled = %q: a document newer than the abandoned time was refused", got)
	}
}

// UC: UC-054 BR-005
// Probe 3, "re-push in the heal round" (5 replaced by 20): an agent-limiting
// kill switch at stage (30), an operator sets conservative (3) in Flagsmith
// while the refusal run lasts, and a lagging copy that still shows stage
// answers the heal round. Since #2621 that round merges with the remote value,
// so where the local value equals it the round changes nothing and queues no
// push over the operator's conservative, which applies with the next document.
func TestHealRoundDoesNotPushOverFlagsmith(t *testing.T) {
	const flag = "llm.max_requests_per_minute"
	key := flagsmith.Key{Set: "agent", Flag: flag, Scope: flagsmith.ScopeStack}
	h := newHarness(t, harnessOpts{before: func(hh *harness) {
		if _, ferr := hh.st.Put(state.WriteRequest{Set: "agent", Flag: flag, Variant: "stage", Writer: "operator"}); ferr != nil {
			t.Fatal(ferr)
		}
		hh.fake.SetDocument(document(tooNew, []map[string]any{fsState(flag, true, "stage")}), "")
	}})
	h.online()
	h.round()
	if h.link.Links().HasPending(key) {
		t.Fatalf("setup: %s pending before the refusal run, want in sync", flag)
	}
	start := h.clock.Now()
	h.fake.SetDocument(document(start, []map[string]any{fsState(flag, true, "conservative")}), "")
	h.round()
	if a := h.lastPull(); a[flagsmith.AttrReason] != flagsmith.ReasonStaleDocument {
		t.Fatalf("setup: pull span %v, want stale_document", a)
	}
	h.clock.Advance(10 * time.Hour)
	h.fake.SetDocument(document(start.Add(-time.Hour), []map[string]any{fsState(flag, true, "stage")}), "")
	h.round()
	if a := h.lastPull(); a[flagsmith.AttrReason] == flagsmith.ReasonStaleDocument {
		t.Fatalf("setup: pull span %v, want the lagging copy accepted by the heal", a)
	}
	if h.link.Links().HasPending(key) {
		t.Fatalf("UC-054/BR-005 the heal round queued stage for Flagsmith over the operator's conservative")
	}
	h.fake.SetDocument(document(start, []map[string]any{fsState(flag, true, "conservative")}), "")
	h.round()
	if v, _ := h.local("agent", flag, flagsmith.ScopeStack); v.Variant != "conservative" {
		t.Fatalf("UC-054/main step 2 %s = %+v after the heal, want the operator's conservative", flag, v)
	}
}

// UC: UC-054 BR-005
// The newest time is genuine (an operator's kill, stamped T) and a lagging
// cache answers now and then with an older copy (enabled on): the correct
// copy read on two full pulls in a row ends each run, so the lagging copy is
// never accepted by the timeout and T is never abandoned (#2602, opposer
// finding of #2732: two lagging hits 10 h apart must not undo the kill).
func TestGenuineNewestEndsRunOnRepeatedReads(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.online()
	genuine := h.clock.Now()
	serveKill := func() {
		// With its Last-Modified: a conditional pull would be a 304, no read.
		h.fake.SetDocument(document(genuine, []map[string]any{fsState("enabled", true, "off")}), genuine.UTC().Format(http.TimeFormat))
	}
	serveLagging := func() {
		h.fake.SetDocument(document(genuine.Add(-time.Hour), []map[string]any{fsState("enabled", true, "on")}), "")
	}
	serveKill()
	h.round()
	if got := enabledVariant(h); got != "off" {
		t.Fatalf("setup: agent/enabled = %q, want off", got)
	}
	for i := range 3 {
		serveLagging()
		h.round()
		wantRefusedLagging(t, h, i)
		serveKill()
		h.round()
		h.round()
		h.clock.Advance(6 * time.Hour)
	}
	serveKill()
	h.round()
	if a := h.lastPull(); a[flagsmith.AttrReason] == flagsmith.ReasonStaleDocument {
		t.Fatalf("UC-054/BR-005 pull span %v: the genuine newest document was refused (abandoned)", a)
	}
	if got := enabledVariant(h); got != "off" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %q at the end, want off", got)
	}
}

func wantRefusedLagging(t *testing.T, h *harness, i int) {
	t.Helper()
	if a := h.lastPull(); a[flagsmith.AttrReason] != flagsmith.ReasonStaleDocument {
		t.Fatalf("UC-054/BR-005 lagging hit %d, %v: pull span %v, want stale_document (the run should have ended with the genuine reads)", i+1, time.Duration(i)*6*time.Hour, a)
	}
	if got := enabledVariant(h); got != "off" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %q after lagging hit %d: the lagging copy undid the kill", got, i+1)
	}
}

// UC: UC-054 BR-005
// An abandoned copy read while a later run is open is refused and neither
// ends nor restarts that run: the run's older document is accepted once the
// run has lasted 10 h.
func TestAbandonedCopyInsideRunKeepsRun(t *testing.T) {
	h := staleWedge(t)
	h.round()
	h.clock.Advance(10 * time.Hour)
	h.round() // the heal: 2100 abandoned, the current document is the newest
	if got := enabledVariant(h); got != "off" {
		t.Fatalf("setup: agent/enabled = %q, want off after the heal", got)
	}
	accepted := h.clock.Now()
	// A second skewed time ahead of the accepted one opens a new run.
	h.fake.SetDocument(document(accepted.Add(24*time.Hour), []map[string]any{fsState("enabled", true, "off")}), "")
	h.round()
	h.fake.SetDocument(document(accepted.Add(time.Minute), []map[string]any{fsState("enabled", true, "on")}), "")
	h.round()
	wantStaleOn := func(when string) {
		t.Helper()
		if a := h.lastPull(); a[flagsmith.AttrReason] != flagsmith.ReasonStaleDocument {
			t.Fatalf("UC-054/BR-005 %s: pull span %v, want stale_document", when, a)
		}
	}
	wantStaleOn("run opened")
	h.clock.Advance(6 * time.Hour)
	serveTooNew(h)
	h.round()
	wantStaleOn("the abandoned copy inside the run")
	h.fake.SetDocument(document(accepted.Add(time.Minute), []map[string]any{fsState("enabled", true, "on")}), "")
	h.clock.Advance(4 * time.Hour)
	h.round()
	if a := h.lastPull(); a[flagsmith.AttrReason] == flagsmith.ReasonStaleDocument {
		t.Fatalf("UC-054/BR-005 pull span %v after 10 h: the abandoned copy inside the run restarted it", a)
	}
	if got := enabledVariant(h); got != "on" {
		t.Fatalf("UC-054/BR-005 agent/enabled = %q, want on (the run's document accepted)", got)
	}
}
