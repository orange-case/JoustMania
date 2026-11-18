package gamecontext

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

// Moment log events (#2658 slice 2, FR-218, UC-041 A7, BR-009). The coordinator
// records a moment as a span event, a log event or both (UC-100 BR-001); a log
// event carries the moment's name as its event_name and the identity attributes
// (lib/moments.py record_moment). The case collector passes only these records to
// the agent (filter/agent_moments, C-001).
const (
	// momentDeath is the death moment, named like its span event.
	momentDeath = spanEventDeath
	// momentWarning is the warning moment: recognised, changes nothing.
	momentWarning = "death_warning"
	// attrMomentID is the moment identity, the same on every record of one moment.
	attrMomentID = "moment.id"
)

// momentIDOf returns the moment.id attribute, or "" when absent.
func momentIDOf(attrs pcommon.Map) string {
	if v, ok := attrs.Get(attrMomentID); ok {
		return v.AsString()
	}
	return ""
}

// ApplyLogs walks a log batch and applies each moment log event to its game's
// existing partition (UC-041 A7). It returns the deduped set of game ids whose
// context a record changed: a new player_death moment. It never creates a
// partition and never feeds the fallback one; it skips the agent's own resources,
// every record that is not a moment the agent reads, and every record without a
// moment id, game id or player (A7 step 4).
func (m *Multiplexer) ApplyLogs(ld plog.Logs) []string {
	touched := newTouchedSet()
	own := m.ownServiceName()
	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		if isOwnResource(rl.Resource(), own) {
			continue
		}
		sls := rl.ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			records := sls.At(j).LogRecords()
			for k := 0; k < records.Len(); k++ {
				if gameID, ok := m.applyMomentLog(records.At(k)); ok {
					touched.add(gameID)
				}
			}
		}
	}
	return touched.ids()
}

// applyMomentLog applies one log record and reports the game id it changed.
// A death_warning is recognised and changes nothing (UC-041 A7 step 2): it adds
// no player and does not refresh the player's LastUpdate, so the freshness gate
// never sees a log record as a live signal. A player_death counts once per
// moment id across span events and log events (BR-009).
func (m *Multiplexer) applyMomentLog(lr plog.LogRecord) (string, bool) {
	name := lr.EventName()
	if name != momentDeath && name != momentWarning {
		return "", false
	}
	attrs := lr.Attributes()
	id := momentIDOf(attrs)
	serial := attrString(attrs, spanAttrSerial)
	gameID := attrString(attrs, spanAttrGameID)
	if id == "" || serial == "" || gameID == "" {
		return "", false
	}
	if name == momentWarning {
		return "", false
	}
	s := m.existingStore(gameID)
	if s == nil {
		return "", false
	}
	if !s.recordDeathMoment(id, serial, deathEliminates(attrs)) {
		return "", false
	}
	return gameID, true
}

// attrString returns a string attribute, or "" when absent.
func attrString(attrs pcommon.Map, key string) string {
	if v, ok := attrs.Get(key); ok {
		return v.AsString()
	}
	return ""
}
