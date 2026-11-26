package decide

import (
	"errors"
	"testing"
)

// ffaPair is a valid answered FFA request with three alive players.
func ffaPair(iv, target string) (Request, Response) {
	st := State{Players: []PlayerState{{Serial: "A", Alive: true}, {Serial: "B", Alive: true}, {Serial: "C", Alive: true}}}
	targets := []string{TargetNone, "A", "B", "C", "group:all", "group:all_except:A"}
	req := Request{State: st, Questions: []Question{
		{ID: QuestionFFAIntervention, Kind: KindChoice, Options: []string{OptionNoop, "grant_shield", "adjust_music_tempo", "grant_group_shield"}},
		{ID: QuestionFFATarget, Kind: KindChoice, Options: targets},
	}}
	resp := Response{Answers: []Answer{
		{QuestionID: QuestionFFAIntervention, Choice: iv, Probabilities: map[string]float64{iv: 1}, Confidence: 0.9},
		{QuestionID: QuestionFFATarget, Choice: target, Probabilities: map[string]float64{target: 1}, Confidence: 0.9},
	}}
	return req, resp
}

// TestValidate_CrossQuestionConsistency: an answer whose target does not fit its
// intervention is an invalid answer (decide-contract §4.3), so the remote path
// falls back to the rules with invalid_answer; there is no outcome invalid.
// UC: UC-040 A8 A2
func TestValidate_CrossQuestionConsistency(t *testing.T) {
	cases := []struct {
		name, iv, target string
		ok               bool
	}{
		{"noop without target", OptionNoop, TargetNone, true},
		{"player lever on a player", "grant_shield", "B", true},
		{"session lever without target", "adjust_music_tempo", TargetNone, true},
		{"group lever on a group", "grant_group_shield", "group:all_except:A", true},
		{"noop with a target", OptionNoop, "A", false},
		{"player lever without target", "grant_shield", TargetNone, false},
		{"session lever with a target", "adjust_music_tempo", "A", false},
		{"group lever on a serial", "grant_group_shield", "A", false},
		{"group lever without target", "grant_group_shield", TargetNone, false},
		{"group option on a single lever", "grant_shield", "group:all", false},
	}
	for _, tc := range cases {
		t.Run("UC-040/A8 "+tc.name, func(t *testing.T) {
			err := Validate(ffaPair(tc.iv, tc.target))
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

// TestValidate_OnlyFullFFARequestsAreCrossChecked: a request without ffa.target (a
// jev stage asks one of the two) is not cross-checked.
// UC: UC-040 A13
func TestValidate_OnlyFullFFARequestsAreCrossChecked(t *testing.T) {
	req, resp := ffaPair("grant_shield", TargetNone)
	req.Questions, resp.Answers = req.Questions[:1], resp.Answers[:1]
	if err := Validate(req, resp); err != nil {
		t.Fatalf("Validate = %v, want nil for an intervention-only request", err)
	}
}

// TestValidate_NoulIsNotAContractKind: the noul is a detail of jev's wire format
// (decide-contract §1.1, §3.1), so a request asking one is invalid.
// UC: UC-040 A12
func TestValidate_NoulIsNotAContractKind(t *testing.T) {
	req := Request{Questions: []Question{{ID: "yn", Kind: "noul", Text: "Is the game close?"}}}
	err := Validate(req, Response{Answers: []Answer{{QuestionID: "yn", Score: 0.5, Confidence: 0}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate = %v, want an ErrInvalid for the unknown kind", err)
	}
}
