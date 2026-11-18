package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// The out side's controller work (#2624, controllers.go): settle a
// controller identity for its owner, or release it. Every request is paced
// and runs on the Run goroutine; an op is idempotent, so a 429 or a moved
// controller just starts it again from the top.

// ctrlStop is how a request of an op ended the op early.
type ctrlStop int

const (
	ctrlGoOn    ctrlStop = iota // the request worked: next step
	ctrlRefused                 // Flagsmith refused it for good: finish the op as refused
	ctrlAgain                   // a 429 was waited out, or the controller moved: start over
)

// controllerStep does one controller op. It reports whether it did something;
// the error ends the Run (link down, or ctx ended).
func (o *out) controllerStep(ctx context.Context) (bool, error) {
	ctrl := o.l.ctrl
	op, ok := ctrl.next()
	if !ok {
		return false, nil
	}
	identifier := ControllerIdentity(op.serial)
	finish := func(readable bool, removed []FeatureState) {
		ctrl.done(op, readable, removed, o.l.flagOfFeature, o.l.Now())
	}

	uuid, fresh, stop, err := o.controllerUUID(ctx, identifier, op.kind == ctrlSettle)
	switch {
	case err != nil:
		return false, err
	case stop == ctrlAgain:
		return true, nil
	case stop == ctrlRefused:
		o.l.Logger().Warn("flagsmith: controller identity not settled", "identifier", identifier)
		finish(false, nil)
		return true, nil
	case uuid == "":
		// Release of an identity Flagsmith does not have: nothing to do.
		finish(true, nil)
		return true, nil
	}
	if op.kind == ctrlSettle {
		// The owner's values go to this identity next (#2639).
		o.markListed(identifier)
	}

	clear := !fresh
	if op.kind == ctrlSettle && op.verify && !fresh && !o.traitsOff.Load() {
		gameID, stop, err := o.controllerGameID(ctx, identifier, uuid)
		switch {
		case err != nil:
			return false, err
		case stop == ctrlAgain:
			return true, nil
		}
		// The flag service restarted while the game was live: its overrides
		// are the owner's.
		clear = gameID != op.game.id
	}
	var removed []FeatureState
	readable := true
	if clear {
		var stop ctrlStop
		removed, stop, err = o.clearController(ctx, op, identifier, uuid)
		switch {
		case err != nil:
			if len(removed) > 0 {
				ctrl.noteRemoved(op.serial, removed, o.l.flagOfFeature, o.l.Now())
			}
			return false, err
		case stop == ctrlAgain:
			if len(removed) > 0 {
				ctrl.noteRemoved(op.serial, removed, o.l.flagOfFeature, o.l.Now())
			}
			return true, nil
		case stop == ctrlRefused:
			readable = false
			o.l.Logger().Warn("flagsmith: controller overrides not removed; its values are not read back for this game", "identifier", identifier)
		}
	}
	if stop, err := o.controllerTraits(ctx, op, identifier, uuid); err != nil {
		return false, err
	} else if stop == ctrlAgain {
		ctrl.noteRemoved(op.serial, removed, o.l.flagOfFeature, o.l.Now())
		return true, nil
	}
	finish(readable, removed)
	return true, nil
}

// ctrlCall paces and runs one request of controller work inside a push span
// and maps its outcome.
func (o *out) ctrlCall(ctx context.Context, k Key, opName string, fn func(context.Context) (int, error)) (outcome, ctrlStop, error) {
	if err := o.pace(ctx); err != nil {
		return outcome{}, ctrlAgain, err
	}
	oc := o.roundTrip(ctx, k, opName, "", 1, trace.SpanContext{}, fn)
	switch {
	case oc.err != nil:
		return oc, ctrlAgain, oc.err
	case oc.wait > 0:
		return oc, ctrlAgain, o.l.Sleep(ctx, oc.wait)
	case !oc.ok && oc.status == http.StatusNotFound && o.l.identityUUID(k.Scope) != "":
		// The identity is gone (a person deleted it): find or create it
		// again.
		o.l.forgetIdentityUUID(k.Scope)
		return oc, ctrlAgain, nil
	case !oc.ok:
		return oc, ctrlRefused, nil
	}
	return oc, ctrlGoOn, nil
}

// controllerUUID finds the controller identity's uuid: known, else an exact
// search, else (create) a new identity (fresh: it holds nothing yet).
func (o *out) controllerUUID(ctx context.Context, identifier string, create bool) (uuid string, fresh bool, stop ctrlStop, err error) {
	l := o.l
	if u := l.identityUUID(identifier); u != "" {
		return u, false, ctrlGoOn, nil
	}
	k := Key{Scope: identifier}
	_, stop, err = o.ctrlCall(ctx, k, OpIdentity, func(c context.Context) (int, error) {
		var found struct {
			Results []edgeIdentity `json:"results"`
		}
		q := url.QueryEscape(`"` + identifier + `"`)
		status, err := l.Client().Admin(c, http.MethodGet, l.envPath("edge-identities/")+"?q="+q, nil, &found)
		for _, r := range found.Results {
			if r.Identifier == identifier {
				uuid = r.IdentityUUID
			}
		}
		return status, err
	})
	if err != nil || stop != ctrlGoOn {
		return "", false, stop, err
	}
	if uuid != "" {
		l.setIdentityUUID(identifier, uuid)
		return uuid, false, ctrlGoOn, nil
	}
	if !create {
		return "", false, ctrlGoOn, nil
	}
	exists := false
	_, stop, err = o.ctrlCall(ctx, k, OpIdentity, func(c context.Context) (int, error) {
		var created struct {
			IdentityUUID string `json:"identity_uuid"`
		}
		status, err := l.Client().Admin(c, http.MethodPost, l.envPath("edge-identities/"), map[string]string{"identifier": identifier}, &created)
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(string(he.Body)), "already exist") {
			// Created meanwhile by someone else.
			exists = true
			return status, nil
		}
		uuid = created.IdentityUUID
		return status, err
	})
	switch {
	case err != nil || stop != ctrlGoOn:
		return "", false, stop, err
	case exists || uuid == "":
		// Flagsmith has it but the search did not show it (or answered no
		// uuid): the settle is refused for this game rather than retried
		// in a loop that would hold every value push back; the values are
		// still sent by identifier.
		return "", false, ctrlRefused, nil
	}
	l.setIdentityUUID(identifier, uuid)
	o.freshOverrides(uuid)
	return uuid, true, ctrlGoOn, nil
}

// controllerGameID reads the game_id trait of a controller identity ("" when
// it has none, or the traits cannot be read: the identity is then cleared).
func (o *out) controllerGameID(ctx context.Context, identifier, uuid string) (string, ctrlStop, error) {
	traits, stop, err := o.controllerTraitList(ctx, identifier, uuid)
	if stop == ctrlRefused {
		return "", ctrlGoOn, nil
	}
	s, _ := traits[TraitGameID].(string)
	return s, stop, err
}

// controllerTraitList reads the traits of a controller identity (key ->
// value; a null value is no trait).
func (o *out) controllerTraitList(ctx context.Context, identifier, uuid string) (map[string]any, ctrlStop, error) {
	l := o.l
	traits := map[string]any{}
	_, stop, err := o.ctrlCall(ctx, Key{Scope: identifier}, OpIdentity, func(c context.Context) (int, error) {
		var raw json.RawMessage
		status, err := l.Client().Admin(c, http.MethodGet, l.envPath("edge-identities/"+url.PathEscape(uuid)+"/list-traits/"), nil, &raw)
		if err == nil {
			traits = traitList(raw)
		}
		return status, err
	})
	return traits, stop, err
}

// traitList reads a trait list answer, plain or paged; a null value is no
// trait.
func traitList(raw json.RawMessage) map[string]any {
	type item struct {
		Key   string `json:"trait_key"`
		Value any    `json:"trait_value"`
	}
	var list []item
	if json.Unmarshal(raw, &list) != nil {
		var paged struct {
			Results []item `json:"results"`
		}
		_ = json.Unmarshal(raw, &paged)
		list = paged.Results
	}
	out := map[string]any{}
	for _, t := range list {
		if t.Value != nil {
			out[t.Key] = t.Value
		}
	}
	return out
}

// clearController removes every override of a registry feature from a
// controller identity: one listing, then one delete per override.
func (o *out) clearController(ctx context.Context, op ctrlOp, identifier, uuid string) ([]FeatureState, ctrlStop, error) {
	l := o.l
	var states []FeatureState
	_, stop, err := o.ctrlCall(ctx, Key{Scope: identifier}, OpIdentity, func(c context.Context) (int, error) {
		var raw json.RawMessage
		status, err := l.Client().Admin(c, http.MethodGet, l.envPath("edge-identities/"+url.PathEscape(uuid)+"/edge-featurestates/"), nil, &raw)
		if err == nil {
			states = overrideList(raw)
		}
		return status, err
	})
	if err != nil || stop != ctrlGoOn {
		return nil, stop, err
	}
	// The listing is complete: the owner's first values need no other (#2682).
	o.listedOverrides(uuid, states)
	var removed []FeatureState
	refused := false
	for _, fs := range states {
		flag := l.flagOfFeature(fs.Feature.ID)
		if flag == "" {
			continue
		}
		if !o.l.ctrl.current(op) {
			return removed, ctrlAgain, nil
		}
		k := Key{Set: l.setOfFlag(flag), Flag: flag, Scope: identifier}
		if fs.FeatureStateUUID == "" {
			// Flagsmith named no featurestate_uuid: it cannot be removed.
			refused = true
			continue
		}
		// The override itself (#2682); a 404 is an override gone already.
		_, stop, err := o.ctrlCall(ctx, k, OpDeleteOverride, func(c context.Context) (int, error) {
			status, err := l.Client().Admin(c, http.MethodDelete, l.overridePath(uuid, fs.FeatureStateUUID), nil, nil)
			return status, ignoreNotFound(err)
		})
		switch {
		case err != nil || stop == ctrlAgain:
			return removed, ctrlAgain, err
		case stop == ctrlRefused:
			refused = true
		default:
			o.setOverride(uuid, fs.Feature.ID, "")
			removed = append(removed, fs)
		}
	}
	if refused {
		return removed, ctrlRefused, nil
	}
	return removed, ctrlGoOn, nil
}

// overrideList reads the override list of an edge identity: plain or paged,
// "feature" as an id or an object.
func overrideList(raw json.RawMessage) []FeatureState {
	type item struct {
		Feature           json.RawMessage `json:"feature"`
		Enabled           bool            `json:"enabled"`
		FeatureStateValue json.RawMessage `json:"feature_state_value"`
		FeatureStateUUID  string          `json:"featurestate_uuid"`
	}
	var list []item
	if json.Unmarshal(raw, &list) != nil {
		var paged struct {
			Results []item `json:"results"`
		}
		_ = json.Unmarshal(raw, &paged)
		list = paged.Results
	}
	out := make([]FeatureState, 0, len(list))
	for _, it := range list {
		var f Feature
		if json.Unmarshal(it.Feature, &f.ID) != nil {
			_ = json.Unmarshal(it.Feature, &f)
		}
		if f.ID == 0 {
			continue
		}
		out = append(out, FeatureState{Feature: f, Enabled: it.Enabled, FeatureStateValue: it.FeatureStateValue, FeatureStateUUID: it.FeatureStateUUID})
	}
	return out
}

// controllerTraits sets the owner's traits (settle) or clears them (release,
// trait_value null). Traits are best effort, as on game identities: a refusal
// skips the rest, a 400 switches traits off for the process.
//
// A settle writes game_id last: an op cut short before it (a 429, the link
// going down) leaves the former game_id, so the restarted first settle does
// not take its own half-done work for a restart while the game was live. A
// release reads the traits first and clears only those that are set, so a
// controller released already costs no write (the start cleanup of idle
// controllers: two reads each, no write).
func (o *out) controllerTraits(ctx context.Context, op ctrlOp, identifier, uuid string) (ctrlStop, error) {
	if o.traitsOff.Load() {
		return ctrlGoOn, nil
	}
	l := o.l
	type traitBody struct {
		Key   string  `json:"trait_key"`
		Value *string `json:"trait_value"`
	}
	var bodies []traitBody
	if op.kind == ctrlSettle {
		g := op.game
		for _, t := range [][2]string{{TraitGameKind, g.kind}, {TraitCoordinatorInstance, g.instance}, {TraitGameID, g.id}} {
			v := t[1]
			bodies = append(bodies, traitBody{Key: t[0], Value: &v})
		}
	} else {
		set, stop, err := o.controllerTraitList(ctx, identifier, uuid)
		switch {
		case err != nil || stop == ctrlAgain:
			return ctrlAgain, err
		case stop == ctrlRefused:
			// Unknown: clear every trait.
			set = map[string]any{TraitGameID: true, TraitGameKind: true, TraitCoordinatorInstance: true}
		}
		for _, key := range []string{TraitGameID, TraitGameKind, TraitCoordinatorInstance} {
			if _, has := set[key]; has {
				bodies = append(bodies, traitBody{Key: key})
			}
		}
	}
	for _, b := range bodies {
		oc, stop, err := o.ctrlCall(ctx, Key{Scope: identifier}, OpIdentity, func(c context.Context) (int, error) {
			return l.Client().Admin(c, http.MethodPut, l.envPath("edge-identities/"+url.PathEscape(uuid)+"/update-traits/"), b, nil)
		})
		switch {
		case err != nil || stop == ctrlAgain:
			return ctrlAgain, err
		case stop == ctrlRefused:
			if oc.status == http.StatusBadRequest && !o.traitsOff.Swap(true) {
				l.Logger().Warn("flagsmith: identity traits are not persisted in this organisation, not sending them")
			}
			return ctrlGoOn, nil
		}
	}
	return ctrlGoOn, nil
}

// flagOfFeature is the registry flag key of a Flagsmith feature id ("" when
// the feature is not a registry flag).
func (l *Link) flagOfFeature(id int) string {
	ids := l.IDs()
	ids.Lock()
	defer ids.Unlock()
	for key, fid := range ids.FeatureID {
		if fid == id {
			return key
		}
	}
	return ""
}

// setOfFlag is the flag set of a registry flag key ("" when unknown).
func (l *Link) setOfFlag(key string) string {
	if l.deps.Registry == nil {
		return ""
	}
	for _, p := range l.deps.Registry.Policies() {
		if p.Key == key {
			return p.Set
		}
	}
	return ""
}
