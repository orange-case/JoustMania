package decision

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/llm"
	"github.com/joustmania/agent/telemetry/keys"
)

// UC: UC-042 A13 BR-010
// The retro reads the brief of the game that ended through the same per-game source
// as the decide path, sends it to the model, and its span reports presence and
// length while llm.retro.user keeps the text only under decision.capture_content.
func TestRetro_OperatorBrief(t *testing.T) {
	const brief = "Player AA:11 should win this game"
	run := func(t *testing.T, capture bool, src OperatorBriefSource) (string, map[string]string) {
		t.Helper()
		snap := retroFlagSnapshot()
		snap.Decision.CaptureContent = capture
		rc, sr := recordingRetro(t, snap)
		be := &promptCapture{}
		rc.SetBackend(be)
		if src != nil {
			rc.SetOperatorBriefSource(src)
		}
		rc.OnGameEnd(endedSession())
		rc.AwaitInflight()
		spans := spansByName(sr.Ended(), SpanLLMRetro)
		if len(spans) != 1 {
			t.Fatalf("agent.llm.retro spans = %d, want 1", len(spans))
		}
		got := map[string]string{}
		for _, k := range []string{keys.AttrLLMRetroOperatorBriefPresent, keys.AttrLLMRetroOperatorBriefLength, AttrLLMRetroUser} {
			if v, ok := attrValue(spans[0], k); ok {
				got[k] = v.Emit()
			}
		}
		be.mu.Lock()
		defer be.mu.Unlock()
		return be.user, got
	}
	var asked string
	src := func(_ context.Context, gameID string) string { asked = gameID; return brief }

	t.Run("UC-042/A13 the brief reaches the model and the span counts it", func(t *testing.T) {
		user, attrs := run(t, false, src)
		if asked != endedSession().SessionID {
			t.Errorf("brief read for %q, want the ended game %q", asked, endedSession().SessionID)
		}
		if !strings.HasPrefix(user, "<operator_brief>\n"+brief+"\n</operator_brief>\n\n") {
			t.Errorf("model prompt does not open with the brief block:\n%s", user)
		}
		if attrs[keys.AttrLLMRetroOperatorBriefPresent] != "true" || attrs[keys.AttrLLMRetroOperatorBriefLength] != "33" {
			t.Errorf("brief attrs = %v, want present=true length=33", attrs)
		}
	})
	t.Run("UC-042/A13 llm.retro.user hides the brief text without content capture", func(t *testing.T) {
		_, attrs := run(t, false, src)
		u := attrs[AttrLLMRetroUser]
		if strings.Contains(u, brief) || !strings.Contains(u, "<operator_brief>\n(not recorded: decision.capture_content off)\n</operator_brief>") {
			t.Errorf("llm.retro.user must carry the placeholder, not the brief:\n%s", u)
		}
	})
	t.Run("UC-042/A13 llm.retro.user keeps the brief text with content capture", func(t *testing.T) {
		_, attrs := run(t, true, src)
		if !strings.Contains(attrs[AttrLLMRetroUser], brief) {
			t.Errorf("llm.retro.user lacks the brief under capture_content:\n%s", attrs[AttrLLMRetroUser])
		}
	})
	t.Run("UC-042/A13 no brief: no block and no brief attributes", func(t *testing.T) {
		for name, s := range map[string]OperatorBriefSource{"no source": nil, "blank": func(context.Context, string) string { return "" }} {
			user, attrs := run(t, false, s)
			if strings.Contains(user, "operator_brief") {
				t.Errorf("%s: prompt has a brief block:\n%s", name, user)
			}
			if _, ok := attrs[keys.AttrLLMRetroOperatorBriefPresent]; ok {
				t.Errorf("%s: brief attribute present: %v", name, attrs)
			}
		}
	})
}

// UC: UC-042 A13 BR-010
// The brief log notes a game only when a remote request carried the brief (claude
// or jev), and the retro tells the analyst whether the in-game agent
// saw the brief.
func TestRetro_OperatorBriefDelivered(t *testing.T) {
	const brief = "keep the red player alive"
	noted := func(t *testing.T, backend string) bool {
		t.Helper()
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.9, "A", 0.5, "shield"))
		be.name = backend
		snap := remoteSnapshot(time.Second)
		snap.Decision.Backend = backend
		snap.Decision.Jev = flags.RemoteBackendConfig{Model: "jev-latest", Timeout: time.Second}
		h := newRemoteHarness(t, snap, be)
		h.loop.SetOperatorBriefSource(func(context.Context, string) string { return brief })
		log := NewBriefLog()
		h.loop.SetBriefLog(log)
		h.run(t)
		return log.Delivered(remoteGameID)
	}
	t.Run("UC-042/A13 a claude request with the brief is noted", func(t *testing.T) {
		if !noted(t, flags.DecisionBackendClaude) {
			t.Error("claude game not noted as brief-delivered")
		}
	})
	t.Run("UC-042/A13 a jev request with the brief is noted", func(t *testing.T) {
		if !noted(t, flags.DecisionBackendJev) {
			t.Error("jev game not noted as brief-delivered")
		}
	})

	system := func(t *testing.T, delivered func(string) bool) string {
		t.Helper()
		rc, _ := recordingRetro(t, retroFlagSnapshot())
		be := &systemCapture{}
		rc.SetBackend(be)
		rc.SetOperatorBriefSource(func(context.Context, string) string { return brief })
		if delivered != nil {
			rc.SetBriefDelivered(delivered)
		}
		rc.OnGameEnd(endedSession())
		rc.AwaitInflight()
		be.mu.Lock()
		defer be.mu.Unlock()
		return strings.Join(strings.Fields(be.system), " ")
	}
	t.Run("UC-042/A13 delivered: judge whether the agent served the brief", func(t *testing.T) {
		var asked string
		s := system(t, func(id string) bool { asked = id; return true })
		if asked != endedSession().SessionID {
			t.Errorf("delivery asked for %q", asked)
		}
		if !strings.Contains(s, "The in-game agent was told to pursue it") || strings.Contains(s, "never saw it") {
			t.Errorf("delivered paragraph wrong:\n%s", s)
		}
	})
	t.Run("UC-042/A13 not delivered: the agent never saw it and is not blamed", func(t *testing.T) {
		for name, d := range map[string]func(string) bool{"no lookup": nil, "not sent": func(string) bool { return false }} {
			s := system(t, d)
			if !strings.Contains(s, "The in-game agent never saw it") || !strings.Contains(s, "do not blame the agent") ||
				strings.Contains(s, "was told to pursue it") {
				t.Errorf("%s: not-delivered paragraph wrong:\n%s", name, s)
			}
		}
	})
}

// systemCapture is a text backend that keeps the system prompt it was asked.
type systemCapture struct {
	mu     sync.Mutex
	system string
}

func (p *systemCapture) Name() string  { return "claude" }
func (p *systemCapture) Model() string { return "claude-test" }
func (p *systemCapture) Infer(_ context.Context, pr llm.Prompt) (string, error) {
	p.mu.Lock()
	p.system = pr.System
	p.mu.Unlock()
	return `{"session_assessment":"ok","suggestions":[],"session_focus":"balanced"}`, nil
}
