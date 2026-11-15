package flagsmith

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
)

// The in side (UC-054, architecture note §4.2 to §6): pull the environment
// document, compare each synced (set, key, scope) 3-way (merge.go) and turn
// each change made in Flagsmith into an in-process write labelled flagsmith.

// Round limits (architecture note §4.2 steps 1, 8, 9).
const (
	// unconditionalEvery: every 10th pull without a live game is unconditional
	// (an edge override change may not move the environment updated_at).
	unconditionalEvery = 10
	// minPullGap: pulls are at least 1 s apart (wake-ups are coalesced).
	minPullGap = time.Second
	// writeBudget: writes per round; the rest follow in the next round.
	writeBudget = 50
	// deferralUnhealthyAfter: consecutive deferrals of one key before its
	// feature is marked unhealthy.
	deferralUnhealthyAfter = 5
	// segmentReason is the health text of a segment override (§4.2 step 10).
	segmentReason = "segment overrides are not applied by JoustMania"
	// segmentScope is the health pseudo-scope of a feature's segment override.
	segmentScope = "segment"
)

// puller implements inSide. Reconcile and Poll never run at the same time
// (Link.Run), so its fields need no lock; health has its own.
type puller struct {
	l      *Link
	byName map[string]*registry.Policy
	// refused holds the document value refused per key (and the local value
	// kept); deleted on any other outcome and when the document changes.
	refused  map[Key]refusal
	deferred map[Key]*deferral
	// forceFull: the next pull is unconditional.
	forceFull bool
	docHash   [32]byte
	pulls     int
	lastPull  time.Time
	notBefore time.Time
	health    *health
	after     func(time.Duration) <-chan time.Time
}

// deferral is a key whose apply was put off (flagd not connected, a stale
// slot, a persist failure, a live real game): it backs off 1, 2, 4 ... 60 s.
type deferral struct {
	n     int
	until time.Time
}

func newIn(l *Link) inSide {
	p := &puller{
		l:        l,
		byName:   map[string]*registry.Policy{},
		refused:  map[Key]refusal{},
		deferred: map[Key]*deferral{},
		after:    time.After,
	}
	if l.deps.Registry != nil {
		for _, pol := range l.deps.Registry.Policies() {
			p.byName[pol.Key] = pol
		}
	}
	p.health = newHealth(l)
	return p
}

// roundStats are a round's counts (span attributes).
type roundStats struct {
	applied, pushed, conflicts, changes int
}

// Reconcile is the full pull and 3-way reconcile at start and on every
// reconnect (FR-210).
func (p *puller) Reconcile(ctx context.Context, trigger string) error {
	ctx, span := StartReconcile(ctx, p.l.Tracer(), trigger)
	rs, err := p.round(ctx, trigger, true)
	span.SetAttributes(
		attribute.Int(AttrApplied, rs.applied),
		attribute.Int(AttrPushed, rs.pushed),
		attribute.Int(AttrConflicts, rs.conflicts),
	)
	if err != nil {
		EndSpan(span, ResultError, Reason(err))
		return err
	}
	EndSpan(span, ResultOK, "")
	return nil
}

// Poll pulls every FLAGSMITH_POLL_SECONDS and on every Wake until ctx ends or
// the link goes down. The realtime reader, when configured, has returned
// before Poll does.
func (p *puller) Poll(ctx context.Context) error {
	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	if p.l.cfg.RealtimeURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.realtime(rctx)
		}()
	}
	interval := p.l.cfg.Poll
	if interval <= 0 {
		interval = DefaultPoll
	}
	for {
		trigger := TriggerPoll
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.after(interval):
		case <-p.l.WakeC():
			trigger = TriggerRealtime
		}
		if err := p.waitGap(ctx); err != nil {
			return err
		}
		_, err := p.round(ctx, trigger, false)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case IsLinkDown(err):
			return err
		default:
			p.l.Logger().Warn("flagsmith pull failed", "reason", Reason(err))
		}
	}
}

// waitGap keeps pulls minPullGap apart and honours a 429 Retry-After.
func (p *puller) waitGap(ctx context.Context) error {
	until := p.lastPull.Add(minPullGap)
	if p.notBefore.After(until) {
		until = p.notBefore
	}
	if d := until.Sub(p.l.Now()); d > 0 {
		return p.l.Sleep(ctx, d)
	}
	return nil
}

// docEntry is one key's value in the document.
type docEntry struct {
	v Value
	// err is a value the flag service cannot read (flagsmith_type_mismatch);
	// v then carries the raw value.
	err *fserr.Error
}

// localEntry is one key's local value with the slot version it was read at.
type localEntry struct {
	v       Value
	slot    uint64
	version uint64
}

// localSnapshot maps State.Changes to keys, and lists the registered games.
func localSnapshot(changes []state.Change) (map[Key]localEntry, map[string]bool) {
	local := map[Key]localEntry{}
	games := map[string]bool{}
	for _, c := range changes {
		if c.Kind == state.ChangeGameRegistered {
			games[c.Scope.GameID] = true
			continue
		}
		k, v, ok := ValueOfChange(c)
		if !ok {
			continue
		}
		local[k] = localEntry{v: v, slot: c.SlotVersion, version: c.Version}
	}
	return local, games
}

// decodeDocument maps the document to keys. Unknown feature names and
// identities are ignored; segment overrides are reported per feature, never
// applied.
func (p *puller) decodeDocument(doc *EnvironmentDocument) (map[Key]docEntry, map[string]bool) {
	out := map[Key]docEntry{}
	segments := map[string]bool{}
	for _, fs := range doc.FeatureStates {
		pol := p.byName[fs.Feature.Name]
		if pol == nil {
			continue
		}
		if isSegment(fs.FeatureSegment) {
			segments[pol.Key] = true
			continue
		}
		k := Key{Set: pol.Set, Flag: pol.Key, Scope: ScopeStack}
		out[k] = p.decodeState(pol, registry.ScopeStack, fs)
	}
	for _, ov := range doc.IdentityOverrides {
		_, scopeName, ok := ScopeOfIdentity(ov.Identifier)
		if !ok {
			continue
		}
		for _, fs := range ov.IdentityFeatures {
			pol := p.byName[fs.Feature.Name]
			if pol == nil {
				continue
			}
			k := Key{Set: pol.Set, Flag: pol.Key, Scope: ov.Identifier}
			out[k] = p.decodeState(pol, scopeName, fs)
		}
	}
	return out, segments
}

func isSegment(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// decodeState turns one feature state into a Value: enabled=false is no
// value; a fraction override is its {"variant", "fraction_percent"} object; a
// declared variant carries its baseline value too.
func (p *puller) decodeState(pol *registry.Policy, scopeName string, fs FeatureState) docEntry {
	var e docEntry
	if !fs.Enabled {
		return e
	}
	fv := FSValueOf(fs.FeatureStateValue)
	b := p.l.BaselineFlag(pol.Set, pol.Key)
	if scopeName == registry.ScopeFraction {
		var s string
		if fv.Type == TypeUnicode && json.Unmarshal(fv.Value, &s) == nil && json.Valid([]byte(s)) {
			e.v = Value{Present: true, Value: compact(json.RawMessage(s))}
			return e
		}
		e.v = Value{Present: true, Value: compact(fs.FeatureStateValue)}
		e.err = fserr.Unprocessable(fserr.FlagsmithTypeMismatch, "%s: a fraction value is the JSON text {\"variant\", \"fraction_percent\"}", pol.Key)
		return e
	}
	variant, value, ferr := Decode(pol, b, fv)
	if ferr != nil {
		e.v = Value{Present: true, Value: compact(fs.FeatureStateValue)}
		e.err = ferr
		return e
	}
	if variant != "" && b != nil {
		value = compact(b.Variants[variant])
	}
	e.v = Value{Present: true, Variant: variant, Value: value}
	return e
}

// docDigest is a canonical hash of the decoded document (a changed document
// makes the next pull unconditional: the same-second Last-Modified miss).
func docDigest(d map[Key]docEntry) [32]byte {
	keys := make([]Key, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sortKeys(keys)
	h := sha256.New()
	for _, k := range keys {
		e := d[k]
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%t\x00%s\x00%s\n", k.Set, k.Flag, k.Scope, e.v.Present, e.v.Variant, e.v.Value)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func sortKeys(keys []Key) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Set != b.Set {
			return a.Set < b.Set
		}
		if a.Flag != b.Flag {
			return a.Flag < b.Flag
		}
		return a.Scope < b.Scope
	})
}

// roundCtx is the state of one round.
type roundCtx struct {
	ctx      context.Context
	span     trace.Span
	trigger  string
	now      time.Time
	updated  time.Time
	hasUpd   bool
	games    map[string]bool
	writes   int
	deferred bool
	stats    roundStats
}

// skip leaves a key for the next round, which pulls in full.
func (rc *roundCtx) skip() { rc.deferred = true }

// round is one pull and reconcile. full: no If-Modified-Since.
func (p *puller) round(ctx context.Context, trigger string, full bool) (roundStats, error) {
	l := p.l
	ctx, span := StartPull(ctx, l.Tracer(), trigger)
	result, reason := ResultError, ""
	rc := &roundCtx{ctx: ctx, span: span, trigger: trigger}
	defer func() {
		span.SetAttributes(attribute.Int(AttrChanges, rc.stats.changes), attribute.Int(AttrConflicts, rc.stats.conflicts))
		EndSpan(span, result, reason)
		l.Metrics().RecordPull(ctx, trigger, result)
	}()

	// The local snapshot and the remote generations are read before the
	// fetch: a local write after it is caught by IfSlotVersion, a push
	// confirmed during it by the generation (a stale document).
	var changes []state.Change
	if st := l.State(); st != nil {
		changes, _ = st.Changes()
	}
	local, games := localSnapshot(changes)
	rc.games = games
	gens := l.links.Gens()

	p.pulls++
	ims := ""
	if !full && !p.forceFull && len(games) == 0 && p.pulls%unconditionalEvery != 0 {
		ims = l.LastModified()
	}
	rc.now = l.Now()
	doc, lm, notModified, err := l.client.EnvironmentDocument(ctx, ims)
	p.lastPull = l.Now()
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusTooManyRequests {
			wait := he.RetryAfter
			if wait <= 0 {
				wait = BackoffMin
			}
			p.notBefore = p.lastPull.Add(wait)
		}
		reason = Reason(err)
		return rc.stats, err
	}
	if notModified {
		result = ResultNotModified
		l.NotePull(rc.now, "")
		return rc.stats, nil
	}
	if !doc.UseIdentityOverridesInLocalEval {
		reason = ReasonIdentityOverridesDisabled
		return rc.stats, ReasonError{Reason: ReasonIdentityOverridesDisabled}
	}
	rc.updated, rc.hasUpd = doc.Updated()
	p.health.setEnvironment(doc.Name)

	d, segments := p.decodeDocument(doc)
	p.markSegments(segments)
	keep := func(k Key) bool {
		g := GameOfIdentity(k.Scope)
		return g == "" || games[g]
	}
	p.health.prune(keep)
	for k := range p.refused {
		if !keep(k) {
			delete(p.refused, k)
		}
	}

	_, attribution, _ := l.RestartReport()
	for _, k := range p.roundKeys(d, local) {
		p.reconcileKey(rc, k, d, local, gens, attribution)
	}

	// Last-Modified (§4.2 step 8): advance only to the document's own update
	// time, never when a key was deferred, and pull in full while the
	// document changed or its second has not passed.
	digest := docDigest(d)
	changed := digest != p.docHash
	p.docHash = digest
	p.forceFull = changed || rc.deferred
	switch {
	case rc.deferred:
		l.NotePull(rc.now, "")
	case rc.hasUpd:
		l.NotePull(rc.now, rc.updated.UTC().Format(http.TimeFormat))
		if hdr, err := http.ParseTime(lm); err == nil && rc.updated.Truncate(time.Second).Before(hdr) {
			p.forceFull = true // the body is older than its header: a stale copy
		}
		if !rc.now.Truncate(time.Second).After(rc.updated.Truncate(time.Second)) {
			p.forceFull = true
		}
	default:
		l.NotePull(rc.now, lm)
	}
	result = ResultOK
	if rc.stats.applied > 0 {
		result = ResultChanged
	}
	return rc.stats, nil
}

// roundKeys is D ∪ LinkState ∪ L, the kill switches first.
func (p *puller) roundKeys(d map[Key]docEntry, local map[Key]localEntry) []Key {
	seen := map[Key]bool{}
	var keys []Key
	add := func(k Key) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range d {
		add(k)
	}
	for _, k := range p.l.links.Keys() {
		add(k)
	}
	for k := range local {
		add(k)
	}
	sortKeys(keys)
	sort.SliceStable(keys, func(i, j int) bool {
		return isKillKey(keys[i]) && !isKillKey(keys[j])
	})
	return keys
}

func isKillKey(k Key) bool {
	return k.Scope == ScopeStack && killRuleOf(k.Set, k.Flag) != nil
}

// reconcileKey merges and acts on one key.
func (p *puller) reconcileKey(rc *roundCtx, k Key, d map[Key]docEntry, local map[Key]localEntry, gens map[Key]uint64, attribution map[Key]Author) {
	l := p.l
	pol := l.Policy(k.Set, k.Flag)
	if pol == nil {
		return
	}
	// Game identities of games not registered: not applied, not refused, not
	// seeded (#2590 R5); they apply once the game registers.
	if g := GameOfIdentity(k.Scope); g != "" && !rc.games[g] {
		return
	}
	// A stale document never reverts a confirmed push (§4.2 step 6).
	if l.links.Gen(k) != gens[k] {
		rc.skip()
		return
	}
	if rc.hasUpd && l.links.ConfirmedAt(k).After(rc.updated) {
		rc.skip()
		return
	}
	// Edge lag: the first round after start skips what the restart rule
	// cleared (#2590 P13).
	if rc.trigger == TriggerStart && attribution[k] == AuthorAPIKey {
		rc.skip()
		return
	}
	if df := p.deferred[k]; df != nil && rc.now.Before(df.until) {
		rc.skip()
		return
	}
	de := d[k]
	le := local[k]
	if rf, ok := p.refused[k]; ok && !rf.d.Equal(de.v) {
		delete(p.refused, k)
	}
	r, hasR := l.links.Remote(k)
	in := mergeIn{R: r, HasR: hasR, D: de.v, L: le.v, Base: l.BaselineFlag(k.Set, k.Flag)}
	if k.Scope == ScopeStack {
		in.Kill = killRuleOf(k.Set, k.Flag)
	}
	if rf, ok := p.refused[k]; ok {
		in.Refused = &rf
	}
	dec := decide(in)
	// The restart rule (BR-006, #2590 P10, R6): without a last state, a value
	// an API key set on an admin setting or on a game identity is not brought
	// back; the local value wins and is sent. Kill switches keep their order
	// (safety over reset).
	if !hasR && in.Kill == nil && dec.action == actApply && attribution[k] == AuthorAPIKey &&
		((k.Scope == ScopeStack && pol.Persist) || GameOfIdentity(k.Scope) != "") {
		dec = decision{action: actKeepLocal}
	}
	if dec.action == actApply && rc.writes >= writeBudget {
		rc.skip()
		return
	}
	if dec.conflict() {
		rc.stats.conflicts++
		ConflictEvent(rc.span, k, describe(le.v), describe(de.v), dec.resolution)
		l.Metrics().RecordConflict(rc.ctx, dec.resolution)
	}
	switch dec.action {
	case actNone:
		l.links.SetInSync(k, true)
	case actSkip:
	case actPush:
		if l.links.Offer(k, le.v, rc.now, le.version) {
			rc.stats.pushed++
		}
	case actAdopt:
		l.links.SetRemote(k, de.v)
		l.links.Offer(k, le.v, rc.now, le.version)
		l.links.ClearRefusal(k)
		p.accepted(k)
	case actKeepLocal:
		l.links.SetRemote(k, de.v)
		if l.links.Offer(k, le.v, rc.now, le.version) {
			rc.stats.pushed++
		}
	case actApply:
		rc.stats.changes++
		p.apply(rc, k, pol, de, le)
	}
}

// describe renders a value for a conflict event (flag values only).
func describe(v Value) string {
	switch {
	case !v.Present:
		return "(none)"
	case v.Variant != "":
		return v.Variant
	}
	return string(v.Value)
}

// accepted forgets k's refusal and deferral and clears its health.
func (p *puller) accepted(k Key) {
	delete(p.refused, k)
	delete(p.deferred, k)
	p.health.accept(k.Flag, k)
}

// markSegments keeps one health entry per synced feature with a segment
// override (§4.2 step 10).
func (p *puller) markSegments(segments map[string]bool) {
	for name, pol := range p.byName {
		k := Key{Set: pol.Set, Flag: pol.Key, Scope: segmentScope}
		if segments[name] {
			p.health.refuse(name, k, segmentReason)
		} else {
			p.health.accept(name, k)
		}
	}
}

// apply writes the document value of k locally (§4.2 step 3). remote is set
// first, so the out side's echo of the write is no change (BR-004).
func (p *puller) apply(rc *roundCtx, k Key, pol *registry.Policy, de docEntry, le localEntry) {
	l := p.l
	if de.err != nil {
		l.links.SetRemote(k, de.v)
		p.refuse(k, de.v, le.v, de.err)
		return
	}
	if isEventKey(pol.Kind, k) {
		p.applyEvent(rc, k, de, le)
		return
	}
	sc, scopeName, ok := scopeOfKey(k, de.v)
	if !ok {
		return
	}
	prev, had := l.links.SetRemote(k, de.v)
	rc.writes++
	var res state.WriteResult
	var ferr *fserr.Error
	w := l.Writer()
	switch {
	case de.v.Present:
		req := state.WriteRequest{Set: k.Set, Flag: k.Flag, Scope: sc, IfSlotVersion: le.slot}
		if de.v.Variant != "" {
			req.Variant = de.v.Variant
		} else {
			req.Value = de.v.Value
		}
		req.Persist = pol.Persist && scopeName == registry.ScopeStack
		res, ferr = w.FlagsmithPut(rc.ctx, req)
	case scopeName == registry.ScopeStack:
		// enabled=false: the baseline applies; the overlay and the admin
		// value go under one lock.
		res, ferr = w.FlagsmithClear(rc.ctx, state.ClearRequest{Set: k.Set, Flag: k.Flag, StackAll: true, IfSlotVersion: le.slot})
	default:
		res, ferr = w.FlagsmithClear(rc.ctx, state.ClearRequest{Set: k.Set, Flag: k.Flag, Scope: sc, IfSlotVersion: le.slot})
	}
	p.result(rc, k, de.v, le.v, prev, had, res, ferr)
}

// scopeOfKey returns the write scope of k. A fraction key always carries a
// percent, so its write can never address the stack.
func scopeOfKey(k Key, v Value) (state.Scope, string, bool) {
	if k.Scope == ScopeStack {
		return state.Scope{}, registry.ScopeStack, true
	}
	sc, name, ok := ScopeOfIdentity(k.Scope)
	if !ok {
		return sc, name, false
	}
	if name == registry.ScopeFraction {
		var fv struct {
			FractionPercent int `json:"fraction_percent"`
		}
		if v.Present {
			_ = json.Unmarshal(v.Value, &fv)
		}
		pct := fv.FractionPercent
		sc.FractionPercent = &pct
	}
	return sc, name, true
}

// applyEvent appends one payload set in Flagsmith (§4.3); the append is
// unconditional (append-only).
func (p *puller) applyEvent(rc *roundCtx, k Key, de docEntry, le localEntry) {
	l := p.l
	if !de.v.Present {
		// The override was deleted in Flagsmith: the list is sent again.
		l.links.SetRemote(k, de.v)
		if l.links.Offer(k, le.v, rc.now, le.version) {
			rc.stats.pushed++
		}
		return
	}
	kind, payload := classifyEvent(de.v, le.v)
	switch kind {
	case evAdopt:
		l.links.SetRemote(k, de.v)
		l.links.Offer(k, le.v, rc.now, le.version)
		p.accepted(k)
		return
	case evStale:
		return
	case evMalformed:
		l.links.SetRemote(k, de.v)
		p.refuse(k, de.v, le.v, fserr.BadRequest("%s: set one event payload (a JSON object) on game:<id>", k.Flag))
		return
	}
	sc, _, _ := ScopeOfIdentity(k.Scope)
	prev, had := l.links.SetRemote(k, de.v)
	rc.writes++
	res, ferr := l.Writer().FlagsmithAppend(rc.ctx, state.AppendRequest{Set: k.Set, Flag: k.Flag, Scope: sc, Payload: payload})
	p.result(rc, k, de.v, le.v, prev, had, res, ferr)
}

// result handles a write's outcome (#2590 plan §3, Results).
func (p *puller) result(rc *roundCtx, k Key, d, lv, prev Value, had bool, res state.WriteResult, ferr *fserr.Error) {
	l := p.l
	if ferr == nil {
		if res.PersistFailed {
			l.Logger().Warn("flagsmith change applied in memory only: the state file could not be written", "flag", k.Flag)
		}
		rc.stats.applied++
		l.links.ClearRefusal(k)
		l.links.SetInSync(k, true)
		p.accepted(k)
		if rc.hasUpd {
			l.Metrics().RecordChangeDelay(rc.ctx, rc.trigger, max(0, rc.now.Sub(rc.updated)))
		}
		return
	}
	switch ferr.Reason {
	case fserr.NoSyncStream, fserr.ReasonPersistFailed, fserr.SlotChanged, fserr.RealGameLive:
		// Not seen: the next round tries again (UC-054 A5).
		l.links.RestoreRemote(k, d, prev, had)
		p.deferKey(rc, k, ferr)
	case fserr.GameNotLive:
		p.refused[k] = refusal{d: d, l: lv}
	default:
		p.refuse(k, d, lv, ferr)
	}
}

// refuse records a refusal (UC-054 A4): remote stays the document value, the
// local value stays, and the feature turns unhealthy.
func (p *puller) refuse(k Key, d, lv Value, ferr *fserr.Error) {
	l := p.l
	p.refused[k] = refusal{d: d, l: lv}
	delete(p.deferred, k)
	l.links.Refuse(k, SideLocal, ferr.Reason)
	l.links.SetInSync(k, false)
	p.health.refuse(k.Flag, k, fmt.Sprintf("%d %s: %s", ferr.Status, ferr.Reason, ferr.Detail))
	l.Logger().Info("flagsmith change refused", "flag", k.Flag, "scope", k.Scope, "reason", ferr.Reason)
}

// deferKey backs k off and marks its feature unhealthy after
// deferralUnhealthyAfter deferrals in a row (#2590 P14).
func (p *puller) deferKey(rc *roundCtx, k Key, ferr *fserr.Error) {
	rc.skip()
	df := p.deferred[k]
	if df == nil {
		df = &deferral{}
		p.deferred[k] = df
	}
	df.n++
	wait := min(BackoffMin<<min(df.n-1, 6), BackoffMax)
	df.until = rc.now.Add(wait)
	if df.n >= deferralUnhealthyAfter {
		p.health.refuse(k.Flag, k, fmt.Sprintf("deferred %d times: %d %s: %s", df.n, ferr.Status, ferr.Reason, ferr.Detail))
	}
}
