package gamecontext

import (
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// moment_logs_uc041_test.go covers #2658 slice 2 (FR-218): the agent reads the
// death_warning and player_death moments as OTLP log events (UC-041 A7) and counts
// each moment once per moment id, across span events and log events (UC-041 BR-009,
// UC-100 BR-005).

// momentLog builds a log batch with one record from service, named eventName, with
// the given string attributes (lib/moments.py record_moment shape: event_name and
// body = the moment name, attributes moment.id, player.serial, game.id, game.kind).
func momentLog(service, eventName string, attrs map[string]string) plog.Logs {
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

// deathLog is a player_death moment log event of serial in gameID; eliminated nil
// leaves death.eliminated out.
func deathLog(gameID, serial, id string, eliminated *bool) plog.Logs {
	ld := momentLog("game-coordinator", "player_death", map[string]string{
		"moment.id": id, "player.serial": serial, "game.id": gameID, "game.kind": "real",
	})
	if eliminated != nil {
		lr := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
		lr.Attributes().PutBool("death.eliminated", *eliminated)
	}
	return ld
}

// deathSpanEvent is a player span of serial in gameID with one player_death event;
// id "" leaves moment.id out (a coordinator before #2658 slice 3), eliminated nil
// leaves death.eliminated out.
func deathSpanEvent(gameID, serial, id string, eliminated *bool) ptrace.Traces {
	td := spanWith(map[string]string{"player.serial": serial, "game.id": gameID}, nil)
	ev := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Events().AppendEmpty()
	ev.SetName("player_death")
	if id != "" {
		ev.Attributes().PutStr("moment.id", id)
	}
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

// UC: UC-041 A7
func TestApplyLogs_MomentLogEvents(t *testing.T) {
	no := false

	t.Run("UC-041/A7 a player_death log event adds the player to the elimination sequence", func(t *testing.T) {
		mux, _ := liveMux(t)
		ids := mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", nil))
		if len(ids) != 1 || ids[0] != "game-A" {
			t.Fatalf("touched = %v, want [game-A]", ids)
		}
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 1 || seq[0] != "P1" {
			t.Fatalf("elimination seq = %v, want [P1]", seq)
		}
	})

	t.Run("UC-041/A7 a player_death log event with death.eliminated false is no elimination", func(t *testing.T) {
		mux, _ := liveMux(t)
		ids := mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", &no))
		if len(ids) != 1 || ids[0] != "game-A" {
			t.Fatalf("touched = %v, want [game-A] (a round loss is still read)", ids)
		}
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 0 {
			t.Fatalf("elimination seq = %v, want none for a round loss (BR-008)", seq)
		}
	})

	t.Run("UC-041/A7 a death_warning log event changes nothing", func(t *testing.T) {
		mux, clk := liveMux(t)
		before, _ := mux.Snapshot("game-A")
		clk.advance(time.Second)
		ids := mux.ApplyLogs(momentLog("game-coordinator", "death_warning", map[string]string{
			"moment.id": "game-A/P1/1", "player.serial": "P1", "game.id": "game-A", "game.kind": "real",
		}))
		if ids != nil {
			t.Fatalf("touched = %v, want nil", ids)
		}
		after, _ := mux.Snapshot("game-A")
		if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("UpdatedAt moved %v -> %v", before.UpdatedAt, after.UpdatedAt)
		}
		if len(after.Players) != len(before.Players) || len(after.Session.EliminationSequence) != 0 {
			t.Fatalf("context changed: players %d -> %d, seq %v", len(before.Players), len(after.Players), after.Session.EliminationSequence)
		}
		requirePartitions(t, mux, 1)
	})

	t.Run("UC-041/A7 a log event without moment id, game id or player changes nothing", func(t *testing.T) {
		full := map[string]string{"moment.id": "game-A/P1/1", "player.serial": "P1", "game.id": "game-A"}
		for _, missing := range []string{"moment.id", "player.serial", "game.id"} {
			for _, empty := range []bool{false, true} {
				name := fmt.Sprintf("%s absent", missing)
				if empty {
					name = fmt.Sprintf("%s empty", missing)
				}
				t.Run(name, func(t *testing.T) {
					mux, _ := liveMux(t)
					attrs := map[string]string{}
					for k, v := range full {
						if k != missing {
							attrs[k] = v
						} else if empty {
							attrs[k] = ""
						}
					}
					if ids := mux.ApplyLogs(momentLog("game-coordinator", "player_death", attrs)); ids != nil {
						t.Fatalf("touched = %v, want nil", ids)
					}
					snap, _ := mux.Snapshot("game-A")
					if seq := snap.Session.EliminationSequence; len(seq) != 0 {
						t.Fatalf("elimination seq = %v, want none", seq)
					}
					requirePartitions(t, mux, 1)
				})
			}
		}
	})

	// Catches existingStore -> store: a death for a game without context must not
	// create one (nor feed the fallback partition).
	t.Run("UC-041/A7 a log event for a game without context changes nothing and creates no context", func(t *testing.T) {
		mux, _ := liveMux(t)
		if ids := mux.ApplyLogs(deathLog("game-Z", "P9", "game-Z/P9/1", nil)); ids != nil {
			t.Fatalf("touched = %v, want nil", ids)
		}
		requirePartitions(t, mux, 1)
		if _, ok := mux.Snapshot("game-Z"); ok {
			t.Fatal("game-Z partition was created by a log event")
		}
	})

	t.Run("UC-041/A7 other log records are ignored", func(t *testing.T) {
		mux, _ := liveMux(t)
		for _, name := range []string{"", "intervention", "game.player_death"} {
			ld := momentLog("game-coordinator", name, map[string]string{
				"moment.id": "game-A:P1:x:1", "player.serial": "P1", "game.id": "game-A",
			})
			if ids := mux.ApplyLogs(ld); ids != nil {
				t.Fatalf("%q: touched = %v, want nil", name, ids)
			}
		}
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 0 {
			t.Fatalf("elimination seq = %v, want none", seq)
		}
	})

	t.Run("UC-041/A7 the agent's own log events are skipped", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.SetOwnService("agent")
		ld := deathLog("game-A", "P1", "game-A/P1/1", nil)
		ld.ResourceLogs().At(0).Resource().Attributes().PutStr("service.name", "agent")
		if ids := mux.ApplyLogs(ld); ids != nil {
			t.Fatalf("touched = %v, want nil for the agent's own telemetry", ids)
		}
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 0 {
			t.Fatalf("elimination seq = %v, want none", seq)
		}
	})
}

// UC: UC-041 BR-009
// UC: UC-100 BR-005
func TestMoments_CountOncePerMomentID(t *testing.T) {
	no, yes := false, true

	t.Run("UC-041/BR-009 a span event then a log event of one moment counts once", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", nil))
		if ids := mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", nil)); ids != nil {
			t.Fatalf("touched = %v, want nil: the moment was read as a span event", ids)
		}
		snap, _ := mux.Snapshot("game-A")
		if n := eliminationEvents(snap.Timeline); n != 1 {
			t.Fatalf("elimination events = %d, want 1", n)
		}
	})

	// The first record does not eliminate and the second would: only a dedupe by
	// moment.id (not the serial dedupe of the elimination sequence) keeps it out.
	t.Run("UC-041/BR-009 a log event then a span event of one moment counts once", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", &no))
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", &yes))
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 0 {
			t.Fatalf("elimination seq = %v, want none: the span event repeats a moment read as a log event", seq)
		}
	})

	t.Run("UC-041/BR-009 a span event then a log event with another verdict changes nothing", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", &no))
		if ids := mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", &yes)); ids != nil {
			t.Fatalf("touched = %v, want nil: the moment was read as a span event", ids)
		}
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 0 {
			t.Fatalf("elimination seq = %v, want none: the first record applies", seq)
		}
	})

	t.Run("UC-041/BR-009 two moments of one player both apply", func(t *testing.T) {
		mux, _ := liveMux(t)
		for i, id := range []string{"game-A/P1/1", "game-A/P1/2"} {
			if ids := mux.ApplyLogs(deathLog("game-A", "P1", id, &no)); len(ids) != 1 {
				t.Fatalf("moment %d: touched = %v, want [game-A]", i+1, ids)
			}
		}
		if ids := mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/3", &yes)); len(ids) != 1 {
			t.Fatalf("moment 3: touched = %v, want [game-A]", ids)
		}
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 1 || seq[0] != "P1" {
			t.Fatalf("elimination seq = %v, want [P1] from the third moment", seq)
		}
	})

	t.Run("UC-041/BR-009 a player_death span event without a moment id counts as before", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "", &no))
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "", &yes))
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 1 || seq[0] != "P1" {
			t.Fatalf("elimination seq = %v, want [P1]: an id-less death is never deduped", seq)
		}
	})
}

// UC: UC-041 BR-009
func TestBoundedIDs(t *testing.T) {
	t.Run("UC-041/BR-009 a present id is not added again", func(t *testing.T) {
		var b boundedIDs
		b.cap = 4
		if !b.add("a") {
			t.Fatal("first add = false, want true")
		}
		if b.add("a") {
			t.Fatal("second add = true, want false")
		}
	})
	t.Run("UC-041/BR-009 the set keeps at most momentIDsCap ids, oldest out first", func(t *testing.T) {
		b := boundedIDs{cap: momentIDsCap}
		for i := 0; i <= momentIDsCap; i++ {
			b.add(fmt.Sprintf("m%d", i))
		}
		if len(b.set) != momentIDsCap || len(b.order) != momentIDsCap {
			t.Fatalf("size = %d/%d, want %d", len(b.set), len(b.order), momentIDsCap)
		}
		if b.has("m0") {
			t.Fatal("m0 still present after the 1025th id")
		}
		if !b.has(fmt.Sprintf("m%d", momentIDsCap)) || !b.has("m1") {
			t.Fatal("newest or second id missing")
		}
		if momentIDsCap != 1024 {
			t.Fatalf("momentIDsCap = %d, want 1024 (UC-041 BR-009)", momentIDsCap)
		}
	})
}
