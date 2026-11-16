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
	// notFoundTries counts the 404s retried for this entry: a push to an
	// identity the link listed or settled moments before, which Flagsmith's
	// edge may not show yet (#2639).
	notFoundTries int
	// sent is the value Next last handed out for this entry (hasSent): the
	// value a Refuse without one refers to.
	sent    Value
	hasSent bool
	// offline: the latest change was made while the link was not connected
	// (UC-054 A2, #2621). Such an entry is never sent: the in side discards
	// it in favour of Flagsmith. Only stack-scope entries are marked.
	offline bool
}

type entry struct {
	remote    Value
	hasRemote bool
	pending   *pending
	inSync    bool
	refused   string
	side      Side
	// pushRefused is the local value Flagsmith last refused (hasPushRefused):
	// it is not offered again until a different local value, a confirmed push,
	// ClearRefusal or the key's deletion clears it (UC-055 A4, #2604).
	pushRefused    Value
	hasPushRefused bool
	// gen counts changes of the remote value (SetRemote, ConfirmPushed,
	// ForgetRemote, RestoreRemote): a pull skips a key whose gen moved while
	// the document was in flight (a stale document vs a confirmed push).
	gen         uint64
	confirmedAt time.Time
	// outSince: when the key last went out of sync (GET /state keys, #2639).
	outSince time.Time
	// refusedAt and refusedStatus: when the last refusal was recorded and the
	// HTTP status Flagsmith answered it with (0: refused without a request).
	refusedAt     time.Time
	refusedStatus int
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
		e = &entry{outSince: s.now()}
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

// HasPending reports whether k has a local value waiting to be sent.
func (s *LinkState) HasPending(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	return e != nil && e.pending != nil
}

// PendingOffline reports whether k's pending local value was made while the
// link was not connected (UC-054 A2, #2621): the in side discards it.
func (s *LinkState) PendingOffline(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	return e != nil && e.pending != nil && e.pending.offline
}

// DropOffline discards k's pending value made offline, only while it is still
// the change of snapshot version (a local write made since is kept).
func (s *LinkState) DropOffline(k Key, version uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil && e.pending != nil && e.pending.offline && e.pending.version == version {
		e.pending = nil
		s.dropFromQueueLocked(k)
	}
}

// HoldRefused keeps v as k's refused local value (UC-055 A4) after a pull
// round reverted it to Flagsmith's value: a re-save of v is not sent again.
func (s *LinkState) HoldRefused(k Key, v Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		e.pushRefused, e.hasPushRefused = v, true
	}
}

// PushRefused returns the local value Flagsmith last refused for k.
func (s *LinkState) PushRefused(k Key) (Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil && e.hasPushRefused {
		return e.pushRefused, true
	}
	return Value{}, false
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
// snapshot version of the change (the push span's link). A value equal to the
// one Flagsmith last refused is not queued (a re-save does not retry, UC-055
// A4); a different value clears that refusal. It runs under State.mu: map work
// only.
func (s *LinkState) Offer(k Key, v Value, at time.Time, version uint64) (queued bool) {
	return s.OfferFrom(k, v, at, version, false)
}

// OfferFrom is Offer that records whether the change was made while the link
// was not connected (offline: marks a stack-scope entry, UC-054 A2, #2621).
func (s *LinkState) OfferFrom(k Key, v Value, at time.Time, version uint64, offline bool) (queued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(k)
	if e.hasRemote && e.remote.Equal(v) {
		s.inSyncLocked(k, e)
		return false
	}
	if e.hasPushRefused {
		if e.pushRefused.Equal(v) {
			// The local value is the refused one again: an older pending
			// value is no longer the target state.
			if e.pending != nil {
				e.pending = nil
				s.dropFromQueueLocked(k)
			}
			s.syncLocked(e, false)
			return false
		}
		e.pushRefused, e.hasPushRefused = Value{}, false
	}
	return s.offerLocked(k, e, v, at, version, offline)
}

// Reoffer is Offer for a pull round (the in side), which re-reads a local value
// that may be old: it never queues the value Flagsmith last refused, whatever
// wrote it, and never touches an existing pending entry (the observer offered
// a newer value already, or the value is in flight). It reports whether it
// queued v.
func (s *LinkState) Reoffer(k Key, v Value, at time.Time, version uint64) (queued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(k)
	switch {
	case e.hasRemote && e.remote.Equal(v):
		s.inSyncLocked(k, e)
		return false
	case e.hasPushRefused && e.pushRefused.Equal(v):
		s.syncLocked(e, false)
		return false
	case e.pending != nil:
		return false
	}
	return s.offerLocked(k, e, v, at, version, false)
}

// syncLocked sets e's in-sync flag and records when it went out of sync.
func (s *LinkState) syncLocked(e *entry, inSync bool) {
	if !inSync && (e.inSync || e.outSince.IsZero()) {
		e.outSince = s.now()
	}
	e.inSync = inSync
}

// inSyncLocked records a local value equal to the remote one: nothing to
// send, no refused value to hold back.
func (s *LinkState) inSyncLocked(k Key, e *entry) {
	if e.pending != nil {
		e.pending = nil
		s.dropFromQueueLocked(k)
	}
	e.pushRefused, e.hasPushRefused = Value{}, false
	s.syncLocked(e, true)
}

func (s *LinkState) offerLocked(k Key, e *entry, v Value, at time.Time, version uint64, offline bool) bool {
	s.syncLocked(e, false)
	if e.pending != nil {
		e.pending.value = v
		e.pending.merged++
		e.pending.version, e.pending.offline = version, offline
		return true
	}
	e.pending = &pending{value: v, at: at, merged: 1, version: version, offline: offline}
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
		if p.notBefore.After(now) || p.offline {
			// An offline-made change waits for the in side (UC-054 A2).
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
// meanwhile. It ends the refusal and the refused value, unless the local value
// went back to that refused value while v was in flight (heldLocalLocked): the
// key then stays refused and out of sync. A key forgotten meanwhile (game end)
// is ignored.
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
	if heldLocalLocked(e, v) {
		s.syncLocked(e, false)
		return
	}
	e.refused, e.side = "", ""
	e.pushRefused, e.hasPushRefused = Value{}, false
	if e.pending != nil && e.pending.value.Equal(v) {
		e.pending = nil
		s.dropFromQueueLocked(k)
	}
	s.syncLocked(e, e.pending == nil)
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
	s.syncLocked(e, e.pending == nil)
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
// Next last handed out for k (the push in flight) and drops the pending entry
// only while it still holds that value: a newer local value offered during the
// push stays pending and is pushed next (as with ConfirmPushed). When Next
// never handed the current entry out (it was created after the refused push,
// e.g. by an Offer following one equal to the remote value), the refusal
// cannot be about it: the refusal is recorded and the entry kept. See
// RefuseValue.
func (s *LinkState) Refuse(k Key, side Side, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil {
		return
	}
	if p := e.pending; p != nil && !p.hasSent {
		e.refused, e.side = reason, side
		e.refusedAt, e.refusedStatus = s.now(), 0
		s.syncLocked(e, false)
		return
	}
	v := Value{}
	if p := e.pending; p != nil {
		v = p.sent
	}
	s.refuseLocked(k, e, v, side, reason, 0)
}

// RefuseValue records a refusal of k on side; a remote refusal of v drops the
// pending entry only while it still holds v (a newer local value stays
// pending) and keeps v as the refused value: neither the sender nor a pull
// round sends it again until a different local value (UC-055 A4).
func (s *LinkState) RefuseValue(k Key, v Value, side Side, reason string) {
	s.RefuseValueStatus(k, v, side, reason, 0)
}

// RefuseValueStatus is RefuseValue that also records the HTTP status of
// Flagsmith's answer (0: the value was refused without a request), shown in
// GET /state (#2639).
func (s *LinkState) RefuseValueStatus(k Key, v Value, side Side, reason string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		held := side == SideRemote && heldLocalLocked(e, v)
		s.refuseLocked(k, e, v, side, reason, status)
		if side == SideRemote && !held {
			e.pushRefused, e.hasPushRefused = v, true
		}
	}
}

// heldLocalLocked reports whether e's local value is its refused value and
// differs from v, the value a push outcome refers to. With no pending entry a
// held refused value is the local value: a pending entry goes only by a local
// value equal to the remote one (which ends the refused value too), by an
// Offer of the refused value, or by the outcome of a push of its own value
// (which makes that value the held one or ends it). So a confirmed or refused
// v that differs from the held value was in flight when the operator went back
// to the refused value: its outcome must neither end nor replace the held
// value (UC-055 A4).
func heldLocalLocked(e *entry, v Value) bool {
	return e.pending == nil && e.hasPushRefused && !e.pushRefused.Equal(v)
}

func (s *LinkState) refuseLocked(k Key, e *entry, v Value, side Side, reason string, status int) {
	e.refused, e.side = reason, side
	e.refusedAt, e.refusedStatus = s.now(), status
	s.syncLocked(e, false)
	if side == SideRemote && e.pending != nil && e.pending.value.Equal(v) {
		e.pending = nil
		s.dropFromQueueLocked(k)
	}
}

// ClearRefusal forgets k's last refusal and the refused local value.
func (s *LinkState) ClearRefusal(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil {
		e.refused, e.side = "", ""
		e.pushRefused, e.hasPushRefused = Value{}, false
	}
}

// ClearLocalRefusal forgets k's last refusal only when the flag service made
// it (SideLocal), under one lock: a push refusal recorded meanwhile stays, and
// so does the refused local value.
func (s *LinkState) ClearLocalRefusal(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[k]; e != nil && e.side == SideLocal {
		e.refused, e.side = "", ""
	}
}

// SetInSync sets k's in-sync flag.
func (s *LinkState) SetInSync(k Key, inSync bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncLocked(s.entryLocked(k), inSync)
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
		if e.refused != "" || e.hasPushRefused {
			refused++
		}
	}
	return pendingN, outOfSync, refused
}

// RetryNotFound counts one more 404 retry of k's pending entry (#2639) and
// returns its number (1 for the first). It reports false when k has no
// pending entry or the entry used its max retries: the caller refuses the
// value then.
func (s *LinkState) RetryNotFound(k Key, max int) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[k]
	if e == nil || e.pending == nil || e.pending.notFoundTries >= max {
		return 0, false
	}
	e.pending.notFoundTries++
	return e.pending.notFoundTries, true
}

// KeyState is one key of the flagsmith keys list of GET /state (services.md
// §7.3, #2639): a key out of sync or refused, never its value.
type KeyState struct {
	FlagSet string `json:"flag_set"`
	Flag    string `json:"flag"`
	// Scope is "stack" or the key's identity (game:<id>, game:<id>:<serial>
	// also for a real game's player value sent to controller:<serial>,
	// experiment:<id>:<arm>, real, fraction).
	Scope   string `json:"scope"`
	InSync  bool   `json:"in_sync"`
	Pending bool   `json:"pending"`
	Reason  string `json:"reason,omitempty"`
	Side    Side   `json:"side,omitempty"`
	// HTTPStatus is Flagsmith's answer to the refused push (absent: refused
	// without a request, e.g. a feature the start did not resolve).
	HTTPStatus int `json:"http_status,omitempty"`
	// Since is when the refusal was recorded, else when the key went out of
	// sync.
	Since time.Time `json:"since"`
}

// MaxStateKeys bounds the keys list of GET /state.
const MaxStateKeys = 50

// Unsynced lists the keys out of sync or refused, refused first, then the
// oldest first, at most limit of them; total is how many there are.
func (s *LinkState) Unsynced(limit int) (list []KeyState, total int) {
	s.mu.Lock()
	for k, e := range s.entries {
		refused := e.refused != "" || e.hasPushRefused
		if e.inSync && !refused {
			continue
		}
		ks := KeyState{FlagSet: k.Set, Flag: k.Flag, Scope: k.Scope, InSync: e.inSync, Pending: e.pending != nil, Since: e.outSince.UTC()}
		if e.refused != "" {
			ks.Reason, ks.Side, ks.HTTPStatus = e.refused, e.side, e.refusedStatus
			if !e.refusedAt.IsZero() {
				ks.Since = e.refusedAt.UTC()
			}
		}
		list = append(list, ks)
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if (a.Reason != "") != (b.Reason != "") {
			return a.Reason != ""
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		if a.FlagSet != b.FlagSet {
			return a.FlagSet < b.FlagSet
		}
		if a.Flag != b.Flag {
			return a.Flag < b.Flag
		}
		return a.Scope < b.Scope
	})
	total = len(list)
	if limit >= 0 && len(list) > limit {
		list = list[:limit]
	}
	return list, total
}
