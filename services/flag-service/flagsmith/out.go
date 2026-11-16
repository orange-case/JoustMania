package flagsmith

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joustmania/flag-service/state"
)

// out is the out side of the link (UC-055, architecture note §4.1): it sends
// the local state to Flagsmith in the background. Changed only records what
// to send (map work under State.mu); one goroutine, Run, does every request,
// the latest value per flag and scope, at most PushRate requests per second.
type out struct {
	l    *Link
	wake chan struct{}
	ids  *identityQueue
	// fallback: the Flagsmith host has no __future__ API (latched for the
	// process, push.go).
	fallback atomic.Bool
	// traitsOff: identity traits are not persisted in this organisation.
	traitsOff atomic.Bool
	// nextAt is the earliest time of the next request (only Run reads it).
	nextAt time.Time
	// listed: when this process last listed or settled an identity (only
	// Run reads and writes it): a 404 right after is retried (#2639).
	listed map[string]time.Time
	// gone: identities the start cleanup deleted while their game
	// registered (identifier -> deleted uuid), handled by Run (#2639).
	goneMu sync.Mutex
	gone   map[string]string
}

func newOut(l *Link) *out {
	return &out{l: l, wake: make(chan struct{}, 1), ids: newIdentityQueue(), listed: map[string]time.Time{}, gone: map[string]string{}}
}

func (o *out) poke() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Changed implements outSide. It runs under State.mu for every Change: it
// does map work only, with no I/O and no call into State or the writer, so a
// slow or absent Flagsmith never delays a local write (NFR-057, BR-001).
func (o *out) Changed(c state.Change) {
	switch c.Kind {
	case state.ChangeValue, state.ChangeCleared, state.ChangeEvents:
		k, v, ok := ValueOfChange(c)
		if !ok {
			return
		}
		if c.Kind == state.ChangeEvents && emptyList(v.Value) {
			// An empty list shows no override.
			v = Value{Writer: c.Writer}
		}
		offline := k.Scope == ScopeStack && !o.l.connected.Load()
		if o.l.Links().OfferFrom(k, v, c.At, c.Version, offline) {
			o.poke()
		}
	case state.ChangeGameRegistered:
		o.gameRegistered(c)
	case state.ChangeGameEnded:
		o.gameEnded(c)
	case state.ChangeExperimentEnded:
		id := "experiment:" + c.Scope.ExperimentID + ":" + c.Scope.Arm
		o.l.Links().ForgetIdentity(id)
		o.ids.Want(id, false, nil)
		o.poke()
	}
}

func emptyList(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "[]" || string(raw) == "null"
}

// gameRegistered lists the game on its first registration. A shadow game
// lists each player it did not have before as game:<id>:<serial> and removes
// a player that left; the players of a real game are the controller map's
// (Link.trackGames, controllers.go), which settles each controller identity
// for its owner.
func (o *out) gameRegistered(c state.Change) {
	g := c.Game
	if g == nil || g.ID == "" {
		return
	}
	if c.Prev == nil {
		o.ids.Want("game:"+g.ID, true, o.gameTraits(g))
	}
	if isShadowKind(g.Kind) {
		for _, s := range g.Serials {
			if c.Prev == nil || !slices.Contains(c.Prev.Serials, s) {
				o.ids.Want("game:"+g.ID+":"+s, true, nil)
			}
		}
		if c.Prev != nil {
			for _, s := range c.Prev.Serials {
				if !slices.Contains(g.Serials, s) {
					id := "game:" + g.ID + ":" + s
					// No override request after the identity is gone.
					o.l.Links().ForgetIdentity(id)
					o.ids.Want(id, false, nil)
				}
			}
		}
	}
	o.poke()
}

// gameTraits are the traits of a game identity (architecture note §2); player
// identities carry none.
func (o *out) gameTraits(g *state.Game) []trait {
	if o.traitsOff.Load() {
		return nil
	}
	var ts []trait
	if g.Kind != "" {
		ts = append(ts, trait{Key: "game_kind", Value: g.Kind})
	}
	if g.CoordinatorInstance != "" {
		ts = append(ts, trait{Key: "coordinator_instance", Value: g.CoordinatorInstance})
	}
	return ts
}

// gameEnded drops every value of the game that was not sent yet and removes
// the game from Flagsmith (UC-055 A2, BR-003): the game identity is deleted,
// and a shadow game's player identities too. The controllers of a real game
// are released by the controller map (Link.trackGames).
func (o *out) gameEnded(c state.Change) {
	id := c.Scope.GameID
	if id == "" && c.Game != nil {
		id = c.Game.ID
	}
	if id == "" {
		return
	}
	o.l.Links().ForgetGame(id)
	o.ids.Want("game:"+id, false, nil)
	if c.Game != nil && isShadowKind(c.Game.Kind) {
		for _, s := range c.Game.Serials {
			o.ids.Want("game:"+id+":"+s, false, nil)
		}
	}
	o.poke()
}

// Run implements outSide: it sends identity work first, then the oldest
// pending value, until ctx ends or the link goes down. A value stays pending
// until Flagsmith accepted or refused it, so a link that drops loses nothing.
func (o *out) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		did, err := o.step(ctx)
		if err != nil {
			return err
		}
		if did {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.wake:
		}
	}
}

// step does one request (or one wait) of work and reports whether it found
// any.
func (o *out) step(ctx context.Context) (bool, error) {
	o.relistGone()
	if did, err := o.identityStep(ctx); did || err != nil {
		return did, err
	}
	if did, err := o.controllerStep(ctx); did || err != nil {
		return did, err
	}
	return o.pushNext(ctx)
}

// identityGone records that the start cleanup deleted identifier (uuid) while
// its game registered (#2639). Any goroutine may call it; Run handles it.
func (l *Link) identityGone(identifier, uuid string) {
	o, ok := l.out.(*out)
	if !ok {
		return
	}
	o.goneMu.Lock()
	o.gone[identifier] = uuid
	o.goneMu.Unlock()
	o.poke()
}

// relistGone handles the identities the start cleanup deleted under a game
// that registered meanwhile, on the Run goroutine, so every push that was in
// flight has ended: the deleted uuid is forgotten, the identity is listed
// again (with the game's traits) and its confirmed values are sent again.
// An identity whose game ended meanwhile is left to the game end.
func (o *out) relistGone() {
	o.goneMu.Lock()
	gone := o.gone
	if len(gone) == 0 {
		o.goneMu.Unlock()
		return
	}
	o.gone = map[string]string{}
	o.goneMu.Unlock()
	l := o.l
	for identifier, uuid := range gone {
		l.ids.Lock()
		if l.ids.IdentityUUID[identifier] == uuid {
			delete(l.ids.IdentityUUID, identifier)
		}
		l.ids.Unlock()
		g, live := l.liveGameOf(identifier)
		if !live {
			continue
		}
		var traits []trait
		if identifier == "game:"+g.ID {
			traits = o.gameTraits(&g)
		}
		o.ids.Recreate(identifier, traits)
		l.Links().Resend(identifier)
	}
}

// liveGameOf returns the registered game identifier belongs to as a whole
// (game:<id>, or game:<id>:<serial> of one of its serials).
func (l *Link) liveGameOf(identifier string) (state.Game, bool) {
	st := l.State()
	if st == nil || !strings.HasPrefix(identifier, "game:") {
		return state.Game{}, false
	}
	for _, g := range st.Games() {
		if identifier == "game:"+g.ID || slices.ContainsFunc(g.Serials, func(s string) bool { return identifier == "game:"+g.ID+":"+s }) {
			return g.Game, true
		}
	}
	return state.Game{}, false
}
