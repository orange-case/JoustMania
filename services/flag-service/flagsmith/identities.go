package flagsmith

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

// trait is one identity trait (best effort, game identities only: the
// architecture note's open decision D4).
type trait struct {
	Key, Value string
}

// identityWant is what the flag service wants of one identity in Flagsmith.
type identityWant struct {
	present bool
	// gen counts flips of present; an operation that finished for an older
	// generation does not touch the entry.
	gen     uint64
	created bool
	// inflight: the create request is on the wire (Start). A game that ends
	// meanwhile must still delete the identity that request creates.
	inflight bool
	uuid     string
	traits   []trait
}

// identityOp is the next step for one identity.
type identityOp struct {
	Identifier string
	Present    bool
	Created    bool
	UUID       string
	// Trait is the next trait to send (Present and Created only).
	Trait *trait
	gen   uint64
}

// identityQueue holds the identity work of the out side: list a game and its
// players when they register, remove them when the game ends (UC-055 A1, A2).
// It coalesces per identifier, so a game that ends before its identities were
// sent costs no request. It has its own mutex: Changed (under State.mu) adds,
// the Run goroutine takes.
type identityQueue struct {
	mu    sync.Mutex
	order []string
	want  map[string]*identityWant
	gen   uint64
}

func newIdentityQueue() *identityQueue {
	return &identityQueue{want: map[string]*identityWant{}}
}

func (q *identityQueue) removeLocked(id string) {
	delete(q.want, id)
	for i, o := range q.order {
		if o == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			return
		}
	}
}

// Want sets the wanted state of identifier. The same state again changes
// nothing. A listing that was never sent and is not wanted any more is
// dropped, with no request at all.
func (q *identityQueue) Want(identifier string, present bool, traits []trait) {
	q.mu.Lock()
	defer q.mu.Unlock()
	w := q.want[identifier]
	switch {
	case w == nil:
		q.gen++
		q.want[identifier] = &identityWant{present: present, gen: q.gen, traits: traits}
		q.order = append(q.order, identifier)
	case w.present == present:
	case present:
		q.gen++
		w.present, w.gen, w.created, w.inflight, w.uuid, w.traits = true, q.gen, false, false, "", traits
	case !w.created && !w.inflight:
		q.removeLocked(identifier)
	default:
		// Created, or being created right now: the identity exists or will,
		// so the entry becomes a delete (never dropped).
		q.gen++
		w.present, w.gen, w.inflight, w.traits = false, q.gen, false, nil
	}
}

// Start marks the create request of op as on the wire.
func (q *identityQueue) Start(op identityOp) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w := q.current(op); w != nil && op.Present && !op.Created {
		w.inflight = true
	}
}

// CreateEnded marks the create request of op as off the wire, whatever its
// outcome (created, refused, link down): a create that did not happen must not
// make a later game end queue a delete.
func (q *identityQueue) CreateEnded(op identityOp) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w := q.current(op); w != nil {
		w.inflight = false
	}
}

// Peek returns the first identity with work left, without removing it.
func (q *identityQueue) Peek() (identityOp, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.order) > 0 {
		id := q.order[0]
		w := q.want[id]
		if w.present && w.created && len(w.traits) == 0 {
			q.removeLocked(id)
			continue
		}
		op := identityOp{Identifier: id, Present: w.present, Created: w.created, UUID: w.uuid, gen: w.gen}
		if w.present && w.created {
			t := w.traits[0]
			op.Trait = &t
		}
		return op, true
	}
	return identityOp{}, false
}

func (q *identityQueue) current(op identityOp) *identityWant {
	if w := q.want[op.Identifier]; w != nil && w.gen == op.gen {
		return w
	}
	return nil
}

// Created records that the identity exists now (uuid "": it existed already,
// so its traits are not sent).
func (q *identityQueue) Created(op identityOp, uuid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w := q.current(op); w != nil {
		w.created, w.uuid = true, uuid
		if uuid == "" {
			w.traits = nil
		}
	}
}

// TraitDone removes the trait that was just sent.
func (q *identityQueue) TraitDone(op identityOp) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w := q.current(op); w != nil && len(w.traits) > 0 {
		w.traits = w.traits[1:]
	}
}

// DropTraits forgets the traits still to send.
func (q *identityQueue) DropTraits(op identityOp) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w := q.current(op); w != nil {
		w.traits = nil
	}
}

// Done removes the identity's entry: its work is finished or was refused.
func (q *identityQueue) Done(op identityOp) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current(op) != nil {
		q.removeLocked(op.Identifier)
	}
}

// identityStep does one request of identity work. It reports whether it did
// something; the error ends the Run (link down, or ctx ended).
func (o *out) identityStep(ctx context.Context) (bool, error) {
	op, ok := o.ids.Peek()
	if !ok {
		return false, nil
	}
	if err := o.pace(ctx); err != nil {
		return false, err
	}
	var oc outcome
	o.ids.Start(op)
	switch {
	case !op.Present:
		oc = o.deleteIdentity(ctx, op)
	case !op.Created:
		oc = o.createIdentity(ctx, op)
	default:
		oc = o.sendTrait(ctx, op)
	}
	if oc.err != nil {
		return false, oc.err
	}
	if oc.wait > 0 {
		return true, o.l.Sleep(ctx, oc.wait)
	}
	return true, nil
}

// createIdentity lists an identity. An identity that exists already is
// success: Flagsmith answers 400 "already exists", and keeps its overrides.
func (o *out) createIdentity(ctx context.Context, op identityOp) outcome {
	l := o.l
	uuid := ""
	oc := o.roundTrip(ctx, Key{Scope: op.Identifier}, OpIdentity, "", 1, trace.SpanContext{}, func(c context.Context) (int, error) {
		var created struct {
			IdentityUUID string `json:"identity_uuid"`
		}
		status, err := l.Client().Admin(c, http.MethodPost, l.envPath("edge-identities/"), map[string]string{"identifier": op.Identifier}, &created)
		if err == nil {
			uuid = created.IdentityUUID
			return status, nil
		}
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(string(he.Body)), "already exist") {
			return status, nil
		}
		return status, err
	})
	o.ids.CreateEnded(op)
	switch {
	case oc.ok:
		if uuid != "" {
			l.setIdentityUUID(op.Identifier, uuid)
		}
		if o.traitsOff.Load() {
			o.ids.DropTraits(op)
		}
		o.ids.Created(op, uuid)
	case oc.err == nil && oc.wait == 0:
		l.Logger().Warn("flagsmith: identity not listed", "identifier", op.Identifier, "reason", oc.reason)
		o.ids.Done(op)
	}
	return oc
}

// sendTrait sends the next trait of a game identity. Traits are best effort:
// a refusal drops the rest, and a 400 (an organisation that does not persist
// trait data, TraitPersistenceError) switches traits off for the process after
// one warning.
func (o *out) sendTrait(ctx context.Context, op identityOp) outcome {
	l := o.l
	t := *op.Trait
	oc := o.roundTrip(ctx, Key{Scope: op.Identifier}, OpIdentity, "", 1, trace.SpanContext{}, func(c context.Context) (int, error) {
		body := struct {
			Key   string `json:"trait_key"`
			Value string `json:"trait_value"`
		}{t.Key, t.Value}
		return l.Client().Admin(c, http.MethodPut, l.envPath("edge-identities/"+url.PathEscape(op.UUID)+"/update-traits/"), body, nil)
	})
	switch {
	case oc.ok:
		o.ids.TraitDone(op)
	case oc.err == nil && oc.wait == 0:
		if oc.status == http.StatusBadRequest && !o.traitsOff.Swap(true) {
			l.Logger().Warn("flagsmith: identity traits are not persisted in this organisation, not sending them")
		}
		o.ids.DropTraits(op)
	}
	return oc
}

// deleteIdentity removes an identity with its overrides. Its uuid is known
// when this process listed it, else an exact search finds it (the quoted
// query); an identity Flagsmith does not know is success.
func (o *out) deleteIdentity(ctx context.Context, op identityOp) outcome {
	l := o.l
	oc := o.roundTrip(ctx, Key{Scope: op.Identifier}, OpDeleteIdentity, "", 1, trace.SpanContext{}, func(c context.Context) (int, error) {
		uuid := l.identityUUID(op.Identifier)
		if uuid == "" {
			var found struct {
				Results []struct {
					IdentityUUID string `json:"identity_uuid"`
					Identifier   string `json:"identifier"`
				} `json:"results"`
			}
			q := url.QueryEscape(`"` + op.Identifier + `"`)
			status, err := l.Client().Admin(c, http.MethodGet, l.envPath("edge-identities/")+"?q="+q, nil, &found)
			if err != nil {
				return status, err
			}
			for _, r := range found.Results {
				if r.Identifier == op.Identifier {
					uuid = r.IdentityUUID
					break
				}
			}
			if uuid == "" {
				return status, nil
			}
			if err := o.pace(c); err != nil {
				return 0, err
			}
		}
		status, err := l.Client().Admin(c, http.MethodDelete, l.envPath("edge-identities/"+url.PathEscape(uuid)+"/"), nil, nil)
		return status, ignoreNotFound(err)
	})
	switch {
	case oc.ok:
		l.forgetIdentityUUID(op.Identifier)
		o.ids.Done(op)
	case oc.err == nil && oc.wait == 0:
		l.Logger().Warn("flagsmith: identity not removed", "identifier", op.Identifier, "reason", oc.reason)
		o.ids.Done(op)
	}
	return oc
}

func (l *Link) identityUUID(identifier string) string {
	ids := l.IDs()
	ids.Lock()
	defer ids.Unlock()
	return ids.IdentityUUID[identifier]
}

func (l *Link) setIdentityUUID(identifier, uuid string) {
	ids := l.IDs()
	ids.Lock()
	defer ids.Unlock()
	ids.IdentityUUID[identifier] = uuid
}

func (l *Link) forgetIdentityUUID(identifier string) {
	ids := l.IDs()
	ids.Lock()
	defer ids.Unlock()
	delete(ids.IdentityUUID, identifier)
}
