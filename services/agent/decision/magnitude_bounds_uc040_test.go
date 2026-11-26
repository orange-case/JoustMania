package decision

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joustmania/agent/decide"
)

// UC: UC-040 BR-045
// magnitude_bounds_uc040_test.go: the game's decision.magnitude_bounds is read per
// decide request and carried on Request.Bounds, so the question text, the answer's
// resolution, the rules answer and the clamp of the rules value use one table; the
// span records where the ranges came from (#2696, decide-contract.md §2.1, §12).

func widened() *decide.MagnitudeTable {
	return decide.ParseMagnitudeOverride(map[string]any{
		"grant_shield":              map[string]any{"min": 2, "max": 8},
		"adjust_player_sensitivity": map[string]any{"min": 0.2, "max": 1.8},
		"bogus":                     map[string]any{"min": 0, "max": 1},
	})
}

func TestRemote_MagnitudeBounds(t *testing.T) {
	t.Run("UC-040/BR-045 the request carries the game's ranges, the answer resolves over them, the span says flag", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.9, "A", 1, "shield"))
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		reads := 0
		h.loop.SetMagnitudeBoundsSource(func(_ context.Context, gameID string) *decide.MagnitudeTable {
			if gameID != remoteGameID {
				t.Errorf("bounds read for %q", gameID)
			}
			reads++
			return widened()
		})
		span := h.run(t)
		if reads != 1 || len(be.reqs) != 1 {
			t.Fatalf("reads %d, requests %d", reads, len(be.reqs))
		}
		req := be.reqs[0]
		if b, _ := req.Bounds.Lookup("grant_shield"); b.Min != 2 || b.Max != 8 {
			t.Fatalf("request bounds %+v", b)
		}
		var text string
		for _, q := range req.Questions {
			if q.ID == decide.QuestionFFAMagnitude {
				text = q.Text
			}
		}
		if !strings.Contains(text, "grant_shield: lowest 2 (shortest protection), highest 8 (longest protection), in seconds") {
			t.Fatalf("magnitude text %q", text)
		}
		h.sink.mu.Lock()
		got := append([]Decision(nil), h.sink.got...)
		h.sink.mu.Unlock()
		if len(got) != 1 || got[0].Value != "8" {
			t.Fatalf("applied %+v, want grant_shield 8 (score 1 over [2,8])", got)
		}
		if v, _ := attrValue(span, AttrDecideMagnitudeBoundsSource); v.AsString() != decide.MagnitudeSourceFlag {
			t.Fatalf("source %q", v.AsString())
		}
		if v, _ := attrValue(span, AttrDecideMagnitudeBoundsInvalid); v.AsString() != "bogus" {
			t.Fatalf("invalid %q", v.AsString())
		}
	})
	t.Run("UC-040/BR-045 without a source the ranges are the defaults and the span says default", func(t *testing.T) {
		be := newFakeDecideBackend(ffaAnswer("grant_shield", 0.9, "A", 1, "shield"))
		h := newRemoteHarness(t, remoteSnapshot(time.Second), be)
		span := h.run(t)
		if v, _ := attrValue(span, AttrDecideMagnitudeBoundsSource); v.AsString() != decide.MagnitudeSourceDefault {
			t.Fatalf("source %q", v.AsString())
		}
		if _, ok := attrValue(span, AttrDecideMagnitudeBoundsInvalid); ok {
			t.Fatal("invalid attribute without an invalid entry")
		}
		h.sink.mu.Lock()
		defer h.sink.mu.Unlock()
		if len(h.sink.got) != 1 || h.sink.got[0].Value != "10" {
			t.Fatalf("applied %+v, want grant_shield 10", h.sink.got)
		}
	})
}

func TestRules_MagnitudeBounds(t *testing.T) {
	narrow := decide.ParseMagnitudeOverride(map[string]any{
		"grant_shield":       map[string]any{"min": 6, "max": 10},
		"adjust_music_tempo": map[string]any{"min": 1.0, "max": 1.2},
	})
	for _, tc := range []struct {
		name string
		in   Decision
		want string
	}{
		{"UC-040/BR-045 a rules value above a narrowed max is clamped to it", Decision{Intervention: InterventionAdjustMusicTempo, Value: "1.3"}, "1.2"},
		{"UC-040/BR-045 an empty value whose default is outside the range becomes explicit", Decision{Intervention: InterventionGrantShield, TargetSerial: "A"}, "6"},
		{"UC-040/BR-045 a value inside the range is unchanged", Decision{Intervention: InterventionGrantShield, Value: "7"}, "7"},
		{"UC-040/BR-045 an intervention without bounds is unchanged", Decision{Intervention: "end_game"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampRulesValue(narrow, tc.in).Value; got != tc.want {
				t.Fatalf("value %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("UC-040/BR-045 at the default ranges no rules value moves", func(t *testing.T) {
		for _, d := range []Decision{
			{Intervention: InterventionAdjustMusicTempo, Value: "1.3"},
			{Intervention: InterventionGrantShield, TargetSerial: "A"},
			{Intervention: InterventionAdjustPlayerSensitivity, TargetSerial: "A"},
		} {
			if got := clampRulesValue(decide.ParseMagnitudeOverride(nil), d); got.Value != d.Value {
				t.Fatalf("%s moved to %q", d.Intervention, got.Value)
			}
			if got := clampRulesValue(nil, d); got.Value != d.Value {
				t.Fatalf("%s moved to %q (nil table)", d.Intervention, got.Value)
			}
		}
	})
	t.Run("UC-040/BR-045 the rules answer normalizes over the request's range", func(t *testing.T) {
		d := &Decision{Intervention: InterventionGrantShield, Value: "5"}
		if got := rulesMagnitude(widened(), d); got != 0.5 {
			t.Fatalf("score %v over [2,8], want 0.5", got)
		}
		if got := rulesMagnitude(nil, d); got != 2.0/7.0 {
			t.Fatalf("score %v over the default [3,10]", got)
		}
	})
	t.Run("UC-040/BR-045 the rules request carries the game's ranges", func(t *testing.T) {
		l := &Loop{}
		l.SetMagnitudeBoundsSource(func(context.Context, string) *decide.MagnitudeTable { return widened() })
		req := l.rulesRequestBuilderCtx(context.Background(), remoteContext(), []string{"grant_shield"})(nil, nil)
		if b, _ := req.Bounds.Lookup("grant_shield"); b.Max != 8 || req.Context == nil {
			t.Fatalf("rules request bounds %+v", b)
		}
	})
}
