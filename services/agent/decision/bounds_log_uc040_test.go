package decision

import (
	"context"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// bounds_log_uc040_test.go: the loop notes the magnitude table of each decide
// request per game, so the post-game retro catalog shows the ranges the game decided
// with (#2696).

func TestBoundsLog_NotesTheRequestsTable(t *testing.T) {
	t.Run("UC-040/BR-045 a decide request's table is the game's latest", func(t *testing.T) {
		log := NewBoundsLog()
		l := &Loop{}
		l.SetBoundsLog(log)
		want := widened()
		l.SetMagnitudeBoundsSource(func(context.Context, string) *decide.MagnitudeTable { return want })
		l.rulesRequestBuilderCtx(context.Background(), remoteContext(), []string{"grant_shield"})(nil, nil)
		got, ok := log.Last(remoteContext().SessionID)
		if !ok || got != want {
			t.Fatalf("log has %v (ok %v), want the request's table", got, ok)
		}
		if _, ok := log.Last("other"); ok {
			t.Fatal("a game without a request has no table")
		}
	})
	t.Run("UC-040/BR-045 the log forgets the oldest game past its bound", func(t *testing.T) {
		log := NewBoundsLog()
		for i := 0; i <= maxBoundsLogGames; i++ {
			log.Note(string(rune('a'+i%26))+string(rune('A'+i/26)), decide.ParseMagnitudeOverride(nil))
		}
		if _, ok := log.Last("aA"); ok {
			t.Fatal("the oldest game must be forgotten")
		}
		var nilLog *BoundsLog
		nilLog.Note("g", nil)
		if _, ok := nilLog.Last("g"); ok {
			t.Fatal("a nil log holds nothing")
		}
	})
}
