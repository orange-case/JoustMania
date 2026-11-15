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
