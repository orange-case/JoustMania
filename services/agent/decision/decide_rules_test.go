package decision

// Spec: decide-contract.md §15.5 (the rules engine; tests tagged UC-040 A1)

import (
	"context"
	"math"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/gamecontext"
)

// allInterventions is the full intervention vocabulary, i.e. the most permissive
// allow-list, so the adapter can express anything Evaluate emits.
func allInterventions() []string {
	out := make([]string, 0, len(interventionWeights))
	for k := range interventionWeights {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ffaRequest(c gamecontext.GameContext, allowed []string) decide.Request {
	st := decide.BuildState(c)
	return decide.Request{State: st, Questions: decide.FFAQuestions(st, allowed), Context: &c}
}

// rulesDecide runs the in-loop rules path (EvaluateDecide) of engine on req and
// returns the request it recorded with the response, answered from the emitted
// decisions as the loop does before it stamps any value.
func rulesDecide(t *testing.T, engine *ObjectiveRules, req decide.Request) (decide.Request, decide.Response) {
	t.Helper()
	out, got, resp, ranked := engine.EvaluateDecide(context.Background(), *req.Context,
		func(_, _ map[string]bool) decide.Request { return req })
	if !ranked {
		t.Fatal("EvaluateDecide was throttled")
	}
	resp.Answers = rulesAnswers(got, out)
	return got, resp
}

// decideFixture is one characterization scenario: a config and a context.
type decideFixture struct {
	name string
	cfg  StaticConfig
	ctx  gamecontext.GameContext
}

func withWeights(w map[string]float64) StaticConfig {
	cfg := DefaultStaticConfig()
	cfg.ObjectiveWeights = w
	return cfg
}

func decideFixtures() []decideFixture {
	return []decideFixture{
		{
			// R1 (slow tempo) and R2 (calming cue) fire; R1 wins.
			name: "endurance",
			cfg:  DefaultStaticConfig(),
			ctx: gameCtx(20, []string{"GONE"}, map[string]testPlayer{
				"A": {active: true}, "B": {active: true},
			}),
		},
		{
			// R3 (handicap the outlier) and R4 (shield the weakest) compete.
			name: "balanced",
			cfg:  withWeights(map[string]float64{ObjectiveBalanced: 1}),
			ctx: gameCtx(30, []string{"GONE"}, map[string]testPlayer{
				"WEAK":   {skill: fptr(0.2), active: true},
				"MID":    {skill: fptr(0.5), active: true},
				"STRONG": {skill: fptr(0.9), active: true},
			}),
		},
		{
			// R5 (slow down) and R6 (eliminate the least active) fire.
			name: "accelerate",
			cfg:  withWeights(map[string]float64{ObjectiveAccelerate: 1}),
			ctx: gameCtx(100, nil, map[string]testPlayer{
				"A": {intensity: fptr(0.2), active: true},
				"B": {intensity: fptr(0.9), active: true},
				"C": {intensity: fptr(1.4), active: true},
			}),
		},
		{
			// R9 only (variance window not filled yet): the seeded rng picks the
			// roll and the target.
			name: "chaos random",
			cfg:  withWeights(map[string]float64{ObjectiveChaos: 1}),
			ctx: gameCtx(5, nil, map[string]testPlayer{
				"A": {active: true}, "B": {active: true}, "C": {active: true}, "D": {active: true},
			}),
		},
		{
			// R8 statue on S plus the seeded R9: two controller effects on
			// (possibly) different targets.
			name: "chaos statue",
			cfg:  withWeights(map[string]float64{ObjectiveChaos: 1}),
			ctx: gameCtx(20, nil, map[string]testPlayer{
				"S": {variance: fptr(0.01), active: true},
				"T": {variance: fptr(0.30), active: true},
			}),
		},
		{
			// Endurance-only weights on a long session: accelerate/balanced
			// candidates all score 0, nothing clears minScore.
			name: "no candidates",
			cfg:  DefaultStaticConfig(),
			ctx: gameCtx(200, nil, map[string]testPlayer{
				"WEAK":   {skill: fptr(0.2), intensity: fptr(0.3), active: true},
				"STRONG": {skill: fptr(0.9), intensity: fptr(1.5), active: true},
			}),
		},
	}
}

// TestRulesPath_MatchesEvaluate is the characterization test: on the same
// context and an identically seeded engine, the answers pick the intervention
// and target that ObjectiveRules.Evaluate emits first (noop when it emits none).
func TestRulesPath_MatchesEvaluate(t *testing.T) {
	for _, fx := range decideFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			clock := time.Unix(10_000, 0)
			want := newTestEngine(fx.cfg, &clock).Evaluate(context.Background(), fx.ctx)

			_, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), ffaRequest(fx.ctx, allInterventions()))
			if resp.Backend != RulesBackendName {
				t.Errorf("Backend = %q", resp.Backend)
			}
			iv, _ := resp.Find(decide.QuestionFFAIntervention)
			tgt, _ := resp.Find(decide.QuestionFFATarget)

			wantIV, wantTgt := decide.OptionNoop, decide.TargetNone
			if len(want) > 0 {
				wantIV = want[0].Intervention
				wantTgt = targetOption(want[0].TargetSerial)
			}
			if iv.Choice != wantIV || tgt.Choice != wantTgt {
				t.Fatalf("answers picked %s -> %s, Evaluate picked %s -> %s (all: %+v)",
					iv.Choice, tgt.Choice, wantIV, wantTgt, want)
			}
			if fx.name != "no candidates" && len(want) == 0 {
				t.Fatalf("fixture %q should produce a decision", fx.name)
			}
			if fx.name == "no candidates" && len(want) != 0 {
				t.Fatalf("fixture %q should produce no decision, got %+v", fx.name, want)
			}
		})
	}
}

// TestRulesPath_AnswersCarryNoConfidence: the rules record what they decided;
// no answer carries probabilities or a confidence.
func TestRulesPath_AnswersCarryNoConfidence(t *testing.T) {
	for _, fx := range decideFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			clock := time.Unix(10_000, 0)
			_, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), ffaRequest(fx.ctx, allInterventions()))
			if len(resp.Answers) != 3 {
				t.Fatalf("answers = %+v, want one per FFA question", resp.Answers)
			}
			for _, a := range resp.Answers {
				if len(a.Probabilities) != 0 || a.Confidence != 0 {
					t.Errorf("answer %+v carries probabilities or a confidence", a)
				}
			}
		})
	}
}

func TestRulesPath_NoCandidatesIsNoop(t *testing.T) {
	fx := decideFixtures()[5]
	clock := time.Unix(10_000, 0)
	_, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), ffaRequest(fx.ctx, allInterventions()))
	iv, _ := resp.Find(decide.QuestionFFAIntervention)
	tgt, _ := resp.Find(decide.QuestionFFATarget)
	mag, _ := resp.Find(decide.QuestionFFAMagnitude)
	if iv.Choice != decide.OptionNoop {
		t.Errorf("intervention = %+v, want noop", iv)
	}
	if tgt.Choice != decide.TargetNone {
		t.Errorf("target = %+v, want none", tgt)
	}
	if mag.Score != 0 {
		t.Errorf("magnitude = %+v, want 0 for noop", mag)
	}
	if resp.Prediction != nil {
		t.Errorf("prediction = %+v, want none without a decision", resp.Prediction)
	}
}

func TestRulesPath_EnduranceDetails(t *testing.T) {
	fx := decideFixtures()[0]
	clock := time.Unix(10_000, 0)
	_, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), ffaRequest(fx.ctx, allInterventions()))
	iv, _ := resp.Find(decide.QuestionFFAIntervention)
	tgt, _ := resp.Find(decide.QuestionFFATarget)
	mag, _ := resp.Find(decide.QuestionFFAMagnitude)
	if iv.Choice != InterventionAdjustMusicTempo {
		t.Fatalf("choice = %s", iv.Choice)
	}
	if tgt.Choice != decide.TargetNone {
		t.Errorf("target = %+v, want none for a session-scoped lever", tgt)
	}
	// R1 writes the fast end of [1.0, 1.3] (#2487, decide-contract.md §15.5), so the
	// magnitude is 1.3, not the 1.15 writer default an empty Value would get.
	if v, _ := decide.ResolveMagnitude(iv.Choice, mag.Score); math.Abs(v-1.3) > 1e-9 {
		t.Errorf("magnitude resolves to %v, want 1.3", v)
	}
}

func TestRulesPath_ChaosTargetIsTheEmittedOne(t *testing.T) {
	fx := decideFixtures()[4]
	clock := time.Unix(10_000, 0)
	_, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), ffaRequest(fx.ctx, allInterventions()))
	iv, _ := resp.Find(decide.QuestionFFAIntervention)
	tgt, _ := resp.Find(decide.QuestionFFATarget)
	if iv.Choice != InterventionSendControllerEffect {
		t.Fatalf("choice = %s", iv.Choice)
	}
	// The statue S (R8, urgency 0.8) beats the R9 nudge (urgency <= 0.6).
	if tgt.Choice != "S" {
		t.Errorf("target = %+v, want S", tgt)
	}
}

// TestRulesPath_AnswersTheEmittedDecisionPastTheAllowList: the rules-path answers
// name the decision the engine emits even when the allow-list does not offer it;
// the questions stay the allow-listed ones and the permission chain blocks it.
func TestRulesPath_AnswersTheEmittedDecisionPastTheAllowList(t *testing.T) {
	fx := decideFixtures()[0]
	clock := time.Unix(10_000, 0)
	want := newTestEngine(fx.cfg, &clock).Evaluate(context.Background(), fx.ctx)
	if len(want) == 0 || want[0].Intervention != InterventionAdjustMusicTempo {
		t.Fatalf("fixture changed: %+v", want)
	}
	allowed := []string{InterventionPlayAudioCue, InterventionGrantShield}
	req, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), ffaRequest(fx.ctx, allowed))
	if iv, _ := resp.Find(decide.QuestionFFAIntervention); iv.Choice != want[0].Intervention {
		t.Fatalf("choice = %s, want the emitted %s", iv.Choice, want[0].Intervention)
	}
	if opts := req.Questions[0].Options; slices.Contains(opts, InterventionAdjustMusicTempo) {
		t.Errorf("questions offer %v, want the allow-listed options only", opts)
	}
}

// TestRulesPath_UnknownQuestionsAreEmpty: questions the rules have no view on get
// an empty answer at the low end of a score range.
func TestRulesPath_UnknownQuestionsAreEmpty(t *testing.T) {
	fx := decideFixtures()[0]
	clock := time.Unix(10_000, 0)
	req := ffaRequest(fx.ctx, allInterventions())
	req.Questions = append(req.Questions,
		decide.Question{ID: "ffa.mood", Kind: decide.KindChoice, Options: []string{"calm", "wild", "odd"}},
		decide.Question{ID: "ffa.volume", Kind: decide.KindScore, Range: [2]float64{2, 5}},
	)
	_, resp := rulesDecide(t, newTestEngine(fx.cfg, &clock), req)
	if a, _ := resp.Find("ffa.mood"); a.Choice != "" || a.Confidence != 0 {
		t.Errorf("unknown choice answer = %+v", a)
	}
	if a, _ := resp.Find("ffa.volume"); a.Score != 2 || a.Confidence != 0 {
		t.Errorf("unknown score answer = %+v", a)
	}
}

// TestDecideContract_VocabularyMatches keeps the leaf decide package's string
// ids in sync with this package's intervention constants.
func TestDecideContract_VocabularyMatches(t *testing.T) {
	if decide.OptionNoop != InterventionNoop {
		t.Errorf("decide.OptionNoop = %q, InterventionNoop = %q", decide.OptionNoop, InterventionNoop)
	}
	for _, iv := range decide.MagnitudeInterventions() {
		if _, ok := interventionWeights[iv]; !ok {
			t.Errorf("bounds table names unknown intervention %q", iv)
		}
	}
}
