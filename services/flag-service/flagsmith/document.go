package flagsmith

import (
	"bytes"
	"encoding/json"
	"time"
)

// EnvironmentDocument is the part of Flagsmith's environment document the link
// reads (GET {document URL}environment-document/, server-side key).
type EnvironmentDocument struct {
	Name                            string             `json:"name"`
	UpdatedAt                       string             `json:"updated_at"`
	UseIdentityOverridesInLocalEval bool               `json:"use_identity_overrides_in_local_eval"`
	FeatureStates                   []FeatureState     `json:"feature_states"`
	IdentityOverrides               []IdentityOverride `json:"identity_overrides"`
}

// Updated parses UpdatedAt (the change time of NFR-058's delay metric).
func (d *EnvironmentDocument) Updated() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, d.UpdatedAt)
	return t, err == nil
}

// Feature is a feature as the document names it.
type Feature struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// FeatureState is one feature's state in the environment or on an identity.
type FeatureState struct {
	Feature           Feature         `json:"feature"`
	Enabled           bool            `json:"enabled"`
	FeatureSegment    json.RawMessage `json:"feature_segment,omitempty"`
	FeatureStateValue json.RawMessage `json:"feature_state_value"`
}

// IdentityOverride is one identity with its overrides (architecture note §2:
// one identity per non-stack scope).
type IdentityOverride struct {
	Identifier       string         `json:"identifier"`
	IdentityUUID     string         `json:"identity_uuid"`
	IdentityFeatures []FeatureState `json:"identity_features"`
}

// Flagsmith value types (architecture note §2.1).
const (
	TypeUnicode = "unicode"
	TypeInt     = "int"
	TypeBool    = "bool"
)

// FSValue is a Flagsmith value: its type and the JSON literal (a JSON string
// for unicode, a number for int, true/false for bool).
type FSValue struct {
	Type  string
	Value json.RawMessage
}

// FSValueOf types a feature_state_value as the document carries it: a string
// is unicode, a number int, a boolean bool; null or absent is the zero FSValue.
func FSValueOf(raw json.RawMessage) FSValue {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return FSValue{}
	}
	switch raw[0] {
	case '"':
		return FSValue{Type: TypeUnicode, Value: raw}
	case 't', 'f':
		return FSValue{Type: TypeBool, Value: raw}
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return FSValue{Type: TypeInt, Value: raw}
	}
	return FSValue{Type: "", Value: raw}
}
