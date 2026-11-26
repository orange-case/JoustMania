package jev

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 A45 BR-034 BR-044
// prediction_uc040_test.go: jev answers the prediction ask with a third call, a
// noul on the decision's lever default, and carries the operator brief in the
// state of every stage (decide-contract.md §3).

func askRequest() decide.Request {
	req := testRequest()
	req.PredictionAsk = decide.PredictionQuestion(req.State)
	return req
}

func predictionReply(pYes float64) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":   "jev-1.13.0",
		"answers": map[string]any{decide.QuestionFFAPrediction: map[string]any{"type": "noul", "noul": pYes}},
	})
	return b
}

func TestPredictionAsk_Jev(t *testing.T) {
	t.Run("UC-040/A45 an acting round asks the lever default as a third call", func(t *testing.T) {
		api := newScriptedAPI(t, ok(actingIntervention(t, "grant_shield")), ok(loadFixture(t, fxDetails)), ok(predictionReply(0.7)))
		resp, err := api.backend(fixtureModel, false).Decide(context.Background(), askRequest())
		if err != nil {
			t.Fatal(err)
		}
		if n := api.calls.Load(); n != 3 {
			t.Fatalf("%d calls, want 3", n)
		}
		tg, _ := resp.Find(decide.QuestionFFATarget)
		p := resp.Prediction
		if p == nil || p.Template != decide.TemplateSurvives || p.HorizonS != 15 || p.Target != tg.Choice ||
			p.P != 0.7 || p.Source != decide.SourceModel || p.Rule != "grant_shield" || resp.PredictionDropped != "" {
			t.Fatalf("prediction = %+v, dropped %q", p, resp.PredictionDropped)
		}
		api.mu.Lock()
		third := api.bodies[2]
		api.mu.Unlock()
		var wire jevRequest
		if err := json.Unmarshal(third, &wire); err != nil {
			t.Fatal(err)
		}
		q, ok := wire.Questions[decide.QuestionFFAPrediction]
		if len(wire.Questions) != 1 || !ok || q.Type != typeNoul || !strings.Contains(q.Instructions, "player "+tg.Choice+" still be alive") ||
			!strings.Contains(q.Instructions, "Within 15 seconds") || wire.State.ChosenIntervention != "grant_shield" {
			t.Fatalf("prediction call = %s", third)
		}
		if got := attrs(api.chatSpans()[2])[AttrStage].AsString(); got != StagePrediction {
			t.Errorf("third span stage = %q", got)
		}
	})
	t.Run("UC-040/A45 a failed prediction call drops only the prediction", func(t *testing.T) {
		api := newScriptedAPI(t, ok(actingIntervention(t, "grant_shield")), ok(loadFixture(t, fxDetails)), fakeReply{status: 500, body: []byte(`{}`)})
		resp, err := api.backend(fixtureModel, false).Decide(context.Background(), askRequest())
		if err != nil {
			t.Fatal(err)
		}
		if resp.Prediction != nil || resp.PredictionDropped != decide.PredictionDropMissing {
			t.Fatalf("prediction %+v, dropped %q", resp.Prediction, resp.PredictionDropped)
		}
		if iv, _ := resp.Find(decide.QuestionFFAIntervention); iv.Choice != "grant_shield" {
			t.Fatalf("the decision changed: %+v", iv)
		}
	})
	t.Run("UC-040/A45 noop asks nothing more and states none", func(t *testing.T) {
		api := newScriptedAPI(t, ok(noopStage1(t)), ok(predictionReply(0.9)))
		resp, err := api.backend(fixtureModel, false).Decide(context.Background(), askRequest())
		if err != nil {
			t.Fatal(err)
		}
		if n := api.calls.Load(); n != 1 || resp.Prediction != nil || resp.PredictionDropped != "" {
			t.Fatalf("calls %d, prediction %+v, dropped %q", n, resp.Prediction, resp.PredictionDropped)
		}
	})
	t.Run("UC-040/A45 without the ask the round is unchanged", func(t *testing.T) {
		api := newScriptedAPI(t, ok(actingIntervention(t, "grant_shield")), ok(loadFixture(t, fxDetails)))
		resp, err := api.backend(fixtureModel, false).Decide(context.Background(), testRequest())
		if err != nil {
			t.Fatal(err)
		}
		if n := api.calls.Load(); n != 2 || resp.Prediction != nil || resp.PredictionDropped != "" {
			t.Fatalf("calls %d, prediction %+v", n, resp.Prediction)
		}
	})
}

func TestOperatorBrief_Jev(t *testing.T) {
	const brief = "Make player MOCK0003 unkillable."
	with := testRequest()
	with.OperatorBrief = brief
	for _, tc := range []struct {
		name, chosen string
		sub          func(decide.Request) decide.Request
	}{
		{"stage 1", "", func(r decide.Request) decide.Request { return splitStages(r).first }},
		{"details", "grant_shield", func(r decide.Request) decide.Request { return detailsRequest(r, "grant_shield").ask }},
	} {
		t.Run("UC-040/BR-044 the "+tc.name+" state carries the brief, the criteria are unchanged", func(t *testing.T) {
			on, err := encodeRequest(fixtureModel, tc.sub(with), tc.chosen)
			if err != nil {
				t.Fatal(err)
			}
			off, err := encodeRequest(fixtureModel, tc.sub(testRequest()), tc.chosen)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(off.Body), "operator_brief") {
				t.Fatalf("a request without a brief carries one: %s", off.Body)
			}
			onV, offV := decodeWire(t, on.Body), decodeWire(t, off.Body)
			st := onV["state"].(map[string]any)
			if st["operator_brief"] != brief {
				t.Fatalf("state.operator_brief = %v", st["operator_brief"])
			}
			delete(st, "operator_brief")
			if !reflect.DeepEqual(onV, offV) {
				t.Error("the brief must change only state.operator_brief")
			}
		})
	}
}
