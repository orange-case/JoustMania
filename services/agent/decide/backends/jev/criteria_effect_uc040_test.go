package jev

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-036
// criteria_effect_uc040_test.go: jev's lever, target and magnitude texts describe
// effects and situations, never a purpose or a verdict (decide-contract.md §3).
// The request bytes are pinned by the fixtures (contract_test.go).

var bannedVerdicts = []string{
	"unfairly", "wasted", "matters most", "lopsided", "to build tension", "to keep a lopsided game close",
	"would help", "would even the game", "keeps the game close", "pile on", "little to balance",
	"often right", "right when", "help a struggling", "rein in",
}

func checkNoVerdict(t *testing.T, where, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, b := range bannedVerdicts {
		if strings.Contains(lower, b) {
			t.Errorf("%s contains %q: %q", where, b, text)
		}
	}
}

func TestCriteria_NoVerdictInRequests(t *testing.T) {
	for _, tc := range []struct {
		name, chosen string
		req          func() decide.Request
	}{
		{"stage 1", "", func() decide.Request { return splitStages(testRequest()).first }},
		{"shield details", "grant_shield", func() decide.Request { return detailsRequest(testRequest(), "grant_shield").ask }},
	} {
		t.Run("UC-040/BR-036 "+tc.name+" questions carry no verdict", func(t *testing.T) {
			out, err := encodeRequest(fixtureModel, tc.req(), tc.chosen)
			if err != nil {
				t.Fatal(err)
			}
			var wire jevRequest
			if err := json.Unmarshal(out.Body, &wire); err != nil {
				t.Fatal(err)
			}
			// The gate criteria are the objective, not a lever text.
			for id, q := range wire.Questions {
				if id == GateQuestionID {
					continue
				}
				raw, _ := json.Marshal(q)
				checkNoVerdict(t, tc.name+" "+id, string(raw))
			}
		})
	}
}

func TestCriteria_Descriptions(t *testing.T) {
	t.Run("UC-040/BR-036 every lever and noop has a description without a verdict", func(t *testing.T) {
		for o, d := range interventionDescriptions {
			if d == "" {
				t.Errorf("%s has no description", o)
			}
			checkNoVerdict(t, o, d)
		}
		if len(interventionDescriptions) != len(decide.Catalog)+1 {
			t.Errorf("descriptions cover %d options, want every lever and noop", len(interventionDescriptions))
		}
		if got := targetInstructions("grant_shield"); !strings.Contains(got, "immune to elimination for a few seconds") {
			t.Errorf("target instructions = %q", got)
		}
	})
}

func TestCriteria_StrengthQuestions(t *testing.T) {
	for _, iv := range []string{"adjust_player_sensitivity", "set_player_handicap"} {
		t.Run("UC-040/BR-036 "+iv+" strength questions describe the effect on the target", func(t *testing.T) {
			plan := detailsRequest(testRequest(), iv)
			out, err := encodeRequest(fixtureModel, plan.ask, iv)
			if err != nil {
				t.Fatal(err)
			}
			var wire jevRequest
			if err := json.Unmarshal(out.Body, &wire); err != nil {
				t.Fatal(err)
			}
			found := 0
			for id, q := range wire.Questions {
				if strengthDirection(id) == "" {
					continue
				}
				found++
				checkNoVerdict(t, iv+" "+id, q.Instructions)
				if !strings.Contains(q.Instructions, "make the target") {
					t.Errorf("%s %s instructions = %q", iv, id, q.Instructions)
				}
			}
			if found == 0 {
				t.Fatal("no strength question asked")
			}
		})
	}
}

func TestCriteria_Suitability(t *testing.T) {
	st := testRequest().State
	dangers := []string{decide.DangerNearThreshold, decide.DangerElevated, decide.DangerSafe, ""}
	ivs := []string{"grant_shield", "toughness_boost", "adjust_player_sensitivity", "set_player_handicap", "soft_penalty", "send_controller_effect", "eliminate_player"}
	t.Run("UC-040/BR-036 suitability states the situation, never a verdict", func(t *testing.T) {
		for _, iv := range ivs {
			for _, danger := range dangers {
				for _, shielded := range []bool{false, true} {
					p := decide.PlayerState{Serial: "X", Alive: true, Danger: danger, Shielded: shielded}
					for _, role := range []string{"", "calmest", "struggling"} {
						s := st
						switch role {
						case "calmest":
							s.MostHeadroom = "X"
						case "struggling":
							s.Struggling = "X"
						}
						checkNoVerdict(t, iv+"/"+danger+"/"+role, suitability(iv, p, s))
					}
				}
			}
		}
		if got := suitability("grant_shield", decide.PlayerState{Danger: decide.DangerSafe}, st); got != "In no danger right now." {
			t.Errorf("safe player = %q", got)
		}
		if got := suitability("eliminate_player", decide.PlayerState{Serial: "X"}, decide.State{MostHeadroom: "X"}); got != "" {
			t.Errorf("eliminate_player must get no suitability: %q", got)
		}
		for serial, d := range describeCandidates(st, "grant_shield") {
			checkNoVerdict(t, serial, d)
		}
	})
}
