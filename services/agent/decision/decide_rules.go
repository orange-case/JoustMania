package decision

import (
	"strconv"

	"github.com/joustmania/agent/decide"
)

// The rules engine is not a decide backend: it records what it decided. Its
// invoke_agent record answers the FFA questions with the cycle's first decision as
// it runs (after the shield-length stamp and the magnitude clamp), with no
// probabilities and no confidence. Remote backends own decide's confidence.

// RulesBackendName is the rules engine's decide.backend value and its
// decision.backend flag value.
const RulesBackendName = "rules"

// rulesAnswers records ds[0] (noop when ds is empty) as answers to req's
// questions. A question the rules have no view on gets an empty answer at the low
// end of its range.
func rulesAnswers(req decide.Request, ds []Decision) []decide.Answer {
	var first *Decision
	if len(ds) > 0 {
		first = &ds[0]
	}
	answers := make([]decide.Answer, 0, len(req.Questions))
	for _, q := range req.Questions {
		a := decide.Answer{QuestionID: q.ID}
		switch {
		case q.ID == decide.QuestionFFAIntervention && q.Kind == decide.KindChoice:
			a.Choice = decide.OptionNoop
			if first != nil {
				a.Choice = first.Intervention
			}
		case q.ID == decide.QuestionFFATarget && q.Kind == decide.KindChoice:
			a.Choice = decide.TargetNone
			if first != nil {
				a.Choice = targetOption(first.TargetSerial)
			}
		case q.ID == decide.QuestionFFAMagnitude && q.Kind == decide.KindScore:
			a.Score = q.Range[0] + rulesMagnitude(req.Bounds, first)*(q.Range[1]-q.Range[0])
		case q.Kind == decide.KindScore:
			a.Score = q.Range[0]
		}
		answers = append(answers, a)
	}
	return answers
}

// rulesMagnitude is d's value normalised over its bounds: the writer's default
// when d carries no number, 0 for noop and interventions without bounds.
func rulesMagnitude(mt *decide.MagnitudeTable, d *Decision) float64 {
	if d == nil {
		return 0
	}
	bounds, ok := mt.Lookup(d.Intervention)
	if !ok {
		return 0
	}
	value := bounds.Default
	if v, err := strconv.ParseFloat(d.Value, 64); err == nil {
		value = v
	}
	norm, _ := mt.Normalize(d.Intervention, value)
	return norm
}

// answerRules fills a ranked rules record's answers from the decisions about to
// run.
func (rec *decideRecord) answerRules(ds []Decision) {
	if rec != nil && rec.hasResp && rec.used == RulesBackendName {
		rec.resp.Answers = rulesAnswers(rec.req, ds)
	}
}

// rulePrediction is one row of the fixed rules prediction table (#2479, UC-040
// A44, BR-034): what the rule expects if its decision is applied. P is a
// documented prior, not a claim; the scoring measures it.
type rulePrediction struct {
	template string
	horizonS int
	p        float64
}

// rulesPredictions is the fixed table. R7 (end the game) has none: the action is
// the outcome. R1 (speed the music up, #2487) has none: the death threshold
// slides from SLOW_MAX to FAST_MAX with the tempo, so the tempo's effect on
// eliminations is confounded with that threshold change (review of #2485), and
// the lever default has none in either direction either. R6 has one only while at least 3 players
// stay alive after the elimination (rulesPrediction), otherwise it is trivially
// true.
var rulesPredictions = map[string]rulePrediction{
	"R2":  {decide.TemplateNoElimination, 15, 0.55},
	"R3":  {decide.TemplateClosenessRises, 15, 0.55},
	"R4":  {decide.TemplateSurvives, 15, 0.7},
	"R4d": {decide.TemplateSurvives, 10, 0.7},
	"R5":  {decide.TemplateElimination, 30, 0.6},
	"R6":  {decide.TemplateGameEnds, 60, 0.5},
	"R8":  {decide.TemplateActivityRises, 10, 0.6},
	"R9":  {decide.TemplateActivityRises, 10, 0.6},
}

// rulesPrediction is the prediction of the winning candidate's rule, nil when the
// rule has none.
func rulesPrediction(best scoredCandidate, st decide.State) *decide.Prediction {
	row, ok := rulesPredictions[best.rule]
	if !ok {
		return nil
	}
	if best.rule == "R6" {
		alive := 0
		for _, p := range st.Players {
			if p.Alive {
				alive++
			}
		}
		if alive-1 < 3 {
			return nil
		}
	}
	target := ""
	if row.template == decide.TemplateSurvives || row.template == decide.TemplateActivityRises {
		target = best.decision.TargetSerial
	}
	p := decide.NewPrediction(row.template, row.horizonS, row.p, decide.SourceRules, best.rule, target)
	return &p
}

func targetOption(serial string) string {
	if serial == "" {
		return decide.TargetNone
	}
	return serial
}
