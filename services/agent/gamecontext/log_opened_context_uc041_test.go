package gamecontext

import (
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

// log_opened_context_uc041_test.go covers #2722 (FR-251): a moment log event or the
// agent's game.start / game.end log event for a game the agent does not know yet
// opens that game's context and holds it (UC-041 A7 step 5, A8, BR-010).

// lifecycleLog is an agent-only game.start or game.end record of gameID at ts
// (zero leaves the timestamp out).
func lifecycleLog(eventName, gameID, kind, mode string, ts time.Time) plog.Logs {
	attrs := map[string]string{"game.id": gameID, "orangecase.audience": "agent"}
	if kind != "" {
		attrs["game.kind"] = kind
	}
	if mode != "" {
		attrs["game.mode"] = mode
	}
	ld := momentLog("game-coordinator", eventName, attrs)
	if !ts.IsZero() {
		ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).SetTimestamp(pcommon.NewTimestampFromTime(ts))
	}
	return ld
}

// UC: UC-041 A7
// UC: UC-041 BR-010
func TestApplyLogs_MomentOpensUnknownGame(t *testing.T) {
	yes := true

	t.Run("UC-041/A7 a player_death log event before any span or metric of its game counts", func(t *testing.T) {
		rec := &momentRecorder{}
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.SetMomentObserver(rec.observe)

		ids := mux.ApplyLogs(deathLog("game-B", "P1", "game-B/P1/1", &yes))
		if len(ids) != 1 || ids[0] != "game-B" {
			t.Fatalf("touched = %v, want [game-B]", ids)
		}
		requireCalls(t, rec.take(), momentCall{"player_death", "log_event", "counted"})
		snap, ok := mux.Snapshot("game-B")
		if !ok {
			t.Fatal("no game-B context")
		}
		if snap.GameKind != GameKindReal {
			t.Fatalf("GameKind = %q, want %q", snap.GameKind, GameKindReal)
		}
		if len(snap.Players) != 0 || (snap.Session.GameActive != nil && *snap.Session.GameActive) {
			t.Fatalf("a log record added players %v or set the game active", snap.Players)
		}

		// The game's metrics and its span event arrive later.
		clk.advance(2 * time.Second)
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		mux.ApplyMetrics(playerIntensityFor("P1", "game-B", 1.0))
		mux.ApplySpans(deathSpanEvent("game-B", "P1", "game-B/P1/1", &yes))
		requireCalls(t, rec.take(), momentCall{"player_death", "span_event", "duplicate"})
		snap, _ = mux.Snapshot("game-B")
		if seq := snap.Session.EliminationSequence; len(seq) != 1 || seq[0] != "P1" {
			t.Fatalf("elimination seq = %v, want [P1] kept across game_active 1", seq)
		}
		if n := eliminationEvents(snap.Timeline); n != 1 {
			t.Fatalf("elimination events = %d, want 1", n)
		}
	})

	t.Run("UC-041/A7 a legacy primary kind opens the context as real, an unknown kind fails closed", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.ApplyLogs(momentLog("game-coordinator", "death_warning", map[string]string{
			"moment.id": "game-B/P1/1", "player.serial": "P1", "game.id": "game-B", "game.kind": "primary",
		}))
		mux.ApplyLogs(momentLog("game-coordinator", "death_warning", map[string]string{
			"moment.id": "game-C/P1/1", "player.serial": "P1", "game.id": "game-C", "game.kind": "odd",
		}))
		if snap, _ := mux.Snapshot("game-B"); snap.GameKind != GameKindReal {
			t.Fatalf("game-B kind = %q, want real", snap.GameKind)
		}
		if snap, _ := mux.Snapshot("game-C"); snap.GameKind != "odd" {
			t.Fatalf("game-C kind = %q, want the unknown kind passed through", snap.GameKind)
		}
	})
}

// UC: UC-041 BR-010
func TestApplyLogs_LogHold(t *testing.T) {
	t.Run("UC-041/BR-010 a log-opened context survives eviction for 60 s, then drains", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.ApplyLogs(deathLog("game-B", "P1", "game-B/P1/1", nil))

		clk.advance(logHoldFor - time.Second)
		if removed := mux.EvictStale(); len(removed) != 0 {
			t.Fatalf("removed %v within the hold", removed)
		}
		clk.advance(time.Second)
		if removed := mux.EvictStale(); len(removed) != 1 || removed[0] != "game-B" {
			t.Fatalf("removed = %v, want [game-B] once the hold is over", removed)
		}
		if logHoldFor != 60*time.Second {
			t.Fatalf("logHoldFor = %v, want 60s (UC-041 BR-010)", logHoldFor)
		}
	})

	t.Run("UC-041/BR-010 a later log record does not extend the hold", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.ApplyLogs(deathLog("game-B", "P1", "game-B/P1/1", nil))
		clk.advance(30 * time.Second)
		mux.ApplyLogs(deathLog("game-B", "P2", "game-B/P2/1", nil))
		clk.advance(30 * time.Second)
		if removed := mux.EvictStale(); len(removed) != 1 {
			t.Fatalf("removed = %v, want [game-B] 60 s after the opening record", removed)
		}
	})

	t.Run("UC-041/BR-010 a metric within the hold continues the context", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.ApplyLogs(deathLog("game-B", "P1", "game-B/P1/1", nil))
		clk.advance(10 * time.Second)
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		mux.ApplyMetrics(playerIntensityFor("P1", "game-B", 1.0))
		clk.advance(2 * time.Minute)
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		mux.ApplyMetrics(playerIntensityFor("P1", "game-B", 1.0))
		if removed := mux.EvictStale(); len(removed) != 0 {
			t.Fatalf("removed %v: a live game must stay", removed)
		}
		snap, _ := mux.Snapshot("game-B")
		if seq := snap.Session.EliminationSequence; len(seq) != 1 || seq[0] != "P1" {
			t.Fatalf("elimination seq = %v, want [P1] from the log record", seq)
		}
	})

	t.Run("UC-041/BR-010 a log record for an ended game id recreates a context that stays ended", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		rec := &endRecorder{}
		mux := newEndMux(clk, rec)
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		mux.ApplyMetrics(gameActiveFor("game-B", 0))
		clk.advance(time.Minute)
		mux.EvictStale()
		if hasPartition(mux, "game-B") {
			t.Fatal("game-B not drained")
		}
		mux.ApplyLogs(deathLog("game-B", "P1", "game-B/P1/1", nil))
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		snap, _ := mux.Snapshot("game-B")
		if snap.Session.GameActive != nil && *snap.Session.GameActive {
			t.Fatal("an ended game started again")
		}
		if got := rec.count(); got != 1 {
			t.Fatalf("OnGameEnd fired %d times, want 1", got)
		}
	})
}

// UC: UC-041 A8
// UC: UC-041 BR-005
func TestApplyLogs_GameStartAndEnd(t *testing.T) {
	t.Run("UC-041/A8 game.start opens the context with mode and kind, not active and with no player", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		if ids := mux.ApplyLogs(lifecycleLog("game.start", "game-B", "shadow", "NonStop", time.Time{})); ids != nil {
			t.Fatalf("touched = %v, want nil: game.start wakes no evaluation", ids)
		}
		snap, ok := mux.Snapshot("game-B")
		if !ok {
			t.Fatal("no game-B context")
		}
		if snap.GameKind != GameKindShadow || snap.Session.GameMode == nil || *snap.Session.GameMode != "NonStop" {
			t.Fatalf("kind/mode = %q/%v, want shadow/NonStop", snap.GameKind, snap.Session.GameMode)
		}
		if snap.Session.GameActive != nil || len(snap.Players) != 0 || !snap.UpdatedAt.IsZero() {
			t.Fatalf("game.start made the game look live: active %v players %d updated %v",
				snap.Session.GameActive, len(snap.Players), snap.UpdatedAt)
		}
		clk.advance(logHoldFor - time.Second)
		if removed := mux.EvictStale(); len(removed) != 0 {
			t.Fatalf("removed %v within the hold", removed)
		}
	})

	t.Run("UC-041/A8 game.start without the agent marker or game id is ignored", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		mux.ApplyLogs(momentLog("game-coordinator", "game.start", map[string]string{"game.id": "game-B"}))
		mux.ApplyLogs(lifecycleLog("game.start", "", "real", "Joust", time.Time{}))
		mux.ApplyLogs(momentLog("game-coordinator", "game.end", map[string]string{"game.id": "game-C"}))
		requirePartitions(t, mux, 0)
	})

	t.Run("UC-041/A8 game.end ends the game once with end_event at the record's time", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		rec := &endRecorder{}
		mux := newEndMux(clk, rec)
		mux.ApplyLogs(lifecycleLog("game.start", "game-B", "real", "Joust", time.Time{}))
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		clk.advance(30 * time.Second)
		endAt := clk.now().Add(-2 * time.Second)
		clk.advance(time.Second)

		mux.ApplyLogs(lifecycleLog("game.end", "game-B", "real", "", endAt))
		if got := rec.count(); got != 1 {
			t.Fatalf("OnGameEnd fired %d times, want 1", got)
		}
		end := rec.last()
		if end.EndReason != EndReasonEndEvent {
			t.Fatalf("EndReason = %q, want %q", end.EndReason, EndReasonEndEvent)
		}
		if d := end.Session.DurationSeconds; d == nil || *d != 28 {
			t.Fatalf("duration = %v, want 28 s (bracket closed at the record's time)", d)
		}

		// The later end signals change nothing, and the ended id stays ended.
		mux.ApplyMetrics(gameActiveFor("game-B", 0))
		mux.ApplySpans(rootSpanFor("game-B", clk.now()))
		mux.ApplyLogs(lifecycleLog("game.end", "game-B", "real", "", clk.now()))
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		if got := rec.count(); got != 1 {
			t.Fatalf("OnGameEnd fired %d times, want 1", got)
		}
		clk.advance(2 * time.Minute)
		mux.EvictStale()
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		if snap, _ := mux.Snapshot("game-B"); snap.Session.GameActive != nil && *snap.Session.GameActive {
			t.Fatal("the ended game started again after eviction")
		}
	})

	t.Run("UC-041/A8 game.end without a timestamp closes the bracket on arrival", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		rec := &endRecorder{}
		mux := newEndMux(clk, rec)
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		clk.advance(12 * time.Second)
		mux.ApplyLogs(lifecycleLog("game.end", "game-B", "", "", time.Time{}))
		if d := rec.last().Session.DurationSeconds; d == nil || *d != 12 {
			t.Fatalf("duration = %v, want 12 s", d)
		}
	})

	t.Run("UC-041/A8 a game.end after another end signal changes nothing", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		rec := &endRecorder{}
		mux := newEndMux(clk, rec)
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		mux.ApplyMetrics(gameActiveFor("game-B", 0))
		mux.ApplyLogs(lifecycleLog("game.end", "game-B", "", "", clk.now()))
		if rec.count() != 1 || rec.last().EndReason != EndReasonZero {
			t.Fatalf("ends = %d reason %q, want one %q end", rec.count(), rec.last().EndReason, EndReasonZero)
		}
	})

	t.Run("UC-041/A8 a game.end for an unknown game latches it, so a late game_active 1 never starts it", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		rec := &endRecorder{}
		mux := newEndMux(clk, rec)
		mux.ApplyLogs(lifecycleLog("game.end", "game-B", "real", "", clk.now()))
		if got := rec.count(); got != 0 {
			t.Fatalf("OnGameEnd fired %d times for a game never seen active, want 0", got)
		}
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		if snap, _ := mux.Snapshot("game-B"); snap.Session.GameActive != nil && *snap.Session.GameActive {
			t.Fatal("a late game_active 1 started an ended game")
		}
		clk.advance(logHoldFor + time.Second)
		mux.EvictStale()
		if hasPartition(mux, "game-B") {
			t.Fatal("game-B not drained after the hold")
		}
		mux.ApplyMetrics(gameActiveFor("game-B", 1))
		if snap, _ := mux.Snapshot("game-B"); snap.Session.GameActive != nil && *snap.Session.GameActive {
			t.Fatal("a game_active 1 after eviction started an ended game")
		}
	})
}

// UC: UC-041 BR-010
func TestApplyLogs_LogOpenRacesEviction(t *testing.T) {
	t.Run("UC-041/BR-010 a death read while eviction runs is never lost to a removed partition", func(t *testing.T) {
		clk := &muxClock{t: time.Unix(4000, 0)}
		mux := newTestMux(clk, 5*time.Second, 15*time.Second, nil, nil)
		var wg sync.WaitGroup
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					mux.EvictStale()
				}
			}
		}()
		for i := 0; i < 200; i++ {
			id := "game-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			if ids := mux.ApplyLogs(deathLog(id, "P1", id+"/P1/1", nil)); len(ids) != 1 {
				t.Errorf("%s: touched = %v", id, ids)
			}
			if _, ok := mux.Snapshot(id); !ok {
				t.Errorf("%s: partition removed within its hold", id)
			}
		}
		close(stop)
		wg.Wait()
	})
}
