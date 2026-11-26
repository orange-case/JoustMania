package state

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/registry"
)

// ChangeKind says what a Change reports (services.md §7.3, the Flagsmith link).
type ChangeKind string

// The kinds of Change.
const (
	// ChangeValue reports that a slot now holds a value (for the stack slot: the value in
	// force, the in-memory overlay over the persisted admin value).
	ChangeValue ChangeKind = "value"
	// ChangeCleared reports that a slot is empty now.
	ChangeCleared ChangeKind = "cleared"
	// ChangeEvents reports that an event list changed (an append or a clear, which reports
	// an empty array).
	ChangeEvents ChangeKind = "events"
	// ChangeGameRegistered reports that a game was registered, or its kind, serials or
	// experiment arm changed (a reconcile that only refreshes LastSeenAt
	// reports nothing).
	ChangeGameRegistered ChangeKind = "game_registered"
	// ChangeGameEnded reports that a game scope ended; every value written for it is gone
	// with it (no per-value Changes follow).
	ChangeGameEnded ChangeKind = "game_ended"
	// ChangeExperimentEnded reports that the values of one experiment arm
	// expired (each value was reported as ChangeCleared first).
	ChangeExperimentEnded ChangeKind = "experiment_ended"
)

// Change is one change of the runtime state, reported to the Observer under
// State.mu, in the order the changes happened.
//
// Values are never mutated after they are placed: Value and Events may be
// kept by the observer without a copy. Game and Prev are copies.
type Change struct {
	Kind      ChangeKind
	Set, Flag string
	// Scope holds only the fields of ScopeName.
	Scope     Scope
	ScopeName string // registry.Scope*
	// Value is the decoded value now in force for the slot; nil when cleared.
	Value json.RawMessage
	// DeclaredVariant is the baseline variant name when the value is a
	// declared variant, else "".
	DeclaredVariant string
	// Events is the event list's events array (ChangeEvents).
	Events json.RawMessage
	// Writer is the writer label of the value in force (ChangeValue), the
	// label of the write that cleared the slot or appended the event, the
	// selector of DELETE /{set}?writer=, or "" for an expiry.
	Writer    string
	Persisted bool
	// Version is the snapshot version after the change.
	Version uint64
	// SlotVersion is the slot's version after the change (the value a pulled
	// write passes as IfSlotVersion, services.md §7.3).
	SlotVersion uint64
	At          time.Time
	Game        *Game    // lifecycle kinds: the game (a copy)
	Prev        *Game    // ChangeGameRegistered: the record before, nil on a first registration
	End         *GameEnd // ChangeGameEnded
}

// Observer receives every Change. StateChanged is called with State.mu held:
// it must not block and must not call State.
type Observer interface {
	StateChanged(Change)
}

// SetObserver installs o (nil removes it). Changes made before are not
// reported; State.Changes gives the full picture.
func (s *State) SetObserver(o Observer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.obs = o
}

// Registry returns the mutable-flag registry the state was built with.
func (s *State) Registry() *registry.Registry { return s.reg }

// Baseline returns the baseline the state was built with.
func (s *State) Baseline() *baseline.Baseline { return s.base }

// firstSlotVersion is the version of a slot no write has touched since start
// (or since its game ended or its experiment expired). 0 is never a slot
// version: it is the "no precondition" value of IfSlotVersion.
//
// Slot versions (services.md §7.3, compare-and-set): every write, clear,
// expiry and serial drop of a slot moves its version to the next value of one
// counter, so a version survives clears and an empty slot can be read and
// checked like a full one. The versions of a game's slots go when the game
// ends, those of an experiment's arms when it expires or, with its arm values
// cleared, when no registered game carries it any more (memory stays bounded,
// NFR-051): a game registered again under the same id starts at 1 (an
// experiment slot is read-only to the link).
const firstSlotVersion = 1

// slotRef returns where a slot's version is kept: the outer key groups the
// slots that end together (a game, an experiment), the inner key names the
// slot within.
func slotRef(f *flagState, scopeName string, sc Scope) (outer, inner string) {
	switch scopeName {
	case registry.ScopeGame:
		return "g:" + sc.GameID, f.key
	case registry.ScopeGameSerial:
		return "g:" + sc.GameID, f.key + "\x00" + sc.Serial
	case registry.ScopeExperiment:
		return "x:" + sc.ExperimentID, f.key + "\x00" + sc.Arm
	}
	return "", f.key + "\x00" + scopeName
}

// SlotVersion returns the slot version a pulled write passes as
// IfSlotVersion for the slot (set, flag, scope), empty slots included. It is
// 0 (no slot) when the flag is not a registry flag of set, the scope
// properties fit no scope, or a game or game:serial scope names a game that is
// not registered; any other slot has a version of at least 1.
func (s *State) SlotVersion(set, flag string, scope Scope) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flags[flag]
	if f == nil || f.set != set || f.policy == nil {
		return 0
	}
	scopeName := scope.Name()
	switch scopeName {
	case "":
		return 0
	case registry.ScopeGame, registry.ScopeGameSerial:
		if s.games[scope.GameID] == nil {
			return 0
		}
	}
	return s.slotVersionLocked(f, scopeName, scope)
}

func (s *State) slotVersionLocked(f *flagState, scopeName string, sc Scope) uint64 {
	outer, inner := slotRef(f, scopeName, sc)
	if v, ok := s.slotVer[outer][inner]; ok {
		return v
	}
	return firstSlotVersion
}

// slotChangedLocked bumps the slot's version and, with an observer, queues a
// Change describing the slot as it is now. writer labels a clear or an event
// append; a value reports its own writer.
func (s *State) slotChangedLocked(f *flagState, scopeName string, sc Scope, writer string) {
	outer, inner := slotRef(f, scopeName, sc)
	s.slotSeq++
	m := s.slotVer[outer]
	if m == nil {
		m = map[string]uint64{}
		s.slotVer[outer] = m
	}
	m[inner] = s.slotSeq
	if s.obs == nil {
		return
	}
	c := s.slotChangeLocked(f, scopeName, sc)
	if c.Kind != ChangeValue {
		c.Writer = writer
	}
	c.SlotVersion = s.slotSeq
	s.outbox = append(s.outbox, c)
}

// slotDroppedLocked reports an experiment arm value removed by expiry (no
// write of its own) and forgets the slot's version: the experiment is gone.
func (s *State) slotDroppedLocked(f *flagState, scopeName string, sc Scope) {
	outer, inner := slotRef(f, scopeName, sc)
	if m := s.slotVer[outer]; m != nil {
		delete(m, inner)
		if len(m) == 0 {
			delete(s.slotVer, outer)
		}
	}
	if s.obs == nil {
		return
	}
	c := s.slotChangeLocked(f, scopeName, sc)
	c.SlotVersion = firstSlotVersion
	s.outbox = append(s.outbox, c)
}

// noteLocked queues a lifecycle Change.
func (s *State) noteLocked(c Change) {
	if s.obs == nil {
		return
	}
	c.At = s.opts.Now()
	s.outbox = append(s.outbox, c)
}

// flushLocked hands the queued Changes to the observer with the current
// snapshot version. changed() calls it; a path that changes no flag value
// (a registration, a game end that dropped nothing) calls it directly.
func (s *State) flushLocked() {
	if len(s.outbox) == 0 {
		return
	}
	out := s.outbox
	s.outbox = nil
	if s.obs == nil {
		return
	}
	for i := range out {
		out[i].Version = s.version
		s.obs.StateChanged(out[i])
	}
}

// scopeOf keeps only the scope fields that address the slot.
func scopeOf(scopeName string, sc Scope) Scope {
	switch scopeName {
	case registry.ScopeGame:
		return Scope{GameID: sc.GameID}
	case registry.ScopeGameSerial:
		return Scope{GameID: sc.GameID, Serial: sc.Serial}
	case registry.ScopeExperiment:
		return Scope{ExperimentID: sc.ExperimentID, Arm: sc.Arm}
	case registry.ScopeReal:
		return Scope{GameKind: gameKindReal}
	}
	return Scope{}
}

// slotValue returns the value in force in a slot (stack: the overlay over the
// admin value), or nil.
func (f *flagState) slotValue(scopeName string, sc Scope) *value {
	switch scopeName {
	case registry.ScopeStack:
		if f.stack != nil {
			return f.stack
		}
		return f.admin
	case registry.ScopeReal:
		return f.real
	case registry.ScopeGame:
		return f.games[sc.GameID]
	case registry.ScopeGameSerial:
		return f.serials[sc.GameID][sc.Serial]
	case registry.ScopeExperiment:
		return f.experiments[sc.ExperimentID][sc.Arm]
	}
	return nil
}

// slotChangeLocked describes a slot as it is now. Observer Changes and
// State.Changes share it, so a snapshot and the stream of Changes agree.
func (s *State) slotChangeLocked(f *flagState, scopeName string, sc Scope) Change {
	c := Change{Set: f.set, Flag: f.key, ScopeName: scopeName, Scope: scopeOf(scopeName, sc), At: s.opts.Now()}
	if f.policy != nil && f.policy.Kind == registry.KindEvent && scopeName == registry.ScopeGame {
		c.Kind = ChangeEvents
		events := []event{}
		if list := f.events[sc.GameID]; list != nil && list.Events != nil {
			events = list.Events
		}
		c.Events, _ = json.Marshal(events)
		return c
	}
	v := f.slotValue(scopeName, sc)
	if v == nil {
		c.Kind = ChangeCleared
		return c
	}
	c.Kind = ChangeValue
	c.Value = f.jsonValue(v)
	c.Writer = v.Writer
	c.Persisted = v.Persisted
	if !v.generated() {
		c.DeclaredVariant = v.Variant
	}
	return c
}

// Changes returns the whole runtime state as Changes, built by the same code
// as the observer's, under one lock, with the snapshot version they describe:
// every registered game (ChangeGameRegistered), the stack slot of every
// registry flag (value or cleared), and every other slot that holds a value or
// an event list. The Flagsmith link reconciles from it (FR-210).
func (s *State) Changes() ([]Change, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Change
	now := s.opts.Now()
	for _, id := range sortedKeys(s.games) {
		g := copyGame(s.games[id])
		out = append(out, Change{Kind: ChangeGameRegistered, Scope: Scope{GameID: id}, ScopeName: registry.ScopeGame, Game: g, At: now})
	}
	keys := make([]string, 0, len(s.flags))
	for k, f := range s.flags {
		if f.policy != nil {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	add := func(f *flagState, scopeName string, sc Scope) {
		c := s.slotChangeLocked(f, scopeName, sc)
		c.SlotVersion = s.slotVersionLocked(f, scopeName, sc)
		out = append(out, c)
	}
	for _, k := range keys {
		f := s.flags[k]
		add(f, registry.ScopeStack, Scope{})
		if f.real != nil {
			add(f, registry.ScopeReal, Scope{})
		}
		for _, id := range sortedKeys(f.games) {
			add(f, registry.ScopeGame, Scope{GameID: id})
		}
		for _, id := range sortedKeys(f.events) {
			add(f, registry.ScopeGame, Scope{GameID: id})
		}
		for _, id := range sortedKeys(f.serials) {
			for _, serial := range sortedKeys(f.serials[id]) {
				add(f, registry.ScopeGameSerial, Scope{GameID: id, Serial: serial})
			}
		}
		for _, id := range sortedKeys(f.experiments) {
			for _, arm := range sortedKeys(f.experiments[id]) {
				add(f, registry.ScopeExperiment, Scope{ExperimentID: id, Arm: arm})
			}
		}
	}
	for i := range out {
		out[i].Version = s.version
	}
	return out, s.version
}

func copyGame(g *Game) *Game {
	if g == nil {
		return nil
	}
	cp := *g
	cp.Serials = append([]string(nil), g.Serials...)
	return &cp
}

// gameChanged reports whether a re-registration changes what the link mirrors
// (kind, serials, experiment arm); a pure LastSeenAt refresh does not.
func gameChanged(old *Game, g *Game) bool {
	if old == nil {
		return true
	}
	if old.Kind != g.Kind || old.ExperimentID != g.ExperimentID || old.Arm != g.Arm || len(old.Serials) != len(g.Serials) {
		return true
	}
	for i := range old.Serials {
		if old.Serials[i] != g.Serials[i] {
			return true
		}
	}
	return false
}
