package decision

import (
	"context"
	"strings"
	"testing"

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
