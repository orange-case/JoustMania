package jev

// prediction.go answers the prediction ask (decide-contract.md §3, UC-040 A45) on
// jev. Jev has no free choice over the templates, so it is asked one noul about
// the decision's lever default (decide.LeverPrediction): P(yes) is the confidence
// that it happens within its horizon. A failed call drops the prediction, never
// the decision.

import (
	"context"
	"strconv"

	"github.com/joustmania/agent/decide"
)

// StagePrediction is the jev.stage of the prediction ask's call.
const StagePrediction = "prediction"

// askPrediction sets resp.Prediction / resp.PredictionDropped for req's ask from
// resp, the round's merged answers.
func (b *Backend) askPrediction(ctx context.Context, req decide.Request, resp *decide.Response) {
	iv, _ := resp.Find(decide.QuestionFFAIntervention)
	tgt, _ := resp.Find(decide.QuestionFFATarget)
	target := ""
	if decide.PlayerTargeted(iv.Choice) && tgt.Choice != decide.TargetNone {
		target = tgt.Choice
	}
	value := ""
	if m, ok := resp.Find(decide.QuestionFFAMagnitude); ok {
		if v, ok := req.Bounds.Resolve(iv.Choice, m.Score); ok {
			value = strconv.FormatFloat(v, 'g', -1, 64)
		}
	}
	lever := decide.LeverPrediction(iv.Choice, value, target, nil)
	if lever == nil {
		// Nothing measurable to expect: the answer is "none".
		return
	}
	ask := decide.Request{
		State:         req.State,
		Questions:     []decide.Question{{ID: decide.QuestionFFAPrediction, Kind: kindNoul, Text: predictionInstructions(iv.Choice, *lever)}},
		Goal:          req.Goal,
		OperatorBrief: req.OperatorBrief,
		Bounds:        req.Bounds,
	}
	out, err := b.call(ctx, StagePrediction, ask, iv.Choice)
	pYes, ok := out.Find(decide.QuestionFFAPrediction)
	if err != nil || !ok {
		resp.PredictionDropped = decide.PredictionDropMissing
		return
	}
	a := decide.Answer{
		QuestionID: decide.QuestionFFAPrediction,
		Choice:     decide.PredictionOption(lever.Template, lever.HorizonS),
		Confidence: pYes.Score,
	}
	resp.Prediction, resp.PredictionDropped = decide.ValidatePrediction(*req.PredictionAsk, a, iv.Choice, tgt.Choice)
}

// predictionInstructions asks whether p happens within its horizon once iv is applied.
func predictionInstructions(iv string, p decide.Prediction) string {
	return "The agent will apply " + iv + " now. Within " + strconv.Itoa(p.HorizonS) + " seconds, will " +
		predictionEvent(p) + "?"
}

// predictionEvent is p's template as an event (the wording of decide's ask text).
func predictionEvent(p decide.Prediction) string {
	switch p.Template {
	case decide.TemplateSurvives:
		return "player " + p.Target + " still be alive"
	case decide.TemplateNoElimination:
		return "no player be eliminated"
	case decide.TemplateElimination:
		return "at least one player be eliminated"
	case decide.TemplateActivityRises:
		if p.Target != "" {
			return "player " + p.Target + "'s movement rise"
		}
		return "the alive players' movement rise"
	case decide.TemplateClosenessRises:
		return "the alive players' movement levels get closer"
	default:
		return "the game end"
	}
}
