package experiment

import "sync"

// fitness_store.go is the FINALIZED-PER-GAME fitness store (#965, #1017). It
// backs the retro's fitness lookup (Get).
//
// THE #1017 GAP, precisely: shadow and real games already emit the SAME fitness
// inputs through the SAME code path, and decision.EvaluateFitness is
// substrate-agnostic — so fitness is COMPUTABLE today; it is simply never STORED
// anywhere. The experiment loop already computes a per-game
// fitness scalar at conclusion (defaultGameFitness → EvaluateFitness) but folds
// it ONLY into the cohort journal for experiment-bound games and DROPS it for
// everything else. This store is the rolling, in-memory, game_id-keyed sink that
// captures that already-computed sample for BOTH:
//
//   - a SHADOW experiment game (read back by game_id), and
//   - a REAL primary game, one sample per objective it could be scored on.
//
// FINALIZED, NOT A LIVE SCRAPE (#958 / #1017): Record is called from onGameEnd
// AFTER the game has concluded and its duration has been recovered
// (withEffectiveDuration), so a reader sees the SETTLED per-game fitness — it
// never races the ~10s OTel span flush (the #958 strategy doc requirement).
//
// NO-PLAYER-IDENTITY (#23): a sample is keyed by game_id and carries only
// per-game aggregates (the objective's fitness scalar, the resolved flag config,
// the duration). It holds NOTHING per-person and is evicted by a bounded ring, so
// it composes cleanly with controllers≠persons — no cross-session profile is ever
// assembled here.

// DefaultFitnessStoreCapacity bounds the store's ring. A few hundred finalized
// samples is ample: readers only ever read a single game_id, so the working set
// is tiny. The ring keeps the store O(1) in memory
// regardless of how long the agent runs.
const DefaultFitnessStoreCapacity = 512

// FitnessSample is one finalized per-game fitness datapoint (#965). It is the unit
// the store holds.
type FitnessSample struct {
	// GameID is the coordinator-assigned game id this sample finalized for. It is
	// the store key.
	GameID string
	// Fitness is the game's finalized objective fitness scalar in [0,1] (higher =
	// better) — the SAME defaultGameFitness → EvaluateFitness value the cohort
	// journal folds.
	Fitness float64
	// Objective is the fitness objective the Fitness scalar scored (endurance /
	// balanced / accelerate). The #958 study is per-objective: only
	// endurance/accelerate carry real signal today (#1015).
	Objective string
	// GameKind is "real" for a primary game or "shadow" for a cohort game.
	GameKind string
	// DurationS is the game's recovered effective duration in seconds (#997), kept
	// alongside the fitness for the audit trail / drift monitor (#1017 §4).
	DurationS float64
	// Config is the resolved per-game flag snapshot in effect for this game (the
	// value(s) under experiment), so a paired (config → fitness) study can assemble
	// observations (#1017 §3 item 3). Optional — nil when the loop has no resolved
	// snapshot to attach. Copied on Record so a caller cannot mutate stored state.
	Config map[string]any
}

// FitnessStore is a bounded, thread-safe, finalized-per-game fitness store keyed
// by game_id (#965). Construct with NewFitnessStore; the experiment loop's
// onGameEnd Records into it, and the retro reads it.
type FitnessStore struct {
	mu  sync.Mutex
	cap int
	// byKey is the authoritative finalized sample per (game_id, objective). A REAL
	// game is scored against EVERY experimentable objective its signals support
	// (#1017 §3 item 1) — so one game_id may hold several samples, one per objective.
	// The composite key keeps them distinct (a single game_id key would clobber). A
	// SHADOW game carries one objective, so it has exactly one entry. Last write wins
	// per key (a defensive re-Record updates in place).
	byKey map[string]FitnessSample
	// order is the insertion order of keys for FIFO eviction once cap is exceeded. A
	// key appears at most once (Record removes a stale dup before re-appending), so
	// order and byKey stay in lockstep.
	order []string
}

// NewFitnessStore builds a store with the given ring capacity. A non-positive
// capacity falls back to DefaultFitnessStoreCapacity (a zero ring would evict
// every sample immediately, defeating the store).
func NewFitnessStore(capacity int) *FitnessStore {
	if capacity <= 0 {
		capacity = DefaultFitnessStoreCapacity
	}
	return &FitnessStore{
		cap:   capacity,
		byKey: make(map[string]FitnessSample, capacity),
	}
}

// sampleKey composes the (game_id, objective) store key. A shadow game's objective
// may be empty (the measurer reads it back by game_id regardless), which is fine —
// the empty-objective key is still distinct per game_id.
func sampleKey(gameID, objective string) string {
	return gameID + "\x00" + objective
}

// Record finalizes one per-game fitness sample, keyed by sample.GameID. It is
// called from onGameEnd after the game concluded and its duration was recovered,
// so the stored value is SETTLED (no span-flush race). A sample with an empty
// GameID is dropped (nothing can read it back). The ring evicts the oldest
// sample(s) once capacity is exceeded.
func (s *FitnessStore) Record(sample FitnessSample) {
	if sample.GameID == "" {
		return
	}
	// Defensive copy so a later mutation of the caller's map cannot reach in.
	sample.Config = copyConfig(sample.Config)

	key := sampleKey(sample.GameID, sample.Objective)

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byKey[key]; exists {
		// Re-Record (rare): update in place, keep its position in the order ring so a
		// duplicate cannot grow order unbounded.
		s.byKey[key] = sample
		return
	}
	s.byKey[key] = sample
	s.order = append(s.order, key)
	s.evictLocked()
}

// Get returns a finalized sample for a game_id and whether one is present. A
// shadow game holds exactly one (game_id, objective) sample, so Get returns it. For a game
// id with several per-objective samples (a real game) Get returns an arbitrary one.
func (s *FitnessStore) Get(gameID string) (FitnessSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sample := range s.byKey {
		if sample.GameID == gameID {
			return sample, true
		}
	}
	return FitnessSample{}, false
}

// Len returns the number of finalized samples currently held (test/observability).
// A real game scored against multiple objectives counts once PER objective.
func (s *FitnessStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byKey)
}

// evictLocked drops the oldest sample(s) until the ring is within capacity.
// Assumes s.mu is held.
func (s *FitnessStore) evictLocked() {
	for len(s.order) > s.cap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.byKey, oldest)
	}
}

// copyConfig deep-enough copies a resolved-config snapshot (a flat map of scalar
// flag values) so the store owns its own copy. nil/empty maps return nil.
func copyConfig(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
