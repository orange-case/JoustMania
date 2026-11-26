package experiment

import (
	"context"
	"fmt"
	"testing"

	"github.com/joustmania/agent/experiment/journal"
	"github.com/joustmania/agent/patterngame"
)

// progress_test.go covers the progress snapshot behind GET /experiments/state: one
// recorded per state change, numbers equal to the journal summary, the live ones
// plus the newest finished ones held, and no span.

// progressLog records every snapshot r records, in order.
func progressLog(r *Registry) *[]ProgressSnapshot {
	var log []ProgressSnapshot
	r.onProgress = func(s ProgressSnapshot) { log = append(log, s) }
	return &log
}

func triggersOf(log []ProgressSnapshot) []string {
	out := make([]string, 0, len(log))
	for _, s := range log {
		out = append(out, s.Trigger)
	}
	return out
}

func equalTriggers(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func progressRegistry(t *testing.T, cfg RegistryConfig) (*Registry, *[]ProgressSnapshot) {
	t.Helper()
	r, _ := rootSpanRegistry(t, cfg)
	return r, progressLog(r)
}

// UC: UC-035 4
func TestProgress_RecordedOnEveryStateChange(t *testing.T) {
	spawner := &fakeSpawner{}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{concludeAtN: 1000}, Targeting: &fakeTargeting{},
		MaxShadowGames: 2,
	})
	id := startedExperiment(t, r)
	r.AllocateAndSpawn(context.Background()) // binds one game per arm
	if len(spawner.calls) != 2 {
		t.Fatalf("spawned = %d, want 2", len(spawner.calls))
	}
	if _, err := r.ConcludeGame(context.Background(), spawner.calls[0].gameID, 0.4, 60); err != nil {
		t.Fatal(err)
	}
	r.ReleaseGame(spawner.calls[1].gameID, "game_error")
	if err := r.Abort(context.Background(), id, "operator stop"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		ProgressTriggerStarted,
		ProgressTriggerGameAssigned, ProgressTriggerGameAssigned,
		ProgressTriggerGameConcluded, ProgressTriggerGameReleased,
		ProgressTriggerTerminal,
	}
	if got := triggersOf(*log); !equalTriggers(got, want) {
		t.Fatalf("UC-035/4 triggers = %v, want %v", got, want)
	}
	last := (*log)[len(want)-1]
	if last.EndReason != "operator stop" || last.Status != string(StatusAborted) {
		t.Errorf("terminal snapshot = %q / %q, want aborted / operator stop", last.Status, last.EndReason)
	}
	for _, s := range (*log)[:len(want)-1] {
		if s.EndReason != "" {
			t.Errorf("%s snapshot carries end_reason %q; terminal only", s.Trigger, s.EndReason)
		}
	}
	if held := r.Progress(); len(held) != 1 || held[0].Trigger != ProgressTriggerTerminal {
		t.Errorf("held = %+v, want the terminal snapshot only", held)
	}
}

// UC: UC-035 4
func TestProgress_Fields(t *testing.T) {
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: &fakeSpawner{}, Verdict: fakeVerdict{concludeAtN: 1000}, Targeting: &fakeTargeting{},
		MaxShadowGames: 2,
	})
	id := startedExperiment(t, r)
	if len(*log) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(*log))
	}
	s := (*log)[0]
	in := sampleIntent()
	expCtx, _ := r.ExperimentSpanContext(id)
	if s.ID != id || s.Status != string(StatusRunning) || s.FlagKey != in.FlagKey || s.Objective != in.Objective ||
		s.Thesis != in.Hypothesis || s.TargetN != r.entries[id].journal.Intent().TargetNPerArm {
		t.Errorf("snapshot = %+v, want the intent's fields", s)
	}
	if fmt.Sprint(s.ExperimentalValue) != "0.5" {
		t.Errorf("experimental_value = %v, want 0.5", s.ExperimentalValue)
	}
	if s.Verdict != nil || s.Recommendation != nil {
		t.Errorf("verdict / recommendation = %v / %v, want none yet", s.Verdict, s.Recommendation)
	}
	if s.TraceID == "" || s.TraceID != expCtx.TraceID().String() {
		t.Errorf("trace_id = %q, want the agent.experiment trace %s", s.TraceID, expCtx.TraceID())
	}
	if len(s.Arms) != 2 || s.Arms[0].Arm != ArmExperimental || s.Arms[1].Arm != ArmControl {
		t.Errorf("arms = %+v, want experimental then control", s.Arms)
	}
	if s.UpdatedAtMs == 0 {
		t.Error("updated_at_ms is unset")
	}
}

// The snapshot's per-arm numbers equal the journal summary after every fold, and a
// game counts in samples only once the journal folded it.
// UC: UC-035 4
func TestProgress_NumbersEqualTheJournalSummary(t *testing.T) {
	spawner := &fakeSpawner{}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{concludeAtN: 1000}, Targeting: &fakeTargeting{},
		MaxShadowGames: 2,
	})
	id := startedExperiment(t, r)
	for i, f := range []float64{0.2, 0.6, 0.5, 0.9} {
		r.AllocateAndSpawn(context.Background())
		g := spawner.calls[len(spawner.calls)-1]
		if _, err := r.ConcludeGame(context.Background(), g.gameID, f, 60); err != nil {
			t.Fatal(err)
		}
		last := (*log)[len(*log)-1]
		if last.Trigger != ProgressTriggerGameConcluded {
			t.Fatalf("round %d: last trigger = %q, want game_concluded", i, last.Trigger)
		}
		summary := r.entries[id].journal.Summary()
		inFlight := map[string]int{}
		for gid, arm := range r.entries[id].games {
			if !isReservation(gid) {
				inFlight[arm]++
			}
		}
		for _, a := range last.Arms {
			st := summary.Arms[a.Arm]
			if st != nil && (a.Samples != st.Count || a.MeanFitness != st.Mean) {
				t.Fatalf("round %d arm %s: samples=%d mean=%v, journal count=%d mean=%v",
					i, a.Arm, a.Samples, a.MeanFitness, st.Count, st.Mean)
			}
			if a.InFlight != inFlight[a.Arm] {
				t.Fatalf("round %d arm %s: in_flight=%d, want %d", i, a.Arm, a.InFlight, inFlight[a.Arm])
			}
		}
	}
}

// A game the registry does not know (never bound, so never folded) records nothing.
// UC: UC-035 4
func TestProgress_UnfoldedGameRecordsNothing(t *testing.T) {
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: &fakeSpawner{}, Verdict: fakeVerdict{concludeAtN: 1000}, Targeting: &fakeTargeting{},
		MaxShadowGames: 2,
	})
	startedExperiment(t, r)
	before := len(*log)
	if _, err := r.ConcludeGame(context.Background(), "not-ours", 0.9, 60); err != nil {
		t.Fatal(err)
	}
	if n := len(*log); n != before {
		t.Fatalf("snapshots after an unknown game = %d, want %d", n, before)
	}
}

// UC: UC-035 4
func TestProgress_RehydrateRecordsExactlyOnce(t *testing.T) {
	root := t.TempDir()
	r1, _ := rootSpanRegistry(t, RegistryConfig{
		Root: root, Spawner: &fakeSpawner{}, Verdict: fakeVerdict{concludeAtN: 1000},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2,
	})
	id := startedExperiment(t, r1)

	r2, log := progressRegistry(t, RegistryConfig{
		Root: root, Spawner: &fakeSpawner{}, Verdict: fakeVerdict{concludeAtN: 1000},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2,
	})
	if err := r2.Rehydrate([]string{id}); err != nil {
		t.Fatal(err)
	}
	if err := r2.Rehydrate([]string{id}); err != nil { // a known id is left alone
		t.Fatal(err)
	}
	if got := triggersOf(*log); !equalTriggers(got, []string{ProgressTriggerRehydrate}) {
		t.Fatalf("triggers after rehydrate = %v, want [rehydrate]", got)
	}
	expCtx, _ := r2.ExperimentSpanContext(id)
	if (*log)[0].TraceID != expCtx.TraceID().String() {
		t.Errorf("rehydrate trace_id = %q, want the fresh agent.experiment trace", (*log)[0].TraceID)
	}
}

// The concluded and promoting transitions each record a snapshot, and the
// game_concluded snapshot already carries the interim verdict of that game.
// UC: UC-035 4
func TestProgress_ConcludedAndPromotingSnapshots(t *testing.T) {
	spawner := &fakeSpawner{}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{outcome: CohortOutcomePromote, concludeAtN: 1},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2,
	})
	startedExperiment(t, r)
	r.AllocateAndSpawn(context.Background())
	if _, err := r.ConcludeGame(context.Background(), spawner.calls[0].gameID, 0.9, 60); err != nil {
		t.Fatal(err)
	}
	want := []string{
		ProgressTriggerStarted,
		ProgressTriggerGameAssigned, ProgressTriggerGameAssigned,
		ProgressTriggerGameConcluded, ProgressTriggerConcluded, ProgressTriggerPromoting,
		ProgressTriggerTerminal,
	}
	if got := triggersOf(*log); !equalTriggers(got, want) {
		t.Fatalf("UC-035/4 triggers = %v, want %v", got, want)
	}
	for i, wantStatus := range map[int]Status{4: StatusConcluded, 5: StatusPromoting} {
		if got := (*log)[i].Status; got != string(wantStatus) {
			t.Errorf("%s snapshot status = %q, want %q", (*log)[i].Trigger, got, wantStatus)
		}
	}
	if v := (*log)[3].Verdict; v == nil || v.Outcome != CohortOutcomePromote {
		t.Errorf("game_concluded verdict = %+v, want %q (the verdict of this game)", v, CohortOutcomePromote)
	}
}

// The verdict on a game_concluded snapshot is the interim verdict computed from that
// game, never the previous game's.
// UC: UC-035 4
func TestProgress_GameConcludedVerdictDoesNotLag(t *testing.T) {
	spawner := &fakeSpawner{}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{concludeAtN: 1000, alwaysReport: true},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2,
	})
	startedExperiment(t, r)
	r.AllocateAndSpawn(context.Background())
	if _, err := r.ConcludeGame(context.Background(), spawner.calls[0].gameID, 0.4, 60); err != nil {
		t.Fatal(err)
	}
	last := (*log)[len(*log)-1]
	if last.Trigger != ProgressTriggerGameConcluded {
		t.Fatalf("last trigger = %q, want game_concluded", last.Trigger)
	}
	if last.Verdict == nil || last.Verdict.Outcome != OutcomeInconclusive {
		t.Fatalf("first game_concluded verdict = %+v, want %q", last.Verdict, OutcomeInconclusive)
	}
}

// abortingVerdict tears every experiment down from inside Evaluate, the window
// between the fold and the game_concluded snapshot that a concurrent AbortAll
// (kill switch, experiments_enabled off) can hit.
type abortingVerdict struct{ r **Registry }

func (v abortingVerdict) Evaluate(journal.Summary) (journal.Verdict, bool) {
	(*v.r).AbortAll(context.Background(), "kill switch")
	return journal.Verdict{Outcome: OutcomeInconclusive}, true
}

// An experiment torn down between the fold and the game_concluded snapshot has
// already recorded its terminal snapshot: no game_concluded follows it.
// UC: UC-035 4
func TestProgress_TornDownDuringConcludeRecordsNoGameConcluded(t *testing.T) {
	spawner := &fakeSpawner{}
	var reg *Registry
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: abortingVerdict{r: &reg},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2,
	})
	reg = r
	startedExperiment(t, r)
	r.AllocateAndSpawn(context.Background())
	if _, err := r.ConcludeGame(context.Background(), spawner.calls[0].gameID, 0.9, 60); err != nil {
		t.Fatal(err)
	}
	want := []string{
		ProgressTriggerStarted,
		ProgressTriggerGameAssigned, ProgressTriggerGameAssigned,
		ProgressTriggerTerminal,
	}
	if got := triggersOf(*log); !equalTriggers(got, want) {
		t.Fatalf("UC-035/4 triggers = %v, want %v (no game_concluded after terminal)", got, want)
	}
}

// A pattern experiment whose pin cannot be restored is discarded on rehydrate: it
// records only its terminal snapshot, never a rehydrate snapshot with status running.
// UC: UC-046 BR-015
func TestProgress_LostPinRehydrateRecordsOnlyTerminal(t *testing.T) {
	root := t.TempDir()
	r1 := newTestRegistry(t, RegistryConfig{Root: root, MaxShadowGames: 2, Spawner: &fakeSpawner{}})
	id := startedExperiment(t, r1)
	r1.AllocateAndSpawn(context.Background()) // a game assigned with no pin

	r2, log := progressRegistry(t, RegistryConfig{Root: root, MaxShadowGames: 2, Spawner: &patternSpawner{},
		Patterns: &queuePinner{pins: []patterngame.Pin{testPin(t, "a")}}})
	if err := r2.Rehydrate([]string{id}); err != nil {
		t.Fatal(err)
	}
	if got := triggersOf(*log); !equalTriggers(got, []string{ProgressTriggerTerminal}) {
		t.Fatalf("UC-046/BR-015 triggers after a lost-pin rehydrate = %v, want [terminal]", got)
	}
}

// hookSpawner runs onSpawn (once) inside its first Spawn call: after the pair's
// slots are reserved, before any game is bound.
type hookSpawner struct {
	fakeSpawner
	onSpawn func()
}

func (h *hookSpawner) Spawn(ctx context.Context, experimentID, arm string, seed uint64) (string, error) {
	if f := h.onSpawn; f != nil {
		h.onSpawn = nil
		f()
	}
	return h.fakeSpawner.Spawn(ctx, experimentID, arm, seed)
}

// in_flight counts bound games only: a pending spawn reservation is not a game yet.
// UC: UC-035 4
func TestProgress_InFlightExcludesSpawnReservations(t *testing.T) {
	spawner := &hookSpawner{}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{concludeAtN: 1000}, Targeting: &fakeTargeting{},
		MaxShadowGames: 2,
	})
	id := startedExperiment(t, r)
	reserved := 0
	spawner.onSpawn = func() {
		r.mu.Lock()
		for gid := range r.entries[id].games {
			if isReservation(gid) {
				reserved++
			}
		}
		r.emitProgressLocked(r.entries[id], "probe", "", r.entries[id].spanCtx)
		r.mu.Unlock()
	}
	r.AllocateAndSpawn(context.Background())
	if reserved == 0 {
		t.Fatal("no spawn reservation pending inside Spawn; the test does not cover the exclusion")
	}
	for _, s := range *log {
		if s.Trigger != "probe" {
			continue
		}
		for _, a := range s.Arms {
			if a.InFlight != 0 {
				t.Errorf("arm %s in_flight = %d with only reservations pending, want 0", a.Arm, a.InFlight)
			}
		}
		return
	}
	t.Fatal("no probe snapshot")
}

// The concluded snapshot carries the FINAL verdict (#2042), not the last interim
// one: a give-up conclusion with no interim verdict still reports the outcome.
// UC: UC-035 4
func TestProgress_ConcludedSnapshotCarriesFinalVerdict(t *testing.T) {
	spawner := &fakeSpawner{}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{concludeAtN: 1000},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2, MaxGamesPerExperiment: 1,
	})
	startedExperiment(t, r)
	r.AllocateAndSpawn(context.Background())
	if _, err := r.ConcludeGame(context.Background(), spawner.calls[0].gameID, 0.4, 60); err != nil {
		t.Fatal(err)
	}
	var gameConcluded, concluded *ProgressSnapshot
	for i := range *log {
		switch (*log)[i].Trigger {
		case ProgressTriggerGameConcluded:
			gameConcluded = &(*log)[i]
		case ProgressTriggerConcluded:
			concluded = &(*log)[i]
		}
	}
	if gameConcluded == nil || concluded == nil {
		t.Fatalf("triggers = %v, want game_concluded and concluded", triggersOf(*log))
	}
	if gameConcluded.Verdict != nil {
		t.Fatalf("game_concluded carries a verdict, want none (no interim verdict)")
	}
	if concluded.Verdict == nil || concluded.Verdict.Outcome != OutcomeInconclusive {
		t.Fatalf("concluded verdict = %+v, want final %q", concluded.Verdict, OutcomeInconclusive)
	}
}

// Every live experiment is held, finished ones only the newest MaxFinishedProgress.
// UC: UC-035 4
func TestProgress_HoldsLiveAndTheNewestFinished(t *testing.T) {
	r := newTestRegistry(t, RegistryConfig{Spawner: &fakeSpawner{}, MaxShadowGames: 2})
	now := int64(0)
	r.mu.Lock()
	for i := 0; i < MaxFinishedProgress+3; i++ {
		now++
		id := fmt.Sprintf("exp_done_%02d", i)
		r.progress = mapWith(r.progress, ProgressSnapshot{ID: id, Status: string(StatusDone), UpdatedAtMs: now})
		r.pruneProgressLocked()
	}
	r.progress = mapWith(r.progress, ProgressSnapshot{ID: "exp_live", Status: string(StatusRunning), UpdatedAtMs: 0})
	r.pruneProgressLocked()
	r.mu.Unlock()

	held := r.Progress()
	if len(held) != MaxFinishedProgress+1 {
		t.Fatalf("held = %d, want %d finished plus the live one", len(held), MaxFinishedProgress)
	}
	if held[0].ID != fmt.Sprintf("exp_done_%02d", MaxFinishedProgress+2) || held[len(held)-1].ID != "exp_live" {
		t.Errorf("order %s..%s, want newest first", held[0].ID, held[len(held)-1].ID)
	}
	var nilRegistry *Registry
	if len(nilRegistry.Progress()) != 0 {
		t.Error("a nil registry holds nothing")
	}
}

func mapWith(m map[string]ProgressSnapshot, s ProgressSnapshot) map[string]ProgressSnapshot {
	if m == nil {
		m = map[string]ProgressSnapshot{}
	}
	m[s.ID] = s
	return m
}

// A promotion refused because another writer owns the real overlay (UC-043 A19,
// #2276) puts the winning value on the experiment's snapshot as a recommendation.
// UC: UC-043 A19
func TestProgress_RefusedPromotionCarriesRecommendation(t *testing.T) {
	spawner := &fakeSpawner{}
	promoter := &fakePromoter{result: PromotionResult{
		Outcome: PromotionDiscarded, Reason: "real_overlay_owned_by_another_writer owner=operator", ForeignOwner: "operator",
	}}
	r, log := progressRegistry(t, RegistryConfig{
		Spawner: spawner, Verdict: fakeVerdict{outcome: CohortOutcomePromote, concludeAtN: 1},
		Targeting: &fakeTargeting{}, MaxShadowGames: 2, Promoter: promoter,
	})
	startedExperiment(t, r)
	r.AllocateAndSpawn(context.Background())
	if _, err := r.ConcludeGame(context.Background(), spawner.calls[0].gameID, 0.9, 60); err != nil {
		t.Fatal(err)
	}
	last := (*log)[len(*log)-1]
	want := ProgressRecommendation{Flag: "death_grace_period_seconds", Value: "0.5", Owner: "operator"}
	if last.Recommendation == nil || *last.Recommendation != want {
		t.Errorf("terminal recommendation = %+v, want %+v", last.Recommendation, want)
	}
	if (*log)[0].Recommendation != nil {
		t.Error("started snapshot carries a recommendation")
	}
}

// UC: UC-043 A19
func TestLastForeignOwner(t *testing.T) {
	ev := []journal.Event{
		{Promotion: &PromotionResult{ForeignOwner: "a"}},
		{Promotion: &PromotionResult{}},
	}
	if got := lastForeignOwner(ev); got != "a" {
		t.Errorf("lastForeignOwner = %q, want a", got)
	}
	if got := lastForeignOwner(nil); got != "" {
		t.Errorf("lastForeignOwner(nil) = %q", got)
	}
}
