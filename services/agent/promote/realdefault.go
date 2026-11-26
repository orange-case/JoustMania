package promote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/joustmania/agent/flagservice"
)

// realdefault.go is the autonomous mode's REAL default change — the SEPARATE,
// higher-privilege write the safety rail isolates from the shadow
// experiment.Writer (W5, epic #1407).
//
// WHY A SEPARATE WRITER: experiment.Writer only ever writes an experiment-arm
// value, which the flag service renders for non-real games of that experiment
// only, so it CANNOT change what a real game resolves. Autonomous promotion
// deliberately DOES change what real games resolve, so it is a DIFFERENT,
// explicitly-gated mechanism: PUT /game/{flag} with scope.game_kind=real (a
// "real" overlay the flag service renders as a defaultVariant substitution).
//
// The overlay lives in the flag service's memory: a flag-service restart drops
// it. The durable path for a promoted default is a human change (FR-064,
// C-007, FR-138). A revert is DELETE /game/{flag}?game_kind=real, which returns
// the flag to whatever the baseline (and any admin or stage overlay) says — the
// exact pre-promotion state, without the value snapshots the file writer needed
// (#1065). The target is always the game flag set (#1293: no configurable path).
//
// NewRealDefaultWriterFromEnv always builds it, because the kill switch needs it
// as reverter and lister (an overlay from before a restart is still removed);
// main.go wires it as the WRITER only when an objective is validated (UC-043
// A23), and the write is additionally behind the agent kill-switch
// (Config.Enabled, checked in promoteAutonomous).

// gameFlagSet is the only flag set a promotion writes.
const gameFlagSet = "game"

// promotionWriter is the informational writer label on promotion writes (D11).
const promotionWriter = "promotion"

// realDefaultService is the part of the flag-service client the writer uses.
type realDefaultService interface {
	Put(ctx context.Context, set, flag string, w flagservice.Write) (flagservice.Result, error)
	Delete(ctx context.Context, set, flag string, query url.Values) error
	GetFlag(ctx context.Context, set, flag string) (flagservice.FlagView, error)
	GetSet(ctx context.Context, set string) (flagservice.SetView, error)
}

// serviceRealDefaultWriter writes and reverts the real default of a game flag
// through the flag service. It implements RealDefaultWriter and
// RealDefaultReverter.
type serviceRealDefaultWriter struct {
	svc realDefaultService
	log *slog.Logger
	mu  sync.Mutex
}

// NewRealDefaultWriterFromEnv builds the flag-service real-default writer
// (FLAG_SERVICE_URL; the flag set is always `game`, #1293). It is always built:
// as reverter it backs the kill switch. Whether it may also WRITE is main.go's
// decision (a validated objective, UC-043 A23).
func NewRealDefaultWriterFromEnv(log *slog.Logger) *serviceRealDefaultWriter {
	if log == nil {
		log = slog.Default()
	}
	return newServiceRealDefaultWriter(flagservice.NewFromEnv(), log)
}

func newServiceRealDefaultWriter(svc realDefaultService, log *slog.Logger) *serviceRealDefaultWriter {
	return &serviceRealDefaultWriter{svc: svc, log: log}
}

// SetRealDefault makes real games resolve value for flagKey: a "real" overlay
// on the flag service. The registry bounds the value (a declared variant's
// value); a value whose JSON kind differs from the current default is refused
// here first (#936 review note 2), as the last line before real players.
//
// WRITE PRECONDITION (#2260, UC-043 A19): the PUT carries
// ?if_writer_in=promotion, so the flag service writes only over a previous
// promotion's overlay or where none is present, compared and written under its
// lock. Another writer's real overlay is refused with 412 writer_mismatch and
// left in place; SetRealDefault then returns ErrRealOverlayOwnedByAnotherWriter
// and the promotion is not made (no retry).
func (w *serviceRealDefaultWriter) SetRealDefault(ctx context.Context, flagKey string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	view, err := w.svc.GetFlag(ctx, gameFlagSet, flagKey)
	if err != nil {
		return fmt.Errorf("read %s/%s before promotion: %w", gameFlagSet, flagKey, err)
	}
	valRaw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	if curRaw, ok := view.VariantValues[view.EffectiveDefault]; ok {
		if ck, nk := jsonKind(curRaw), jsonKind(valRaw); ck != "" && nk != "" && ck != nk {
			return fmt.Errorf("refusing real-default write for %q: value kind %q != current default %q kind %q", flagKey, nk, view.EffectiveDefault, ck)
		}
	}
	res, err := w.svc.Put(ctx, gameFlagSet, flagKey, flagservice.Write{
		Value:  value,
		Scope:  flagservice.Scope{GameKind: "real"},
		Writer: promotionWriter,
		Reason: "autonomous_promotion",
		// The promotion's own overlay ("promotion") or none (""), #2260.
		IfWriterIn: []string{promotionWriter, ""},
	})
	if flagservice.IsReason(err, flagservice.ReasonWriterMismatch) {
		owner := ""
		if view.RealOverlay != nil {
			owner = view.RealOverlay.Writer
		}
		w.log.Warn("code_improvement promotion not made: another writer owns the real overlay (writer_mismatch); nothing written",
			"flag", flagKey, "owner", owner)
		return fmt.Errorf("promote %s/%s: %w", gameFlagSet, flagKey, &foreignOverlayError{Owner: owner})
	}
	if err != nil {
		return fmt.Errorf("promote %s/%s: %w", gameFlagSet, flagKey, err)
	}
	w.log.Warn("code_improvement.autonomous changing the REAL default (#936)",
		"flag", flagKey, "variant", res.Variant, "epoch", res.Epoch)
	return nil
}

// RevertRealDefault removes the promotion's real overlay, so real games resolve
// the pre-promotion default again (#1065). With no promotion overlay present (a
// flag-service restart dropped it) real games already resolve the pre-promotion
// default: the revert writes nothing and succeeds (PR #1417 review).
//
// COMPARE-AND-DELETE (#1877, closes the #1753 window): the DELETE carries
// ?if_writer=promotion, so the flag service clears the real overlay only while
// its writer is still "promotion", atomically under its lock. Another writer's
// overlay set between the read and the delete is refused with 412
// writer_mismatch and left in place; the revert is then dropped like the
// "no promotion overlay" case (logged once, nil returned). Every overlay this
// agent writes carries writer "promotion", so the precondition never blocks the
// kill switch from clearing one of them.
func (w *serviceRealDefaultWriter) RevertRealDefault(ctx context.Context, flagKey string) error {
	return w.revert(ctx, flagKey)
}

// ErrSupersededByAnotherWriter is returned by a revert whose DELETE the flag service
// refused with 412 writer_mismatch (#1877): nothing was cleared, and the revert
// counts as done. Callers treat it as success; the kill
// switch adds SupersededReasonSuffix to the journaled reason so "the agent cleared
// it" and "another writer owns it" can be told apart.
var ErrSupersededByAnotherWriter = errors.New("real overlay superseded by another writer (writer_mismatch); nothing removed")

// ReasonRealOverlayForeign is the journaled reason (outcome discarded) of a
// promotion the flag service refused because another writer owns the real
// overlay (PUT ?if_writer_in=, 412 writer_mismatch, #2260, UC-043 A19).
const ReasonRealOverlayForeign = "real_overlay_owned_by_another_writer"

// foreignOverlayError wraps ErrRealOverlayOwnedByAnotherWriter with the writer
// that owns the overlay (as read before the write), for the log and the journal.
type foreignOverlayError struct{ Owner string }

func (e *foreignOverlayError) Error() string {
	return fmt.Sprintf("%s (owner %q)", ErrRealOverlayOwnedByAnotherWriter, e.Owner)
}

func (e *foreignOverlayError) Is(target error) bool {
	return target == ErrRealOverlayOwnedByAnotherWriter
}

// foreignReason is ReasonRealOverlayForeign, with " owner=<writer>" appended
// when the owner is known; the fixed reason token stays the prefix.
func foreignReason(err error) string {
	var fe *foreignOverlayError
	if errors.As(err, &fe) && fe.Owner != "" {
		return ReasonRealOverlayForeign + " owner=" + fe.Owner
	}
	return ReasonRealOverlayForeign
}

// foreignOwner is the writer label of a foreignOverlayError ("unknown" when the
// owner could not be read before the write).
func foreignOwner(err error) string {
	var fe *foreignOverlayError
	if errors.As(err, &fe) && fe.Owner != "" {
		return fe.Owner
	}
	return "unknown"
}

// ErrRealOverlayOwnedByAnotherWriter is returned by SetRealDefault when the flag
// service refused the write with 412 writer_mismatch: nothing was written.
var ErrRealOverlayOwnedByAnotherWriter = errors.New(ReasonRealOverlayForeign + " (writer_mismatch); nothing written")

// SupersededReasonSuffix is appended to the kill-switch journal reason when the
// revert was dropped on a writer_mismatch.
const SupersededReasonSuffix = "; superseded by another writer"

func (w *serviceRealDefaultWriter) revert(ctx context.Context, flagKey string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	view, err := w.svc.GetFlag(ctx, gameFlagSet, flagKey)
	if err != nil {
		return fmt.Errorf("read %s/%s before revert: %w", gameFlagSet, flagKey, err)
	}
	if view.RealOverlay == nil || view.RealOverlay.Writer != promotionWriter {
		// No overlay of ours: a flag-service restart dropped it, or another writer's
		// real overlay replaced it. That overlay is not the promotion's to remove
		// (#1704): nothing is deleted.
		w.log.Warn("code_improvement revert: no promotion overlay; nothing to remove",
			"flag", flagKey, "real_overlay_writer", realOverlayWriter(view))
		return nil
	}
	query := url.Values{"game_kind": {"real"}, "writer": {promotionWriter}, "if_writer": {promotionWriter}}
	if err := w.svc.Delete(ctx, gameFlagSet, flagKey, query); err != nil {
		if flagservice.IsReason(err, flagservice.ReasonWriterMismatch) {
			// Another writer replaced the overlay after the read (#1877): it owns
			// the real default now, so the revert is dropped, not retried.
			w.log.Warn("code_improvement revert dropped: another writer owns the real overlay now (writer_mismatch); nothing removed",
				"flag", flagKey)
			return ErrSupersededByAnotherWriter
		}
		return fmt.Errorf("revert %s/%s: %w", gameFlagSet, flagKey, err)
	}
	// 204 also when the overlay vanished between the read and the DELETE (a
	// flag-service restart): the promotion's overlay is gone either way.
	w.log.Warn("code_improvement.autonomous REVERTED: the promotion's real-default overlay is gone (cleared now, or already absent); real games resolve the default beneath it (#1065)", "flag", flagKey)
	return nil
}

// PromotedFlags lists the game flags whose real overlay a promotion wrote
// (writer "promotion"), read from the flag service, so the kill switch reverts
// them even after an agent restart (#1704). Overlays of other writers are never
// listed.
func (w *serviceRealDefaultWriter) PromotedFlags(ctx context.Context) ([]string, error) {
	set, err := w.svc.GetSet(ctx, gameFlagSet)
	if err != nil {
		return nil, fmt.Errorf("list %s overlays: %w", gameFlagSet, err)
	}
	var out []string
	for _, f := range set.Flags {
		if f.RealOverlay != nil && f.RealOverlay.Writer == promotionWriter {
			out = append(out, f.Flag)
		}
	}
	return out, nil
}

// PromotionOverlay reads the value of flagKey's real overlay when a promotion wrote
// it (ok false otherwise), so a revert restored after a restart deletes only its
// own value (UC-043 A17).
func (w *serviceRealDefaultWriter) PromotionOverlay(ctx context.Context, flagKey string) (json.RawMessage, bool, error) {
	view, err := w.svc.GetFlag(ctx, gameFlagSet, flagKey)
	if err != nil {
		return nil, false, err
	}
	if view.RealOverlay == nil || view.RealOverlay.Writer != promotionWriter {
		return nil, false, nil
	}
	return view.RealOverlay.Value, true, nil
}

// realOverlayWriter is the writer of view's real overlay, or "" when none.
func realOverlayWriter(view flagservice.FlagView) string {
	if view.RealOverlay == nil {
		return ""
	}
	return view.RealOverlay.Writer
}

// jsonKind classifies a raw JSON value as number/string/bool/array/object, or ""
// for null/unparseable (kindless — imposes no type constraint). All JSON numerics
// are one "number" kind, so an int promoted value never mismatches a float default.
func jsonKind(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch v.(type) {
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "bool"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "" // null
	}
}
