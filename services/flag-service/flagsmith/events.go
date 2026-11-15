package flagsmith

import (
	"bytes"
	"encoding/json"

	"github.com/joustmania/flag-service/registry"
)

// Events from Flagsmith (architecture note §4.3, UC-054 step 4): a person sets
// an override on game:<id> of an event flag with ONE event payload (a JSON
// object); the link appends it (unconditionally: the list is append-only), the
// out side pushes the resulting list back, and remote is set before the
// append so the payload is never appended twice. A document list equal to, or
// a prefix of, the local list is a stale or own value and does nothing.

// eventKind classifies the document value of an event key.
type eventKind int

const (
	// evAppend: one payload to append.
	evAppend eventKind = iota
	// evAdopt: the document holds the local list.
	evAdopt
	// evStale: the document holds an older list (a prefix of the local one).
	evStale
	// evMalformed: anything else (refused: set one event payload).
	evMalformed
)

// classifyEvent reads d, the document value of an event key, against l, the
// local event list. payload is set for evAppend.
func classifyEvent(d, l Value) (eventKind, map[string]any) {
	raw := bytes.TrimSpace(d.Value)
	if len(raw) == 0 {
		return evMalformed, nil
	}
	switch raw[0] {
	case '{':
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil {
			return evMalformed, nil
		}
		if _, isList := payload["events"]; isList {
			return evMalformed, nil
		}
		return evAppend, payload
	case '[':
		var dl, ll []json.RawMessage
		if json.Unmarshal(raw, &dl) != nil {
			return evMalformed, nil
		}
		if l.Present {
			_ = json.Unmarshal(l.Value, &ll)
		}
		if len(dl) > len(ll) {
			return evMalformed, nil
		}
		for i := range dl {
			if !jsonEqual(dl[i], ll[i]) {
				return evMalformed, nil
			}
		}
		if len(dl) == len(ll) {
			return evAdopt, nil
		}
		return evStale, nil
	}
	return evMalformed, nil
}

// isEventKey reports whether k is the game scope of an event flag.
func isEventKey(kind string, k Key) bool {
	return kind == registry.KindEvent && GameOfIdentity(k.Scope) != "" && !hasSerial(k.Scope)
}

func hasSerial(identifier string) bool {
	sc, _, ok := ScopeOfIdentity(identifier)
	return ok && sc.Serial != ""
}
