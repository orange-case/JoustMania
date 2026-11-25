package gamecontext

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// moment_counter_uc041_test.go covers #2658 slice 3: every moment record the agent
// reads is reported to the moment observer (agent_moments_received_total{moment,
// shape,outcome}, telemetry.md §6), with the 7 reachable series: player_death as a
// span event (counted, duplicate), player_death as a log event (counted, duplicate,
// dropped), death_warning as a log event (ignored, dropped).

type momentCall struct{ moment, shape, outcome string }

type momentRecorder struct {
	mu    sync.Mutex
	calls []momentCall
}

func (r *momentRecorder) observe(moment, shape, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, momentCall{moment, shape, outcome})
}

func (r *momentRecorder) take() []momentCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// observedMux is liveMux with a moment observer; game-A is live with player P1.
func observedMux(t *testing.T) (*Multiplexer, *momentRecorder) {
	t.Helper()
	rec := &momentRecorder{}
	clk := &muxClock{t: time.Unix(4000, 0)}
	mux := newTestMux(clk, time.Hour, time.Hour, nil, nil)
	mux.SetMomentObserver(rec.observe)
	mux.ApplyMetrics(gameActiveFor("game-A", 1))
	mux.ApplyMetrics(playerIntensityFor("P1", "game-A", 1.0))
	return mux, rec
}

func requireCalls(t *testing.T, got []momentCall, want ...momentCall) {
	t.Helper()
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observed %v, want %v", got, want)
	}
}

// UC: UC-041 A7
// UC: UC-041 BR-009
func TestMomentObserver_Outcomes(t *testing.T) {
	yes := true

	t.Run("UC-041/BR-009 a span event death is counted, the same moment as a log event is a duplicate", func(t *testing.T) {
		mux, rec := observedMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", &yes))
		requireCalls(t, rec.take(), momentCall{"player_death", "span_event", "counted"})
		mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", &yes))
		requireCalls(t, rec.take(), momentCall{"player_death", "log_event", "duplicate"})
	})

	t.Run("UC-041/A7 a log event death is counted, the same moment as a span event is a duplicate", func(t *testing.T) {
		mux, rec := observedMux(t)
		mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", nil))
		requireCalls(t, rec.take(), momentCall{"player_death", "log_event", "counted"})
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", nil))
		requireCalls(t, rec.take(), momentCall{"player_death", "span_event", "duplicate"})
	})

	t.Run("UC-041/A7 a warning log event with its identity is ignored", func(t *testing.T) {
		mux, rec := observedMux(t)
		mux.ApplyLogs(momentLog("game-coordinator", "death_warning", map[string]string{
			"moment.id": "game-A/P1/1", "player.serial": "P1", "game.id": "game-A",
		}))
		requireCalls(t, rec.take(), momentCall{"death_warning", "log_event", "ignored"})
	})

	t.Run("UC-041/A7 a log event without identity is dropped, one for an unknown game is counted", func(t *testing.T) {
		mux, rec := observedMux(t)
		mux.ApplyLogs(momentLog("game-coordinator", "death_warning", map[string]string{
			"player.serial": "P1", "game.id": "game-A",
		}))
		mux.ApplyLogs(momentLog("game-coordinator", "player_death", map[string]string{
			"moment.id": "game-A/P1/1", "game.id": "game-A",
		}))
		mux.ApplyLogs(deathLog("game-unknown", "P1", "game-unknown/P1/1", nil))
		requireCalls(t, rec.take(),
			momentCall{"death_warning", "log_event", "dropped"},
			momentCall{"player_death", "log_event", "dropped"},
			momentCall{"player_death", "log_event", "counted"},
		)
	})

	t.Run("UC-041/A7 a log record with another event name is not counted", func(t *testing.T) {
		mux, rec := observedMux(t)
		mux.ApplyLogs(momentLog("game-coordinator", "intervention", map[string]string{
			"moment.id": "game-A/P1/1", "player.serial": "P1", "game.id": "game-A",
		}))
		mux.ApplyLogs(momentLog("game-coordinator", "", map[string]string{"game.id": "game-A"}))
		requireCalls(t, rec.take())
	})

	t.Run("UC-041/BR-009 an id-less span event death is counted every time", func(t *testing.T) {
		mux, rec := observedMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "", nil))
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "", nil))
		requireCalls(t, rec.take(),
			momentCall{"player_death", "span_event", "counted"},
			momentCall{"player_death", "span_event", "counted"},
		)
	})

	t.Run("UC-041/BR-008 a game.player_death moment span is not read and not counted", func(t *testing.T) {
		mux, rec := observedMux(t)
		td := momentSpan("game.player_death", "P1", "game-A")
		td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes().PutStr("moment.id", "game-A/P1/1")
		mux.ApplySpans(td)
		requireCalls(t, rec.take())
		snap, _ := mux.Snapshot("game-A")
		if seq := snap.Session.EliminationSequence; len(seq) != 0 {
			t.Fatalf("elimination seq = %v, want none: the span shape leaves the order to the gauge", seq)
		}
	})

	t.Run("UC-041/BR-009 no observer set: nothing panics", func(t *testing.T) {
		mux, _ := liveMux(t)
		mux.ApplySpans(deathSpanEvent("game-A", "P1", "game-A/P1/1", nil))
		mux.ApplyLogs(deathLog("game-A", "P1", "game-A/P1/1", nil))
	})
}
