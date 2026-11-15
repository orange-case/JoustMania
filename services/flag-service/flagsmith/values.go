package flagsmith

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// Value is a synced value: what the stack runs or what Flagsmith shows for one
// Key. Present false: no value (stack: no overlay, the baseline applies;
// Flagsmith: enabled=false or no override).
type Value struct {
	Present bool
	// Variant is the declared variant name when the value is one, else "".
	Variant string
	// Value is the JSON value (compact). Event lists: the events array;
	// fraction: FractionValue's object.
	Value json.RawMessage
	// Writer is the writer label of the local change; it is not part of
	// Equal (Flagsmith does not store it).
	Writer string
}

// Equal compares presence, then the variant names when both carry one, else
// the JSON values (key order and number spelling ignored).
func (v Value) Equal(o Value) bool {
	if v.Present != o.Present {
		return false
	}
	if !v.Present {
		return true
	}
	if v.Variant != "" && o.Variant != "" {
		return v.Variant == o.Variant
	}
	return jsonEqual(v.Value, o.Value)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	return reflect.DeepEqual(x, y)
}

func compact(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}

// FractionValue is the one form of a fraction overlay's value (architecture
// note §2): {"variant": <declared name, or the value>, "fraction_percent": n}.
func FractionValue(variant string, value json.RawMessage, percent int) json.RawMessage {
	v := compact(value)
	if variant != "" {
		v, _ = json.Marshal(variant)
	}
	out, _ := json.Marshal(struct {
		Variant         json.RawMessage `json:"variant"`
		FractionPercent int             `json:"fraction_percent"`
	}{v, percent})
	return out
}

// ValueOfChange maps an observer Change to its Key and Value, the one mapping
// the out and in sides compare with. ok is false for the lifecycle kinds.
func ValueOfChange(c state.Change) (Key, Value, bool) {
	k := KeyOf(c.Set, c.Flag, c.ScopeName, c.Scope)
	switch c.Kind {
	case state.ChangeCleared:
		return k, Value{Writer: c.Writer}, true
	case state.ChangeEvents:
		return k, Value{Present: true, Value: compact(c.Events), Writer: c.Writer}, true
	case state.ChangeValue:
		if c.ScopeName == registry.ScopeFraction {
			pct := 0
			if c.Scope.FractionPercent != nil {
				pct = *c.Scope.FractionPercent
			}
			return k, Value{Present: true, Value: FractionValue(c.DeclaredVariant, c.Value, pct), Writer: c.Writer}, true
		}
		return k, Value{Present: true, Variant: c.DeclaredVariant, Value: compact(c.Value), Writer: c.Writer}, true
	}
	return Key{}, Value{}, false
}

// Encode turns a Value into the Flagsmith value of architecture note §2.1: a
// declared variant is its name (unicode); an integer range is int; a float
// range is decimal text; a string is itself; an object or array (shape flags,
// event lists, fraction) is compact JSON text. A Value that is not Present
// encodes the baseline default (the defaultVariant's name), which Flagsmith
// shows with enabled=false for reference.
func Encode(p *registry.Policy, b *baseline.Flag, v Value) (FSValue, error) {
	if b == nil {
		return FSValue{}, fmt.Errorf("flagsmith: no baseline flag")
	}
	if !v.Present {
		v = Value{Present: true, Variant: b.DefaultVariant, Value: b.Variants[b.DefaultVariant]}
	}
	if _, ok := b.Variants[v.Variant]; ok && v.Variant != "" {
		return unicode(v.Variant), nil
	}
	raw := bytes.TrimSpace(v.Value)
	if len(raw) == 0 {
		return FSValue{}, fmt.Errorf("flagsmith: empty value")
	}
	switch raw[0] {
	case '{', '[':
		return unicode(string(compact(raw))), nil
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return FSValue{}, fmt.Errorf("flagsmith: bad string value")
		}
		return unicode(s), nil
	}
	if p != nil && p.Bounds.Range != nil {
		f, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return FSValue{}, fmt.Errorf("flagsmith: %s is not a number", raw)
		}
		if p.Bounds.Integer && f == math.Trunc(f) {
			return FSValue{Type: TypeInt, Value: json.RawMessage(strconv.FormatInt(int64(f), 10))}, nil
		}
		return unicode(string(raw)), nil
	}
	return unicode(string(raw)), nil
}

func unicode(s string) FSValue {
	raw, _ := json.Marshal(s)
	return FSValue{Type: TypeUnicode, Value: raw}
}

// Decode turns a Flagsmith value into the variant or value of a write
// (architecture note §2.1): a string naming a declared variant selects it;
// otherwise the value is parsed for the flag's bounds. It checks the type
// only: the write path checks the bounds. A type the table does not give the
// flag is 422 flagsmith_type_mismatch (UC-054 A4).
func Decode(p *registry.Policy, b *baseline.Flag, fv FSValue) (variant string, value json.RawMessage, ferr *fserr.Error) {
	if p == nil || b == nil {
		return "", nil, fserr.Unprocessable(fserr.FlagReadOnly, "no registry row")
	}
	mismatch := func() (string, json.RawMessage, *fserr.Error) {
		return "", nil, fserr.Unprocessable(fserr.FlagsmithTypeMismatch, "%s: a Flagsmith %s value does not fit the flag's type (architecture note §2.1)", p.Key, typeName(fv.Type))
	}
	switch fv.Type {
	case TypeInt:
		if p.Bounds.Range == nil {
			return mismatch()
		}
		return "", compact(fv.Value), nil
	case TypeUnicode:
	default:
		return mismatch()
	}
	var s string
	if err := json.Unmarshal(fv.Value, &s); err != nil {
		return mismatch()
	}
	if _, ok := b.Variants[s]; ok {
		return s, nil, nil
	}
	switch {
	case p.Bounds.Range != nil:
		if p.Bounds.Integer {
			return mismatch()
		}
		if _, err := strconv.ParseFloat(s, 64); err != nil || !json.Valid([]byte(s)) {
			return mismatch()
		}
		return "", json.RawMessage(s), nil
	case p.Bounds.Pattern != nil, p.Bounds.FreeString:
		raw, _ := json.Marshal(s)
		return "", raw, nil
	case p.Bounds.Shape, p.Kind == registry.KindEvent:
		if s == "" || (s[0] != '{' && s[0] != '[') || !json.Valid([]byte(s)) {
			return mismatch()
		}
		return "", compact(json.RawMessage(s)), nil
	}
	raw, _ := json.Marshal(s)
	return "", raw, nil
}

func typeName(t string) string {
	if t == "" {
		return "empty"
	}
	return t
}
