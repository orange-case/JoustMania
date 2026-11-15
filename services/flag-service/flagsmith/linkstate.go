package flagsmith

import (
	"sort"
	"sync"
	"time"
)

// Side names where a refusal happened.
type Side string

// The sides of the link.
const (
	SideLocal  Side = "local"  // the flag service refused a pulled change
	SideRemote Side = "remote" // Flagsmith refused a push
)

// pending is a local value waiting to be sent (the outbox entry).
type pending struct {
	value     Value
	at        time.Time // first change since the last send
	merged    int       // local changes merged into this entry
	version   uint64    // snapshot version of the latest change (push span link)
	notBefore time.Time // retry backoff after a failed push
	// sent is the value Next last handed out for this entry (hasSent): the
	// value a Refuse without one refers to.
	sent    Value
	hasSent bool
}

type entry struct {
	remote    Value
	hasRemote bool
	pending   *pending
	inSync    bool
	refused   string
	side      Side
	// gen counts changes of the remote value (SetRemote, ConfirmPushed,
	// ForgetRemote, RestoreRemote): a pull skips a key whose gen moved while
	// the document was in flight (a stale document vs a confirmed push).
	gen         uint64
	confirmedAt time.Time
}

// LinkState is the link's in-memory state per Key (architecture note §3,
// services.md §7.3 State): the last value seen in Flagsmith, the pending local
// value, in-sync and the last refusal. Nothing of it is persisted (F5).
//
// Lock order: State.mu -> LinkState.mu, never the reverse. LinkState never
// calls out while holding its lock, so the observer (under State.mu) may call
// Offer; callers must never call State or the in-process writer while
// holding anything that Offer waits for.
type LinkState struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[Key]*entry
	queue   []Key                       // pending keys in first-change order
	byGame  map[string]map[Key]struct{} // keys of game:<id> and game:<id>:<serial>
}

// NewLinkState returns an empty LinkState; now (nil: time.Now) times the retry
// backoff.
func NewLinkState(now func() time.Time) *LinkState {
	if now == nil {
		now = time.Now
	}
	return &LinkState{now: now, entries: map[Key]*entry{}, byGame: map[string]map[Key]struct{}{}}
}

func (s *LinkState) entryLocked(k Key) *entry {
	e := s.entries[k]
	if e == nil {
		e = &entry{}
		s.entries[k] = e
		if g := GameOfIdentity(k.Scope); g != "" {
			m := s.byGame[g]
			if m == nil {
				m = map[Key]struct{}{}
				s.byGame[g] = m
			}
			m[k] = struct{}{}
		}
	}
	return e
}

func (s *LinkState) dropFromQueueLocked(k Key) {
	for i, q := range s.queue {
		if q == k {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

func (s *LinkState) deleteLocked(k Key) {
	if e := s.entries[k]; e != nil && e.pending != nil {
		s.dropFromQueueLocked(k)
	}
	delete(s.entries, k)
	if g := GameOfIdentity(k.Scope); g != "" {
		if m := s.byGame[g]; m != nil {
			delete(m, k)
			if len(m) == 0 {
				delete(s.byGame, g)
			}
		}
	}
}

// Offer records a local value. A value equal to the last remote one drops any
// pending entry for k and marks it in sync (nothing to send). Otherwise it
// becomes the pending entry: a newer value replaces an older one in place
// (merged counts them, the first-change order is kept). version is the
// snapshot version of the change (the push span's link). It runs under
// State.mu: map work only.
func (s *LinkState) Offer(k Key, v Value, at time.Time, version uint64) (queued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(k)
	if e.hasRemote && e.remote.Equal(v) {
		if e.pending != nil {
			e.pending = nil
			s.dropFromQueueLocked(k)
		}
		e.inSync = true
		return false
	}
	e.inSync = false
	if e.pending != nil {
		e.pending.value = v
		e.pending.merged++
		e.pending.version = version
		return true
	}
	e.pending = &pending{value: v, at: at, merged: 1, version: version}
	s.queue = append(s.queue, k)
	return true
}

// Next peeks at the oldest pending entry whose retry time has come; it does
// not remove it (ConfirmPushed, Refuse or Retry do). It records the value it
// hands out: Refuse refers to it.
func (s *LinkState) Next() (k Key, v Value, merged int, version uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, q := range s.queue {
		p := s.entries[q].pending
		if p.notBefore.After(now) {
			continue
		}
		p.sent, p.hasSent = p.value, true
		return q, p.value, p.merged, p.version, true
	}
	return Key{}, Value{}, 0, 0, false
}

// Retry keeps k pending, moves it to the back and holds it until notBefore
// (a failed push: no hot loop).
func (s *LinkState) Retry(k Key, notBefore time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil || e.pending == nil {
		return
	}
	e.pending.notBefore = notBefore
	s.dropFromQueueLocked(k)
	s.queue = append(s.queue, k)
}

// ConfirmPushed records that Flagsmith accepted v for k: v is the remote value
// now, and the pending entry goes unless a newer local value replaced it
// meanwhile. A key forgotten meanwhile (game end) is ignored.
func (s *LinkState) ConfirmPushed(k Key, v Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil {
		return
	}
	e.remote, e.hasRemote = v, true
	e.gen++
	e.confirmedAt = s.now()
	e.refused, e.side = "", ""
	if e.pending != nil && e.pending.value.Equal(v) {
		e.pending = nil
		s.dropFromQueueLocked(k)
	}
	e.inSync = e.pending == nil
}

// SetRemote records the value last seen in Flagsmith and returns the previous
// one (had: one was known). The key is in sync when nothing is pending (the
// pull applies the value; a refusal of that write sets it out of sync again).
func (s *LinkState) SetRemote(k Key, v Value) (prev Value, had bool) {
	prev, had, _ = s.SetRemoteGen(k, v)
	return prev, had
}

// SetRemoteGen is SetRemote that also returns the generation it produced,
// for an exact RestoreRemoteGen.
func (s *LinkState) SetRemoteGen(k Key, v Value) (prev Value, had bool, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(k)
	prev, had = e.remote, e.hasRemote
	e.remote, e.hasRemote = v, true
	e.gen++
	e.inSync = e.pending == nil
	return prev, had, e.gen
}

// RestoreRemoteGen puts prev back (or forgets the remote value when !had),
// only while k's generation is still gen (from SetRemoteGen): the exact undo
// of a SetRemote whose write was refused, which never undoes a later
// SetRemote, confirm or forget, even one that recorded the same value.
func (s *LinkState) RestoreRemoteGen(k Key, gen uint64, prev Value, had bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil || e.gen != gen {
		return false
	}
	s.restoreLocked(e, prev, had)
	return true
}

// RestoreRemote puts prev back (or forgets the remote value when !had), only
// while the remote value still equals expect. It compares values, not
// generations: a later SetRemote of the same value is undone too (the next
// pull heals it). Prefer SetRemoteGen + RestoreRemoteGen.
func (s *LinkState) RestoreRemote(k Key, expect Value, prev Value, had bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil || !e.hasRemote || !e.remote.Equal(expect) {
		return false
	}
	s.restoreLocked(e, prev, had)
	return true
}

func (s *LinkState) restoreLocked(e *entry, prev Value, had bool) {
	if had {
		e.remote = prev
	} else {
		e.remote, e.hasRemote = Value{}, false
	}
	e.gen++
}

// Gen returns k's remote generation (0: never seen).
func (s *LinkState) Gen(k Key) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		return e.gen
	}
	return 0
}

// Gens returns a snapshot of every key's remote generation.
func (s *LinkState) Gens() map[Key]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Key]uint64, len(s.entries))
	for k, e := range s.entries {
		out[k] = e.gen
	}
	return out
}

// ConfirmedAt returns when Flagsmith last confirmed a push of k (zero: never).
func (s *LinkState) ConfirmedAt(k Key) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		return e.confirmedAt
	}
	return time.Time{}
}

// ForgetRemote restores "never seen in Flagsmith" for k.
func (s *LinkState) ForgetRemote(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		e.remote, e.hasRemote = Value{}, false
		e.gen++
	}
}

// Remote returns the value last seen in Flagsmith.
func (s *LinkState) Remote(k Key) (Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil || !e.hasRemote {
		return Value{}, false
	}
	return e.remote, true
}

// Refuse records a refusal of k on side. A remote refusal refers to the value
// Next last handed out for k (the push in flight; the pending value when Next
// never handed one out) and drops the pending entry only while it still holds
// that value: a newer local value offered during the push stays pending and
// is pushed next (as with ConfirmPushed). See RefuseValue.
func (s *LinkState) Refuse(k Key, side Side, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil {
		return
	}
	v := Value{}
	if p := e.pending; p != nil {
		v = p.value
		if p.hasSent {
			v = p.sent
		}
	}
	s.refuseLocked(k, e, v, side, reason)
}

// RefuseValue records a refusal of k on side; a remote refusal of v drops the
// pending entry only while it still holds v (a newer local value stays
// pending).
func (s *LinkState) RefuseValue(k Key, v Value, side Side, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		s.refuseLocked(k, e, v, side, reason)
	}
}

func (s *LinkState) refuseLocked(k Key, e *entry, v Value, side Side, reason string) {
	e.refused, e.side = reason, side
	e.inSync = false
	if side == SideRemote && e.pending != nil && e.pending.value.Equal(v) {
		e.pending = nil
		s.dropFromQueueLocked(k)
	}
}

// ClearRefusal forgets k's last refusal.
func (s *LinkState) ClearRefusal(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		e.refused, e.side = "", ""
	}
}

// SetInSync sets k's in-sync flag.
func (s *LinkState) SetInSync(k Key, inSync bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entryLocked(k).inSync = inSync
}

// Refusal returns k's last refusal and its side ("" when none).
func (s *LinkState) Refusal(k Key) (string, Side) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		return e.refused, e.side
	}
	return "", ""
}

// ForgetGame drops every key of game gameID (game:<id> and
// game:<id>:<serial>), pending entries included, in O(keys of the game).
func (s *LinkState) ForgetGame(gameID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.byGame[gameID] {
		s.deleteLocked(k)
	}
	delete(s.byGame, gameID)
}

// ForgetIdentity drops every key on one identity (a dropped serial, an ended
// experiment arm).
func (s *LinkState) ForgetIdentity(identifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := GameOfIdentity(identifier); g != "" {
		for k := range s.byGame[g] {
			if k.Scope == identifier {
				s.deleteLocked(k)
			}
		}
		return
	}
	for k := range s.entries {
		if k.Scope == identifier {
			s.deleteLocked(k)
		}
	}
}

// Keys returns every known key, sorted.
func (s *LinkState) Keys() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, 0, len(s.entries))
	for k := range s.entries {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Set != b.Set {
			return a.Set < b.Set
		}
		if a.Flag != b.Flag {
			return a.Flag < b.Flag
		}
		return a.Scope < b.Scope
	})
	return out
}

// Counts returns the pending entries, the keys not in sync and the keys with
// a refusal (GET /state, the gauges).
func (s *LinkState) Counts() (pendingN, outOfSync, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.pending != nil {
			pendingN++
		}
		if !e.inSync {
			outOfSync++
		}
		if e.refused != "" {
			refused++
		}
	}
	return pendingN, outOfSync, refused
}
