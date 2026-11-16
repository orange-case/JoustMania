package decision

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joustmania/agent/gamecontext"
)

// Outcomes of a noted intervention (#2640), as the agent.game.summary span lists
// them. Noop answers are only counted.
const (
	NotedApplied     = "applied"
	NotedBlocked     = "blocked"
	NotedGatedOff    = "gated_off"
	NotedWriteFailed = "write_failed"
	NotedHeld        = "held"
)

// maxInterventionLogEntries bounds the entries kept per game; later ones are only
// counted (Dropped). maxInterventionLogGames / maxInterventionLogTaken bound the
// games kept and the taken ids remembered, as for the GroupLog.
const (
	maxInterventionLogEntries = 64
	maxInterventionLogGames   = 8
	maxInterventionLogTaken   = 64
)

// InterventionRecord is one agent intervention of a game as the summary span lists
// it (#2640): what the agent did, to whom, when, and how it ended.
type InterventionRecord struct {
	At         time.Time
	Action     string
	Target     string // serial, or the comma-joined serials of a group shield
	TargetName string // never set for a shadow game
	Value      string
	Outcome    string
	Backend    string
}

// GameInterventions is everything the log noted for one game.
type GameInterventions struct {
	Records  []InterventionRecord // decision order, at most maxInterventionLogEntries
	Dropped  int                  // entries over the cap
	Held     int                  // held answers, the dropped ones included
	Noop     int                  // remote answers that chose noop
	Backends []string             // decide backends behind Records and Noop, sorted
}

// InterventionLog keeps the interventions of the live games until the game
// summary takes them (#2640). One log is shared by every per-game Loop; safe for
// concurrent use. Observation only: nothing reads it to decide.
type InterventionLog struct {
	mu     sync.Mutex
	byGame map[string]*gameInterventionsAcc
	order  []string        // game ids of byGame, oldest first
	taken  map[string]bool // game ids Take already returned
	takenQ []string
}

type gameInterventionsAcc struct {
	out      GameInterventions
	backends map[string]bool
}

// NewInterventionLog builds an empty log.
func NewInterventionLog() *InterventionLog {
	return &InterventionLog{byGame: make(map[string]*gameInterventionsAcc), taken: make(map[string]bool)}
}

// game returns the accumulator for gameID, creating it; nil when the summary
// already took the game. Caller holds mu.
func (g *InterventionLog) game(gameID string) *gameInterventionsAcc {
	if g.taken[gameID] {
		return nil
	}
	acc, ok := g.byGame[gameID]
	if !ok {
		acc = &gameInterventionsAcc{backends: make(map[string]bool)}
		g.byGame[gameID] = acc
		g.order = append(g.order, gameID)
		if len(g.order) > maxInterventionLogGames {
			delete(g.byGame, g.order[0])
			g.order = g.order[1:]
		}
	}
	return acc
}

func (g *InterventionLog) note(gameID string, r InterventionRecord) {
	if g == nil || gameID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	acc := g.game(gameID)
	if acc == nil {
		return
	}
	if r.Backend != "" {
		acc.backends[r.Backend] = true
	}
	if r.Outcome == NotedHeld {
		acc.out.Held++
	}
	if len(acc.out.Records) >= maxInterventionLogEntries {
		acc.out.Dropped++
		return
	}
	acc.out.Records = append(acc.out.Records, r)
}

func (g *InterventionLog) noteNoop(gameID, backend string) {
	if g == nil || gameID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	acc := g.game(gameID)
	if acc == nil {
		return
	}
	if backend != "" {
		acc.backends[backend] = true
	}
	acc.out.Noop++
}

// Take returns what the log noted for the game and forgets it. A game with nothing
// noted yields the zero value.
func (g *InterventionLog) Take(gameID string) GameInterventions {
	if g == nil {
		return GameInterventions{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out GameInterventions
	if acc, ok := g.byGame[gameID]; ok {
		out = acc.out
		for b := range acc.backends {
			out.Backends = append(out.Backends, b)
		}
		sort.Strings(out.Backends)
		delete(g.byGame, gameID)
		for i, id := range g.order {
			if id == gameID {
				g.order = append(g.order[:i], g.order[i+1:]...)
				break
			}
		}
	}
	if !g.taken[gameID] {
		g.taken[gameID] = true
		g.takenQ = append(g.takenQ, gameID)
		if len(g.takenQ) > maxInterventionLogTaken {
			delete(g.taken, g.takenQ[0])
			g.takenQ = g.takenQ[1:]
		}
	}
	return out
}

// SetInterventionLog wires the shared log. Set at construction, before the loop serves.
func (l *Loop) SetInterventionLog(g *InterventionLog) { l.interventionLog = g }

// noteIntervention records one decision of this game for the summary span (#2640).
func (l *Loop) noteIntervention(c gamecontext.GameContext, d Decision, outcome, backend string, at time.Time) {
	if l.interventionLog == nil {
		return
	}
	if backend == "" {
		backend = RulesBackendName
	}
	r := InterventionRecord{At: at, Action: d.Intervention, Value: d.Value, Outcome: outcome, Backend: backend}
	switch {
	case len(d.TargetSerials) > 0:
		r.Target = strings.Join(d.TargetSerials, ",")
	case d.TargetSerial != "":
		r.Target = d.TargetSerial
		// Shadow games run on mock serials that collide with real ones (targetAttributes).
		if l.playerName != nil && c.GameKind != gamecontext.GameKindShadow {
			r.TargetName = l.playerName(d.TargetSerial)
		}
	}
	l.interventionLog.note(l.summaryGameID(c), r)
}

// noteNoopAnswer counts a remote answer that chose noop (#2640).
func (l *Loop) noteNoopAnswer(c gamecontext.GameContext, backend string) {
	if l.interventionLog == nil {
		return
	}
	l.interventionLog.noteNoop(l.summaryGameID(c), backend)
}

// summaryGameID is the game a note belongs to: the context's, else this loop's
// (a remote answer landing after the partition was dropped carries none).
func (l *Loop) summaryGameID(c gamecontext.GameContext) string {
	if c.SessionID != "" {
		return c.SessionID
	}
	return l.gameID
}
