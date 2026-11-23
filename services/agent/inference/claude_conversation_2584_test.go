package inference

import (
	"context"
	"testing"

	"github.com/joustmania/agent/llm"
)

// UC: UC-042 main
func TestClaude_ConversationIDAndCacheWrite(t *testing.T) {
	for _, tc := range []struct {
		name, id string
	}{{"UC-042/main retro (game g1)", "g1"}, {"UC-042/main proposer (no game)", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMessages{body: messageBody(`{"ok":true}`, "end_turn")}
			c := newTestClaude(t, f, Config{})
			if _, err := c.Infer(context.Background(), llm.Prompt{System: "s", User: "u", ConversationID: tc.id}); err != nil {
				t.Fatal(err)
			}
			a := spanAttrs(lastChatSpan(t))
			v, ok := a["gen_ai.conversation.id"]
			if tc.id == "" && ok {
				t.Errorf("gen_ai.conversation.id = %q, want absent", v.AsString())
			}
			if tc.id != "" && v.AsString() != tc.id {
				t.Errorf("gen_ai.conversation.id = %q (present %v), want %q", v.AsString(), ok, tc.id)
			}
			for _, key := range []string{"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.cache_creation.input_tokens"} {
				if _, ok := a[key]; !ok {
					t.Errorf("%s missing on the chat span", key)
				}
			}
		})
	}
}
