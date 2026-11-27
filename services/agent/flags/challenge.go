package flags

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/open-feature/go-sdk/openfeature"
)

// ChallengeFlagPrefixes mark the challenge flags (FR-255, docs/specs/flags.md §1.4). Kept equal
// to lib/challenge_flags.py CHALLENGE_FLAG_PREFIXES; lib/tests checks it.
var ChallengeFlagPrefixes = []string{"challenge.", "stowaway."}

// maxChallengeGames bounds the per-game state; past it the map is cleared.
const maxChallengeGames = 256

// challengeGameAttr is the evaluation-context attribute that names the game (game.go).
const challengeGameAttr = "gameId"

// IsChallengeFlag reports whether key starts with a challenge prefix.
func IsChallengeFlag(key string) bool {
	for _, p := range ChallengeFlagPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

type challengeOutcome struct {
	variant string
	value   any
	isError bool
}

// challengeTracesHook passes every evaluation to the upstream traces hook, except that a
// challenge flag's event is recorded only when its (variant, value, is-error) changed for the
// evaluation's game. A panic in the check skips the event, never the evaluation.
type challengeTracesHook struct {
	openfeature.Hook
	mu       sync.Mutex
	last     map[string]map[string]challengeOutcome
	reported sync.Once
}

func newChallengeTracesHook(traces openfeature.Hook) *challengeTracesHook {
	return &challengeTracesHook{Hook: traces, last: map[string]map[string]challengeOutcome{}}
}

func (h *challengeTracesHook) Finally(ctx context.Context, hCtx openfeature.HookContext,
	details openfeature.InterfaceEvaluationDetails, hints openfeature.HookHints,
) {
	if IsChallengeFlag(hCtx.FlagKey()) && !h.changed(hCtx, details) {
		return
	}
	h.Hook.Finally(ctx, hCtx, details, hints)
}

func (h *challengeTracesHook) changed(hCtx openfeature.HookContext, details openfeature.InterfaceEvaluationDetails) (changed bool) {
	defer func() {
		if r := recover(); r != nil {
			h.reported.Do(func() { slog.Warn("challenge flag tracking failed; event skipped", "panic", r) })
			changed = false
		}
	}()
	game := ""
	if v := hCtx.EvaluationContext().Attribute(challengeGameAttr); v != nil {
		game = fmt.Sprint(v)
	}
	outcome := challengeOutcome{
		variant: details.Variant,
		value:   details.Value,
		isError: details.ErrorCode != "" || details.Reason == openfeature.ErrorReason,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	perGame, ok := h.last[game]
	if !ok {
		if len(h.last) >= maxChallengeGames {
			h.last = map[string]map[string]challengeOutcome{}
		}
		perGame = map[string]challengeOutcome{}
		h.last[game] = perGame
	}
	prev, seen := perGame[hCtx.FlagKey()]
	if seen && prev.variant == outcome.variant && prev.isError == outcome.isError &&
		reflect.DeepEqual(prev.value, outcome.value) {
		return false
	}
	perGame[hCtx.FlagKey()] = outcome
	return true
}
