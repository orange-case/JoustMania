package flagsmith

import (
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// Controller identities (#2624, architecture note §2.2). Each Move controller
// is one Flagsmith identity, controller:<serial>, that outlives games. The
// player values of a real game live on it as overrides; a shadow game keeps
// its per-game identities game:<id>:<serial>, so a shadow player never touches
// a real controller.
//
// The link's own keys do not change: a player value is still the key
// game:<id>:<serial> (LinkState, the refused-value memory of #2604, the value
// guards), and only the identity a key is sent to and
// read from is the controller's. So a value refused or guarded in one game is
// never held back in the next.
//
// The rule that keeps the controller's overrides honest: the overrides on
// controller:<serial> always belong to the controller's owner, the one live
// real game it plays in (the latest registered when two hold it). The link
// settles the identity for a new owner before any of the owner's values is
// sent or read: it removes every override of a registry feature that is on
// it (a former game's, or one a person set while no game was live), then
// sets the traits game_id, game_kind and coordinator_instance. When the
// controller leaves its game (game end, the serial leaves, the expiry sweep)
// the link releases it: the same removal, and the traits are cleared. The
// identity itself is never deleted.
//
// One exception keeps a restart cheap and honest: the first settle of a
// controller in this process reads its game_id trait first, and when it names
// the owner (the flag service restarted while the game was live) nothing is
// removed. A controller whose traits cannot be read is cleared.
//
// All controller requests run on the out side's one sender goroutine, before
// any value push (out.step), so a push and a removal of the same controller
// never overlap, and a value of the new owner is sent only after the old
// overrides are gone.

// ControllerPrefix starts the identifier of a controller identity.
const ControllerPrefix = "controller:"

// gameKindShadow is the one game kind whose players keep per-game identities.
// Any other kind, empty or unknown included, is a real game (fail closed).
const gameKindShadow = "shadow"

// Controller traits (architecture note §2.2).
const (
	TraitGameID              = "game_id"
	TraitGameKind            = "game_kind"
	TraitCoordinatorInstance = "coordinator_instance"
)

// ControllerIdentity is the identifier of serial's controller identity.
func ControllerIdentity(serial string) string { return ControllerPrefix + serial }

// SerialOfController returns the serial of a controller:<serial> identifier.
func SerialOfController(identifier string) (string, bool) {
	s, ok := strings.CutPrefix(identifier, ControllerPrefix)
	return s, ok && s != ""
}

// isShadowKind reports whether a game kind is the shadow kind.
func isShadowKind(kind string) bool { return kind == gameKindShadow }

// ctrlGame is one live game as the controller map knows it.
type ctrlGame struct {
	id, kind, instance string
	registeredAt       time.Time
	serials            []string
}

func (g ctrlGame) shadow() bool { return isShadowKind(g.kind) }

// ctrlEntry is one controller.
type ctrlEntry struct {
	// owner is the live real game the controller plays in ("" none).
	owner string
	// settled is the game the identity was last settled for ("" none, or
	// released): its overrides belong to that game.
	settled string
	// readable: the settle removed everything it had to; the pull may read
	// the identity for settled. A refused removal leaves it unreadable for
	// that game (the values are still sent).
	readable bool
	// verified: the identity was settled or released once in this process;
	// a later settle always removes (no trait check).
	verified bool
	// release: remove the overrides and the traits although no game was
	// settled in this process (start cleanup).
	release bool
	// gen moves on every owner change and every finished step: an op that
	// finished for an older gen records nothing, and a pull round skips the
	// controller when it moved while the document was in flight.
	gen uint64
	// removed holds, per flag key, the override values the last removals
	// took off the identity (pull guard against a lagging document).
	removed map[string][]removedValue
}

// removedValue is one override a removal took off a controller identity.
type removedValue struct {
	fs FeatureState
	at time.Time
}

// ctrlOpKind is the kind of controller work.
type ctrlOpKind int

const (
	ctrlSettle ctrlOpKind = iota
	ctrlRelease
)

// ctrlOp is the next piece of controller work.
type ctrlOp struct {
	serial string
	kind   ctrlOpKind
	game   ctrlGame // settle: the owner
	verify bool     // settle: the first in this process, read game_id first
	gen    uint64
}

// controllers is the controller map of one link: the live games, each
// controller's owner and how far its identity is settled. Changed (under
// State.mu) updates it, the out side's Run works it off, and the pull reads a
// view of it. Lock order: State.mu -> controllers.mu; it never calls out.
type controllers struct {
	mu      sync.Mutex
	games   map[string]ctrlGame
	entries map[string]*ctrlEntry
	gen     uint64
}

func newControllers() *controllers {
	return &controllers{games: map[string]ctrlGame{}, entries: map[string]*ctrlEntry{}}
}

func (c *controllers) entryLocked(serial string) *ctrlEntry {
	e := c.entries[serial]
	if e == nil {
		e = &ctrlEntry{}
		c.entries[serial] = e
	}
	return e
}

func (c *controllers) bumpLocked(e *ctrlEntry) {
	c.gen++
	e.gen = c.gen
}

// ownerLocked is the owner of serial: among the live real games that hold
// it, the one registered last (then the larger game id).
func (c *controllers) ownerLocked(serial string) string {
	var best *ctrlGame
	for id := range c.games {
		g := c.games[id]
		if g.shadow() || !slices.Contains(g.serials, serial) {
			continue
		}
		if best == nil || g.registeredAt.After(best.registeredAt) ||
			(g.registeredAt.Equal(best.registeredAt) && g.id > best.id) {
			best = &g
		}
	}
	if best == nil {
		return ""
	}
	return best.id
}

// recomputeLocked sets the owner of each serial and returns the games that
// lost a serial to another owner or to none (serial -> former owner).
func (c *controllers) recomputeLocked(serials []string) map[string]string {
	lost := map[string]string{}
	for _, s := range serials {
		owner := c.ownerLocked(s)
		e := c.entries[s]
		if e == nil && owner == "" {
			continue
		}
		e = c.entryLocked(s)
		if e.owner == owner {
			continue
		}
		if e.owner != "" {
			lost[s] = e.owner
		}
		e.owner = owner
		c.bumpLocked(e)
	}
	return lost
}

// register records a registration (first or repeated) and returns the
// serials whose former owner lost them (serial -> that game).
func (c *controllers) register(g *state.Game) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, had := c.games[g.ID]
	at := g.RegisteredAt
	if had && !prev.registeredAt.IsZero() {
		at = prev.registeredAt
	}
	c.games[g.ID] = ctrlGame{id: g.ID, kind: g.Kind, instance: g.CoordinatorInstance, registeredAt: at, serials: slices.Clone(g.Serials)}
	affected := slices.Clone(g.Serials)
	if had {
		affected = append(affected, prev.serials...)
	}
	return c.recomputeLocked(affected)
}

// end removes a game and returns it (ok false when unknown).
func (c *controllers) end(id string) (ctrlGame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.games[id]
	if !ok {
		return g, false
	}
	delete(c.games, id)
	c.recomputeLocked(g.serials)
	return g, true
}

// sendDecision is what pushNext does with a pending key.
type sendDecision int

const (
	sendNow  sendDecision = iota // send it to the identity
	sendDrop                     // drop the key: its game ended or does not own the controller
	sendWait                     // keep it pending: its controller's settle is still due
)

// identityFor is the identity a key's value is sent to. A player key of a
// real game goes to its controller while that game owns it, and only once
// the controller is settled for it (sendWait until then: no value of the
// owner reaches the controller before the former overrides are gone,
// UC-055 BR-004); a player key of a game that is not live or does not own
// the controller is dropped. Every other key keeps its own identity.
func (c *controllers) identityFor(k Key) (string, sendDecision) {
	sc, name, ok := ScopeOfIdentity(k.Scope)
	if !ok || name != registry.ScopeGameSerial {
		return IdentityOf(k), sendNow
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, live := c.games[sc.GameID]
	switch {
	case !live:
		return "", sendDrop
	case g.shadow():
		return k.Scope, sendNow
	}
	e := c.entries[sc.Serial]
	switch {
	case e == nil || e.owner != g.id:
		return "", sendDrop
	case e.settled != e.owner:
		return "", sendWait
	}
	return ControllerIdentity(sc.Serial), sendNow
}

// markRelease asks for the release of a controller without an owner (the
// start cleanup found its identity). It reports whether work was queued.
func (c *controllers) markRelease(serial string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entryLocked(serial)
	if e.owner != "" || e.release {
		return false
	}
	e.release = true
	c.bumpLocked(e)
	return true
}

// workLocked is the op e needs, if any.
func (c *controllers) workLocked(serial string, e *ctrlEntry) (ctrlOp, bool) {
	switch {
	case e.owner != "" && e.settled != e.owner:
		return ctrlOp{serial: serial, kind: ctrlSettle, game: c.games[e.owner], verify: !e.verified, gen: e.gen}, true
	case e.owner == "" && (e.settled != "" || e.release):
		return ctrlOp{serial: serial, kind: ctrlRelease, gen: e.gen}, true
	}
	return ctrlOp{}, false
}

// next returns the next controller op (serial order), without removing it.
func (c *controllers) next() (ctrlOp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	serials := make([]string, 0, len(c.entries))
	for s := range c.entries {
		serials = append(serials, s)
	}
	sort.Strings(serials)
	for _, s := range serials {
		if op, ok := c.workLocked(s, c.entries[s]); ok {
			return op, true
		}
	}
	return ctrlOp{}, false
}

// busy reports whether any controller has work left.
func (c *controllers) busy() bool {
	_, ok := c.next()
	return ok
}

// current reports whether op is still the controller's work.
func (c *controllers) current(op ctrlOp) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[op.serial]
	return e != nil && e.gen == op.gen
}

// done records a finished op: settled for its game (readable when nothing was
// refused) or released. removed are the overrides it took off (they guard the
// pull either way). An op whose controller moved meanwhile records only them.
func (c *controllers) done(op ctrlOp, readable bool, removed []FeatureState, flagOf func(int) string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[op.serial]
	if e == nil {
		return
	}
	noteRemovedLocked(e, removed, flagOf, now)
	if e.gen != op.gen {
		return
	}
	switch op.kind {
	case ctrlSettle:
		e.settled, e.readable = op.game.id, readable
	case ctrlRelease:
		e.settled, e.readable, e.release = "", false, false
	}
	e.verified = true
	c.bumpLocked(e)
}

// noteRemoved records overrides an op took off before it had to start over.
func (c *controllers) noteRemoved(serial string, removed []FeatureState, flagOf func(int) string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[serial]; e != nil {
		noteRemovedLocked(e, removed, flagOf, now)
	}
}

func noteRemovedLocked(e *ctrlEntry, removed []FeatureState, flagOf func(int) string, now time.Time) {
	for _, fs := range removed {
		flag := flagOf(fs.Feature.ID)
		if flag == "" {
			continue
		}
		if e.removed == nil {
			e.removed = map[string][]removedValue{}
		}
		e.removed[flag] = append(e.removed[flag], removedValue{fs: fs, at: now})
	}
}

// ctrlView is what a pull round knows of one controller.
type ctrlView struct {
	owner    string
	readable bool // settled for owner, nothing refused, no work left
	gen      uint64
}

// view is a pull round's snapshot: each controller, and each live game with
// its kind.
func (c *controllers) view() (map[string]ctrlView, map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]ctrlView, len(c.entries))
	for s, e := range c.entries {
		_, work := c.workLocked(s, e)
		out[s] = ctrlView{owner: e.owner, readable: e.owner != "" && e.settled == e.owner && e.readable && !work, gen: e.gen}
	}
	kinds := make(map[string]string, len(c.games))
	for id, g := range c.games {
		kinds[id] = g.kind
	}
	return out, kinds
}

// genOf is a controller's gen now.
func (c *controllers) genOf(serial string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[serial]; e != nil {
		return e.gen
	}
	return 0
}

// removedValues returns the guard values of a controller's flag that are
// younger than ttl (older ones are dropped).
func (c *controllers) removedValues(serial, flag string, now time.Time, ttl time.Duration) []FeatureState {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[serial]
	if e == nil || len(e.removed[flag]) == 0 {
		return nil
	}
	var keep []removedValue
	var out []FeatureState
	for _, r := range e.removed[flag] {
		if now.Sub(r.at) > ttl {
			continue
		}
		keep = append(keep, r)
		out = append(out, r.fs)
	}
	if len(keep) == 0 {
		delete(e.removed, flag)
	} else {
		e.removed[flag] = keep
	}
	return out
}

// dropRemoved ends the guard of a controller's flag: the document no longer
// shows a removed value.
func (c *controllers) dropRemoved(serial, flag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[serial]; e != nil {
		delete(e.removed, flag)
	}
}

// scopeOfIdentity maps a document identity to the key scope a pull round
// reads it as: a readable controller as its owner's player key, a
// game:<id>:<serial> identity only for a live shadow game, every other
// identity as itself. ok is false for an identity the round ignores.
func (rc *roundCtx) scopeOfIdentity(identifier string) (string, bool) {
	if serial, isCtrl := SerialOfController(identifier); isCtrl {
		v, known := rc.ctrl[serial]
		if !known || !v.readable {
			return "", false
		}
		return "game:" + v.owner + ":" + serial, true
	}
	sc, name, ok := ScopeOfIdentity(identifier)
	if !ok {
		return "", false
	}
	if name == registry.ScopeGameSerial {
		if kind, live := rc.kinds[sc.GameID]; !live || !isShadowKind(kind) {
			return "", false
		}
	}
	return identifier, true
}

// controllerOf returns the serial of a player key that lives on a
// controller identity (any game that is not a live shadow game).
func (rc *roundCtx) controllerOf(k Key) (string, bool) {
	sc, name, ok := ScopeOfIdentity(k.Scope)
	if !ok || name != registry.ScopeGameSerial {
		return "", false
	}
	if kind, live := rc.kinds[sc.GameID]; live && isShadowKind(kind) {
		return "", false
	}
	return sc.Serial, true
}

// controllerGate decides whether a round looks at a controller's player key
// at all (#2624): only the key of the controller's owner, only while the
// controller is settled for it, and only when the controller did not move
// while the document was in flight (then the round skips it and the next
// pull is in full). handled is true when the round is done with k.
func (p *puller) controllerGate(rc *roundCtx, k Key) (handled bool) {
	serial, ok := rc.controllerOf(k)
	if !ok {
		return false
	}
	v := rc.ctrl[serial]
	if !v.readable || "game:"+v.owner+":"+serial != k.Scope {
		return true
	}
	if p.l.ctrl.genOf(serial) != v.gen {
		rc.skip()
		return true
	}
	return false
}

// removedGuard skips a controller key whose document value is one the
// controller's last removal took off and that differs from the remote value:
// a document that lags behind the removal must not hand a former game's
// override to the new owner. The guard ends once the document shows anything
// else, or after the push guard's lifetime.
func (p *puller) removedGuard(rc *roundCtx, k Key, pol *registry.Policy, d, r Value) (skip bool) {
	serial, ok := rc.controllerOf(k)
	if !ok {
		return false
	}
	removed := p.l.ctrl.removedValues(serial, k.Flag, rc.now, p.guardTTL())
	if len(removed) == 0 {
		return false
	}
	if d.Present && !r.Equal(d) {
		for _, fs := range removed {
			if p.decodeState(pol, fs).v.Equal(d) {
				rc.skip()
				return true
			}
		}
	}
	p.l.ctrl.dropRemoved(serial, k.Flag)
	return false
}

// trackGames keeps the controller map in step with the games (#2624). It
// runs under State.mu for every Change, before the out side sees it: a
// registration updates the owners (a controller's former owner loses its
// keys for it: they are never sent to a controller another game owns), and
// an end drops the game (its controllers are released). A game end without
// its game record gets the one the map knew, so the out side still removes a
// shadow game's player identities.
func (l *Link) trackGames(c state.Change) state.Change {
	switch c.Kind {
	case state.ChangeGameRegistered:
		if c.Game == nil || c.Game.ID == "" {
			return c
		}
		for s, former := range l.ctrl.register(c.Game) {
			l.links.ForgetIdentity("game:" + former + ":" + s)
		}
	case state.ChangeGameEnded:
		id := c.Scope.GameID
		if id == "" && c.Game != nil {
			id = c.Game.ID
		}
		if g, ok := l.ctrl.end(id); ok && c.Game == nil {
			c.Game = &state.Game{ID: g.id, Kind: g.kind, CoordinatorInstance: g.instance, Serials: slices.Clone(g.serials)}
		}
	}
	return c
}
