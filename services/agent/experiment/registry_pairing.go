package experiment

// registry_pairing.go holds the Registry's PAIRING phase (#2635 split of registry.go):
// capacity allocation, the paired-arm plan and reservation, the spawn of both arms
// and the binding of a spawned game to its (experiment, arm).

import (
	"context"
	"errors"
	"hash/fnv"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/experiment/journal"
)

// AllocateAndSpawn fills FREE shadow capacity across the live experiments by
// equal-share / round-robin (epic #982 decision 3), spawning at most one game per
// experiment per pass until the EFFECTIVE concurrency cap is reached or no live
// experiment wants another game. It is the registry's capacity scheduler: call it
// whenever capacity may have freed (a game concluded, an experiment started). It
// returns the number of games spawned this call.
//
// Fairness: the live experiments are scanned starting from a rotating cursor and
// each gets at most one new game per pass, so K experiments share the cap evenly
// and none starves. The effective cap (#998) bounds in-flight to the
// coordinator's REAL concurrency so the registry never over-spawns doomed starts.
//
// Backpressure (#998): the game-coordinator runs ONE game at a time and rejects a
// concurrent start with "Game already in progress". That is NOT a failure — the
// spawner returns it wrapped as ErrSpawnBackpressure. AllocateAndSpawn releases
// the reservation cleanly (no slot leak), logs at DEBUG (not WARN), and stops
// this pass so the start is retried on the next tick. Only a GENUINE spawn error
// still WARNs as experiment.spawn_failed.
func (r *Registry) AllocateAndSpawn(ctx context.Context) int {
	if r.spawner == nil {
		return 0
	}
	// UC-043 A17 step 2: a running hold-flag experiment whose condition flipped ends.
	r.discardHoldStale(ctx)
	// UC-046 step 2: pin the archetype selection of experiments that have not spawned yet.
	r.pinPending(ctx)
	// UC-046 A6 / BR-015: conclude pattern experiments whose archetypes are all used
	// or skipped, also when the last arms failed at spawn.
	defer r.concludeExhaustedPatterns(ctx)
	spawned := 0
	for {
		// Each iteration is one round-robin PASS: pick the next live experiment (from
		// the rotating cursor) with capacity and reserve the next arm(s) of its
		// current Common-Random-Numbers pair (#1003). When the effective cap admits
		// two slots a whole (experimental, control) pair is reserved atomically; when
		// it admits one (the single-game coordinator) one arm is reserved now and its
		// seed-matched sibling follows on a later tick. Re-evaluate capacity each pick.
		plan, ok := r.nextPair()
		if !ok {
			return spawned
		}
		// ATOMIC for the slots reserved THIS tick (#1003): spawn the reserved arm(s)
		// with the shared seed. If a multi-arm reservation's second spawn fails (or
		// hits backpressure), spawnPair rolls the WHOLE same-tick group back — including
		// a sibling that already started — so a half-pair never enters the cohort
		// statistics from a single tick. (A pair split across ticks under effective
		// cap 1 is completed by the verdict's pair_index matching in #1004.)
		boundThisPair, stop := r.spawnPair(ctx, plan)
		spawned += boundThisPair
		if stop {
			return spawned
		}
	}
}

// pairPlan is the result of reserving the next arm(s) of one experiment's current
// CRN pair: the experiment, the shared per-pair seed and pair index, and the arm(s)
// reserved THIS tick (one or both, depending on the free effective budget). Their
// reservation slots are already held; spawnPair turns them into real games or rolls
// the same-tick group back.
type pairPlan struct {
	id        string
	seed      uint64
	pairIndex int
	arms      []string // arm(s) reserved this tick, in reservation order
	// pattern is set for a pattern experiment's arm (UC-046 step 3).
	pattern *PatternSpawn
}

// startedArm is one arm of a CRN pair that successfully spawned a live game. It is
// tracked during spawnPair so a later-arm failure can force-release the live
// sibling(s) and keep the pair atomic.
type startedArm struct {
	arm    string
	gameID string
}

// spawnPair spawns BOTH arms of a reserved CRN pair with the shared seed and binds
// them. It returns how many arms it genuinely bound (a live in-flight game) and
// whether AllocateAndSpawn should STOP this pass (backpressure or a genuine spawn
// failure). On any spawn error it releases EVERY still-held reservation for the
// pair — including a sibling arm that already spawned a live game — so a half-pair
// never enters the cohort statistics (the atomic-pair invariant).
func (r *Registry) spawnPair(ctx context.Context, plan pairPlan) (bound int, stop bool) {
	// Spawn-traceparent propagation (#1182, epic #1181): carry this experiment's
	// long-lived ROOT span context on the spawn ctx as a REMOTE parent. The
	// spawner reads it off the ctx and (for an experiment-bound shadow game)
	// injects it as the outgoing traceparent so the coordinator's game span
	// becomes a CHILD of the experiment span. When the experiment has no valid
	// root span (tracer unwired / torn down) the ctx is unchanged, so nothing is
	// injected and the game stays own-rooted — graceful, exactly as before.
	spawnCtx := ctx
	if sc, ok := r.ExperimentSpanContext(plan.id); ok {
		spawnCtx = trace.ContextWithRemoteSpanContext(ctx, sc)
	}
	if plan.pattern != nil {
		if r.flagDigest != nil {
			plan.pattern.FlagDigest = r.flagDigest(ctx, plan.id, plan.arms[0])
		}
		spawnCtx = WithPatternSpawn(spawnCtx, *plan.pattern)
	}
	if err := r.reassertArmValue(ctx, plan.id); err != nil {
		r.rollbackPair(plan, 0, nil)
		// #2362: a teardown began before or during the re-assert; it owns the
		// experiment's end, so no discard and no retry here.
		if errors.Is(err, errReassertTornDown) {
			r.log.Debug("experiment.arm_value_reassert_torn_down", "experiment_id", plan.id, "error", err)
			return 0, true
		}
		// UC-043 A10 step 3 (#2366): the gate refuses the flag as not experimentable
		// (it left the experiment catalog, e.g. an experiment journaled before an
		// upgrade and rehydrated after it). No retry can succeed, so end it discarded
		// (journaled), which frees its slot and its flag; other errors retry next tick.
		var rej *RejectError
		if errors.As(err, &rej) && rej.Reason == ReasonNotExperimentable {
			r.log.Warn("experiment.arm_value_not_experimentable", "experiment_id", plan.id, "error", err)
			r.teardown(ctx, plan.id, r.flagKeyOf(plan.id), StatusDiscarded, string(ReasonNotExperimentable)+": "+rej.Detail)
			return 0, true
		}
		r.log.Warn("experiment.arm_value_reassert_failed", "experiment_id", plan.id, "error", err)
		return 0, true
	}
	var live []startedArm
	for i, arm := range plan.arms {
		gameID, err := r.spawner.Spawn(spawnCtx, plan.id, arm, plan.seed)
		if err != nil {
			// Roll the WHOLE pair back atomically (#1003): release the failed arm and
			// every arm AFTER it (whose Spawn was never attempted, still reserved), and
			// force-release any earlier arm that already became a live game — so no
			// half-pair survives into the cohort statistics. All cap slots return.
			r.rollbackPair(plan, i, live)
			if errors.Is(err, ErrSpawnBackpressure) {
				r.log.Debug("experiment.spawn_backpressure",
					"experiment_id", plan.id, "arm", arm, "seed", plan.seed, "error", err)
				return 0, true
			}
			r.log.Warn("experiment.spawn_failed",
				"experiment_id", plan.id, "arm", arm, "seed", plan.seed, "error", err)
			return 0, true
		}
		if r.bindGame(plan.id, gameID, arm, plan.pairIndex, plan.pattern) {
			live = append(live, startedArm{arm: arm, gameID: gameID})
		}
		// If bindGame did NOT bind (the #1014 bind-vs-fast-terminal race freed the
		// reservation instead) the slot is already back in the cap; the sibling arm
		// still spawns so the pair completes if it can.
	}
	return len(live), false
}

// reassertArmValue makes sure the experiment's arm value is still served before
// a pair spawns: a flag-service restart drops it (the targeting writer compares
// boot epochs and writes it again). A targeting writer without the capability,
// or an experiment without a flag, needs nothing.
//
// The write runs outside r.mu. A teardown (Abort / AbortAll: kill switch,
// experiments_enabled off, shutdown) that begins while it is in flight may strip
// before the write lands, so after the write (success or error) a begun teardown
// gets a compensating strip and errReassertTornDown is returned (#2362).
func (r *Registry) reassertArmValue(ctx context.Context, id string) error {
	ra, ok := r.target.(interface {
		Reassert(ctx context.Context, p Proposal) error
	})
	if !ok {
		return nil
	}
	r.mu.Lock()
	e := r.entries[id]
	if e == nil {
		r.mu.Unlock()
		return nil
	}
	if e.teardownBegun || e.status.IsTerminal() {
		r.mu.Unlock()
		return errReassertTornDown // the teardown owns the strip; write nothing
	}
	intent := e.journal.Intent()
	r.mu.Unlock()
	if intent.FlagKey == "" {
		return nil
	}
	writeErr := ra.Reassert(ctx, Proposal{
		FlagKey:           intent.FlagKey,
		ExperimentID:      id,
		ExperimentalValue: intent.ExperimentalValue,
		Rationale:         intent.Hypothesis,
	})

	r.mu.Lock()
	e = r.entries[id]
	if e != nil && !e.teardownBegun && !e.status.IsTerminal() {
		r.mu.Unlock()
		return writeErr
	}
	// A teardown began while the write was in flight: an erroring write may still
	// have reached the flag service, so strip in both cases. The teardown does not
	// strip a second time.
	var jrnl *journal.Journal
	if e != nil {
		jrnl = e.journal
	}
	r.mu.Unlock()
	if stripErr := r.stripLateTargetingWrite(ctx, lateWriteReassert, id, intent.FlagKey, jrnl); stripErr != nil {
		return errors.Join(errReassertTornDown, stripErr)
	}
	return errReassertTornDown
}

// rollbackPair undoes a partially-spawned CRN pair on a spawn error (#1003), keeping
// the pair atomic. failedIdx is the index in plan.arms whose Spawn just errored. It:
//  1. releases the failed arm's still-held reservation (failedIdx),
//  2. releases every arm AFTER failedIdx — their Spawn was never attempted, so they
//     still hold a reservation placeholder, and
//  3. force-releases (ReleaseGame) every earlier arm that already became a LIVE game,
//     so a half-pair never enters the cohort statistics.
//
// Arms before failedIdx that did NOT become live (the #1014 bind-vs-fast-terminal
// race already freed their reservation) need no action. Takes the lock per helper.
func (r *Registry) rollbackPair(plan pairPlan, failedIdx int, liveSiblings []startedArm) {
	// (1)+(2): the failed arm and all later, never-attempted arms still hold a
	// reservation placeholder — release one per arm.
	for i := failedIdx; i < len(plan.arms); i++ {
		r.releaseReservation(plan.id, plan.arms[i])
	}
	// (3): tear down any earlier arm that already started a live game.
	for _, s := range liveSiblings {
		r.ReleaseGame(s.gameID, "crn pair rollback: sibling arm spawn failed (#1003)")
	}
	// UC-046 BR-015: a failed pattern spawn uses up no archetype; reserve it again.
	if plan.pattern != nil {
		r.rewindFailedSpawn(plan.id, len(plan.arms)-failedIdx)
	}
}

// nextPair reserves the next Common-Random-Numbers PAIR (#1003) for a live
// experiment under both capacity caps, or reports ok=false when no live experiment
// can fit a full pair. A pair is two arms (experimental + control) sharing one seed
// = pairSeed(experimentID, pairIndex). It reserves BOTH arm slots atomically (under
// r.mu) so the pair is all-or-nothing against the cap; spawnPair rolls them back on
// a spawn failure. It advances the cross-experiment round-robin cursor (fairness),
// the chosen experiment's per-arm cursor (even arm fill), and its monotonic pair
// cursor. Assumes nothing; takes the lock.
func (r *Registry) nextPair() (pairPlan, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	free, live, ok := r.pairGateLocked()
	if !ok {
		return pairPlan{}, false
	}
	// Scan starting from the rotating cursor so a different experiment leads each
	// pass — equal-share over time even when the cap is the binding constraint.
	start := r.rrCursor % len(live)
	for i := 0; i < len(live); i++ {
		cand := live[(start+i)%len(live)]
		e := r.entries[cand]
		share := r.perExperimentShareLocked(len(live))
		n, pairIndex, pattern, ok := r.planPairLocked(cand, e, free, share)
		if !ok {
			continue
		}
		seed := pairSeed(cand, pairIndex)
		r.rrCursor = (start + i + 1) % len(live)
		arms := r.reserveArmsLocked(cand, e, n)
		return pairPlan{id: cand, seed: seed, pairIndex: pairIndex, arms: arms, pattern: pattern}, true
	}
	return pairPlan{}, false
}

// pairGateLocked returns the free effective budget and the live experiments, or
// ok=false when no pair can be reserved this tick. The caller holds r.mu; it does
// not lock.
func (r *Registry) pairGateLocked() (free int, live []string, ok bool) {
	// Gate on the EFFECTIVE concurrency cap (#998): the registry must not hold more
	// in-flight spawns than the coordinator can actually run. At least ONE free slot
	// is required to make any progress.
	free = r.effectiveCap - r.inFlightLocked()
	if free <= 0 {
		return 0, nil, false
	}
	// UC-046 BR-010: no other shadow game while a pattern game is in flight.
	if r.patternInFlightLocked() {
		return 0, nil, false
	}
	live = r.liveIDsLocked()
	if len(live) == 0 {
		return 0, nil, false
	}
	return free, live, true
}

// planPairLocked decides how many arms of experiment cand's current CRN pair to
// reserve this tick (n), the pair index, and the archetype of a pattern arm, or
// ok=false to skip the experiment. It is side-effect free: it never moves a cursor
// or reserves a slot. The caller holds r.mu; it does not lock.
func (r *Registry) planPairLocked(cand string, e *entry, free, share int) (n, pairIndex int, pattern *PatternSpawn, ok bool) {
	nArms := len(e.arms)
	if nArms == 0 {
		return 0, 0, nil, false
	}
	// Equal-share guard: bound an experiment to its fair ceil(cap/K) share so K
	// live experiments converge to an even split. Skip an experiment with no
	// room for even a single arm.
	shareRoom := share - e.inFlight()
	if shareRoom <= 0 {
		return 0, 0, nil, false
	}

	// CRN pairing (#1003): the per-experiment arm cursor lays out games as
	// consecutive (experimental, control) PAIRS. The pair index is armCursor /
	// nArms, and the shared seed = pairSeed(id, pairIndex) — so BOTH arms of a
	// pair derive the SAME seed automatically (the seed only changes at a pair
	// boundary). We reserve as many of the CURRENT pair's REMAINING arms as fit
	// the free + share budget THIS tick:
	//   - effective cap >= 2 (and a fresh pair) ⇒ reserve BOTH arms atomically,
	//     so spawnPair rolls the whole pair back if either start fails.
	//   - effective cap == 1 (the single-game coordinator) ⇒ reserve ONE arm now;
	//     its seed-matched sibling is reserved on a later tick (after this game
	//     concludes and frees the slot) with the SAME derived seed. The pair is
	//     still seed-matched; it just spans two ticks.
	pairIndex = e.armCursor / nArms
	armInPair := e.armCursor % nArms
	remainingInPair := nArms - armInPair // arms left to dispatch in this pair
	budget := free
	if shareRoom < budget {
		budget = shareRoom
	}
	n = remainingInPair
	if budget < n {
		n = budget
	}
	if n <= 0 {
		return 0, 0, nil, false
	}
	if e.pin == nil && r.patterns != nil {
		return 0, 0, nil, false // not pinned yet (pinPending runs first; a fresh Start waits a tick)
	}
	if r.patterns != nil && !e.patternActive() {
		return 0, 0, nil, false // archetype-only: never spawn without a pinned archetype (UC-046 A1)
	}
	if e.patternActive() {
		// UC-046 BR-010: a pattern arm spawns only when no shadow game is in flight,
		// one at a time. A6: one pair per pinned archetype, then no more.
		if r.inFlightLocked() > 0 || pairIndex >= len(e.pin.Archetypes) {
			return 0, 0, nil, false
		}
		n = 1
		pattern = &PatternSpawn{
			Archetype:      e.pin.Archetypes[pairIndex],
			MaxGameSeconds: e.pin.MaxGameSeconds,
			PairIndex:      pairIndex,
		}
	}
	return n, pairIndex, pattern, true
}

// reserveArmsLocked reserves the next n arms of experiment cand against the cap: it
// advances the arm cursor, writes a placeholder game per arm and counts each as an
// in-flight spawn. The caller holds r.mu; it does not lock.
func (r *Registry) reserveArmsLocked(cand string, e *entry, n int) []string {
	nArms := len(e.arms)
	arms := make([]string, 0, n)
	for s := 0; s < n; s++ {
		arm := e.arms[e.armCursor%nArms]
		e.armCursor++
		// Reserve against the cap by pre-incrementing via a placeholder game id; it
		// is replaced by the real game id in bindGame, or released on spawn failure.
		placeholder := reservationKey(cand, e.armCursor)
		e.games[placeholder] = arm
		// A spawn is now in flight (reserved, not yet bound). It is the only window
		// in which a real game_id can be reported-terminal before bind, so this
		// gates and bounds the tombstone map. Decremented in bindGame /
		// releaseReservation (one per reserved arm).
		r.spawnsInFlight++
		arms = append(arms, arm)
	}
	return arms
}

// perExperimentShareLocked is the equal-share ceiling per live experiment:
// ceil(cap / K). It bounds any one experiment to its fair slice so the cap splits
// evenly across K live experiments (no starvation). Assumes r.mu is held.
func (r *Registry) perExperimentShareLocked(k int) int {
	if k <= 0 {
		return r.maxCap
	}
	return (r.maxCap + k - 1) / k
}

// reservationKey builds the placeholder game id a reserved-but-not-yet-spawned
// slot occupies in entry.games. It is distinct from any coordinator game_id
// (which is "game_<hex>"), so bindGame/releaseReservation can find and replace it.
func reservationKey(id string, seq int) string {
	return "__reserved__" + id + "__" + strconv.Itoa(seq)
}

// pairSeed derives the shared Common-Random-Numbers seed for a (experimental,
// control) pair from (experimentID, pairIndex) using FNV-1a/64 (#1003). It is a
// PURE function of its inputs — NEVER a clock — so the same experiment/pair maps
// to the same seed across process restarts and journal rehydrates, which is what
// makes a paired arm reproducible. Both arms of a pair receive this identical
// value, so their shadow games face the same RNG-driven bracket/role/team schedule
// and the only between-arm difference is the flag under test.
//
// The seed is never 0 for a real pair: a 0 seed means "entropy" coordinator-side,
// so if the FNV digest happens to be 0 we nudge it to 1 (astronomically rare, but
// it keeps a legitimate pair from accidentally falling into the entropy path).
func pairSeed(experimentID string, pairIndex int) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(experimentID))
	_, _ = h.Write([]byte{0}) // domain separator so id|index can't alias
	_, _ = h.Write([]byte(strconv.Itoa(pairIndex)))
	seed := h.Sum64()
	if seed == 0 {
		seed = 1
	}
	return seed
}

// bindGame replaces the most-recent reservation for the experiment with the real
// coordinator game_id, records a game_assigned journal event, and (re)allocation
// is the caller's concern. Assumes the reservation exists (nextAllocation made it).
//
// BIND-VS-FAST-TERMINAL RACE (#1014): a game can hit a terminal outcome — and fire
// its TerminalCallback → ReleaseGame(gameID) — BEFORE this bind runs (the drive
// goroutine starts inside Spawn, before Spawn returns the game_id). When that
// happens ReleaseGame cannot find the not-yet-bound game_id, so it leaves a
// tombstone instead. bindGame detects that tombstone and, rather than recording the
// game_id permanently (which would leak the slot forever — the terminal callback
// already fired and will never fire again), it drops the reservation WITHOUT binding
// and records a non-counting release. The slot returns to the cap immediately.
//
// It returns true when it bound the game_id (a live in-flight game), false when it
// freed the reservation without binding (the race lost, or the experiment vanished)
// — the caller counts only genuinely-started games.
//
// pairIndex is the Common-Random-Numbers pair this game belongs to (#1003); it is
// stamped onto the game_assigned event (Event.PairIndex) so the paired-difference
// verdict (#1004) can recover which two games share a seed.
func (r *Registry) bindGame(id, gameID, arm string, pairIndex int, pattern *PatternSpawn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The spawn that produced this game_id has resolved (whichever branch we take
	// below). Account for it so the tombstone map stays bounded and self-cleaning.
	defer r.spawnResolvedLocked()
	e := r.entries[id]
	if e == nil {
		// Experiment vanished (torn down) between spawn and bind: nothing to bind, and
		// teardown already cleared the reservation. Drop any tombstone so it cannot
		// outlive the experiment.
		delete(r.releaseTombstones, gameID)
		return false
	}

	// TERMINAL-STATUS GUARD (#1004 carried-over item 3): a Spawn in flight during
	// teardown (which clears e.games and sets a terminal status) could otherwise
	// bindGame the real game_id back into a CONCLUDED/ABORTED experiment, re-adding a
	// live slot to an experiment that is already done — a leak widened by CRN's
	// two-arm reservation (two spawns in flight per pair). Refuse to bind into a
	// terminal experiment: release the in-flight game externally (its drive goroutine
	// is still running) and record a non-counting release for the audit trail, rather
	// than resurrecting the experiment's in-flight set.
	if e.status.IsTerminal() {
		delete(r.releaseTombstones, gameID)
		r.appendEvent(e.journal, journal.Event{
			Kind:   journal.KindGameReleased,
			GameID: gameID,
			Arm:    arm,
			Note:   "bind refused: experiment terminal at bind time (#1004)",
		})
		r.log.Warn("experiment.bind_refused_terminal (#1004)",
			"experiment_id", id, "game_id", gameID, "arm", arm, "status", string(e.status))
		return false
	}

	// If the game already terminated before we could bind it, do NOT record the
	// game_id — free the reservation slot instead and consume the tombstone.
	if reason, tombstoned := r.releaseTombstones[gameID]; tombstoned {
		delete(r.releaseTombstones, gameID)
		r.dropOneReservationLocked(e, arm)
		r.appendEvent(e.journal, journal.Event{
			Kind:   journal.KindGameReleased,
			GameID: gameID,
			Arm:    arm,
			Note:   reason,
		})
		// UC-046 BR-015: the pattern arm ended without a sample; its pair runs again.
		r.rerunPairLocked(e, pairIndex, reason)
		r.log.Warn("experiment.game_released_before_bind (#1014)",
			"experiment_id", id, "game_id", gameID, "arm", arm, "reason", reason)
		return false
	}

	// Replace the matching reservation placeholder (an arbitrary one for this arm)
	// with the real game id. We find ANY reservation for this arm to convert.
	r.dropOneReservationLocked(e, arm)
	e.games[gameID] = arm
	e.gamePairs[gameID] = pairIndex
	pi := pairIndex
	r.appendEvent(e.journal, journal.Event{
		Kind:      journal.KindGameAssigned,
		GameID:    gameID,
		Arm:       arm,
		PairIndex: &pi,
		Pattern:   e.patternFactsLocked(pairIndex, pattern),
	})
	r.emitProgressLocked(e, ProgressTriggerGameAssigned, "", e.spanCtx) // #2059
	r.log.Info("experiment.game_assigned",
		"experiment_id", id, "game_id", gameID, "arm", arm, "pair_index", pairIndex)
	return true
}

// dropOneReservationLocked deletes one reservation placeholder for the arm from the
// experiment's game map (an arbitrary one — they are interchangeable for the same
// arm). It is the shared helper for bindGame (convert/free a reservation) and
// releaseReservation (free on spawn failure). Assumes r.mu is held.
func (r *Registry) dropOneReservationLocked(e *entry, arm string) {
	for k, a := range e.games {
		if isReservation(k) && a == arm {
			delete(e.games, k)
			return
		}
	}
}

// releaseReservation drops one reservation placeholder for the arm (used when a
// spawn fails) so a flaky spawn does not permanently consume a capacity slot.
func (r *Registry) releaseReservation(id, arm string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The spawn resolved (with an error); account for it whether or not the
	// experiment still exists, so spawnsInFlight cannot drift positive and pin the
	// tombstone map open.
	defer r.spawnResolvedLocked()
	e := r.entries[id]
	if e == nil {
		return
	}
	r.dropOneReservationLocked(e, arm)
}

// spawnResolvedLocked records that one in-flight spawn has resolved (bound, freed,
// or failed). When the count returns to zero NO bind can be pending, so any tombstone
// still in the map is a proven orphan (a stale conclude-then-release or double-release
// that no bindGame will ever consume) and is swept — keeping releaseTombstones bounded
// by concurrent in-flight spawns and empty whenever the registry is quiescent. Assumes
// r.mu is held.
func (r *Registry) spawnResolvedLocked() {
	if r.spawnsInFlight > 0 {
		r.spawnsInFlight--
	}
	if r.spawnsInFlight == 0 && len(r.releaseTombstones) > 0 {
		for k := range r.releaseTombstones {
			delete(r.releaseTombstones, k)
		}
	}
}

// isReservation reports whether a game-map key is a reservation placeholder
// rather than a real coordinator game_id.
func isReservation(k string) bool { return strings.HasPrefix(k, "__reserved__") }
