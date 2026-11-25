package gamecontext

import (
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

// Moment log events (#2658 slice 2, FR-218, UC-041 A7, BR-009). The coordinator
// records a moment as a span event, a log event or both (UC-100 BR-001); a log
// event carries the moment's name as its event_name and the identity attributes
// (lib/moments.py record_moment). The case collector passes only these records and
// the agent's own lifecycle log events to the agent (filter/agent_moments, C-001).
const (
	// momentDeath is the death moment, named like its span event.
	momentDeath = spanEventDeath
	// momentWarning is the warning moment: recognised, changes nothing.
	momentWarning = "death_warning"
	// attrMomentID is the moment identity, the same on every record of one moment.
	attrMomentID = "moment.id"
)

// The agent's own lifecycle log events (#2722, FR-250, UC-041 A8): sent by the
// coordinator only to the agent, marked attrAudience = audienceAgent.
const (
	logEventGameStart = "game.start"
	logEventGameEnd   = "game.end"
	attrAudience      = "orangecase.audience"
	audienceAgent     = "agent"
)

// The moment observer's labels (#2658 slice 3, agent_moments_received_total{moment,
// shape,outcome}, telemetry.md §6): the channel a moment record arrived on and what
// the agent did with it. moment is momentDeath or momentWarning, never another name.
const (
	MomentShapeSpanEvent = "span_event"
	MomentShapeLogEvent  = "log_event"
	// MomentCounted: a new player_death applied (UC-041 BR-008, A7 step 2).
	MomentCounted = "counted"
	// MomentDuplicate: a player_death whose moment id was applied before (BR-009).
	MomentDuplicate = "duplicate"
	// MomentIgnored: a death_warning with its identity; it changes nothing (A7 step 2).
	MomentIgnored = "ignored"
	// MomentDropped: a log record without moment id, game id or player (A7 step 4).
	MomentDropped = "dropped"
)

// MomentObserver is told about every moment record the agent reads: the moment's
// name, the shape it arrived in and the outcome. Called outside every store lock.
type MomentObserver func(moment, shape, outcome string)

// observeMoment calls obs when set.
func observeMoment(obs MomentObserver, moment, shape, outcome string) {
	if obs != nil {
		obs(moment, shape, outcome)
	}
}

// momentIDOf returns the moment.id attribute, or "" when absent.
func momentIDOf(attrs pcommon.Map) string {
	if v, ok := attrs.Get(attrMomentID); ok {
		return v.AsString()
	}
	return ""
}

// ApplyLogs walks a log batch and applies each moment log event (UC-041 A7) and
// each game.start / game.end log event (A8) to its game's partition, opening a
// held one for an unknown game (BR-010), never the fallback. It returns the
// deduped set of game ids whose context a new player_death moment changed. It
// skips the agent's own resources and every other record.
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
				if gameID, ok := m.applyLog(records.At(k)); ok {
					touched.add(gameID)
				}
			}
		}
	}
	return touched.ids()
}

// applyLog dispatches one record on its event name and reports the game id a new
// death changed.
func (m *Multiplexer) applyLog(lr plog.LogRecord) (string, bool) {
	switch lr.EventName() {
	case momentDeath, momentWarning:
		return m.applyMomentLog(lr)
	case logEventGameStart:
		m.applyGameStart(lr)
	case logEventGameEnd:
		m.applyGameEnd(lr)
	}
	return "", false
}

// applyMomentLog applies one moment log record and reports the game id it changed.
// A death_warning is recognised and changes nothing (UC-041 A7 step 2) beyond
// opening its game's partition. No moment record adds a player or refreshes a
// player's LastUpdate, so the freshness gate never sees a log record as a live
// signal (BR-010). A player_death counts once per moment id across span events
// and log events (BR-009).
func (m *Multiplexer) applyMomentLog(lr plog.LogRecord) (string, bool) {
	name := lr.EventName()
	obs := m.momentObserver()
	attrs := lr.Attributes()
	id := momentIDOf(attrs)
	serial := attrString(attrs, spanAttrSerial)
	gameID := attrString(attrs, spanAttrGameID)
	if id == "" || serial == "" || gameID == "" {
		observeMoment(obs, name, MomentShapeLogEvent, MomentDropped)
		return "", false
	}
	s := m.openFromLog(gameID)
	s.setFromLog(attrString(attrs, spanAttrGameKind), "")
	if name == momentWarning {
		observeMoment(obs, name, MomentShapeLogEvent, MomentIgnored)
		return "", false
	}
	outcome := s.recordDeathMoment(id, serial, deathEliminates(attrs))
	observeMoment(obs, name, MomentShapeLogEvent, outcome)
	if outcome != MomentCounted {
		return "", false
	}
	return gameID, true
}

// applyGameStart opens and holds the game's partition with its kind and mode
// (UC-041 A8 step 2). It never sets the game active: game_active stays the
// source of that (BR-001).
func (m *Multiplexer) applyGameStart(lr plog.LogRecord) {
	attrs := lr.Attributes()
	gameID := attrString(attrs, spanAttrGameID)
	if gameID == "" || !forAgent(attrs) {
		return
	}
	m.openFromLog(gameID).setFromLog(attrString(attrs, spanAttrGameKind), attrString(attrs, spanAttrGameMode))
}

// applyGameEnd ends the game through the single end transition with
// EndReasonEndEvent, the bracket closed at the record's time (UC-041 A8 step 4,
// BR-005). For an unknown game id it opens a held partition that only latches
// the end, so a late game_active 1 never starts that game.
func (m *Multiplexer) applyGameEnd(lr plog.LogRecord) {
	attrs := lr.Attributes()
	gameID := attrString(attrs, spanAttrGameID)
	if gameID == "" || !forAgent(attrs) {
		return
	}
	s := m.openFromLog(gameID)
	s.setFromLog(attrString(attrs, spanAttrGameKind), "")
	s.EndGameFromEvent(recordTime(lr))
}

// forAgent reports whether a record carries the agent-only marker (UC-100 BR-012).
func forAgent(attrs pcommon.Map) bool {
	return attrString(attrs, attrAudience) == audienceAgent
}

// recordTime is the record's timestamp, else its observed timestamp, else zero.
func recordTime(lr plog.LogRecord) time.Time {
	if ts := lr.Timestamp(); ts != 0 {
		return ts.AsTime()
	}
	if ts := lr.ObservedTimestamp(); ts != 0 {
		return ts.AsTime()
	}
	return time.Time{}
}

// attrString returns a string attribute, or "" when absent.
func attrString(attrs pcommon.Map, key string) string {
	if v, ok := attrs.Get(key); ok {
		return v.AsString()
	}
	return ""
}
