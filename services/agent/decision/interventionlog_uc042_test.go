package decision

import (
	"context"
	"fmt"
	"testing"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
)

// UC: UC-042 main step 3
func TestInterventionLog_NotesEveryOutcome(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-042/main step 3 applied and blocked decisions are noted with target and backend", func(t *testing.T) {
		l, _, clock, _ := budgetLoop(t, 100)
		log := NewInterventionLog()
		l.SetInterventionLog(log)
		c := groupCtx("shadow", "A", "B")
		var st LayerState
		st.DecideBackend = "jev"
		l.runDecision(ctx, groupSnapshot(groupPolicy()), c, groupDecision("A", "B"), *clock, &st)
		blocked := groupPolicy()
		blocked.MaxInterventionsPerMinute = 0
		l.runDecision(ctx, groupSnapshot(blocked), c, groupDecision("A", "B"), *clock, &st)

		got := log.Take(c.SessionID)
		if len(got.Records) != 2 {
			t.Fatalf("records = %+v, want 2", got.Records)
		}
		if r := got.Records[0]; r.Outcome != NotedApplied || r.Action != InterventionGrantGroupShield || r.Target != "A,B" || r.Backend != "jev" || !r.At.Equal(*clock) {
			t.Errorf("applied record = %+v", r)
		}
		if r := got.Records[1]; r.Outcome != NotedBlocked {
			t.Errorf("blocked record = %+v", r)
		}
		if fmt.Sprint(got.Backends) != "[jev]" {
			t.Errorf("backends = %v", got.Backends)
		}
	})

	t.Run("UC-042/main step 3 a held answer is listed and a noop answer counted", func(t *testing.T) {
		l, _, _, _ := budgetLoop(t, 100)
		log := NewInterventionLog()
		l.SetInterventionLog(log)
		c := groupCtx("real", "A", "B")
		var st LayerState

		resp, _ := ffaAnswer("play_audio_cue", 0.5, "none", 0.6, "maybe")(decide.Request{})
		l.decideRemoteApply(ctx, flags.Snapshot{}, c, true, &decideRecord{used: "claude", resp: resp, threshold: 0.9}, &st)
		resp, _ = ffaAnswer(decide.OptionNoop, 0.9, "none", 0, "calm")(decide.Request{})
		l.decideRemoteApply(ctx, flags.Snapshot{}, c, true, &decideRecord{used: "claude", resp: resp, threshold: 0.9}, &st)

		got := log.Take(c.SessionID)
		if len(got.Records) != 1 || got.Records[0].Outcome != NotedHeld || got.Records[0].Backend != "claude" {
			t.Fatalf("records = %+v, want one held", got.Records)
		}
		if got.Held != 1 || got.Noop != 1 {
			t.Errorf("held/noop = %d/%d, want 1/1", got.Held, got.Noop)
		}
	})

	t.Run("UC-042/main step 3 the list is capped and a taken game is forgotten", func(t *testing.T) {
		log := NewInterventionLog()
		for i := 0; i < maxInterventionLogEntries+3; i++ {
			log.note("g", InterventionRecord{Action: "x", Outcome: NotedHeld, Backend: "rules"})
		}
		got := log.Take("g")
		if len(got.Records) != maxInterventionLogEntries || got.Dropped != 3 || got.Held != maxInterventionLogEntries+3 {
			t.Fatalf("records/dropped/held = %d/%d/%d", len(got.Records), got.Dropped, got.Held)
		}
		log.note("g", InterventionRecord{Action: "late"})
		if again := log.Take("g"); len(again.Records) != 0 {
			t.Fatalf("a note after Take must be ignored, got %+v", again)
		}
	})
}
