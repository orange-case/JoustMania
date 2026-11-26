package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
)

// Scope is a write's scope properties (services.md §7.2).
type Scope struct {
	GameID       string `json:"game_id,omitempty"`
	Serial       string `json:"serial,omitempty"`
	ExperimentID string `json:"experiment_id,omitempty"`
	Arm          string `json:"arm,omitempty"`
	GameKind     string `json:"game_kind,omitempty"`
}

// Name derives the registry scope a set of scope properties addresses, or ""
// when the combination fits no scope.
func (sc Scope) Name() string {
	hasGame, hasSerial := sc.GameID != "", sc.Serial != ""
	hasExp := sc.ExperimentID != "" || sc.Arm != ""
	hasKind := sc.GameKind != ""
	switch {
	case hasGame && hasSerial && !hasExp && !hasKind:
		return registry.ScopeGameSerial
	case hasGame && !hasSerial && !hasExp && !hasKind:
		return registry.ScopeGame
	case hasExp && !hasGame && !hasSerial && !hasKind:
		if sc.ExperimentID == "" || sc.Arm == "" {
			return ""
		}
		return registry.ScopeExperiment
	case hasKind && !hasGame && !hasSerial && !hasExp:
		if sc.GameKind != gameKindReal {
			return ""
		}
		return registry.ScopeReal
	case !hasGame && !hasSerial && !hasExp && !hasKind:
		return registry.ScopeStack
	}
	return ""
}

// WriteRequest is a PUT /{flagSetId}/{flagName}.
type WriteRequest struct {
	Set        string
	Flag       string
	Variant    string
	Value      json.RawMessage // nil when absent
	Scope      Scope
	TTLSeconds *float64
	// Persist is optional and changes nothing: a stack write of a persist row
	// always persists. It is still refused where nothing would be persisted.
	Persist    bool
	Writer     string
	DecisionID string
	// DryRun validates and renders the write without applying it: the result
	// carries the flag rendered before and after, computed under one lock, and
	// nothing changes (the agent's experiment Gate checks before it writes).
	DryRun bool
	// IfWriterIn is the opt-in write precondition (?if_writer_in=, #2260,
	// UC-043 A19), like If-Match: in scope real only, the overlay is written
	// only when the stored real overlay's writer is one of the labels, or when
	// no real overlay is present and the list holds "". Else 412
	// writer_mismatch and nothing changes. nil = no precondition.
	IfWriterIn []string
	// IfSlotVersion is the in-process compare-and-set of a pulled Flagsmith
	// write (services.md §7.3): the write applies only while the slot's
	// version (Change.SlotVersion) is still this one, else 412 slot_changed
	// and nothing changes. 0 = no precondition. Never parsed from HTTP.
	IfSlotVersion uint64
}

// MaxIfWriterIn bounds the entries of ?if_writer_in=.
const MaxIfWriterIn = 16

// AppendRequest is a POST /{flagSetId}/{flagName} (an event).
type AppendRequest struct {
	Set        string
	Flag       string
	Scope      Scope
	Payload    map[string]any
	Writer     string
	DecisionID string
	// IfSlotVersion: as WriteRequest.IfSlotVersion (in process only).
	IfSlotVersion uint64
}

// ClearRequest is a DELETE /{flagSetId}/{flagName}. Without scope it clears the
// stack value: the persisted one of a persist row, else the in-memory overlay.
// Persist changes nothing (refused with a scope).
type ClearRequest struct {
	Set     string
	Flag    string
	Scope   Scope
	Writer  string
	Persist bool
	// IfWriter is the opt-in compare-and-delete precondition (?if_writer=,
	// #1877), like If-Match: in scope real or stack, the value is cleared only when its writer label
	// equals IfWriter, else 412 writer_mismatch and nothing changes. "" = no precondition. It is not the informational
	// Writer label, which is never checked (D11).
	IfWriter string
	// IfSlotVersion: as WriteRequest.IfSlotVersion (in process only).
	IfSlotVersion uint64
}

// WriteResult is the response of an accepted write, and the telemetry of a
// rejected one as far as it got (services.md §7.2 WriteResult; telemetry.md
// §10.1 flagservice.write attributes).
type WriteResult struct {
	FlagSet         string          `json:"flag_set"`
	Flag            string          `json:"flag"`
	Variant         string          `json:"variant,omitempty"`
	Value           json.RawMessage `json:"value,omitempty"`
	Seq             int64           `json:"seq,omitempty"`
	Epoch           string          `json:"epoch"`
	SnapshotVersion uint64          `json:"snapshot_version"`
	Persisted       bool            `json:"persisted"`
	// PersistFailed: a persisted clear of a kill switch applied in memory but
	// the state file could not be written (#2368); the DELETE answers 204 with
	// the header X-Flag-Persisted: false.
	PersistFailed bool `json:"-"`
	// Dry runs only: the flagd flag before and after the write.
	RenderedBefore json.RawMessage `json:"rendered_before,omitempty"`
	Rendered       json.RawMessage `json:"rendered,omitempty"`
	DryRun         bool            `json:"dry_run,omitempty"`
	// Telemetry only.
	Kind     string `json:"-"`
	ScopeKey string `json:"-"`
	GameKind string `json:"-"`
	Cleared  int    `json:"-"`
}

func checkIDs(sc Scope, writer string) *fserr.Error {
	for _, s := range []string{sc.GameID, sc.Serial, sc.ExperimentID, sc.Arm, sc.GameKind, writer} {
		if len(s) > MaxIDLength {
			return fserr.BadRequest("an id or label is longer than %d characters", MaxIDLength)
		}
	}
	return nil
}

// lookupLocked resolves (set, flag) and its policy: 404 for an unknown set or
// flag, 422 flag_read_only for a flag without a registry row.
func (s *State) lookupLocked(set, key string, res *WriteResult) (*flagState, *fserr.Error) {
	if s.base.Sets[set] == nil {
		return nil, fserr.NotFound(fserr.UnknownFlagSet, "no flag set %q", set)
	}
	f := s.flags[key]
	if f == nil || f.set != set {
		return nil, fserr.NotFound(fserr.UnknownFlag, "no flag %q in set %q", key, set)
	}
	if f.policy == nil {
		return nil, fserr.Unprocessable(fserr.FlagReadOnly, "%s/%s is read-only baseline (no registry row, flags.md §13)", set, key)
	}
	res.Kind = f.policy.Kind
	return f, nil
}

// liveChecksLocked rejects a write flagd could not see or that names a game
// that is not live (FR-133).
func (s *State) liveChecksLocked(f *flagState, sc Scope, scopeName string, res *WriteResult) *fserr.Error {
	if !s.served[f.set] {
		return fserr.Conflict(fserr.SetNotServed, "flag set %q is not served by the flag service yet (still a file source)", f.set)
	}
	if scopeName == registry.ScopeGame || scopeName == registry.ScopeGameSerial {
		g := s.games[sc.GameID]
		if g == nil {
			return fserr.Conflict(fserr.GameNotLive, "game %q is not registered or has ended", sc.GameID)
		}
		res.GameKind = g.Kind
		if scopeName == registry.ScopeGameSerial && !g.HasSerial(sc.Serial) {
			return fserr.Unprocessable(fserr.SerialNotInGame, "serial %q is not a player of game %q", sc.Serial, sc.GameID)
		}
	}
	return nil
}

func (s *State) streamCheckLocked(set string) *fserr.Error {
	if !s.hasStreamForLocked(set) {
		return fserr.Unavailable(fserr.NoSyncStream, "flagd has no open sync stream for set %q; the write would not reach readers", set)
	}
	return nil
}

// Put sets a value in a scope (PUT /{flagSetId}/{flagName}).
//
// UC-053 BR-002: validation, not authorization. A write is checked only
// against the registry (mutable, in bounds, scope fits), the game scope and
// the sync stream; the writer label is informational and grants nothing.
func (s *State) Put(req WriteRequest) (WriteResult, *fserr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := WriteResult{FlagSet: req.Set, Flag: req.Flag, Epoch: s.opts.Epoch, ScopeKey: req.Scope.Name()}

	plan, err := s.checkPutLocked(req, &res)
	if err != nil {
		return res, err
	}
	f, scopeName, checked := plan.f, plan.scope, plan.checked

	v := s.newPutValueLocked(req, plan)
	res.Value = checked.Value

	if req.DryRun {
		return s.dryRunLocked(f, req, scopeName, v, res)
	}

	if plan.persist {
		// A persist row has one persisted stack value, kept across restarts (A2).
		prevAdmin, prevStack := f.admin, f.stack
		f.admin, f.stack = v, nil
		if err := s.saveAdminLocked(); err != nil {
			if !f.policy.AppliesWhenPersistFails() {
				// Memory must match disk: undo, and tell the caller (#2368).
				f.admin, f.stack = prevAdmin, prevStack
				return res, fserr.PersistFailed("%s was not changed: the state file could not be written: %v", req.Flag, err)
			}
			// A kill switch takes effect now; the answer says
			// persisted:false (#2368, UC-053 A2/A12).
			s.opts.Logger.Error("flag service: kill switch applied in memory only; the state file could not be written", "flag", f.key, "error", err)
			// Not on disk: flagservice_overlays must not count it
			// as persisted until a later save writes it.
			v.Persisted = false
		}
	} else {
		f.place(scopeName, req.Scope, v)
	}
	s.slotChangedLocked(f, scopeName, req.Scope, req.Writer)
	s.changed()
	res.Variant = v.Variant
	res.SnapshotVersion = s.version
	res.Persisted = plan.persist && s.lastPersistOK
	return res, nil
}

// newPutValueLocked builds the value a checked Put places.
func (s *State) newPutValueLocked(req WriteRequest, plan putPlan) *value {
	now := s.opts.Now()
	v := &value{Writer: req.Writer, DecisionID: servedDecisionID(req.DecisionID), WrittenAt: now, Persisted: plan.persist}
	if plan.ttl > 0 {
		v.ExpiresAt = now.Add(plan.ttl)
	}
	if plan.stackLike && plan.checked.Variant != "" {
		v.Variant = plan.checked.Variant // D13: point defaultVariant at the declared variant
		return v
	}
	prefix := "r"
	if plan.stackLike {
		prefix = "o"
	}
	v.Variant = s.nextVariant(prefix)
	v.Value = plan.checked.Value
	return v
}

// putPlan is a checked Put, ready to apply.
type putPlan struct {
	f         *flagState
	scope     string
	stackLike bool
	persist   bool // a stack write of a persist row
	ttl       time.Duration
	checked   *registry.Checked
}

// checkPutLocked runs every precondition of a Put in this order (the first
// failure answers; TestPreconditionMatrix_Put pins each pair):
//
//	lookup 404/422 > kind 400 > variant|value 400 > ids 400 > scope fit 400 >
//	ttl 400 > if_writer_in shape 400 > persist 400 > live (served, game,
//	serial, real game) 409/422 > value 422 > persisted ttl 400 > stream 503 >
//	if_writer_in 412 > if_slot_version 412 (in process only)
//
// The 412 is last, a dry run included, under the lock of the write, so no
// other write can slip in between the compare and the write (#2260, UC-043
// A19), and every 4xx/5xx above it takes precedence.
func (s *State) checkPutLocked(req WriteRequest, res *WriteResult) (putPlan, *fserr.Error) {
	var plan putPlan
	f, err := s.lookupLocked(req.Set, req.Flag, res)
	if err != nil {
		return plan, err
	}
	if err := f.policy.WriterAllowed(req.Writer); err != nil {
		return plan, err
	}
	p := f.policy
	if p.Kind == registry.KindEvent {
		return plan, fserr.BadRequest("%s is an event flag: append with POST", req.Flag)
	}
	if (req.Variant == "") == (req.Value == nil) {
		return plan, fserr.BadRequest("exactly one of variant and value")
	}
	if err := checkIDs(req.Scope, req.Writer); err != nil {
		return plan, err
	}
	scopeName := res.ScopeKey
	if err := checkScopeFit(f, scopeName); err != nil {
		return plan, err
	}
	plan.f, plan.scope = f, scopeName
	plan.stackLike = scopeName == registry.ScopeStack || scopeName == registry.ScopeReal
	plan.persist = scopeName == registry.ScopeStack && p.Persist
	if req.TTLSeconds != nil {
		if !plan.stackLike || *req.TTLSeconds <= 0 || math.IsInf(*req.TTLSeconds, 0) || math.IsNaN(*req.TTLSeconds) {
			return plan, fserr.BadRequest("ttl_seconds must be positive and applies to stack overlays only")
		}
		plan.ttl = time.Duration(*req.TTLSeconds * float64(time.Second))
	}
	if err := checkIfWriterIn(req.IfWriterIn, scopeName); err != nil {
		return plan, err
	}
	if req.Persist {
		// Refused where nothing would be persisted, so an older caller that
		// expects persistence fails loudly (UC-053 BR-004).
		switch {
		case !p.Persist || scopeName != registry.ScopeStack:
			return plan, fserr.BadRequest("%s cannot persist in scope %s (registry persist: %v)", req.Flag, scopeName, p.Persist)
		case req.TTLSeconds != nil:
			return plan, fserr.BadRequest("persist and ttl_seconds exclude each other")
		case req.DryRun:
			return plan, fserr.BadRequest("a dry run cannot persist")
		}
	}
	if err := s.liveChecksLocked(f, req.Scope, scopeName, res); err != nil {
		return plan, err
	}
	plan.checked, err = registry.CheckValue(p, f.base, req.Variant, req.Value)
	if err != nil {
		return plan, err
	}
	if plan.persist && req.TTLSeconds != nil {
		return plan, fserr.BadRequest("%s is persisted in scope stack and takes no ttl_seconds", req.Flag)
	}
	if err := s.streamCheckLocked(f.set); err != nil {
		return plan, err
	}
	if req.IfWriterIn != nil && !realWriterAllowed(f.real, req.IfWriterIn) {
		return plan, fserr.PreconditionFailed(fserr.WriterMismatch, "the real overlay of %s is owned by %s, not one of %q; nothing written", f.key, describeRealWriter(f.real), req.IfWriterIn)
	}
	if err := s.checkSlotVersionLocked(f, scopeName, req.Scope, req.IfSlotVersion); err != nil {
		return plan, err
	}
	return plan, nil
}

// checkScopeFit is the 400 of a scope that fits no registry scope or is not
// one of the flag's scopes. Put and Clear share it.
func checkScopeFit(f *flagState, scopeName string) *fserr.Error {
	if scopeName == "" || !f.policy.AllowsScope(scopeName) {
		return fserr.BadRequest("the scope does not fit %s (allowed: %v)", f.key, f.policy.Scopes)
	}
	return nil
}

// checkIfWriterIn is the shape check of ?if_writer_in= (real scope only, at
// most MaxIfWriterIn entries, each at most MaxIDLength).
func checkIfWriterIn(list []string, scopeName string) *fserr.Error {
	if list != nil && scopeName != registry.ScopeReal {
		return fserr.BadRequest("if_writer_in applies to the real overlay only (game_kind=real)")
	}
	if len(list) > MaxIfWriterIn {
		return fserr.BadRequest("if_writer_in has more than %d entries", MaxIfWriterIn)
	}
	for _, w := range list {
		if len(w) > MaxIDLength {
			return fserr.BadRequest("an if_writer_in entry is longer than %d characters", MaxIDLength)
		}
	}
	return nil
}

// realWriterAllowed reports whether a real overlay (nil = none) passes an
// if_writer_in list: an absent overlay matches the empty entry, a present one
// matches a non-empty entry equal to its writer (an unlabeled overlay matches
// nothing, so the precondition fails closed).
func realWriterAllowed(cur *value, allowed []string) bool {
	for _, w := range allowed {
		if cur == nil && w == "" {
			return true
		}
		if cur != nil && w != "" && cur.Writer == w {
			return true
		}
	}
	return false
}

func describeRealWriter(cur *value) string {
	if cur == nil {
		return "nobody (no overlay)"
	}
	return fmt.Sprintf("writer %q", cur.Writer)
}

// Append adds one event to a game's event list (POST /{flagSetId}/{flagName},
// FR-137, UC-053 BR-006). The list keeps the last K events.
func (s *State) Append(req AppendRequest) (WriteResult, *fserr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := WriteResult{FlagSet: req.Set, Flag: req.Flag, Epoch: s.opts.Epoch, ScopeKey: req.Scope.Name()}

	f, err := s.lookupLocked(req.Set, req.Flag, &res)
	if err != nil {
		return res, err
	}
	if err := f.policy.WriterAllowed(req.Writer); err != nil {
		return res, err
	}
	if f.policy.Kind != registry.KindEvent {
		return res, fserr.BadRequest("%s is not an event flag: set it with PUT", req.Flag)
	}
	if err := checkIDs(req.Scope, req.Writer); err != nil {
		return res, err
	}
	if res.ScopeKey != registry.ScopeGame {
		return res, fserr.BadRequest("an event is appended in a game scope (scope.game_id only)")
	}
	if err := s.liveChecksLocked(f, req.Scope, res.ScopeKey, &res); err != nil {
		return res, err
	}
	payload := req.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	game := s.games[req.Scope.GameID]
	if err := registry.PayloadCheck(f.policy, payload, game.HasSerial); err != nil {
		return res, err
	}
	if err := s.streamCheckLocked(f.set); err != nil {
		return res, err
	}
	if err := s.checkSlotVersionLocked(f, res.ScopeKey, req.Scope, req.IfSlotVersion); err != nil {
		return res, err
	}

	list := f.events[req.Scope.GameID]
	if list == nil {
		list = &eventList{Events: []event{}}
		f.events[req.Scope.GameID] = list
	}
	list.Seq++
	list.Events = append(list.Events, event{Seq: list.Seq, Payload: payload, DecisionID: servedDecisionID(req.DecisionID)})
	if k := s.opts.EventListLen; len(list.Events) > k {
		list.Events = append([]event(nil), list.Events[len(list.Events)-k:]...)
	}
	list.Variant = s.nextVariant("r")
	s.slotChangedLocked(f, registry.ScopeGame, req.Scope, req.Writer)
	s.changed()
	raw, _ := json.Marshal(payload)
	res.Value = raw
	res.Variant = list.Variant
	res.Seq = list.Seq
	res.SnapshotVersion = s.version
	return res, nil
}

// Clear removes the value in a scope (DELETE /{flagSetId}/{flagName}). No
// scope clears the stack overlay; a persisted overlay also leaves the state
// file. Clearing a value that is not there is not an error.
func (s *State) Clear(req ClearRequest) (WriteResult, *fserr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := WriteResult{FlagSet: req.Set, Flag: req.Flag, Epoch: s.opts.Epoch, ScopeKey: req.Scope.Name()}

	f, err := s.checkClearLocked(req, &res)
	if err != nil {
		return res, err
	}
	scopeName := res.ScopeKey
	if scopeName == registry.ScopeStack {
		if err := s.clearStackLocked(f, req, &res); err != nil {
			return res, err
		}
	} else if s.removeLocked(f, scopeName, req.Scope) {
		res.Cleared = 1
	}
	if res.Cleared > 0 {
		s.slotChangedLocked(f, scopeName, req.Scope, req.Writer)
		s.changed()
	}
	res.SnapshotVersion = s.version
	return res, nil
}

// checkSlotVersionLocked is the 412 slot_changed of an in-process write with
// IfSlotVersion (0 = none): checked last, under the lock of the write, so a
// local write landing between the link's read and its write is never
// overwritten by an older Flagsmith value (UC-054, FR-210).
func (s *State) checkSlotVersionLocked(f *flagState, scopeName string, sc Scope, want uint64) *fserr.Error {
	if want == 0 {
		return nil
	}
	if got := s.slotVersionLocked(f, scopeName, sc); got != want {
		return fserr.PreconditionFailed(fserr.SlotChanged, "%s changed since it was read (slot version %d, not %d); nothing written", f.key, got, want)
	}
	return nil
}

// checkClearLocked runs every precondition of a Clear in this order (the first
// failure answers; TestPreconditionMatrix_Clear pins each pair):
//
//	lookup 404/422 > ids 400 > scope fit 400 > served 409 > stream 503 > persist 400 >
//	if_writer shape 400 > guarded revert 409 > if_writer 412 >
//	if_slot_version 412 (in process only)
//
// Like Put it refuses an id or label over MaxIDLength with 400 (#2370); unlike
// Put there is no game-live check: clearing what is not there is an
// idempotent 204. The 412 is last, under the lock of
// the clear, and evaluated only when the clear would otherwise succeed
// (#1877, UC-043 A18, like HTTP If-Match, RFC 9110 §13.2.2): a missing
// overlay is not a mismatch.
func (s *State) checkClearLocked(req ClearRequest, res *WriteResult) (*flagState, *fserr.Error) {
	f, err := s.lookupLocked(req.Set, req.Flag, res)
	if err != nil {
		return nil, err
	}
	if err := f.policy.WriterAllowed(req.Writer); err != nil {
		return nil, err
	}
	if err := checkIDs(req.Scope, req.Writer); err != nil {
		return nil, err
	}
	scopeName := res.ScopeKey
	if err := checkScopeFit(f, scopeName); err != nil {
		return nil, err
	}
	if !s.served[f.set] {
		return nil, fserr.Conflict(fserr.SetNotServed, "flag set %q is not served by the flag service yet", f.set)
	}
	if err := s.streamCheckLocked(f.set); err != nil {
		return nil, err
	}
	if req.Persist && scopeName != registry.ScopeStack {
		return nil, fserr.BadRequest("persist=true clears a stack-wide admin value only")
	}
	if req.IfWriter != "" && scopeName != registry.ScopeReal && scopeName != registry.ScopeStack {
		return nil, fserr.BadRequest("if_writer applies to the real overlay (game_kind=real) or the stack value only")
	}
	if req.IfWriter != "" {
		cur, layer := f.real, "real"
		if scopeName == registry.ScopeStack {
			cur, layer = f.slotValue(registry.ScopeStack, Scope{}), "stack"
		}
		if cur != nil && cur.Writer != req.IfWriter {
			return nil, fserr.PreconditionFailed(fserr.WriterMismatch, "the %s overlay of %s was written by %q, not %q; nothing cleared", layer, f.key, cur.Writer, req.IfWriter)
		}
	}
	if err := s.checkSlotVersionLocked(f, scopeName, req.Scope, req.IfSlotVersion); err != nil {
		return nil, err
	}
	return f, nil
}

// clearStackLocked clears the stack value in force: an in-memory one (a row
// without persist), else the persisted value of a persist row (also leaving the state file).
func (s *State) clearStackLocked(f *flagState, req ClearRequest, res *WriteResult) *fserr.Error {
	switch {
	case !f.policy.Persist || f.stack != nil:
		if f.stack != nil {
			f.stack, res.Cleared = nil, 1
		}
	case f.admin != nil:
		prevAdmin := f.admin
		f.admin = nil
		res.Cleared = 1
		if err := s.saveAdminLocked(); err != nil {
			if f.policy.AppliesWhenPersistFails() {
				// A kill switch is cleared now; the 204 carries the
				// not-persisted header (#2368, services.md §7.2).
				s.opts.Logger.Error("flag service: kill switch cleared in memory only; the state file could not be written", "flag", f.key, "error", err)
				res.PersistFailed = true
				return nil
			}
			// The old value would come back after a restart: keep it
			// live too, and tell the caller (#2368).
			f.admin, res.Cleared = prevAdmin, 0
			return fserr.PersistFailed("the persisted value of %s was not cleared: the state file could not be written: %v", req.Flag, err)
		}
		res.Persisted = s.lastPersistOK
	case s.persistBlocked:
		// No value in memory, but the unreadable state file may still hold
		// one that returns at the next start (UC-053 A12): a 204 would claim
		// it is gone (#2368). saveAdminLocked counts the failure.
		err := s.saveAdminLocked()
		if f.policy.AppliesWhenPersistFails() {
			s.opts.Logger.Error("flag service: kill switch clear not persisted; the state file was unreadable at start", "flag", f.key, "error", err)
			res.PersistFailed = true
			return nil
		}
		return fserr.PersistFailed("the persisted value of %s was not cleared: %v", req.Flag, err)
	}
	return nil
}

// removeLocked empties the slot scopeName and sc address (any scope but stack) and
// reports whether something was there. An event flag's game slot is its
// list: clearing empties it but keeps its seq, so the next event gets the
// next seq and the coordinator (which applies only a seq above the last one
// it applied) never drops it (PR #1417 review). The list goes with the game
// scope at game end.
func (s *State) removeLocked(f *flagState, scopeName string, sc Scope) bool {
	switch scopeName {
	case registry.ScopeReal:
		return clearSlot(&f.real)
	case registry.ScopeGame:
		if f.policy.Kind == registry.KindEvent {
			if list, ok := f.events[sc.GameID]; ok && len(list.Events) > 0 {
				list.Events = []event{}
				list.Variant = s.nextVariant("r")
				return true
			}
			return false
		}
		return deleteKey(f.games, sc.GameID)
	case registry.ScopeGameSerial:
		return deleteNested(f.serials, sc.GameID, sc.Serial)
	case registry.ScopeExperiment:
		return deleteNested(f.experiments, sc.ExperimentID, sc.Arm)
	}
	return false
}

func clearSlot(slot **value) bool {
	if *slot == nil {
		return false
	}
	*slot = nil
	return true
}

func deleteKey(m map[string]*value, key string) bool {
	if _, ok := m[key]; !ok {
		return false
	}
	delete(m, key)
	return true
}

// deleteNested deletes outer[o][inner] and drops the outer entry when it
// empties.
func deleteNested(outer map[string]map[string]*value, o, inner string) bool {
	m := outer[o]
	if m == nil || !deleteKey(m, inner) {
		return false
	}
	if len(m) == 0 {
		delete(outer, o)
	}
	return true
}

// ClearWriter removes every stack-wide overlay (stack, real) that
// writer set in a flag set (DELETE /{flagSetId}?writer=, the talk reset).
// It returns how many overlays it removed.
func (s *State) ClearWriter(set, writer string) (int, *fserr.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base.Sets[set] == nil {
		return 0, fserr.NotFound(fserr.UnknownFlagSet, "no flag set %q", set)
	}
	if writer == "" {
		return 0, fserr.BadRequest("writer is required")
	}
	if !s.served[set] {
		return 0, fserr.Conflict(fserr.SetNotServed, "flag set %q is not served by the flag service yet", set)
	}
	if err := s.streamCheckLocked(set); err != nil {
		return 0, err
	}
	// Persisted admin values are the host's settings: only an explicit
	// DELETE ?persist=true removes them, never a writer reset (PR #1417 review).
	n := 0
	for _, f := range s.flags {
		// A Flagsmith-only flag changes from Flagsmith alone (#2705): an HTTP
		// writer reset, even ?writer=flagsmith, never clears it.
		if f.set != set || f.policy.FlagsmithOnly() {
			continue
		}
		if f.stack != nil && f.stack.Writer == writer {
			f.stack = nil
			n++
			s.slotChangedLocked(f, registry.ScopeStack, Scope{}, writer)
		}
		if f.real != nil && f.real.Writer == writer {
			f.real = nil
			n++
			s.slotChangedLocked(f, registry.ScopeReal, Scope{}, writer)
		}
	}
	if n > 0 {
		s.changed()
	}
	return n, nil
}

// ExpireOverlays drops stack-wide overlays whose TTL ran out and returns how
// many.
func (s *State) ExpireOverlays() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	n := 0
	expired := func(v *value) bool { return v != nil && !v.ExpiresAt.IsZero() && !now.Before(v.ExpiresAt) }
	for _, f := range s.flags {
		if expired(f.stack) {
			f.stack = nil
			n++
			s.slotChangedLocked(f, registry.ScopeStack, Scope{}, "")
		}
		if expired(f.real) {
			f.real = nil
			n++
			s.slotChangedLocked(f, registry.ScopeReal, Scope{}, "")
		}
	}
	if n > 0 {
		s.changed()
	}
	return n
}

// jsonValue renders a value for views: the declared variant's value or the
// generated one.
func (f *flagState) jsonValue(v *value) json.RawMessage {
	if v.Value != nil {
		return v.Value
	}
	return bytes.TrimSpace(f.base.Variants[v.Variant])
}

// ExpireExperiments drops the arm values of experiments that no registered game
// has carried, and nobody wrote, for ExperimentIdle (NFR-051). It returns how
// many experiments it dropped.
func (s *State) ExpireExperiments() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	for _, g := range s.games {
		if g.ExperimentID != "" {
			s.expSeen[g.ExperimentID] = now
		}
	}
	// Idleness is decided per experiment, over every flag the experiment holds
	// arms for: an experiment never loses some flags' arms and keeps others.
	lastWrite := map[string]time.Time{}
	for _, f := range s.flags {
		for id, byArm := range f.experiments {
			last := lastWrite[id]
			for _, v := range byArm {
				if v.WrittenAt.After(last) {
					last = v.WrittenAt
				}
			}
			lastWrite[id] = last
		}
	}
	dropped := map[string]bool{}
	present := map[string]bool{}
	for id, written := range lastWrite {
		last := s.expSeen[id]
		if written.After(last) {
			last = written
		}
		if now.Sub(last) >= s.opts.ExperimentIdle {
			dropped[id] = true
		} else {
			present[id] = true
		}
	}
	arms := map[string]map[string]bool{}
	for _, f := range s.flags {
		for id, byArm := range f.experiments {
			if !dropped[id] {
				continue
			}
			delete(f.experiments, id)
			if arms[id] == nil {
				arms[id] = map[string]bool{}
			}
			for _, arm := range sortedKeys(byArm) {
				arms[id][arm] = true
				s.slotDroppedLocked(f, registry.ScopeExperiment, Scope{ExperimentID: id, Arm: arm})
			}
		}
	}
	for _, id := range sortedKeys(arms) {
		delete(s.slotVer, "x:"+id)
		for _, arm := range sortedKeys(arms[id]) {
			s.noteLocked(Change{Kind: ChangeExperimentEnded, ScopeName: registry.ScopeExperiment, Scope: Scope{ExperimentID: id, Arm: arm}})
		}
	}
	// The slot versions of an experiment whose arm values were all cleared
	// (DELETE) go once no registered game carries it: the loops above never
	// see an experiment without values (NFR-051, #2594). expSeen still holds
	// last sweep's ids here, so a cleared experiment keeps its versions while
	// a game carries it.
	for outer := range s.slotVer {
		id, ok := strings.CutPrefix(outer, "x:")
		if !ok {
			continue
		}
		if _, hasValues := lastWrite[id]; hasValues {
			continue
		}
		if _, seen := s.expSeen[id]; seen {
			continue
		}
		delete(s.slotVer, outer)
	}
	for id := range s.expSeen {
		if !present[id] {
			delete(s.expSeen, id)
		}
	}
	if len(dropped) > 0 {
		s.changed()
	}
	return len(dropped)
}

// dryRunLocked renders f before and with v in place, then puts the previous
// value back. Nothing else observes f meanwhile (s.mu is held), the snapshot
// version does not move and no sync stream is notified.
func (s *State) dryRunLocked(f *flagState, req WriteRequest, scopeName string, v *value, res WriteResult) (WriteResult, *fserr.Error) {
	before, err := f.render(s.opts.Epoch)
	if err != nil {
		return res, fserr.As(err)
	}
	restore := f.place(scopeName, req.Scope, v)
	after, err := f.render(s.opts.Epoch)
	restore()
	if err != nil {
		return res, fserr.As(err)
	}
	res.RenderedBefore, _ = json.Marshal(before)
	res.Rendered, _ = json.Marshal(after)
	res.Variant = v.Variant
	res.SnapshotVersion = s.version
	res.DryRun = true
	return res, nil
}

// place puts v in the slot scopeName and sc address (stack: the in-memory
// overlay) and returns a func that restores the previous content.
func (f *flagState) place(scopeName string, sc Scope, v *value) func() {
	switch scopeName {
	case registry.ScopeStack:
		old := f.stack
		f.stack = v
		return func() { f.stack = old }
	case registry.ScopeReal:
		old := f.real
		f.real = v
		return func() { f.real = old }
	case registry.ScopeGame:
		old, had := f.games[sc.GameID]
		f.games[sc.GameID] = v
		return func() {
			if had {
				f.games[sc.GameID] = old
			} else {
				delete(f.games, sc.GameID)
			}
		}
	case registry.ScopeGameSerial:
		bySerial, hadGame := f.serials[sc.GameID]
		if !hadGame {
			bySerial = map[string]*value{}
			f.serials[sc.GameID] = bySerial
		}
		old, had := bySerial[sc.Serial]
		bySerial[sc.Serial] = v
		return func() {
			switch {
			case !hadGame:
				delete(f.serials, sc.GameID)
			case had:
				bySerial[sc.Serial] = old
			default:
				delete(bySerial, sc.Serial)
			}
		}
	case registry.ScopeExperiment:
		byArm, hadExp := f.experiments[sc.ExperimentID]
		if !hadExp {
			byArm = map[string]*value{}
			f.experiments[sc.ExperimentID] = byArm
		}
		old, had := byArm[sc.Arm]
		byArm[sc.Arm] = v
		return func() {
			switch {
			case !hadExp:
				delete(f.experiments, sc.ExperimentID)
			case had:
				byArm[sc.Arm] = old
			default:
				delete(byArm, sc.Arm)
			}
		}
	}
	return func() {}
}

// servedDecisionID returns the decision_id the service keeps and serves with a
// value or event: the write's id, or "" when it is longer than
// MaxDecisionIDLength (UC-101 BR-003, NFR-061: dropped, never refused, #2665).
func servedDecisionID(id string) string {
	if len(id) > MaxDecisionIDLength {
		return ""
	}
	return id
}
