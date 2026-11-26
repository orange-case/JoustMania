package gamecontext

import (
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

// The agent's own lifecycle log events (#2722, FR-250, UC-041 A8): sent by the
// coordinator only to the agent, marked attrAudience = audienceAgent. The case
// collector passes only these records to the agent (filter/agent_events, C-001).
// No log record carries a death: eliminations come from the elimination-order
// gauge alone (UC-041 BR-008).
const (
	logEventGameStart  = "game.start"
	logEventGameEnd    = "game.end"
	logEventPhaseStart = "game.phase.start"
	attrAudience       = "orangecase.audience"
	audienceAgent      = "agent"
	attrPhaseSpan      = "game.phase.span"
	phaseGameplay      = "game.phase.gameplay"
	phaseGameCycle     = "game.cycle"

	// Pre-#2837 coordinator values, accepted for one release (telemetry.md §8).
	phaseGameplayLegacy  = "gameplay_phase"
	phaseGameCycleLegacy = "game_cycle"
)

// ApplyLogs walks a log batch and applies each game.start / game.phase.start /
// game.end log event (UC-041 A8) to its game's partition, opening a held one for
// an unknown game (BR-010), never the fallback. It skips the agent's own resources
// and every other record. No record changes a context the gate evaluates, so the
// receiver starts no evaluation from a log batch.
func (m *Multiplexer) ApplyLogs(ld plog.Logs) {
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
				m.applyLog(records.At(k))
			}
		}
	}
}

// applyLog dispatches one record on its event name.
func (m *Multiplexer) applyLog(lr plog.LogRecord) {
	switch lr.EventName() {
	case logEventGameStart:
		m.applyGameStart(lr)
	case logEventGameEnd:
		m.applyGameEnd(lr)
	case logEventPhaseStart:
		m.applyPhaseStart(lr)
	}
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
	s := m.openFromLog(gameID)
	s.setFromLog(attrString(attrs, spanAttrGameKind), attrString(attrs, spanAttrGameMode))
	if traceID, spanID, ok := sampledContext(lr); ok {
		s.setTraceFromLog(logTraceIDs{trace: traceID, root: spanID})
	}
}

// applyPhaseStart opens and holds the game's partition like game.start and takes
// the opened span's id from the record's trace context (UC-041 A8 step 5, BR-004),
// plus the game's trace id when a lost game.start left it unknown. The root span id
// comes only from game.start.
func (m *Multiplexer) applyPhaseStart(lr plog.LogRecord) {
	attrs := lr.Attributes()
	gameID := attrString(attrs, spanAttrGameID)
	if gameID == "" || !forAgent(attrs) {
		return
	}
	s := m.openFromLog(gameID)
	s.setFromLog(attrString(attrs, spanAttrGameKind), "")
	traceID, spanID, ok := sampledContext(lr)
	if !ok {
		return
	}
	switch attrString(attrs, attrPhaseSpan) {
	case phaseGameplay, phaseGameplayLegacy:
		s.setTraceFromLog(logTraceIDs{phase: spanID, phaseTrace: traceID})
	case phaseGameCycle, phaseGameCycleLegacy:
		s.setTraceFromLog(logTraceIDs{cycle: spanID, phaseTrace: traceID})
	}
}

// sampledContext returns the record's trace and span id in hex, ok only for a
// valid, sampled context: an unsampled span is never exported, so parenting on it
// would dangle.
func sampledContext(lr plog.LogRecord) (traceID, spanID string, ok bool) {
	tid, sid := lr.TraceID(), lr.SpanID()
	if tid.IsEmpty() || sid.IsEmpty() || !lr.Flags().IsSampled() {
		return "", "", false
	}
	return tid.String(), sid.String(), true
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
	m.openFromLog(gameID).EndGameFromEvent(recordTime(lr))
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
