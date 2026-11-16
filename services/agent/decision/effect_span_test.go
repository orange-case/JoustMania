package decision

import (
	"encoding/json"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/joustmania/agent/gamecontext"
	"github.com/joustmania/agent/telemetry/keys"
)

// effectSignalsOf decodes the intervention.signals JSON attribute of an effect span
// (#2640), failing the test when it is missing or malformed.
func effectSignalsOf(t *testing.T, s sdktrace.ReadOnlySpan) map[string]effectSignalEntry {
	t.Helper()
	v, ok := attrValue(s, keys.AttrEffectSignals)
	if !ok {
		t.Fatalf("effect span has no %s: %v", keys.AttrEffectSignals, attrMap(s.Attributes()))
	}
	out := map[string]effectSignalEntry{}
	if err := json.Unmarshal([]byte(v.AsString()), &out); err != nil {
		t.Fatalf("%s is not a JSON object: %v", keys.AttrEffectSignals, err)
	}
	return out
}

// UC: UC-040 BR-013
func TestEffect_OneSpanPerIntervention(t *testing.T) {
	t.Run("UC-040/BR-013 every signal is an attribute of the one effect span", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_one"
		provider.set(gameID, durationContext(gameID, 90))
		l, sr, _, sched := effectLoop(t, provider, gameID)

		baseline := enduranceBaseline()
		baseline["balanced.skill_gap"] = 0.4 // not evaluable at follow-up: no players
		if l.schedule(shieldDecision(), baseline, defaultFit()) == "" {
			t.Fatal("expected a scheduled sample")
		}
		sched.fire()
		l.AwaitEffects()

		spans := spansByName(sr.Ended(), SpanInterventionEffect)
		if len(spans) != 1 {
			t.Fatalf("effect spans = %d, want 1 for one intervention", len(spans))
		}
		v, _ := attrValue(spans[0], keys.AttrEffectSignals)
		const want = `{"balanced.skill_gap":{"baseline":0.4,"aborted":true},` +
			`"endurance.min_session_seconds":{"baseline":120,"followup":120,"delta":0},` +
			`"endurance.session_progress":{"baseline":0.25,"followup":0.75,"delta":0.5},` +
			`"endurance.session_seconds":{"baseline":30,"followup":90,"delta":60}}`
		if got := v.AsString(); got != want {
			t.Errorf("%s =\n %s\nwant\n %s", keys.AttrEffectSignals, got, want)
		}
		attrs := attrMap(spans[0].Attributes())
		if attrs[AttrEffectSignal] != "endurance.session_progress" {
			t.Errorf("primary signal = %v, want endurance.session_progress", attrs[AttrEffectSignal])
		}
		if attrs[AttrEffectBaseline] != float64Attr(0.25) || attrs[AttrEffectFollowUp] != float64Attr(0.75) || attrs[AttrEffectDelta] != float64Attr(0.5) {
			t.Errorf("primary baseline/followup/delta = %v/%v/%v, want 0.25/0.75/0.5",
				attrs[AttrEffectBaseline], attrs[AttrEffectFollowUp], attrs[AttrEffectDelta])
		}
		if _, aborted := attrs[AttrEffectAborted]; aborted {
			t.Error("a measured effect must not carry intervention.effect_aborted")
		}
	})

	t.Run("UC-040/BR-013 no signal left at follow-up marks the one span aborted", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_nosig"
		provider.set(gameID, gamecontext.GameContext{SessionID: gameID, GameKind: "real"}) // no duration
		l, sr, _, sched := effectLoop(t, provider, gameID)

		if l.schedule(shieldDecision(), enduranceBaseline(), defaultFit()) == "" {
			t.Fatal("expected a scheduled sample")
		}
		sched.fire()
		l.AwaitEffects()

		spans := spansByName(sr.Ended(), SpanInterventionEffect)
		if len(spans) != 1 {
			t.Fatalf("effect spans = %d, want 1", len(spans))
		}
		if v, ok := attrValue(spans[0], AttrEffectAborted); !ok || !v.AsBool() {
			t.Error("want intervention.effect_aborted=true when no signal survived")
		}
		if len(effectSignalsOf(t, spans[0])) != len(enduranceBaseline()) {
			t.Error("the aborted span still lists every baseline signal")
		}
		if _, ok := attrValue(spans[0], AttrEffectDelta); ok {
			t.Error("no primary delta without a follow-up value")
		}
	})

	t.Run("UC-040/BR-013 an objective without a primary signal has no headline delta", func(t *testing.T) {
		provider := newFakeContextProvider()
		const gameID = "game_unset"
		provider.set(gameID, durationContext(gameID, 90))
		l, sr, _, sched := effectLoop(t, provider, gameID)

		d := shieldDecision()
		d.ObjectiveServed = "" // "unset"
		if l.schedule(d, enduranceBaseline(), defaultFit()) == "" {
			t.Fatal("expected a scheduled sample")
		}
		sched.fire()
		l.AwaitEffects()

		spans := spansByName(sr.Ended(), SpanInterventionEffect)
		if len(spans) != 1 {
			t.Fatalf("effect spans = %d, want 1", len(spans))
		}
		if _, ok := attrValue(spans[0], AttrEffectSignal); ok {
			t.Error("objective unset: no intervention.signal")
		}
		if len(effectSignalsOf(t, spans[0])) != len(enduranceBaseline()) {
			t.Error("every signal still listed")
		}
	})
}
