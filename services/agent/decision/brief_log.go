package decision

import "sync"

// maxBriefLogGames bounds the games BriefLog remembers (oldest forgotten first).
const maxBriefLogGames = 64

// BriefLog remembers which games sent the operator brief to an in-game model
// (#2695, UC-042 A13): a remote decide request that carried a non-empty
// Request.OperatorBrief (claude and jev, operatorBriefFor). The retro reads
// it at game end to tell the analyst whether the in-game agent ever saw the brief.
// Reading never forgets, so the order of the game-end consumers does not matter.
type BriefLog struct {
	mu    sync.Mutex
	sent  map[string]bool
	order []string
}

// NewBriefLog builds an empty log.
func NewBriefLog() *BriefLog { return &BriefLog{sent: make(map[string]bool)} }

// Note records that gameID sent the brief to its decide backend.
func (b *BriefLog) Note(gameID string) {
	if b == nil || gameID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sent[gameID] {
		return
	}
	b.sent[gameID] = true
	b.order = append(b.order, gameID)
	if len(b.order) > maxBriefLogGames {
		delete(b.sent, b.order[0])
		b.order = b.order[1:]
	}
}

// Delivered reports whether gameID sent the brief to its decide backend at least once.
func (b *BriefLog) Delivered(gameID string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent[gameID]
}

// SetBriefLog injects the log the loop notes brief-carrying requests in; nil (the
// default) notes nothing. Set at construction, before the loop serves.
func (l *Loop) SetBriefLog(b *BriefLog) { l.briefLog = b }
