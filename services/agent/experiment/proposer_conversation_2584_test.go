package experiment

import (
	"context"
	"testing"
)

// UC: UC-042 main
//
// #2584: the proposer works across games, so its model call carries no
// conversation id (its chat span has no gen_ai.conversation.id).
func TestProposeOnce_PromptHasNoConversationID(t *testing.T) {
	backend := &capturingBackend{}
	p, _, _, _ := proposerWithRecorder(t, backend)
	p.ProposeOnce(context.Background(), Narrative{GameMode: "joust"}, ProposerConfig{Permit: allowPropose})
	if len(backend.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1", len(backend.prompts))
	}
	if id := backend.prompts[0].ConversationID; id != "" {
		t.Errorf("proposer ConversationID = %q, want empty (cross-game)", id)
	}
}
