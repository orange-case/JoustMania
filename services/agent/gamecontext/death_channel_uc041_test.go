package gamecontext

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// death_channel_uc041_test.go covers UC-041 BR-008 and BR-009: the agent learns of
// an elimination only from game_player_elimination_order; no booth moment record
// (span event, span or log event, any telemetry.moment_shape) changes its context.

// logEvent builds a log batch with one record from service, named eventName, with
// the given string attributes.
func logEvent(service, eventName string, attrs map[string]string) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	if service != "" {
		rl.Resource().Attributes().PutStr("service.name", service)
	}
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetEventName(eventName)
	lr.Body().SetStr(eventName)
	for k, v := range attrs {
		lr.Attributes().PutStr(k, v)
	}
	return ld
}

// deathLog is a player_death moment log event of serial in gameID.
func deathLog(gameID, serial, id string) plog.Logs {
	return logEvent("game-coordinator", "player_death", map[string]string{
		"moment.id": id, "player.serial": serial, "game.id": gameID, "game.kind": "real",
	})
}

// deathSpanEvent is a player span of serial in gameID with one player_death event;
// eliminated nil leaves death.eliminated out.
func deathSpanEvent(gameID, serial, id string, eliminated *bool) ptrace.Traces {
	td := spanWith(map[string]string{"player.serial": serial, "game.id": gameID}, nil)
	ev := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Events().AppendEmpty()
	ev.SetName("player_death")
	ev.Attributes().PutStr("moment.id", id)
	if eliminated != nil {
		ev.Attributes().PutBool("death.eliminated", *eliminated)
	}
	return td
}

// eliminationEvents counts the EventElimination entries of a timeline.
func eliminationEvents(tl []TimelineEvent) int {
	n := 0
	for _, e := range tl {
		if e.Kind == EventElimination {
			n++
		}
	}
	return n
}

// liveMux is a multiplexer with game-A started and its player P1 known.
func liveMux(t *testing.T) (*Multiplexer, *muxClock) {
	t.Helper()
	clk := &muxClock{t: time.Unix(4000, 0)}
	mux := newTestMux(clk, time.Hour, time.Hour, nil, nil)
	mux.ApplyMetrics(gameActiveFor("game-A", 1))
	mux.ApplyMetrics(playerIntensityFor("P1", "game-A", 1.0))
	return mux, clk
}

func requirePartitions(t *testing.T, mux *Multiplexer, want int) {
	t.Helper()
	if got := mux.Partitions(); len(got) != want {
		t.Fatalf("partitions = %v, want %d", got, want)
	}
}

func requireNoElimination(t *testing.T, mux *Multiplexer) {
	t.Helper()
	snap, _ := mux.Snapshot("game-A")
	if seq := snap.Session.EliminationSequence; len(seq) != 0 {
		t.Fatalf("elimination seq = %v, want none", seq)
	}
	if n := eliminationEvents(snap.Timeline); n != 0 {
		t.Fatalf("elimination events = %d, want 0", n)
	}
}

// UC: UC-041 BR-008 BR-009
// UC: UC-100 BR-007
func TestDeaths_OneChannel(t *testing.T) {
	no, yes := false, true

	t.Run("UC-041/BR-009 a player_death span event is no elimination, whatever death.eliminated says", func(t *testing.T) {
		for _, el := range []*bool{nil, &no, &yes} {
			mux, _ := liveMux(t)
			mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", el))
			requireNoElimination(t, mux)
		}
	})

	t.Run("UC-041/BR-009 a player_death log event changes nothing and opens no context", func(t *testing.T) {
		mux, clk := liveMux(t)
		before, _ := mux.Snapshot("game-A")
		clk.advance(time.Second)
		mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1"))
		mux.ApplyLogs(deathLog("game-Z", "P9", "game-Z/P9/1"))
		requireNoElimination(t, mux)
		requirePartitions(t, mux, 1)
		after, _ := mux.Snapshot("game-A")
		if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("UpdatedAt moved %v -> %v", before.UpdatedAt, after.UpdatedAt)
		}
	})

	t.Run("UC-041/BR-008 the elimination-order gauge is the elimination", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", nil))
		requireNoElimination(t, mux)
		ids := mux.ApplyMetrics(eliminationOrderFor("P1", "game-A", 1))
		if len(ids) != 1 || ids[0] != "game-A" {
			t.Fatalf("touched = %v, want [game-A]", ids)
		}
		// Re-exported every interval: one elimination, narrated once.
		mux.ApplyMetrics(eliminationOrderFor("P1", "game-A", 1))
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 1 || seq[0] != "P1" {
			t.Fatalf("elimination seq = %v, want [P1]", seq)
		}
		if n := eliminationEvents(snap.Timeline); n != 1 {
			t.Fatalf("elimination events = %d, want 1", n)
		}
	})
}

// UC: UC-041 BR-005
func TestBoundedIDs(t *testing.T) {
	t.Run("UC-041/BR-005 a present id is not added again", func(t *testing.T) {
		b := boundedIDs{cap: 4}
		if !b.add("a") {
			t.Fatal("first add = false, want true")
		}
		if b.add("a") {
			t.Fatal("second add = true, want false")
		}
	})
	t.Run("UC-041/BR-005 the set keeps at most cap ids, oldest out first", func(t *testing.T) {
		b := boundedIDs{cap: 3}
		for _, id := range []string{"m0", "m1", "m2", "m3"} {
			b.add(id)
		}
		if len(b.set) != 3 || len(b.order) != 3 {
			t.Fatalf("size = %d/%d, want 3", len(b.set), len(b.order))
		}
		if b.has("m0") || !b.has("m1") || !b.has("m3") {
			t.Fatal("want m0 forgotten, m1 and m3 kept")
		}
	})
}
