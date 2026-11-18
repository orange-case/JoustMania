package flagsmith

import (
	"encoding/json"
	"testing"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

func flagOf(def string, variants map[string]string) *baseline.Flag {
	f := &baseline.Flag{DefaultVariant: def, Variants: map[string]json.RawMessage{}}
	for k, v := range variants {
		f.Variants[k] = json.RawMessage(v)
	}
	return f
}

// UC: UC-054 A4
// UC: UC-055 BR-001
func TestEncodeDecode(t *testing.T) {
	variants := &registry.Policy{Key: "sensitivity", Kind: registry.KindOverlay}
	sens := flagOf("mid", map[string]string{"slow": "1", "mid": "2"})
	intRange := &registry.Policy{Key: "n", Kind: registry.KindOverlay, Bounds: registry.Bounds{Range: []float64{0, 600}, Integer: true}}
	floatRange := &registry.Policy{Key: "v", Kind: registry.KindOverlay, Bounds: registry.Bounds{Range: []float64{0, 1}}}
	plain := flagOf("d", map[string]string{"d": "0"})

	t.Run("UC-054/A4 a declared variant is its name, an absent value the baseline default", func(t *testing.T) {
		fv, err := Encode(variants, sens, Value{Present: true, Variant: "slow", Value: json.RawMessage(`1`)})
		if err != nil || fv.Type != TypeUnicode || string(fv.Value) != `"slow"` {
			t.Fatalf("got %+v %v", fv, err)
		}
		if v, raw, ferr := Decode(variants, sens, fv); ferr != nil || v != "slow" || raw != nil {
			t.Fatalf("decode %q %s %v, want the variant slow", v, raw, ferr)
		}
		absent, err := Encode(variants, sens, Value{})
		if err != nil || string(absent.Value) != `"mid"` {
			t.Fatalf("absent encodes %s %v, want the default mid", absent.Value, err)
		}
		if v, raw, ferr := Decode(variants, sens, absent); ferr != nil || v != "mid" || raw != nil {
			t.Fatalf("decode %q %s %v, want the variant mid", v, raw, ferr)
		}
	})
	t.Run("UC-054/A4 integer range is int, float range decimal text", func(t *testing.T) {
		if fv, _ := Encode(intRange, plain, val(`120`)); fv.Type != TypeInt || string(fv.Value) != "120" {
			t.Fatalf("got %+v", fv)
		}
		if fv, _ := Encode(floatRange, plain, val(`0.25`)); fv.Type != TypeUnicode || string(fv.Value) != `"0.25"` {
			t.Fatalf("got %+v", fv)
		}
		if _, raw, ferr := Decode(floatRange, plain, FSValue{Type: TypeUnicode, Value: json.RawMessage(`"0.25"`)}); ferr != nil || string(raw) != "0.25" {
			t.Fatalf("decode %s %v", raw, ferr)
		}
	})
	t.Run("UC-054/A4 a value of another type is flagsmith_type_mismatch", func(t *testing.T) {
		for _, c := range []struct {
			p  *registry.Policy
			fv FSValue
		}{
			{intRange, FSValue{Type: TypeUnicode, Value: json.RawMessage(`"abc"`)}},
			{variants, FSValue{Type: TypeInt, Value: json.RawMessage(`3`)}},
			{variants, FSValue{Type: TypeBool, Value: json.RawMessage(`true`)}},
			{floatRange, FSValue{Type: TypeUnicode, Value: json.RawMessage(`"x"`)}},
		} {
			_, _, ferr := Decode(c.p, plain, c.fv)
			if ferr == nil || ferr.Reason != fserr.FlagsmithTypeMismatch || ferr.Status != 422 {
				t.Fatalf("%s %+v: got %v, want 422 flagsmith_type_mismatch", c.p.Key, c.fv, ferr)
			}
		}
	})
	t.Run("UC-054/A4 a value decodes by the flag's type, not its JSON type (#2682)", func(t *testing.T) {
		str := func(s string) FSValue { raw, _ := json.Marshal(s); return FSValue{Type: TypeUnicode, Value: raw} }
		num := func(s string) FSValue { return FSValue{Type: TypeInt, Value: json.RawMessage(s)} }
		for _, c := range []struct {
			name string
			p    *registry.Policy
			fv   FSValue
			want string
		}{
			{"integer as a string (the edge document's int override)", intRange, str("10"), `10`},
			{"integer as a number", intRange, num("10"), `10`},
			{"integer whole-number text", intRange, str("5.0"), `5`},
			{"integer with whitespace", intRange, str(" 7 "), `7`},
			{"float pushed as text", floatRange, str("0.25"), `0.25`},
			{"float whole number as text", floatRange, str("2"), `2`},
			{"float as a number", floatRange, num("2"), `2`},
			{"float with a trailing zero", floatRange, str("0.50"), `0.50`},
			{"float with whitespace", floatRange, str(" 0.5\n"), `0.5`},
			{"integer in exponent form", intRange, str("2e1"), `20`},
			{"negative integer as text", intRange, str("-2"), `-2`},
			{"negative integer as a number", intRange, num("-1"), `-1`},
			{"negative float as text", floatRange, str("-0.25"), `-0.25`},
			{"negative float as a number", floatRange, num("-1"), `-1`},
		} {
			_, raw, ferr := Decode(c.p, plain, c.fv)
			// The exact bytes: the write path checks this value (an integer
			// range refuses the text "5.0").
			if ferr != nil || string(raw) != c.want {
				t.Fatalf("%s: decode %s %v, want %s", c.name, raw, ferr, c.want)
			}
		}
		// The decoded value equals the local one: the link's own push reads
		// back unchanged.
		_, raw, _ := Decode(floatRange, plain, str("0.50"))
		if !(Value{Present: true, Value: raw}).Equal(Value{Present: true, Value: json.RawMessage(`0.5`)}) {
			t.Fatalf("\"0.50\" decodes to %s, not equal to a local 0.5", raw)
		}
		_, raw, _ = Decode(intRange, plain, str("10"))
		if !(Value{Present: true, Value: raw}).Equal(Value{Present: true, Value: json.RawMessage(`10`)}) {
			t.Fatalf("\"10\" decodes to %s, not equal to a local 10", raw)
		}
		onOff := &registry.Policy{Key: "b", Kind: registry.KindOverlay}
		bools := flagOf("off", map[string]string{"on": "true", "off": "false"})
		for _, fv := range []FSValue{{Type: TypeBool, Value: json.RawMessage(`true`)}, str("true")} {
			if v, _, ferr := Decode(onOff, bools, fv); ferr != nil || v != "on" {
				t.Fatalf("bool %s: variant %q %v, want on", fv.Value, v, ferr)
			}
		}
		if v, _, ferr := Decode(onOff, bools, str("false")); ferr != nil || v != "off" {
			t.Fatalf("bool \"false\": variant %q %v, want off", v, ferr)
		}
		// Not a boolean of the flag: refused, never coerced (§2.1).
		for _, s := range []string{"True", "1", "yes"} {
			if _, _, ferr := Decode(onOff, bools, str(s)); ferr == nil || ferr.Reason != fserr.FlagsmithTypeMismatch {
				t.Fatalf("bool %q: got %v, want flagsmith_type_mismatch", s, ferr)
			}
		}
	})
	t.Run("UC-054/A4 text that is not the flag's number stays flagsmith_type_mismatch (#2682)", func(t *testing.T) {
		str := func(s string) FSValue { raw, _ := json.Marshal(s); return FSValue{Type: TypeUnicode, Value: raw} }
		for _, c := range []struct {
			p  *registry.Policy
			fv FSValue
		}{
			{intRange, str("5.5")},
			{intRange, FSValue{Type: TypeInt, Value: json.RawMessage(`5.5`)}},
			{intRange, str("")},
			{floatRange, str(".5")},
			{floatRange, str("1,5")},
			{floatRange, str("NaN")},
			{floatRange, str("+1")},
			{intRange, str("Infinity")},
		} {
			_, _, ferr := Decode(c.p, plain, c.fv)
			if ferr == nil || ferr.Reason != fserr.FlagsmithTypeMismatch {
				t.Fatalf("%s %s: got %v, want flagsmith_type_mismatch", c.p.Key, c.fv.Value, ferr)
			}
		}
	})
	t.Run("UC-055/BR-001 ValueOfChange: fraction has one form, lifecycle kinds map to nothing", func(t *testing.T) {
		pct := 10
		k, v, ok := ValueOfChange(state.Change{Kind: state.ChangeValue, Set: "controller", Flag: "bluetooth_backend", ScopeName: registry.ScopeFraction,
			Scope: state.Scope{FractionPercent: &pct}, DeclaredVariant: "rust", Value: json.RawMessage(`"rust"`)})
		if !ok || k.Scope != "fraction" || string(v.Value) != `{"variant":"rust","fraction_percent":10}` {
			t.Fatalf("key %v value %s", k, v.Value)
		}
		if _, _, ok := ValueOfChange(state.Change{Kind: state.ChangeGameEnded}); ok {
			t.Fatal("a lifecycle Change mapped to a value")
		}
	})
	t.Run("UC-055/BR-001 identities map both ways", func(t *testing.T) {
		for _, id := range []string{"game:g1", "game:g1:MOCK0001", "experiment:e1:control", "real", "fraction"} {
			sc, name, ok := ScopeOfIdentity(id)
			if !ok || IdentityOf(KeyOf("s", "f", name, sc)) != id {
				t.Fatalf("%s: %+v %q %v", id, sc, name, ok)
			}
		}
		if _, _, ok := ScopeOfIdentity("game:"); ok {
			t.Fatal("game: parsed")
		}
	})
}
