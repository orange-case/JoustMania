package promote

import (
	"strings"
	"testing"

	"github.com/joustmania/agent/experiment"
)

// TestRenderIssueBodyStructured asserts the issue body carries the evidence
// sections (fitness before/after, reasoning, flag state).
func TestRenderIssueBodyStructured(t *testing.T) {
	body := renderIssueBody(promoteProposal, testEvidence())
	for _, want := range []string{
		"### Fitness before/after",
		"Before (real-game baseline):** 0.5000",
		"After (synthetic suite):** 0.7200",
		"Delta:** +0.2200",
		"longer grace reduces early eliminations",
		"### Flag state at improvement time",
		"death_grace_period_ms: default=300, experimental=500",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
}

// TestRenderIssueBodyHasHypothesisAndFlag asserts the issue body names the
// hypothesis and the implicated flag (#936 issue-mode contract).
func TestRenderIssueBodyHasHypothesisAndFlag(t *testing.T) {
	body := renderIssueBody(promoteProposal, testEvidence())
	if !strings.Contains(body, promoteProposal.FlagKey) {
		t.Errorf("issue body missing flag key:\n%s", body)
	}
	if !strings.Contains(body, "longer grace reduces early eliminations") {
		t.Errorf("issue body missing reasoning:\n%s", body)
	}
	if !strings.Contains(body, "no code or flag change has been applied") {
		t.Errorf("issue body should state nothing was applied:\n%s", body)
	}
}

// TestReasoningFallback: with no Evidence reasoning, the body falls back to the
// Proposal rationale, then to an explicit marker.
func TestReasoningFallback(t *testing.T) {
	if got := reasoningOrFallback(promoteProposal, Evidence{}); got != promoteProposal.Rationale {
		t.Errorf("fallback to rationale = %q, want %q", got, promoteProposal.Rationale)
	}
	if got := reasoningOrFallback(experiment.Proposal{}, Evidence{}); !strings.Contains(got, "no reasoning") {
		t.Errorf("empty fallback = %q, want a no-reasoning marker", got)
	}
}

// TestFlagStateMarkerWhenAbsent: an empty flag state renders an explicit marker.
func TestFlagStateMarkerWhenAbsent(t *testing.T) {
	if got := renderFlagStateSection(Evidence{}); !strings.Contains(got, "not captured") {
		t.Errorf("absent flag state = %q, want a not-captured marker", got)
	}
}
