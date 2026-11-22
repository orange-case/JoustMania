package decision

import (
	"context"
	"sync"
	"testing"

	"github.com/joustmania/agent/llm"
)

// UC: UC-042 main
//
// #2584: the retro's model call carries the game as its conversation id, so the
// backend's chat span (gen_ai.conversation.id) joins the game's per-game spend.
func TestRetroCapture_PromptCarriesTheGameAsConversation(t *testing.T) {
	var mu sync.Mutex
	var got []llm.Prompt
	rc, _ := recordingRetro(t, retroFlagSnapshot())
	rc.SetBackend(fakeText("claude-test", func(_ context.Context, p llm.Prompt) (string, error) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		return `{"session_assessment":"ok","suggestions":[],"session_focus":"balanced"}`, nil
	}))
	game := endedSession()
	rc.OnGameEnd(game)
	rc.AwaitInflight()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("model calls = %d, want 1", len(got))
	}
	if got[0].ConversationID != game.SessionID {
		t.Errorf("prompt ConversationID = %q, want the game %q", got[0].ConversationID, game.SessionID)
	}
}
