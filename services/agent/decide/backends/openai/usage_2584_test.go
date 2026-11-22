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
