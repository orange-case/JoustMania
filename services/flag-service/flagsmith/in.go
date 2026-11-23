package flagsmith

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
	// pushGuardMin is the shortest life of a push guard (§4.2 step 6): a
	// document that still shows the value a confirmed push replaced is
	// treated as stale for max(pushGuardMin, pushGuardPolls x poll interval).
	pushGuardMin   = 10 * time.Minute
	pushGuardPolls = 10
	// staleAcceptMin and staleAcceptPolls bound the older-document refusal
	// (§4.2 step 6): once consecutive stale_document refusals have lasted
	// max(staleAcceptMin, staleAcceptPolls x poll interval) of local elapsed
	// time, the document is accepted and becomes the newest update time, so
	// one update time ahead of all later ones (server clock skew, a database
	// restore, a garbled but in-range value) cannot wedge the in side.
	staleAcceptMin   = 10 * time.Minute
	staleAcceptPolls = 10
	// maxAbandoned is how many given-up newest times are remembered (#2602).
	maxAbandoned = 8
	// sameToEnd: documents stamped exactly the newest time, read on this
	// many full pulls in a row, end a run of stale_document refusals (#2602).
	sameToEnd = 2
	// ReasonStaleDocument is the pull reason of a document older than one
	// this process already read (a cache or region serving an older copy):
	// the round is skipped (telemetry.md §10.4).
	ReasonStaleDocument = "stale_document"
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
	// reverting holds the refused stack value a write in progress reverts
	// (UC-055 A4); it is set and cleared around that one write (#2631 N1).
	reverting map[Key]Value
	// forceFull: the next pull is unconditional.
	forceFull bool
	docHash   [32]byte
	pulls     int
	lastPull  time.Time
	notBefore time.Time
	health    *health
	after     func(time.Duration) <-chan time.Time
	// seen is every key's remote value, generation and confirmation time
	// at the end of the last round: a change of them between two rounds is
	// a push the out side confirmed (or a remote value it forgot).
	seen map[Key]remoteMark
	// guards holds, per key, the remote values a confirmed push replaced
	// (#2590 P1, value guard): a document that still shows one of them is
	// stale and is not applied.
	guards map[Key]*pushGuard
	// lastDoc is every key's value in the last document a round read (nil
	// before one): a push confirmed between two rounds replaced it too, also
	// when the remote value was unknown (a deferred first apply).
	lastDoc map[Key]Value
	// newest is the newest document update time this process read (server
	// clock only): an older document is a stale copy and is not applied.
	newest time.Time
	// staleRun: the last documents read were all refused as older, the first
	// of them at staleSince (local clock, compared with the local clock only:
	// time.Now carries a monotonic reading, so this is elapsed time).
	staleRun   bool
	staleSince time.Time
	// sameRead counts the documents stamped exactly the newest time read in
	// a row during a run (#2602): sameToEnd of them end it, so a genuine
	// newest document ends the run and a too-new copy served now and then
	// between refusals does not.
	sameRead int
	// abandoned are the newest times given up when an older document was
	// accepted (the last maxAbandoned, #2602): a document stamped exactly one
	// of them is the too-new copy again and is refused as stale.
	abandoned []time.Time
	// read are the distinct newest times this process read, the latest
	// maxAbandoned (ascending): a heal abandons every one after the accepted
	// time, so an earlier too-new copy cannot undo it either (#2602). Refused
	// times are not kept: during a run they are mostly Flagsmith's genuine
	// changes, which a heal must not abandon.
	read []time.Time
}

// remoteMark is one key's remote state as LinkState holds it.
type remoteMark struct {
	v    Value // Value{} when no remote value is known
	gen  uint64
	conf time.Time
}

// pushGuard is a confirmed push of a key: the remote values it replaced and
// when the in side saw it (local clock, compared with the local clock only).
type pushGuard struct {
	replaced []Value
	at       time.Time
}

func (g *pushGuard) covers(v Value) bool {
	for _, r := range g.replaced {
		if r.Equal(v) {
			return true
		}
	}
	return false
}

// deferral is a key whose apply was put off (flagd not connected, a stale
// slot, a persist failure, a live real game): it backs off 1, 2, 4 ... 60 s.
type deferral struct {
	n     int
	until time.Time
}

func newIn(l *Link) inSide {
	p := &puller{
		l:         l,
		byName:    map[string]*registry.Policy{},
		refused:   map[Key]refusal{},
		deferred:  map[Key]*deferral{},
		reverting: map[Key]Value{},
		after:     time.After,
		seen:      map[Key]remoteMark{},
		guards:    map[Key]*pushGuard{},
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
	p.health.resume()
	rs, err := p.round(ctx, trigger, true)
	span.SetAttributes(
		attribute.Int(AttrApplied, rs.applied),
		attribute.Int(AttrPushed, rs.pushed),
		attribute.Int(AttrConflicts, rs.conflicts),
	)
	if err != nil {
		// The link goes offline: the health posts stop with it (the queue
		// is kept for the next Reconcile or Poll).
		p.health.stop()
		EndSpan(span, ResultError, Reason(err))
		return err
	}
	EndSpan(span, ResultOK, "")
	return nil
}

// Poll pulls every FLAGSMITH_POLL_SECONDS and on every Wake until ctx ends or
// the link goes down. The realtime reader and the health posts have returned
// before Poll does.
func (p *puller) Poll(ctx context.Context) error {
	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	p.health.resume()
	defer func() {
		cancel()
		wg.Wait()
		p.health.stop()
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
//
// Player values (#2624): an override on controller:<serial> is read as the
// player value of the controller's owner, and only while the controller is
// settled for that owner (rc.ctrl); with no owner it is ignored. A
// game:<id>:<serial> identity is read only for a live shadow game, so a
// player value never comes from two identities.
func (p *puller) decodeDocument(doc *EnvironmentDocument, rc *roundCtx) (map[Key]docEntry, map[string]bool) {
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
		scope, scopeName, ok := rc.scopeOfIdentity(ov.Identifier)
		if !ok {
			continue
		}
		for _, fs := range ov.IdentityFeatures {
			pol := p.byName[fs.Feature.Name]
			if pol == nil {
				continue
			}
			k := Key{Set: pol.Set, Flag: pol.Key, Scope: scope}
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
	// Variant options (#2709, §2.3): one option at 100 % is the value, any
	// other split is refused. The refused value carries the weights, so a
	// later pick of the same value is a change.
	picked, ok, splitErr := pickedValue(pol, fs.MultivariateValues)
	if splitErr != nil {
		val := compact(fs.FeatureStateValue)
		if len(bytes.TrimSpace(val)) == 0 {
			val = json.RawMessage("null")
		}
		type weight struct {
			Option int     `json:"option"`
			Weight float64 `json:"weight"`
		}
		split := make([]weight, 0, len(fs.MultivariateValues))
		for _, mv := range fs.MultivariateValues {
			split = append(split, weight{mv.Option.ID, mv.PercentageAllocation})
		}
		raw, _ := json.Marshal(struct {
			Split []weight        `json:"percentage_split"`
			Value json.RawMessage `json:"value"`
		}{split, val})
		e.v = Value{Present: true, Value: raw}
		e.err = splitErr
		return e
	}
	if ok {
		// The picked option is a declared variant by its title (#2713); one
		// that is none is read as its value, checked like any value.
		if v := p.l.pickedVariant(pol, picked); v != "" {
			fs.FeatureStateValue, _ = json.Marshal(v)
		} else {
			fs.FeatureStateValue = picked.Option.Value
		}
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
	ctx     context.Context
	span    trace.Span
	trigger string
	now     time.Time
	updated time.Time
	hasUpd  bool
	games   map[string]bool
	// ctrl and kinds are the controller map at the round's start (#2624):
	// each controller's owner and whether the pull may read it, each live
	// game's kind.
	ctrl     map[string]ctrlView
	kinds    map[string]string
	doc      map[Key]docEntry // the decoded document (nil before it is read)
	writes   int
	deferred bool
	stats    roundStats
	// read is the snapshot version of the local state the round read.
	read uint64

	// queued: the round queued at least one local value to send (offer);
	// the round wakes the out side once at its end (#2608).
	queued bool
}

// skip leaves a key for the next round, which pulls in full.
func (rc *roundCtx) skip() { rc.deferred = true }

// round is one pull and reconcile. full: no If-Modified-Since.
func (p *puller) round(ctx context.Context, trigger string, full bool) (roundStats, error) {
	l := p.l
	ctx, span := StartPull(ctx, l.Tracer(), trigger)
	result, reason := ResultError, ""
	rc := &roundCtx{ctx: ctx, span: span, trigger: trigger}
	var start map[Key]remoteMark
	defer func() {
		p.guardPushesDuring(rc, start)
		span.SetAttributes(attribute.Int(AttrChanges, rc.stats.changes), attribute.Int(AttrConflicts, rc.stats.conflicts))
		EndSpan(span, result, reason)
		l.Metrics().RecordPull(ctx, trigger, result)
	}()

	// The local snapshot and the remote state are read before the fetch: a
	// local write after it is caught by IfSlotVersion, a push confirmed
	// during it by the generation and a push guard (a stale document).
	var changes []state.Change
	if st := l.State(); st != nil {
		changes, rc.read = st.Changes()
	}
	local, games := localSnapshot(changes)
	rc.games = games
	rc.ctrl, rc.kinds = l.ctrl.view()
	rc.now = l.Now()
	start = p.remoteMarks()
	p.guardPushesBetween(start, rc.now)

	p.pulls++
	ims := ""
	force := l.fullPull.Swap(false)
	if !full && !force && !p.forceFull && len(games) == 0 && p.pulls%unconditionalEvery != 0 {
		ims = l.LastModified()
	}
	doc, lm, notModified, err := l.client.EnvironmentDocument(ctx, ims)
	if err == nil {
		l.connected.Store(true) // local changes from now on are made online
	}
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
	rc.updated, rc.hasUpd = documentUpdated(doc)
	// A document older than one already read is a stale copy (a second
	// cache or region): applying it could revert a push it predates, so the
	// round is skipped, Last-Modified kept and the next pull is in full.
	// After max(10 min, 10 polls) of refusals in a row (local elapsed time)
	// the document is accepted: the newest time itself may be wrong. The
	// newest time it gave up is abandoned: a copy stamped with it is refused
	// from then on, never accepted by the timeout, so it cannot undo the
	// accepted document (#2602). It neither starts nor ends a run.
	if rc.hasUpd && p.isAbandoned(rc.updated) {
		p.sameRead = 0
		p.forceFull = true
		l.NotePull(rc.now, "")
		reason = ReasonStaleDocument
		return rc.stats, nil
	}
	if rc.hasUpd && rc.updated.Round(time.Microsecond).Before(p.newest.Round(time.Microsecond)) {
		if !p.staleRun {
			p.staleRun, p.staleSince = true, rc.now
		}
		p.sameRead = 0
		refusing := rc.now.Sub(p.staleSince)
		if refusing < p.staleAcceptAfter() {
			p.forceFull = true
			l.NotePull(rc.now, "")
			reason = ReasonStaleDocument
			return rc.stats, nil
		}

		p.abandonAfter(rc.updated)
		l.Logger().Warn("flagsmith stale document accepted",
			"updated_at", rc.updated.UTC().Format(time.RFC3339Nano),
			"newest", p.newest.UTC().Format(time.RFC3339Nano),
			"refused_for", refusing.String(),
			"abandoned", len(p.abandoned))
	}
	// A run ends with a strictly newer document, a document without an update
	// time, or the newest time read on sameToEnd full pulls in a row: a copy
	// stamped with the newest time served now and then between refusals (a
	// cache still holding a too-new time) is applied but does not end it
	// (#2602).
	switch {
	case !p.staleRun:
	case rc.hasUpd && sameTime(rc.updated, p.newest):
		p.sameRead++
		if p.sameRead >= sameToEnd {
			p.staleRun, p.sameRead = false, 0
		}
	default:
		p.staleRun, p.sameRead = false, 0
	}
	if rc.hasUpd {
		p.noteRead(rc.updated)
		p.newest = rc.updated
	}
	p.health.setEnvironment(doc.Name)

	d, segments := p.decodeDocument(doc, rc)
	rc.doc = d
	p.markSegments(segments)
	keys := p.roundKeys(d, local)
	p.prune(keys, games)

	for _, k := range keys {
		p.reconcileKey(rc, k, d, local, start)
	}
	// A value the round queued is sent now, not with the next local write
	// (#2608): one wake per round, none when every offer was held back (a
	// refused value, an existing pending entry).
	if rc.queued {
		l.wakeOut()
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
	if p.staleRun {
		// Every pull of a run is in full (#2602): a 304 is no read, so the
		// newest document read in a row must come with a body.
		p.forceFull = true
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

// prune forgets the refusals, deferrals, push guards and health entries of
// keys that left the round (a game that ended or is not registered, a
// forgotten identity, an override deleted on both sides): their feature turns
// healthy when nothing else of it is refused (P7).
func (p *puller) prune(keys []Key, games map[string]bool) {
	inRound := make(map[Key]bool, len(keys))
	for _, k := range keys {
		inRound[k] = true
	}
	keep := func(k Key) bool {
		if k.Scope == segmentScope {
			return true
		}
		g := GameOfIdentity(k.Scope)
		return inRound[k] && (g == "" || games[g])
	}
	p.health.prune(keep)
	for k := range p.refused {
		if !keep(k) {
			delete(p.refused, k)
		}
	}
	for k := range p.deferred {
		if !keep(k) {
			delete(p.deferred, k)
		}
	}
	for k := range p.guards {
		if !keep(k) {
			delete(p.guards, k)
		}
	}
}

// documentUpdated is the document's update time: the body updated_at
// (ISO 8601), else the X-Flagsmith-Document-Updated-At header
// (doc.HeaderUpdatedAt), a float Unix epoch in seconds with a fraction
// ("1731693731.123456"); any other header format is no update time.
//
// A time that is not a finite epoch after 1970 and before year 10000 counts
// as no update time, so a garbled value never pins the newest time and
// refuses every later document. No local clock is involved (a Pi without RTC
// may run at 1970 before NTP).
func documentUpdated(doc *EnvironmentDocument) (time.Time, bool) {
	if t, ok := doc.Updated(); ok {
		if u := t.Unix(); u <= 0 || u > maxEpoch {
			return time.Time{}, false
		}
		return t, true
	}
	header := strings.TrimSpace(doc.HeaderUpdatedAt)
	if header == "" {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(header, 64)
	// The range check before the conversion keeps float-to-int64 defined.
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > maxEpoch {
		return time.Time{}, false
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
}

// maxEpoch is the last second of year 9999 as a Unix epoch.
const maxEpoch = 253402300799

// remoteMarks reads every key's remote value, generation and confirmation
// time, each key consistently (re-read while its generation moves).
func (p *puller) remoteMarks() map[Key]remoteMark {
	ls := p.l.links
	keys := ls.Keys()
	out := make(map[Key]remoteMark, len(keys))
	for _, k := range keys {
		for range 3 {
			gen := ls.Gen(k)
			conf := ls.ConfirmedAt(k)
			v, _ := ls.Remote(k)
			out[k] = remoteMark{v: v, gen: gen, conf: conf}
			if ls.Gen(k) == gen {
				break
			}
		}
	}
	return out
}

// guardPushesBetween guards every key whose remote state changed since the
// last round: only the out side changes it between rounds (a confirmed push,
// a forgotten remote value), so the value it replaced is stale from then on.
// The replaced value is the remote value at the end of the last round and the
// value the last document showed: the remote value is unknown after a
// deferred first apply, and that document may still be served (#2596).
func (p *puller) guardPushesBetween(start map[Key]remoteMark, now time.Time) {
	for k, m := range start {
		prev, ok := p.seen[k]
		if !ok && m.conf.IsZero() {
			continue // a key the out side only queued
		}
		if ok && m.gen == prev.gen {
			continue
		}
		p.addGuard(k, prev.v, m.v, now)
		if p.lastDoc != nil {
			p.addGuard(k, p.lastDoc[k], m.v, now)
		}
	}
	for k := range p.guards {
		if _, ok := start[k]; !ok {
			delete(p.guards, k) // forgotten (game end, dropped identity)
		}
	}
}

// guardPushesDuring guards every key whose push was confirmed while the round
// ran: the value it replaced is the remote value at the start of the round or
// the document value the round set. It then records the remote state and the
// document's values for the next round.
func (p *puller) guardPushesDuring(rc *roundCtx, start map[Key]remoteMark) {
	end := p.remoteMarks()
	for k, m := range end {
		s := start[k]
		if m.conf.Equal(s.conf) {
			continue
		}
		p.addGuard(k, s.v, m.v, rc.now)
		if rc.doc != nil {
			p.addGuard(k, rc.doc[k].v, m.v, rc.now)
		} else if p.lastDoc != nil {
			// The round read no document (an error, a stale document, 304):
			// the next one may still be the last document read, whose value
			// the push replaced too (a deferred first apply leaves s.v absent).
			p.addGuard(k, p.lastDoc[k], m.v, rc.now)
		}
	}
	p.seen = end
	if rc.doc != nil {
		p.lastDoc = make(map[Key]Value, len(rc.doc))
		for k, de := range rc.doc {
			p.lastDoc[k] = de.v
		}
	}
}

// addGuard records that a confirmed push replaced v by now (nothing when the
// push did not change the value).
func (p *puller) addGuard(k Key, v, now Value, at time.Time) {
	if v.Equal(now) {
		return
	}
	g := p.guards[k]
	if g == nil {
		g = &pushGuard{}
		p.guards[k] = g
	}
	g.at = at
	if !g.covers(v) {
		g.replaced = append(g.replaced, v)
	}
}

// guardTTL is how long a push guard lives: max(10 min, 10 poll intervals).
func (p *puller) guardTTL() time.Duration {
	interval := p.l.cfg.Poll
	if interval <= 0 {
		interval = DefaultPoll
	}
	return max(pushGuardMin, pushGuardPolls*interval)
}

// staleAcceptAfter is how long older documents are refused in a row before
// one is accepted: max(10 min, 10 poll intervals).
func (p *puller) staleAcceptAfter() time.Duration {
	interval := p.l.cfg.Poll
	if interval <= 0 {
		interval = DefaultPoll
	}
	return max(staleAcceptMin, staleAcceptPolls*interval)
}

// isAbandoned: t is a newest time given up when an older document was
// accepted (#2602).
func (p *puller) isAbandoned(t time.Time) bool {
	for _, a := range p.abandoned {
		if sameTime(a, t) {
			return true
		}
	}
	return false
}

// sameTime compares two update times to the microsecond: the header fallback
// is a float epoch, a few hundred ns off the body time of the same document.
func sameTime(a, b time.Time) bool {
	return a.Round(time.Microsecond).Equal(b.Round(time.Microsecond))
}

// noteRead records a newest time read in read, kept ascending and distinct;
// beyond maxAbandoned the earliest is dropped (#2602).
func (p *puller) noteRead(t time.Time) {
	i := sort.Search(len(p.read), func(i int) bool {
		return !p.read[i].Round(time.Microsecond).Before(t.Round(time.Microsecond))
	})
	if i < len(p.read) && sameTime(p.read[i], t) {
		return
	}
	p.read = append(p.read, time.Time{})
	copy(p.read[i+1:], p.read[i:])
	p.read[i] = t
	if len(p.read) > maxAbandoned {
		p.read = p.read[1:]
	}
}

// abandonAfter gives up, at a heal that accepts a document stamped accepted,
// the newest time and every newest time read after accepted (#2602).
func (p *puller) abandonAfter(accepted time.Time) {
	p.abandon(p.newest)
	kept := p.read[:0]
	for _, t := range p.read {
		if t.Round(time.Microsecond).After(accepted.Round(time.Microsecond)) {
			p.abandon(t)
			continue
		}
		kept = append(kept, t)
	}
	p.read = kept
}

// abandon remembers a given-up newest time, the last maxAbandoned of them.
func (p *puller) abandon(t time.Time) {
	if t.IsZero() || p.isAbandoned(t) {
		return
	}
	p.abandoned = append(p.abandoned, t)
	if len(p.abandoned) > maxAbandoned {
		p.abandoned = p.abandoned[1:]
	}
}

// reconcileKey merges and acts on one key.
func (p *puller) reconcileKey(rc *roundCtx, k Key, d map[Key]docEntry, local map[Key]localEntry, start map[Key]remoteMark) {
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
	if p.controllerGate(rc, k) {
		return
	}
	// A stale document never reverts a confirmed push (§4.2 step 6): a push
	// confirmed while the document was in flight skips the key this round.
	if l.links.Gen(k) != start[k].gen {
		rc.skip()
		return
	}
	de := d[k]
	r, hasR := l.links.Remote(k)
	// After a confirmed push, a document that still shows a value the push
	// replaced is stale (value guard, no clock comparison): skipped until the
	// document shows the pushed value or the guard expires.
	if g := p.guards[k]; g != nil {
		switch {
		case r.Equal(de.v) || rc.now.Sub(g.at) > p.guardTTL():
			delete(p.guards, k)
		case g.covers(de.v):
			rc.skip()
			return
		}
	}
	// A controller's former game's override in a lagging document (#2624).
	if p.removedGuard(rc, k, pol, de.v, r) {
		return
	}
	if df := p.deferred[k]; df != nil && rc.now.Before(df.until) {
		rc.skip()
		return
	}
	le, ok := local[k]
	if !ok {
		// An empty game, game:serial or fraction slot is not in the snapshot:
		// its version still makes the write conditional (#2590 P2).
		le.slot = p.slotVersion(k, de.v)
	}
	if rf, ok := p.refused[k]; ok && !rf.d.Equal(de.v) {
		delete(p.refused, k)
	}
	in := mergeIn{R: r, HasR: hasR, D: de.v, L: le.v}
	// Flagsmith is the source of truth once online (UC-054 A2, UC-055 A4,
	// #2621). A stack-scope local change made while the link was not
	// connected (before the first connect, or while offline) is discarded:
	// the out side never sends it (LinkState.Next) and this round applies
	// Flagsmith's value, or the baseline. The mark is on the pending entry, so
	// it holds across a skip or deferral and ends with the next change made
	// online. A stack value Flagsmith refused is reverted the same way, and
	// stays held, so a re-save is not sent again. Game and player values are
	// live game state with no Flagsmith counterpart: they are sent.
	revert := false
	if k.Scope == ScopeStack {
		if l.links.PendingOffline(k) {
			in.Discard = true
		} else if rv, ok := l.links.PushRefused(k); ok && rv.Equal(le.v) && !l.links.HasPending(k) {
			in.Discard, revert = true, true
		}
	}
	if rf, ok := p.refused[k]; ok {
		in.Refused = &rf
	}
	dec := decide(in)
	if in.Discard {
		switch dec.action {
		case actApply:
			if revert {
				// A refusal is reported as one (state, span, metric), not
				// as a conflict.
				dec.resolution = ""
			}
			if de.err != nil {
				// Flagsmith's value is refused here: the offline change
				// stays in force but is never sent.
				l.links.DropOffline(k, le.version)
			}
		case actNone, actAdopt, actSkip:
			l.links.DropOffline(k, le.version)
		}
	}
	// Events are one-shot payloads, not values (architecture note D3): without
	// a last state (after a restart) an event list in the document is the
	// list the link pushed before the restart. It is not brought back; the
	// local list is kept and sent. A single payload a person set is applied.
	if !hasR && dec.action == actApply && isEventKey(pol.Kind, k) && isEventList(de.v) {
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
		l.links.SetInSyncRead(k, rc.read)
		p.accepted(k)
	case actSkip:
	case actPush:
		if p.offer(rc, k, le) {
			rc.stats.pushed++
		}
		p.accepted(k)
	case actAdopt:
		l.links.SetRemoteRead(k, de.v, rc.read)
		p.offer(rc, k, le)
		l.links.ClearRefusalRead(k, rc.read)
		p.accepted(k)
	case actKeepLocal:
		l.links.SetRemoteRead(k, de.v, rc.read)
		if p.offer(rc, k, le) {
			rc.stats.pushed++
		}
		p.accepted(k)
	case actApply:
		rc.stats.changes++
		// The refused value is marked only for this write (#2631 N1): a revert
		// skipped by the write budget or refused here leaves no mark behind
		// for a later, ordinary Flagsmith change to hold.
		if revert {
			p.reverting[k] = le.v
			l.links.BeginRevert(k)
		}
		p.apply(rc, k, pol, de, le)
		if revert {
			l.links.EndRevert(k)
		}
		delete(p.reverting, k)
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

// accepted ends k's refusal and deferral and clears its health: the
// feature's health follows its set of refused keys (P7).
func (p *puller) accepted(k Key) {
	delete(p.refused, k)
	delete(p.deferred, k)
	p.l.links.ClearLocalRefusal(k)
	p.health.accept(k.Flag, k)
}

// slotVersion reads the current version of k's slot (0: no slot).
func (p *puller) slotVersion(k Key, v Value) uint64 {
	st := p.l.State()
	if st == nil {
		return 0
	}
	sc, _, ok := scopeOfKey(k, v)
	if !ok {
		return 0
	}
	return st.SlotVersion(k.Set, k.Flag, sc)
}

// offer queues the local value read in the snapshot for the out side, only
// while its slot has not changed since: a newer local write was offered by
// the observer already and must not be replaced by the older value (#2596
// finding 6). Reoffer never re-queues a value Flagsmith refused (UC-055 A4,
// #2604) nor replaces a pending entry.
func (p *puller) offer(rc *roundCtx, k Key, le localEntry) bool {
	if le.slot != 0 && p.slotVersion(k, le.v) != le.slot {
		return false
	}
	// The slot check above and ReofferRead hold no common lock; read closes
	// the gap between them.
	if !p.l.links.ReofferRead(k, le.v, rc.now, le.version, rc.read) {
		return false
	}
	rc.queued = true
	return true
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
	prev, had, gen := l.links.SetRemoteGen(k, de.v)
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
	p.result(rc, k, de.v, le.v, undo{prev: prev, had: had, gen: gen}, res, ferr)
}

// undo is the remote value a pulled write replaced, for an exact restore.
type undo struct {
	prev Value
	had  bool
	gen  uint64
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
		if p.offer(rc, k, le) {
			rc.stats.pushed++
		}
		p.accepted(k)
		return
	}
	kind, payload := classifyEvent(de.v, le.v)
	switch kind {
	case evAdopt:
		l.links.SetRemote(k, de.v)
		p.offer(rc, k, le)
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
	prev, had, gen := l.links.SetRemoteGen(k, de.v)
	rc.writes++
	res, ferr := l.Writer().FlagsmithAppend(rc.ctx, state.AppendRequest{Set: k.Set, Flag: k.Flag, Scope: sc, Payload: payload})
	p.result(rc, k, de.v, le.v, undo{prev: prev, had: had, gen: gen}, res, ferr)
}

// result handles a write's outcome (#2590 plan §3, Results).
func (p *puller) result(rc *roundCtx, k Key, d, lv Value, u undo, res state.WriteResult, ferr *fserr.Error) {
	l := p.l
	if ferr == nil {
		if res.PersistFailed {
			l.Logger().Warn("flagsmith change applied in memory only: the state file could not be written", "flag", k.Flag)
		}
		rc.stats.applied++
		// A local write after this one (its snapshot version) keeps the state
		// it left.
		read := res.SnapshotVersion
		if read == 0 {
			read = math.MaxUint64
		}
		if _, ok := p.reverting[k]; ok {
			// A refused stack value reverted (UC-055 A4): the refusal stays
			// reported and the value held (BeginRevert), so a re-save is not
			// sent again.
			delete(p.reverting, k)
		} else {
			l.links.ClearRefusalRead(k, read)
		}
		l.links.SetInSyncRead(k, read)
		p.accepted(k)
		if rc.hasUpd {
			l.Metrics().RecordChangeDelay(rc.ctx, rc.trigger, max(0, rc.now.Sub(rc.updated)))
		}
		return
	}
	delete(p.reverting, k)
	switch ferr.Reason {
	case fserr.NoSyncStream, fserr.ReasonPersistFailed, fserr.SlotChanged, fserr.RealGameLive:
		// Not seen: the next round tries again (UC-054 A5).
		l.links.RestoreRemoteGen(k, u.gen, u.prev, u.had)
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
