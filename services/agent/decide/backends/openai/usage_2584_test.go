package openai

import (
	"context"
	"reflect"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 main
//
// #2584: the openai call reports its usage to the invocation's accumulator (the
// values on its chat span), and the backend's request settings are the ones it
// sends.
func TestUsage_OpenAIRecordsTheCall(t *testing.T) {
	api := newFakeAPI(t, fixed(200, completion("stop", answerJSON, nil)))
	b := api.backend("gpt-4.1-mini", false)
	usage := &decide.Usage{}
	if _, err := b.Decide(decide.WithUsage(context.Background(), usage), testRequest()); err != nil {
		t.Fatal(err)
	}
	got := usage.Totals()
	want := decide.UsageTotals{
		Calls:                 1,
		InputTokens:           decide.Tokens(900),
		OutputTokens:          decide.Tokens(150),
		CacheReadInputTokens:  decide.Tokens(100),
		ReasoningOutputTokens: decide.Tokens(40),
		FinishReasons:         []string{"stop"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("totals = %+v, want %+v", got, want)
	}

	body := api.body(0)
	s := b.RequestSettings()
	if float64(s.MaxTokens) != body["max_completion_tokens"].(float64) {
		t.Errorf("RequestSettings max_tokens = %d, request sends %v", s.MaxTokens, body["max_completion_tokens"])
	}
	if s.Temperature == nil || *s.Temperature != body["temperature"].(float64) {
		t.Errorf("RequestSettings temperature = %v, request sends %v", s.Temperature, body["temperature"])
	}
}

// UC: UC-040 main
//
// #2584: the json_schema -> json_object retry is two calls: the refused first
// (no response: "error", no tokens) and the answered second, summed in order.
func TestUsage_OpenAIJSONObjectRetrySumsBothCalls(t *testing.T) {
	api := newFakeAPI(t, func(body map[string]any) reply {
		if body["response_format"].(map[string]any)["type"] == "json_schema" {
			return reply{status: 400, body: []byte(`{"error":{"message":"response_format json_schema is not supported","type":"invalid_request_error"}}`)}
		}
		return reply{status: 200, body: completion("stop", answerJSON, nil)}
	})
	usage := &decide.Usage{}
	if _, err := api.backend("llama-3.3-70b", false).Decide(decide.WithUsage(context.Background(), usage), testRequest()); err != nil {
		t.Fatal(err)
	}
	got := usage.Totals()
	if got.Calls != 2 || got.InputTokens != decide.Tokens(900) || got.OutputTokens != decide.Tokens(150) {
		t.Errorf("totals = %+v, want 2 calls, 900 / 150 tokens (only the answered call)", got)
	}
	if !reflect.DeepEqual(got.FinishReasons, []string{"error", "stop"}) {
		t.Errorf("finish reasons = %v, want [error stop]", got.FinishReasons)
	}
}

// UC: UC-040 A2
//
// #2584: a status failure has no response: "error" on the chat span and in the
// totals, no tokens.
func TestUsage_OpenAIStatusFailureIsError(t *testing.T) {
	api := newFakeAPI(t, fixed(500, []byte(`{"error":{"message":"boom"}}`)))
	usage := &decide.Usage{}
	if _, err := api.backend("gpt-4.1-mini", false).Decide(decide.WithUsage(context.Background(), usage), testRequest()); err == nil {
		t.Fatal("Decide succeeded")
	}
	got := usage.Totals()
	if got.InputTokens.OK || !reflect.DeepEqual(got.FinishReasons, []string{"error"}) {
		t.Errorf("totals = %+v, want no tokens and [error]", got)
	}
	spans := api.spans(t)
	if fr := attrs(spans[len(spans)-1])["gen_ai.response.finish_reasons"].AsStringSlice(); !reflect.DeepEqual(fr, []string{"error"}) {
		t.Errorf("chat span finish_reasons = %v, want [error]", fr)
	}
}
