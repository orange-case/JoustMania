package claude

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 main
func TestUsage_ClaudeRecordsTheCall(t *testing.T) {
	api := newFakeAPI(t, 200, message("end_turn", textBlock(answerJSON)), 0)
	b := api.backend("claude-haiku-4-5", false)
	usage := &decide.Usage{}
	if _, err := b.Decide(decide.WithUsage(context.Background(), usage), testRequest()); err != nil {
		t.Fatal(err)
	}
	got := usage.Totals()
	want := decide.UsageTotals{
		Calls:                 1,
		InputTokens:           decide.Tokens(900), // 800 + cache read 100 + cache creation 0
		OutputTokens:          decide.Tokens(150),
		CacheReadInputTokens:  decide.Tokens(100),
		CacheWriteInputTokens: decide.Tokens(0),
		FinishReasons:         []string{"end_turn"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("totals = %+v, want %+v", got, want)
	}

	body := api.requestBody(t)
	s := b.RequestSettings()
	if float64(s.MaxTokens) != body["max_tokens"].(float64) {
		t.Errorf("RequestSettings max_tokens = %d, request sends %v", s.MaxTokens, body["max_tokens"])
	}
	if s.Temperature == nil || *s.Temperature != body["temperature"].(float64) {
		t.Errorf("RequestSettings temperature = %v, request sends %v", s.Temperature, body["temperature"])
	}
}

// UC: UC-040 A2
func TestUsage_ClaudeFailedCallAddsOnlyItsFinishReason(t *testing.T) {
	api := newFakeAPI(t, 500, []byte(`{"type":"error","error":{"type":"api_error","message":"x"}}`), 0)
	usage := &decide.Usage{}
	_, _ = api.backend("claude-haiku-4-5", false).Decide(decide.WithUsage(context.Background(), usage), testRequest())
	got := usage.Totals()
	if got.Calls == 0 || got.InputTokens.OK || got.OutputTokens.OK {
		t.Errorf("totals = %+v, want calls but no token counts", got)
	}
	if !reflect.DeepEqual(got.FinishReasons, []string{"error"}) {
		t.Errorf("finish reasons = %v, want [error]", got.FinishReasons)
	}
}

// UC: UC-040 A2
// A refusal or max_tokens cut-off keeps its tokens; a timeout has none.
func TestUsage_ClaudeAnsweredFailuresKeepTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   []byte
		reason string
	}{
		{"UC-040/A2 refusal", refusalMessage("cyber"), "refusal"},
		{"UC-040/A2 max_tokens", message("max_tokens", textBlock(answerJSON[:40])), "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, 200, tc.body, 0)
			usage := &decide.Usage{}
			if _, err := api.backend("claude-haiku-4-5", false).Decide(decide.WithUsage(context.Background(), usage), testRequest()); err == nil {
				t.Fatal("Decide succeeded")
			}
			got := usage.Totals()
			if got.InputTokens != decide.Tokens(900) || got.OutputTokens != decide.Tokens(150) {
				t.Errorf("tokens = %+v / %+v, want 900 / 150 (the model answered)", got.InputTokens, got.OutputTokens)
			}
			if !reflect.DeepEqual(got.FinishReasons, []string{tc.reason}) {
				t.Errorf("finish reasons = %v, want [%s]", got.FinishReasons, tc.reason)
			}
		})
	}

	t.Run("UC-040/A2 timeout", func(t *testing.T) {
		api := newFakeAPI(t, 200, message("end_turn", textBlock(answerJSON)), 2*time.Second)
		b := api.backend("claude-haiku-4-5", false)
		b.cfg.Timeout = 100 * time.Millisecond
		usage := &decide.Usage{}
		if _, err := b.Decide(decide.WithUsage(context.Background(), usage), testRequest()); err == nil {
			t.Fatal("Decide succeeded")
		}
		got := usage.Totals()
		if got.InputTokens.OK || got.OutputTokens.OK {
			t.Errorf("tokens on a timeout: %+v", got)
		}
		if !reflect.DeepEqual(got.FinishReasons, []string{"error"}) {
			t.Errorf("finish reasons = %v, want [error]", got.FinishReasons)
		}
	})
}
