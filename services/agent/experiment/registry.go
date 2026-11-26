package experiment

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/experiment/journal"
	"github.com/joustmania/agent/patterngame"
)

// registry.go is the agent-owned experiment COORDINATOR (design §7, #978). It is
// the experiment-timescale counterpart to the per-game LoopSet/Multiplexer
// (#845): where those manage one decision loop/store per live game, the Registry
// manages one entry per live EXPERIMENT. It mints experiment ids, holds each
// experiment's intent + status, allocates finite shadow capacity across the live
// set, decides (experiment_id, arm) at spawn, folds concluded games into the
// journal, drives the verdict + status machine, and tears the experiment down on
// a terminal status.
//
// SOURCE OF TRUTH: the durable journal (#983). The in-memory Registry is a VIEW
// rebuilt from disk on startup via Rehydrate (the #831 in-memory-loss fix) — the
// journal's append-only event log + rolling summary are authoritative; the
// Registry never holds state the journal does not.
//
// SEAMS (registry_seams.go): every consequential effect is injected —
// ShadowSpawner (#976), CohortVerdict (#979), Promoter (#961/#980),
// TargetingWriter (#977). The Registry owns ONLY the lifecycle logic, so it is
// unit-testable with recording fakes.

// Status is an experiment's lifecycle status (design §7.2). The set is closed so
// the journal/dashboards can enumerate every state.
//
//	PROPOSED ─▶ RUNNING ─▶ CONCLUDED ─▶ {PROMOTING ─▶ DONE | DISCARDED}
//	                 │
//	                 └─▶ ABORTED   (kill-switch / capacity reclaim / error)
type Status string

const (
	// StatusProposed — intent recorded; targeting not yet written; no games.
	StatusProposed Status = "proposed"
	// StatusRunning — targeting written; the experiment may accrue shadow games.
	StatusRunning Status = "running"
	// StatusConcluded — target N reached (or under-powered timeout); verdict computed.
	StatusConcluded Status = "concluded"
	// StatusPromoting — a concluded+promote experiment being routed to the Promoter.
	StatusPromoting Status = "promoting"
	// StatusDone — terminal: promotion finished; targeting torn down; capacity freed.
	StatusDone Status = "done"
	// StatusDiscarded — terminal: concluded without promotion; torn down.
	StatusDiscarded Status = "discarded"
	// StatusAborted — terminal: kill-switch / capacity reclaim / error; torn down.
	StatusAborted Status = "aborted"
)

// AbortReasonStartFailed is the abort reason of an experiment whose start failed
// (UC-043 A8, UC-042 A4): it never stays PROPOSED holding its flag (#1700).
const AbortReasonStartFailed = "start_failed"

// AbortReasonPromotionInterrupted is the abort reason of an experiment restored
// as promoting with no journaled promotion event (UC-043 A15, #2042): what its
// promotion did is unknown, so it is never promoted again.
const AbortReasonPromotionInterrupted = "promotion_interrupted"

// IsTerminal reports whether a status is terminal (the experiment is finished and
// its capacity + targeting have been released).
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusDiscarded || s == StatusAborted
}

// IsLive reports whether the experiment still occupies a registry slot and may be
// allocated shadow capacity (PROPOSED/RUNNING — concluded experiments hold their
// game map for the verdict but are not allocated new capacity).
func (s Status) IsLive() bool {
	return s == StatusProposed || s == StatusRunning
}

// DefaultMaxShadowGames is the fixed cap on concurrent shadow games across ALL
// live experiments (epic #982 decision 2: a fixed configured cap, not dynamic
// host-headroom). Not configurable (the env override is gone, #1907). 20 mirrors the issue's
// suggested default. Real games still preempt shadow at the coordinator (the
// registry does not fight that — this cap bounds only the registry's own
// in-flight shadow spawns).
const DefaultMaxShadowGames = 20

// DefaultEffectiveConcurrency is the default cap on the number of in-flight
// shadow spawns the registry will hold at once — the EFFECTIVE concurrency, as
// distinct from the registry's capacity bookkeeping (DefaultMaxShadowGames).
//
// #1018 (option b) taught the game-coordinator to run CONCURRENT shadow games:
// the single-game guard now applies to REAL games only, and shadow sessions run
// concurrently up to GAME_MAX_SHADOW_GAMES (coordinator default 4). So the
// registry can keep multiple shadow starts in flight without each over-spawn
// being a doomed "Game already in progress" start. Defaulting to 4 matches the
// coordinator's default shadow cap and makes #1003's same-tick atomic pair
// reservation activate (it needs effective concurrency >= 2 to reserve BOTH arms
// in one tick → tighter CRN pairing). 4 is a deliberately conservative >1 value
// for a single-interpreter coordinator on Pi 5-class hardware; it is well under
// the DefaultMaxShadowGames=20 registry ceiling and is clamped to it. An
// operator raises/lowers it via the experiment_shadow_effective_concurrency flag
// (this constant equals its flag default; the env fallback is gone, #1907). Backpressure
// still applies: a coordinator-at-capacity rejection is release+retry next tick.
const DefaultEffectiveConcurrency = 4

// Arm names. The experimental arm resolves the candidate value; the control arm
// falls through to the existing default (within-experiment baseline). Both are
// game_kind="shadow" (the hard invariant). These mirror the targeting contract
// (ArmExperimental in targeting.go) and #975's lib values.
const (
	ArmControl = "control"
)

// DefaultArms is the standard two-arm split (epic #982 decision 5: a
// within-experiment shadow control arm IS the decision basis, with the recent-real
// baseline as a sanity cross-check). Round-robin allocation interleaves them.
func DefaultArms() []string { return []string{ArmExperimental, ArmControl} }

// Intent is the agent's definition of an experiment — the structured hypothesis
// the registry persists to the journal at PROPOSED. It is the registry-level
// counterpart to a Proposal (which carries only the flag write): an Intent adds
// the experiment framing (objective, arms). Minting the
// experiment_id is the registry's job (Declare), so it is NOT a field here.
type Intent struct {
	// Hypothesis is the agent's reasoning: what change + why it might help.
	Hypothesis string
	// FlagKey is the game flag under experiment. It is the Proposal's FlagKey.
	FlagKey string
	// ExperimentalValue is the candidate value the experimental arm resolves.
	ExperimentalValue any
	// Objective is the fitness objective being optimized (one of "endurance"/"balanced"/"accelerate"; chaos is rejected at Declare, #1015).
	Objective string
	// Arms are the arm names. Empty ⇒ DefaultArms() (experimental + control).
	Arms []string
}

// MintExperimentID returns a fresh experiment id in the "exp_<uuid hex[:12]>"
// shape, mirroring the game_<...> game-id shape (#845). Exported so a caller can
// pre-mint an id (e.g. to correlate a span) before Declare.
func MintExperimentID() string {
	return "exp_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

// entry is the registry's in-memory view of one experiment. It is a cache of the
// journal: status + the (game_id → arm) map + the round-robin cursor. The journal
// remains authoritative; on restart Rehydrate rebuilds every entry from disk.
type entry struct {
	id      string
	journal *journal.Journal
	status  Status
	flagKey string
	// teardownBegun is set (under r.mu) before teardown strips the targeting. A
	// Start whose targeting write lands after that point strips it again (#2340).
	teardownBegun bool
	// arms is the experiment's arm list (from intent), used for round-robin.
	arms []string
	// games maps a live (in-flight) shadow game_id → its arm. A game is removed on
	// conclusion. len(games) is the experiment's current in-flight shadow count.
	games map[string]string
	// gamePairs maps a bound (in-flight) shadow game_id → its Common-Random-Numbers
	// pair index (#1003), recorded at bind alongside the game_assigned event. It lets
	// ConcludeGame / ReleaseGame recover the pair index for the game_concluded /
	// game_released journal event so the paired-difference verdict (#1004) can match
	// the two games of a pair. Reservation placeholders are NOT tracked here (they
	// carry no real game_id yet); the entry is added in bindGame and removed when the
	// game concludes or is released. It is rebuilt by Rehydrate's empty-in-flight
	// reset just like games.
	gamePairs map[string]int
	// armCursor indexes arms for round-robin arm assignment within the experiment,
	// so an experiment fills its arms evenly (experimental, control, experimental…).
	//
	// CRN pairing (#1003): consecutive arms form (experimental, control) PAIRS. The
	// pair index is armCursor / len(arms) and the shared seed = pairSeed(id,
	// pairIndex), so both arms of a pair derive the SAME seed automatically (it only
	// changes at a pair boundary). The pair index is stamped on each game_assigned
	// journal event (Event.PairIndex) so the paired-difference verdict (#1004) can
	// recover which two games share a seed. The seed is a pure function of
	// (experimentID, pairIndex) — NEVER a clock — so it is reproducible.
	armCursor int

	// promotionInFlight is set while the experiment's promotion runs and its own
	// promotion event is not journaled yet; RecordPromotion holds later outcomes in
	// laterPromotions until then (the re-watch may journal first).
	promotionInFlight bool
	laterPromotions   []PromotionResult
	// recommendOwner is the writer owning the real overlay when the experiment's
	// promotion was refused with 412 writer_mismatch (UC-043 A19, #2276). Non-empty
	// = every progress snapshot carries the recommendation.
	recommendOwner string
	// endReason is the teardown reason of a terminal experiment; a recommendation
	// snapshot repeats it as its end reason (the dashboard reason line).
	endReason string

	// span is the long-lived experiment ROOT span (#1188, epic #1181 PR2): the
	// agent.experiment span started when the experiment becomes RUNNING (Start /
	// Rehydrate) and ended on the terminal transition (teardown). spanCtx is its
	// SpanContext, exposed via ExperimentSpanContext so the per-experiment analysis
	// spans (retro / summary, emitted from other components) can re-parent under it.
	//
	// LIFECYCLE INVARIANT (no leak): the span is started exactly once per entry (the
	// startSpanLocked guard is idempotent) and End()ed exactly once, in teardown — the
	// SINGLE terminal chokepoint every terminal transition (concluded → discarded/done,
	// aborted) routes through. A nil span (tracer unwired in tests, or never started) is
	// always safe to End/read.
	//
	// RESTART DISCONTINUITY (#1188): the SpanContext is NOT persisted. A rehydrated
	// experiment (Rehydrate, after a restart) gets a BRAND-NEW root span with a fresh
	// trace_id, so its post-restart analysis spans hang under the new root — the
	// pre-restart trace is a separate, already-closed trace. This is deliberate for this
	// cut: restoring a SpanContext across a process boundary would require persisting +
	// re-injecting trace ids the journal does not record, for a span that can no longer
	// be appended to anyway.
	span    trace.Span
	spanCtx trace.SpanContext

	// pin is the experiment's pinned archetype selection (UC-046 step 2): nil until
	// its first allocation pass resolves it; restored from the journal on rehydrate
	// (BR-015).
	pin *patterngame.Pin
	// pairReruns counts reruns per pair, any reason; pairSkipped marks archetypes
	// given up after maxPairReruns (BR-015).
	pairReruns  map[int]int
	pairSkipped map[int]bool
	// patternGames / patternTimeouts / patternAgentEnded count finished pattern
	// games, timeouts and agent ends (A14) per arm (BR-013 reporting).
	patternGames      map[string]int
	patternTimeouts   map[string]int
	patternAgentEnded map[string]int
	// calibration is the per-arm prediction tally (UC-043 A21, BR-010, #2498):
	// in memory only, never journaled, never read by the verdict; a restart or a
	// rehydrate starts it empty. pendingCalibration holds a bound game's tally
	// (keyed by game id) until ConcludeGame folds the game's fitness; a released
	// or torn-down game drops it, so only folded games count.
	calibration        map[string]PredictionTally
	pendingCalibration map[string]PredictionTally
}

// inFlight returns the experiment's current count of live (unconcluded) shadow
// games — the slots it occupies against the global cap.
func (e *entry) inFlight() int { return len(e.games) }

// Registry is the agent-owned experiment coordinator (design §7). Safe for
// concurrent use: a single mutex serializes the lifecycle/capacity book-keeping,
// matching the LoopSet/Multiplexer pattern. The journal handles their own
// locking; the registry holds at most ONE journal handle per experiment (the
// single-writer-per-experiment invariant the journal package requires).
type Registry struct {
	root string
	// onTerminal, when set, is told an experiment's id once it reaches a terminal
	// status (teardown), outside the registry lock. The cross-game window uses it to
	// evict the experiment's linked shadow set (UC-042 BR-005). Set before serving.
	onTerminal func(experimentID string)
	// maxCap is the registry's capacity-bookkeeping cap on total in-flight shadow
	// games across all experiments (DefaultMaxShadowGames). It governs the fair
	// per-experiment share split.
	maxCap int
	// effectiveCap bounds the number of in-flight spawns the registry actually
	// holds at once — the coordinator's real concurrency (#998). It is <= maxCap;
	// the registry stops allocating once in-flight reaches it, so it never hammers
	// the single-game coordinator with doomed concurrent starts.
	effectiveCap int
	spawner      ShadowSpawner
	verdict      CohortVerdict
	promo        Promoter
	target       TargetingWriter
	clock        func() time.Time
	log          *slog.Logger
	// tracer emits the long-lived agent.experiment ROOT span (#1188). nil-safe like
	// the #1142 experiment_loop tracer: a nil tracer disables the root span entirely
	// (every start/end/read is guarded), so the registry stays fully functional in
	// unit tests that do not wire telemetry. Production injects otel.Tracer(...).
	tracer trace.Tracer

	// shutdown is the context whose cancellation means the agent's graceful stop has
	// begun (UC-043 A20, #2332); the experiment loop sets it to its run context. Once
	// it is cancelled the registry declares and starts no experiment and a conclusion
	// does not call the promoter. Unset: never stopping (test wiring).
	shutdown atomic.Pointer[context.Context]

	mu      sync.Mutex
	entries map[string]*entry
	// progress is the newest snapshot per experiment for GET /experiments/state
	// (progress.go): every live one plus the newest finished ones.
	progress map[string]ProgressSnapshot
	// onProgress, when set (tests), sees every recorded snapshot in order.
	onProgress func(ProgressSnapshot)
	// rrCursor is the cross-experiment round-robin cursor for fair capacity
	// allocation: AllocateAndSpawn rotates the start of the live-experiment scan so
	// no single experiment is always served first (equal-share, no starvation).
	rrCursor int
	// releaseTombstones records game_ids whose terminal callback (ReleaseGame) fired
	// BEFORE the registry bound the game_id into e.games — the bind-vs-fast-terminal
	// race (#1014). A game that hits a terminal outcome the instant it starts (e.g.
	// game_error at admission) fires its TerminalCallback while only the reservation
	// placeholder is in e.games, so findGameLocked cannot match it and ReleaseGame
	// would no-op, then bindGame would record the game_id permanently and leak the
	// slot forever. To close the race, ReleaseGame for an unfound game_id leaves a
	// tombstone here ONLY WHILE A SPAWN IS IN FLIGHT (spawnsInFlight > 0); bindGame
	// checks it and, on a hit, drops the reservation WITHOUT binding (freeing the
	// slot, non-counting) and consumes the tombstone.
	//
	// BOUNDEDNESS: a tombstone is only meaningful for a bind that is still coming, and
	// a bind can only be pending while a spawn is in flight. So we (1) refuse to record
	// a tombstone when spawnsInFlight == 0 (an unfound ReleaseGame then is provably a
	// stale/duplicate call — already concluded or already released — not the race), and
	// (2) clear the whole map whenever spawnsInFlight returns to 0 (no bind can be
	// pending, so any survivor is a proven orphan). The map is therefore bounded by the
	// number of CONCURRENT in-flight spawns — genuinely O(in-flight) — and is empty
	// whenever the registry is quiescent. This is what makes the conclude-then-release
	// and double-release orderings (which would otherwise leave an un-consumable
	// tombstone) self-cleaning.
	releaseTombstones map[string]string // game_id → release reason
	// spawnsInFlight counts spawns currently between nextAllocation's reservation and
	// bindGame (the only window in which a real game_id can be reported-terminal before
	// it is bound). It gates and bounds releaseTombstones (see above).
	spawnsInFlight int

	// patterns pins archetype selections (UC-046). Production always wires it; nil
	// is a test-only seam for the allocation mechanics (the production spawner
	// refuses any spawn without an archetype).
	patterns PatternPinner
	// holdGate checks an experiment on a hold flag against the current defaults
	// (UC-043 A17); nil skips the check (test seam).
	holdGate HoldGate
	// flagDigest computes UC-046 BR-011's digest for an arm before it spawns.
	flagDigest func(ctx context.Context, experimentID, arm string) string
	// patternTracker force-ends in-flight pattern games on the kill switch (A12).
	patternTracker *patterngame.Tracker
}

// RegistryConfig configures a Registry. Every seam may be nil — a nil seam
// degrades to a safe but DEGRADED behavior (not merely an inert no-op), so a
// Registry is always safe to construct in a test or before a follow-up wires the
// real implementation. Production always injects all four seams. The degraded
// behaviors are:
//
//   - nil Spawner   ⇒ no game is ever started (the no-op stub errors), so the
//     experiment runs but never accrues shadow games.
//   - nil Verdict   ⇒ the verdict is never recomputed, so the experiment NEVER
//     reaches CONCLUDED on its own — it runs until explicitly aborted. (It does
//     not silently conclude; it simply has no terminal trigger.)
//   - nil Promoter  ⇒ a concluded+promote experiment is recorded as routed but the
//     promotion is a no-op; teardown still proceeds (capacity is freed).
//   - nil Targeting ⇒ Start still transitions the experiment to RUNNING, but with
//     NO shadow-scoped flag rule written. Its games therefore resolve the flag
//     DEFAULT (the non-experimental value): a silent, non-experimental no-op
//     experiment that produces no arm differentiation. Production always injects
//     #977's writer; a nil writer is a test/degraded shape only.
type RegistryConfig struct {
	// Root is the journal experiments root (journal.DirFromEnv() in production; a
	// t.TempDir in tests). Empty ⇒ journal.DefaultDir.
	Root string
	// MaxShadowGames is the fixed concurrent-shadow-game cap. Non-positive ⇒
	// DefaultMaxShadowGames.
	MaxShadowGames int
	// EffectiveConcurrency bounds the in-flight spawns the registry holds at once
	// to the coordinator's REAL concurrency (#998). The game-coordinator runs one
	// game at a time, so allocating more than this many concurrent starts just
	// produces doomed "Game already in progress" rejections. Non-positive ⇒
	// DefaultEffectiveConcurrency. It is clamped to <= MaxShadowGames.
	EffectiveConcurrency int
	// Spawner / Verdict / Promoter / Targeting are the injected seams.
	Spawner   ShadowSpawner
	Verdict   CohortVerdict
	Promoter  Promoter
	Targeting TargetingWriter
	// Clock is injected so Declare/AppendEvent timestamps are deterministic in
	// tests. nil ⇒ time.Now (UTC).
	Clock func() time.Time
	// Log nil ⇒ slog.Default().
	Log *slog.Logger
	// Tracer emits the agent.experiment ROOT span (#1188). nil ⇒ otel.Tracer(...) with
	// the agent instrumentation scope (the production default; the global provider is a
	// no-op until configured, so this is safe). A test may inject a recording tracer to
	// assert the root span, or leave it nil to disable the span. The root span and its
	// re-parenting are OBSERVABILITY ONLY — a nil tracer changes no lifecycle behavior.
	Tracer trace.Tracer
	// Patterns pins the experiment.mock_archetypes selection in each experiment's
	// first allocation pass (UC-046). Production always wires it; nil is a test-only
	// seam for the allocation mechanics.
	Patterns PatternPinner
	// HoldGate returns why an experiment on a hold flag cannot run on the current
	// defaults ("" = it can), at its pin and at every tick (UC-043 A17 step 2). An
	// error keeps the experiment. nil skips the check (test seam).
	HoldGate HoldGate
	// FlagDigest computes the arm's resolved-values flag digest (UC-046 BR-011)
	// before the spawn; nil ⇒ no digest.
	FlagDigest func(ctx context.Context, experimentID, arm string) string
	// PatternTracker is the live pattern-game tracker the kill switch force-ends
	// games through (UC-046 A12). nil ⇒ no force-end.
	PatternTracker *patterngame.Tracker
}

// NewRegistry builds a Registry over the injected seams. It does NOT touch the
// filesystem or rehydrate — call Rehydrate after construction to rebuild the live
// set from the journal.
func NewRegistry(cfg RegistryConfig) *Registry {
	maxCap, effectiveCap := registryCaps(cfg)
	clock, log, tracer := registryDefaults(cfg)
	return &Registry{
		root:              cfg.Root,
		maxCap:            maxCap,
		effectiveCap:      effectiveCap,
		spawner:           cfg.Spawner,
		verdict:           cfg.Verdict,
		promo:             cfg.Promoter,
		target:            cfg.Targeting,
		clock:             clock,
		log:               log,
		tracer:            tracer,
		entries:           make(map[string]*entry),
		releaseTombstones: make(map[string]string),
		patterns:          cfg.Patterns,
		holdGate:          cfg.HoldGate,
		flagDigest:        cfg.FlagDigest,
		patternTracker:    cfg.PatternTracker,
	}
}

// registryCaps is NewRegistry's capacity step: the hard shadow-game cap and the
// effective concurrency, defaulted and clamped.
func registryCaps(cfg RegistryConfig) (maxCap, effectiveCap int) {
	maxCap = cfg.MaxShadowGames
	if maxCap <= 0 {
		maxCap = DefaultMaxShadowGames
	}
	effectiveCap = cfg.EffectiveConcurrency
	if effectiveCap <= 0 {
		effectiveCap = DefaultEffectiveConcurrency
	}
	// The registry can never hold more in-flight than its capacity bookkeeping
	// allows, so clamp the effective cap to maxCap (a misconfigured higher value
	// would otherwise silently never bind).
	if effectiveCap > maxCap {
		effectiveCap = maxCap
	}
	return maxCap, effectiveCap
}

// registryDefaults is NewRegistry's seam-default step: the clock, logger and
// tracer a config left nil.
func registryDefaults(cfg RegistryConfig) (clock func() time.Time, log *slog.Logger, tracer trace.Tracer) {
	clock = cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	log = cfg.Log
	if log == nil {
		log = slog.Default()
	}
	tracer = cfg.Tracer
	if tracer == nil {
		// Default to the agent instrumentation scope (#1188). The global provider is a
		// no-op until the agent configures OTEL, so this never emits spurious spans in a
		// test that does not set up a provider.
		tracer = otel.Tracer(experimentInstrumentationName)
	}
	return clock, log, tracer
}

// experimentInstrumentationName scopes the registry's root-span tracer. It matches
// the decision package's instrumentationName so every agent span shares one
// instrumentation scope in Jaeger.
const experimentInstrumentationName = "github.com/joustmania/agent"

// Declare defines a new experiment: it mints an experiment_id, persists the
// immutable intent to the journal (journal.Create), records it as PROPOSED in the
// registry, and returns the id. It does NOT spawn games or write targeting yet —
// Start moves the experiment to RUNNING and writes the shadow targeting. Declare
// fails if the journal already has this id (re-mint collision is astronomically
// unlikely; a real collision is surfaced, not silently clobbered).
func (r *Registry) Declare(in Intent) (string, error) {
	if err := validateIntent(in); err != nil {
		return "", err
	}

	arms := in.Arms
	if len(arms) == 0 {
		arms = DefaultArms()
	}
	id := MintExperimentID()

	// #1731: hold r.mu from before the journal file exists until the entry is in
	// r.entries. Rehydrate loads under r.mu and skips an id already registered, so
	// a declare racing the first enable's Rehydrate (proposer goroutine vs the
	// experiment tick) is either fully registered when Rehydrate sees its journal,
	// or not on disk yet. Otherwise Rehydrate would read the fresh PROPOSED journal
	// as a leftover of a crash and abort it with start_failed (UC-043 A8).
	r.mu.Lock()
	defer r.mu.Unlock()
	// UC-043 A20 (#2332): checked under r.mu, where AbortAll takes its snapshot, so
	// an experiment declared before the stop began is in that snapshot and none is
	// declared after it.
	if r.shuttingDown() {
		return "", fmt.Errorf("registry: declare experiment: %w", ErrShuttingDown)
	}
	// Keep this call inside the lock: TestDeclareCreatesJournalUnderRegistryLock_1731
	// probes r.mu through r.clock().
	now := r.clock()

	jrnl, err := journal.Create(r.root, journal.Intent{
		ExperimentID:      id,
		CreatedAt:         now,
		Hypothesis:        in.Hypothesis,
		FlagKey:           in.FlagKey,
		ExperimentalValue: in.ExperimentalValue,
		Objective:         in.Objective,
		Arms:              arms,
	})
	if err != nil {
		return "", fmt.Errorf("registry: declare experiment: %w", err)
	}

	r.registerDeclaredLocked(id, jrnl, in, arms)
	return id, nil
}

// validateIntent is Declare's validation step: it rejects an intent that can
// never produce a useful experiment before any id is minted or journal written.
func validateIntent(in Intent) error {
	// UC-043 BR-002 (no chaos experiments).
	// Fail-fast on a non-experimentable objective (#1015). chaos has no fitness
	// function — every game on it would fold the worst-case 0 and the experiment
	// would run its whole budget (up to the #1001 target) only to conclude
	// inconclusive. Reject it at declaration so the wasted run never starts; the
	// experimentable objectives (endurance/balanced/accelerate, and the empty
	// objective which folds as balanced) pass through unchanged.
	if !decision.IsExperimentable(in.Objective) {
		return fmt.Errorf("registry: declare experiment: objective %q is not experimentable: no fitness function", in.Objective)
	}
	// UC-043 A17 step 1 (#2096): shield.activation is experimented on only toward
	// hold, whoever declares it (the declarer forces it; a proposer cannot pick another
	// value and experiment its way out of hold).
	if err := checkHoldValue(in.FlagKey, in.ExperimentalValue); err != nil {
		return fmt.Errorf("registry: declare experiment: %w", err)
	}
	return nil
}

// registerDeclaredLocked is Declare's state step, called under r.mu: the PROPOSED
// entry goes into r.entries and its decision event into the journal.
func (r *Registry) registerDeclaredLocked(id string, jrnl *journal.Journal, in Intent, arms []string) {
	r.entries[id] = &entry{
		id:        id,
		journal:   jrnl,
		status:    StatusProposed,
		flagKey:   in.FlagKey,
		arms:      arms,
		games:     make(map[string]string),
		gamePairs: make(map[string]int),
	}
	// Record the PROPOSED transition as a decision event for the audit trail.
	r.recordDecision(jrnl, StatusProposed, "experiment declared")
	r.log.Info("experiment.declared", "experiment_id", id, "flag_key", in.FlagKey, "objective", in.Objective)
}

// SetShutdownContext sets the context whose cancellation marks the start of the
// agent's graceful stop (UC-043 A20, #2332). The experiment loop calls it with its
// run context before any proposer or tick can reach the registry's write paths.
func (r *Registry) SetShutdownContext(ctx context.Context) { r.shutdown.Store(&ctx) }

// shuttingDown reports whether the graceful stop has begun.
func (r *Registry) shuttingDown() bool {
	p := r.shutdown.Load()
	return p != nil && (*p).Err() != nil
}

// SetTerminalHook registers f to be told an experiment's id when it reaches a
// terminal status (UC-042 BR-005: its linked shadow history is evicted). Call it
// before the registry serves; f must not call back into the registry.
func (r *Registry) SetTerminalHook(f func(experimentID string)) { r.onTerminal = f }

// --- read accessors (telemetry / tests) ---

// Status returns the current status of an experiment, or ("", false) if unknown.
func (r *Registry) Status(id string) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return "", false
	}
	return e.status, true
}

// InFlight returns the experiment's current in-flight shadow-game count (bound
// games + reservations), or 0 if unknown.
func (r *Registry) InFlight(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil {
		return e.inFlight()
	}
	return 0
}

// TotalInFlight returns the total in-flight shadow games across all experiments —
// the registry's current draw against the cap.
func (r *Registry) TotalInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlightLocked()
}

// Capacity returns the configured fixed shadow-game cap (the capacity-bookkeeping
// bound, DefaultMaxShadowGames).
func (r *Registry) Capacity() int { return r.maxCap }

// EffectiveCapacity returns the effective in-flight concurrency cap (#998) — the
// number of concurrent shadow spawns the registry will actually hold, bounded to
// the coordinator's real concurrency (the experiment_shadow_effective_concurrency
// flag, default 4) and clamped to Capacity.
func (r *Registry) EffectiveCapacity() int { return r.effectiveCap }

// Reconfigure live-updates the shadow concurrency cap and the verdict'"'"'s min-pairs
// gate from the live flags. It only changes the cap the allocator reads next; an
// in-flight game is untouched. A non-positive value is ignored (a zero cap would
// stall every experiment; the verdict ignores a non-positive min-pairs itself).
func (r *Registry) Reconfigure(effectiveConcurrency, minPairs int) {
	r.mu.Lock()
	if effectiveConcurrency > 0 {
		eff := effectiveConcurrency
		if eff > r.maxCap {
			eff = r.maxCap // never exceed the capacity-bookkeeping bound.
		}
		r.effectiveCap = eff
	}
	r.mu.Unlock()

	// The verdict has its own lock; mirror outside r.mu.
	if v, ok := r.verdict.(*Verdict); ok {
		v.SetThresholds(minPairs)
	}
}

// ReconfigureVerdictFloats live-mirrors the float verdict gates onto the verdict;
// it touches no registry state.
func (r *Registry) ReconfigureVerdictFloats(effectThreshold, minRawEffect, sdFloor float64) {
	if v, ok := r.verdict.(*Verdict); ok {
		v.SetFloatThresholds(effectThreshold, minRawEffect, sdFloor)
	}
}

// Live returns the ids of experiments that may be allocated shadow capacity —
// i.e. those in RUNNING (targeting written, ready to accrue games), sorted. A
// PROPOSED experiment occupies a registry slot but is not yet spawnable, so it is
// not returned here.
func (r *Registry) Live() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.liveIDsLocked()
}

// ActiveFlags returns the set of flag keys that have at least one LIVE
// (non-terminal: PROPOSED or RUNNING) experiment. The dynamic
// declarer (#995) uses it to avoid stacking a second concurrent experiment on a
// flag that is already under test. Any other flag torn down (terminal) is not
// included — its targeting branch is stripped, so it is free to experiment on again.
func (r *Registry) ActiveFlags() map[string]struct{} {
	out := map[string]struct{}{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if !e.status.IsTerminal() {
			out[e.flagKey] = struct{}{}
		}
	}
	return out
}

// LiveCount returns the number of LIVE (non-terminal) experiments the registry is
// managing — PROPOSED + RUNNING + CONCLUDED/PROMOTING (anything not yet torn
// down). The dynamic declarer (#995) uses it as the over-declaration backstop:
// it stops declaring once the live set fills the concurrent-experiment budget.
func (r *Registry) LiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.entries {
		if !e.status.IsTerminal() {
			n++
		}
	}
	return n
}

// ExperimentSpanContext returns the SpanContext of an experiment's long-lived
// agent.experiment ROOT span (#1188), for re-parenting that experiment's analysis
// spans (retro / summary) under it from components OUTSIDE the registry. It is a
// clean READ accessor: it touches no lifecycle/mutation state, takes the lock only to
// read the entry's recorded SpanContext, and never mutates.
//
// ok=false (and the zero SpanContext) when the experiment is unknown, has no root span
// (tracer unwired, or already torn down + span ended), or the SpanContext is invalid —
// every "no usable parent" case. Callers fall back to their CURRENT behavior on
// ok=false, so a NON-experiment game (empty/unknown experiment_id) never re-parents:
// graceful, never breaking span emission.
func (r *Registry) ExperimentSpanContext(id string) (trace.SpanContext, bool) {
	r.mu.Lock()
	e := r.entries[id]
	var sc trace.SpanContext
	if e != nil {
		sc = e.spanCtx
	}
	r.mu.Unlock()
	if e == nil || !sc.IsValid() {
		return trace.SpanContext{}, false
	}
	return sc, true
}

// CompactView returns the bounded journal view for an experiment (intent +
// per-arm aggregates + recent tail) — the read surface the dashboard / LLM
// consume. ok=false if unknown.
func (r *Registry) CompactView(id string) (journal.CompactView, bool) {
	r.mu.Lock()
	e := r.entries[id]
	r.mu.Unlock()
	if e == nil {
		return journal.CompactView{}, false
	}
	return e.journal.CompactView(), true
}

// --- locked helpers ---

// inFlightLocked sums in-flight games across all experiments. Assumes r.mu held.
func (r *Registry) inFlightLocked() int {
	total := 0
	for _, e := range r.entries {
		total += e.inFlight()
	}
	return total
}

// liveIDsLocked returns the sorted ids of PROPOSED/RUNNING experiments. Assumes
// r.mu held. Only RUNNING experiments are spawnable, but PROPOSED ones still
// occupy a registry slot; nextAllocation filters to RUNNING via the share check
// implicitly — only RUNNING experiments are returned here for allocation fairness.
func (r *Registry) liveIDsLocked() []string {
	ids := make([]string, 0, len(r.entries))
	for id, e := range r.entries {
		if e.status == StatusRunning {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// IsBound reports whether gameID is an in-flight game bound to one of this
// registry's experiments (UC-046 BR-016, #1405). It is read-only: the experiment
// loop uses it to tell its own shadow games, whose telemetry end waits for the
// runner's terminal outcome, from foreign or already-released ids.
func (r *Registry) IsBound(gameID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _, e := r.findGameLocked(gameID)
	return e != nil
}

// ExperimentFlag returns the flag under test in a game (UC-040 A40, BR-028; #1975):
// the experiment bound to gameID at spawn, else the experiment experimentID names.
// ok=false when the registry knows neither, or the experiment is in a terminal status.
func (r *Registry) ExperimentFlag(gameID, experimentID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if gameID != "" {
		if _, _, e := r.findGameLocked(gameID); e != nil {
			return e.flagKey, true
		}
	}
	// A torn-down experiment (aborted, concluded, discarded) is not known any more:
	// its leftover games decide as before (UC-040 A40 step 4).
	if e := r.entries[experimentID]; experimentID != "" && e != nil && !e.status.IsTerminal() {
		return e.flagKey, true
	}
	return "", false
}

// findGameLocked looks up the experiment + arm owning a coordinator game_id.
// Assumes r.mu held.
func (r *Registry) findGameLocked(gameID string) (id, arm string, e *entry) {
	for eid, ent := range r.entries {
		if a, ok := ent.games[gameID]; ok {
			return eid, a, ent
		}
	}
	return "", "", nil
}

// recordDecision appends a KindDecision status-transition event. Assumes the
// caller holds r.mu (the journal serializes its own write). Errors are logged,
// never fatal — the in-memory transition already happened and Rehydrate would
// re-derive from the prior durable state.
func (r *Registry) recordDecision(jrnl *journal.Journal, status Status, note string) {
	r.appendEvent(jrnl, journal.Event{
		Kind:   journal.KindDecision,
		Status: string(status),
		Note:   note,
	})
}

// appendEvent stamps the event time from the registry clock (if unset) and
// appends it to the journal, logging any error. Centralizes the time injection so
// every registry-emitted event carries a timestamp.
func (r *Registry) appendEvent(jrnl *journal.Journal, e journal.Event) {
	_ = r.appendEventErr(jrnl, e)
}

// appendEventErr is appendEvent that also returns the error, for a caller that
// must know whether the event is durable (UC-043 BR-007).
func (r *Registry) appendEventErr(jrnl *journal.Journal, e journal.Event) error {
	if e.At.IsZero() {
		e.At = r.clock()
	}
	err := jrnl.AppendEvent(e)
	if err != nil {
		r.log.Warn("experiment.journal_append_failed", "kind", string(e.Kind), "error", err)
	}
	return err
}

// lastForeignOwner returns the writer of the latest promotion event the flag
// service refused with 412 writer_mismatch, or "" (UC-043 A19, #2276).
func lastForeignOwner(events []journal.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if p := events[i].Promotion; p != nil && p.ForeignOwner != "" {
			return p.ForeignOwner
		}
	}
	return ""
}

// teardownReasonOf returns the teardown reason (the note of the last outcome event).
func teardownReasonOf(events []journal.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == journal.KindOutcome {
			return events[i].Note
		}
	}
	return ""
}
