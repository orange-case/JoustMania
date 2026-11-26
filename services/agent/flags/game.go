package flags

import (
	"context"
	"errors"

	"github.com/open-feature/go-sdk/openfeature"

	"github.com/joustmania/agent/decide"
)

// GameDomain is the domain / flagSetId of the game set (services/flagd/game.json).
// The agent only READS flags from it: the shadow start-config keys of an
// experiment arm (ShadowGameInt, UC-046 BR-019, #2392). It never writes them (C-034, agent_set_never_written_c034_test.go).
const GameDomain = "game"

// GameAttrs is a game's kind and experiment attribution as the agent saw them on its
// telemetry (gamecontext Session.GameKind, ExperimentID, Arm).
type GameAttrs struct {
	GameKind     string
	ExperimentID string
	Arm          string
}

// SetGameAttrs wires the per-game attribute lookup for the per-game reads (#2096,
// UC-043 A17): in a shadow game of an experiment the agent then resolves the arm's
// value, as the coordinator does in the game's flag context.
func (f *Flags) SetGameAttrs(lookup func(gameID string) GameAttrs) {
	if f != nil {
		f.gameAttrs = lookup
	}
}

// GameContext is the agent's per-game evaluation context (#2467 PR 5, generalised
// from the shield reads): gameId, the game's game_kind (real when unknown) and, for
// a game of an experiment, experiment_id and arm, from the lookup SetGameAttrs wired.
// decision.operator_brief is read with it.
func (f *Flags) GameContext(gameID string) openfeature.EvaluationContext {
	var attrs GameAttrs
	if f != nil && f.gameAttrs != nil {
		attrs = f.gameAttrs(gameID)
	}
	return shieldChargeContext(gameID, attrs)
}

// keyDecisionOperatorBrief is decision.operator_brief (agent set, flags.md §3, #2687).
const keyDecisionOperatorBrief = "decision.operator_brief"

// OperatorBrief reads decision.operator_brief for one game from the agent set, in
// the game's context (GameContext), and cleans it (decide.NormalizeOperatorBrief,
// decide-contract.md §3, UC-040 BR-044). A nil wrapper, a nil client, any
// evaluation error or a blank value gives "" (no brief, the prompts stay
// byte-identical); debug log only, the remote path reads it per request. Human-only
// (C-034): the agent never writes it.
func (f *Flags) OperatorBrief(ctx context.Context, gameID string) string {
	if f == nil || f.client == nil {
		return ""
	}
	v, err := f.client.StringValue(ctx, keyDecisionOperatorBrief, "", f.GameContext(gameID))
	if err != nil {
		f.log.Debug("flags decision.operator_brief unreadable, no brief", "game_id", gameID, "error", err)
		return ""
	}
	return decide.NormalizeOperatorBrief(v)
}

// shieldChargeContext is the agent's evaluation context for the two shield flags: gameId
// and the game's game_kind (real when unknown), plus experiment_id and arm for a game of
// an experiment (#2096: shield.activation has an experiment rung since FR-162). The coordinator reads them inside grant_shield with more: the
// InterventionManager runs the grant in the game's flag transaction context (#1970), so
// its read also carries targetingKey and game_mode (the mode), controller_count,
// sensitivity and the game's game_kind
// (services/game_coordinator/tests/test_shield_charge_flag_context.py). The agent still
// resolves what the coordinator does: it reads these flags only for a real FFA game
// (game_kind=real on both sides) or, in a shadow game, with the game's kind and arm, and
// flags.md §4 restricts their targeting to gameId, game_kind, experiment_id and arm
// (tests/contract/test_flagd_resolution.py and HANDLER_FLAG_READS in
// services/game_coordinator/handler_flag_reads.py), so the extra attributes never
// change the result.
//
// Keep in sync with _agent_context() in tests/contract/test_flagd_resolution.py, the
// live parity test that resolves both contexts against flagd.
func shieldChargeContext(gameID string, attrs GameAttrs) openfeature.EvaluationContext {
	kind := attrs.GameKind
	if kind == "" {
		kind = "real"
	}
	evalCtx := map[string]any{
		"game_kind": kind,
		"gameId":    gameID,
	}
	if attrs.ExperimentID != "" {
		evalCtx["experiment_id"] = attrs.ExperimentID
	}
	if attrs.Arm != "" {
		evalCtx["arm"] = attrs.Arm
	}
	return openfeature.NewEvaluationContext("", evalCtx)
}

// ShadowGameInt resolves one integer game flag for a shadow game of an experiment
// arm, with the context the game coordinator gives that game's own flag reads:
// game_kind shadow, the game mode, experiment_id and arm (UC-046 BR-019, #2392). The
// shadow spawner puts the result into the game's StartGameConfig, so an experiment
// on a start-config key (sensitivity) gives each arm its own value. An evaluation
// error, a nil game client or an unready domain is returned as an error: the caller
// refuses the spawn rather than start an arm with a value the other arm may not
// share.
func (f *Flags) ShadowGameInt(ctx context.Context, key, mode, experimentID, arm string) (int64, error) {
	if f == nil || f.game == nil {
		return 0, errors.New("game flag set not wired")
	}
	evalCtx := openfeature.NewEvaluationContext("", map[string]any{
		"game_kind":     "shadow",
		"game_mode":     mode,
		"experiment_id": experimentID,
		"arm":           arm,
	})
	return f.game.IntValue(ctx, key, 0, evalCtx)
}
