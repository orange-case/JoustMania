package experiment

import (
	"strings"
	"sync"
)

// RetroSuggestion is one post-game retro recommendation (UC-042 A3). It mirrors
// llm.RetroSuggestion so this package needs no llm import on the record path.
type RetroSuggestion struct {
	InterventionType string
	Emphasis         string
	Reason           string
}

// MaxRetroNotes is how many recent suggestions the proposer prompt carries.
const MaxRetroNotes = 4

// RetroNotes keeps the most recent retro suggestions for the proposer prompt. It
// is in memory only; a restart forgets them. Safe for concurrent use.
type RetroNotes struct {
	mu    sync.Mutex
	items []RetroSuggestion
}

// Record keeps s, dropping the oldest beyond MaxRetroNotes. A suggestion with
// neither a reason nor an intervention type carries nothing and is dropped.
func (r *RetroNotes) Record(s RetroSuggestion) {
	if strings.TrimSpace(s.Reason) == "" && strings.TrimSpace(s.InterventionType) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, s)
	if over := len(r.items) - MaxRetroNotes; over > 0 {
		r.items = append(r.items[:0], r.items[over:]...)
	}
}

// Recent returns a copy of the kept suggestions, oldest first.
func (r *RetroNotes) Recent() []RetroSuggestion {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RetroSuggestion(nil), r.items...)
}
