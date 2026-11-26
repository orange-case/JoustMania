package experiment

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// The progress snapshot of one experiment, recorded under r.mu after every state
// change and served by GET /experiments/state (docs/specs/services.md §5): the
// dashboard's Experiments tab. Not a span: the agent holds the newest snapshot of
// every live experiment and of the newest MaxFinishedProgress finished ones.
const (
	ProgressTriggerStarted       = "started"
	ProgressTriggerGameAssigned  = "game_assigned"
	ProgressTriggerGameConcluded = "game_concluded"
	ProgressTriggerGameReleased  = "game_released"
	ProgressTriggerConcluded     = "concluded"
	ProgressTriggerPromoting     = "promoting"
	ProgressTriggerTerminal      = "terminal"
	ProgressTriggerRehydrate     = "rehydrate"

	// MaxFinishedProgress bounds the finished experiments kept (newest first).
	MaxFinishedProgress = 10
)

// ProgressSnapshot is one experiment as the registry last recorded it.
type ProgressSnapshot struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	FlagKey   string `json:"flag_key"`
	Objective string `json:"objective"`
	TargetN   int    `json:"target_n"`
	Thesis    string `json:"thesis"`
	// ExperimentalValue is the experimental arm's flag value; the control arm
	// resolves the flag's current value.
	ExperimentalValue any              `json:"experimental_value"`
	Verdict           *ProgressVerdict `json:"verdict"`
	// EndReason is the teardown reason, set once the experiment ended.
	EndReason string `json:"end_reason"`
	// Recommendation is set once a promotion was refused because another writer owns
	// the real overlay (UC-043 A19, #2276).
	Recommendation *ProgressRecommendation `json:"recommendation"`
	Arms           []ProgressArm           `json:"arms"`
	// Trigger is the state change that recorded the snapshot (ProgressTrigger*).
	Trigger string `json:"trigger"`
	// UpdatedAtMs is when it was recorded, epoch ms on the agent's clock.
	UpdatedAtMs int64 `json:"updated_at_ms"`
	// TraceID is the agent.experiment trace (the Jaeger drill-down), "" without one.
	TraceID string `json:"trace_id"`
}

// ProgressVerdict is the journal summary's verdict, interim while the experiment runs.
type ProgressVerdict struct {
	Outcome     string  `json:"outcome"`
	Delta       float64 `json:"delta"`
	Significant bool    `json:"significant"`
}

// ProgressRecommendation is the winning value whose promotion was refused.
type ProgressRecommendation struct {
	Flag string `json:"flag"`
	// Value is the JSON encoding of the value.
	Value string `json:"value"`
	// Owner is the real overlay's writer, "unknown" when unreadable.
	Owner string `json:"owner"`
}

// ProgressArm is one arm of a snapshot.
type ProgressArm struct {
	Arm string `json:"arm"`
	// InFlight counts the games bound to the arm and still running (point in time;
	// 0 once the experiment ended, teardown clears them). A spawn reservation does
	// not count.
	InFlight int `json:"in_flight"`
	// Samples / MeanFitness are the journal summary's per-arm count and mean.
	Samples     int     `json:"samples"`
	MeanFitness float64 `json:"mean_fitness"`
	// The report-only shadow calibration (UC-043 A21, calibration.go); the mean
	// Brier score only once a prediction is scored (0 would read as perfect).
	PredictionsScored int      `json:"predictions_scored"`
	PredictionHits    int      `json:"prediction_hits"`
	PredictionBrier   *float64 `json:"prediction_brier"`
}

// emitProgressLocked records the progress snapshot of e. link is the agent.experiment
// span context (teardown passes the one it captured before ending the span).
// endReason is set on the terminal snapshot. Assumes r.mu is held.
func (r *Registry) emitProgressLocked(e *entry, trigger, endReason string, link trace.SpanContext) {
	if e == nil || e.journal == nil {
		return
	}
	intent := e.journal.Intent()
	summary := e.journal.Summary()
	snap := ProgressSnapshot{
		ID:                e.id,
		Status:            string(e.status),
		FlagKey:           intent.FlagKey,
		Objective:         intent.Objective,
		TargetN:           intent.TargetNPerArm,
		Thesis:            intent.Hypothesis,
		ExperimentalValue: intent.ExperimentalValue,
		EndReason:         endReason,
		Arms:              make([]ProgressArm, 0, len(e.arms)),
		Trigger:           trigger,
		UpdatedAtMs:       time.Now().UnixMilli(), // not r.clock: tests count its calls as journal stamps
	}
	if link.IsValid() {
		snap.TraceID = link.TraceID().String()
	}
	if v := summary.Verdict; v != nil {
		snap.Verdict = &ProgressVerdict{Outcome: v.Outcome, Delta: v.Delta, Significant: v.Significant}
	}
	if e.recommendOwner != "" {
		value, err := json.Marshal(intent.ExperimentalValue)
		if err != nil {
			value = []byte(fmt.Sprint(intent.ExperimentalValue))
		}
		snap.Recommendation = &ProgressRecommendation{Flag: intent.FlagKey, Value: string(value), Owner: e.recommendOwner}
	}
	inFlight := make(map[string]int, len(e.arms))
	for gameID, arm := range e.games {
		if !isReservation(gameID) {
			inFlight[arm]++
		}
	}
	for _, arm := range e.arms {
		a := ProgressArm{Arm: arm, InFlight: inFlight[arm]}
		if st := summary.Arms[arm]; st != nil {
			a.Samples, a.MeanFitness = st.Count, st.Mean
		}
		cal := e.calibration[arm]
		a.PredictionsScored, a.PredictionHits = cal.Scored, cal.Hits
		if cal.Scored > 0 {
			brier := cal.Brier()
			a.PredictionBrier = &brier
		}
		snap.Arms = append(snap.Arms, a)
	}
	if r.progress == nil {
		r.progress = map[string]ProgressSnapshot{}
	}
	r.progress[e.id] = snap
	r.pruneProgressLocked()
	if r.onProgress != nil {
		r.onProgress(snap)
	}
}

// pruneProgressLocked drops all but the newest MaxFinishedProgress finished snapshots.
func (r *Registry) pruneProgressLocked() {
	var finished []ProgressSnapshot
	for _, s := range r.progress {
		if Status(s.Status).IsTerminal() {
			finished = append(finished, s)
		}
	}
	if len(finished) <= MaxFinishedProgress {
		return
	}
	sortNewestFirst(finished)
	for _, s := range finished[MaxFinishedProgress:] {
		delete(r.progress, s.ID)
	}
}

// Progress returns the held snapshots, newest first. Read-only; nil-safe.
func (r *Registry) Progress() []ProgressSnapshot {
	if r == nil {
		return []ProgressSnapshot{}
	}
	r.mu.Lock()
	out := make([]ProgressSnapshot, 0, len(r.progress))
	for _, s := range r.progress {
		out = append(out, s)
	}
	r.mu.Unlock()
	sortNewestFirst(out)
	return out
}

func sortNewestFirst(s []ProgressSnapshot) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].UpdatedAtMs != s[j].UpdatedAtMs {
			return s[i].UpdatedAtMs > s[j].UpdatedAtMs
		}
		return s[i].ID < s[j].ID
	})
}
