package llmio

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/internal/anthropichttp"
)

// UC: UC-040 BR-036 BR-034
// prompt_golden_test.go pins the in-game prompt byte for byte (decide-contract.md
// §3): the system prompt without and with goal lines, the user message and the
// answer schema of testRequest() (no ask, no goal, no brief). The talk runs on
// this prompt: a diff here is a reviewed change of it, never regenerate to make a
// change pass.
//
//	go test ./decide/llmio -run TestPromptGolden -update-prompt-golden

var updatePromptGolden = flag.Bool("update-prompt-golden", false, "rewrite decide/llmio/testdata/prompt_*.golden")

// The fingerprints (llm.prompt.system_sha256) of the two system prompts.
const (
	systemSHA     = "88c134b1ec07944d"
	systemGoalSHA = "a35ca4f233ae58cf"
)

// bannedPhrases are purpose and verdict phrases the lever text must not carry
// (UC-040 BR-036).
var bannedPhrases = []string{
	"unfairly", "wasted", "matters most", "lopsided", "to build tension",
	"to keep a lopsided game close", "often the right answer",
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updatePromptGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s changed (a change of the talk's prompt)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPromptGolden(t *testing.T) {
	t.Run("UC-040/BR-036 BR-034 system prompt, user message and schema are byte-identical", func(t *testing.T) {
		req := testRequest()
		if req.PredictionAsk != nil || req.Goal != nil || req.OperatorBrief != "" {
			t.Fatal("testRequest must carry no ask, goal or brief")
		}
		p := Render(req)
		checkGolden(t, "prompt_system.golden", []byte(p.System))
		checkGolden(t, "prompt_user.golden", []byte(p.User))
		schema, err := json.Marshal(AnswerSchema())
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, "prompt_schema.golden", schema)
		req.Goal = goalLines()
		checkGolden(t, "prompt_system_goal.golden", []byte(Render(req).System))
	})
	if *updatePromptGolden {
		return
	}
	t.Run("UC-040/BR-036 the fingerprints are pinned", func(t *testing.T) {
		if got := anthropichttp.SystemSHA(golden(t, "prompt_system.golden")); got != systemSHA {
			t.Fatalf("system fingerprint = %s, want %s (the prompt changed: update the const in the same reviewed diff)", got, systemSHA)
		}
		if got := anthropichttp.SystemSHA(golden(t, "prompt_system_goal.golden")); got != systemGoalSHA {
			t.Fatalf("goal system fingerprint = %s, want %s", got, systemGoalSHA)
		}
	})
}

func TestPrompt_LeverLines(t *testing.T) {
	all := decide.Names(nil)
	st := testRequest().State
	req := decide.Request{State: st, Questions: decide.FFAQuestions(st, all)}
	sys := Render(req).System

	t.Run("UC-040/BR-036 every offered lever has its effect line", func(t *testing.T) {
		var offered []string
		for _, q := range req.Questions {
			if q.ID == decide.QuestionFFAIntervention {
				offered = q.Options
			}
		}
		if len(offered) != len(all)+1 {
			t.Fatalf("offered = %v, want every catalog lever and noop", offered)
		}
		for _, o := range offered {
			want := "- " + o + ": " + decide.NoopDescription
			if iv, ok := decide.ByName(o); ok {
				want = "- " + o + ": " + iv.Effect
			}
			if !strings.Contains(sys, want+"\n") {
				t.Errorf("prompt has no line %q", want)
			}
		}
		if !strings.Contains(sys, "\nLevers:\n") {
			t.Error("prompt has no Levers block")
		}
	})
	t.Run("UC-040/BR-036 none of the purpose or verdict phrases, the restraint clause kept", func(t *testing.T) {
		lower := strings.ToLower(sys)
		for _, banned := range bannedPhrases {
			if strings.Contains(lower, banned) {
				t.Errorf("prompt contains %q", banned)
			}
		}
		if !strings.Contains(sys, `"noop" (do nothing) is always a valid answer; only intervene when the game state clearly calls for it.`) {
			t.Error("prompt lost the noop sentence with its restraint clause")
		}
	})
	t.Run("UC-040/BR-036 the system prompt is static", func(t *testing.T) {
		few := Render(decide.Request{State: st, Questions: decide.FFAQuestions(st, []string{"grant_shield"})}).System
		if few != sys {
			t.Error("the system prompt must not depend on the offered levers")
		}
	})
}
