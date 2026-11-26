package experiment

// registry_teardown.go holds the Registry's ABORT / TEARDOWN phase:
// Abort, AbortAll and the terminal teardown.

import (
	"context"
	"fmt"
	"sort"

	"github.com/joustmania/agent/experiment/journal"
	"github.com/joustmania/agent/patterngame"
)

// Abort terminates an experiment (kill-switch / capacity reclaim / error): it
// records the ABORTED transition, tears down the targeting, and frees capacity.
// A terminal experiment is a no-op. reason is recorded for the audit trail.
func (r *Registry) Abort(ctx context.Context, id, reason string) error {
	r.mu.Lock()
	e := r.entries[id]
	if e == nil {
		r.mu.Unlock()
		return fmt.Errorf("registry: abort: unknown experiment %q", id)
	}
	if e.status.IsTerminal() {
		r.mu.Unlock()
		return nil
	}
	flagKey := e.flagKey
	r.mu.Unlock()

	r.teardown(ctx, id, flagKey, StatusAborted, reason)
	return nil
}

// AbortAll aborts every live (non-terminal) experiment — the kill-switch path
// (design §7.2 / §10: auto-stop on agent.json `enabled` false). It is called when
// the agent is disabled. Returns the ids aborted.
func (r *Registry) AbortAll(ctx context.Context, reason string) []string {
	r.mu.Lock()
	ids := make([]string, 0, len(r.entries))
	for id, e := range r.entries {
		if !e.status.IsTerminal() {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()

	sort.Strings(ids) // deterministic order (tests + stable logs)
	// UC-046 A12: force-end in-flight pattern games, marked non-exact kill_switch.
	var patternGames []string
	if r.patternTracker != nil {
		r.mu.Lock()
		patternGames = r.patternGamesLocked(ids)
		r.mu.Unlock()
	}
	for _, gid := range patternGames {
		r.patternTracker.Abort(gid, patterngame.ReasonKillSwitch)
	}
	for _, id := range ids {
		_ = r.Abort(ctx, id, reason)
	}
	return ids
}

// teardown is the inverse of Start (design §10): it strips the experiment's
// shadow targeting branch via the TargetingWriter (#977), records the terminal
// decision, sets the terminal status, and frees capacity (clearing the in-flight
// game map so the freed slots are available to other experiments on the next
// AllocateAndSpawn). Idempotent on a terminal experiment.
func (r *Registry) teardown(ctx context.Context, id, flagKey string, terminal Status, reason string) {
	// Runs after the registry lock below is released (defers are LIFO).
	if r.onTerminal != nil {
		defer r.onTerminal(id)
	}
	// Mark the teardown before the strip, so a Start whose targeting write lands
	// after the strip below removes it again (#2340).
	r.mu.Lock()
	if e := r.entries[id]; e != nil {
		e.teardownBegun = true
	}
	r.mu.Unlock()
	// Strip the targeting OUTSIDE the registry lock (it does file I/O). Stripping a
	// shadow branch only ever affects shadow games, so it is invariant-safe.
	if r.target != nil {
		if err := r.target.Strip(ctx, flagKey, id); err != nil {
			r.log.Warn("experiment.teardown_strip_failed", "experiment_id", id, "error", err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return
	}
	// #1188: end the long-lived agent.experiment ROOT span here — teardown is the
	// SINGLE terminal chokepoint every terminal transition routes through (conclude →
	// discarded/done, abort), so ending it here guarantees no leak on ANY terminal path.
	// It is placed BEFORE the idempotency guard (and is itself idempotent) so a path
	// that finds the status already terminal still ends the span exactly once.
	progressLink := e.spanCtx // #2059: the Link target, read before the span ends
	r.endSpanLocked(e)
	// A terminal status is never overwritten: an AbortAll that raced a conclusion
	// keeps the experiment aborted (#1701).
	if e.status.IsTerminal() {
		return
	}
	e.status = terminal
	e.endReason = reason
	// Free capacity: drop the in-flight games (reservations + bound games). Their
	// slots return to the global cap for other experiments. gamePairs is the parallel
	// (game_id → pair index) map; clear it too so no stale binding survives teardown.
	e.games = make(map[string]string)
	e.gamePairs = make(map[string]int)
	e.pendingCalibration = nil
	r.appendEvent(e.journal, journal.Event{
		Kind:   journal.KindOutcome,
		Status: string(terminal),
		Note:   reason,
	})
	r.emitProgressLocked(e, ProgressTriggerTerminal, reason, progressLink) // #2059
	r.log.Info("experiment.torn_down", "experiment_id", id, "status", string(terminal), "reason", reason)
}
