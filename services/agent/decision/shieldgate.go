package decision

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/joustmania/agent/decide"
	"github.com/joustmania/agent/flags"
	"github.com/joustmania/agent/gamecontext"
)

// The shield gate (UC-040 A31, A33, A37; decide-contract §4.5 step 4): a targeted
// grant_shield is refused from the coordinator's own view of the player, the
// game_player_shield_gate gauges (a held trigger-mode charge, the agent shields
// applied in this game, the end of the cooldown) and in a hold game the
// game_player_shield_* pool gauges. The agent keeps no ledger: the gauges are up to
// one export interval old, so a grant written inside that window can still be
// refused by the coordinator, which stays authoritative and refunds its refusal.
// Missing gauges (an agent restart, a collector outage) refuse nothing.

// DefaultShieldSeconds is the fail-closed grant_shield length (UC-040 BR-029, #2395):
// the loop stamps a grant with no value with the shield_seconds "default" variant
// (SetShieldSecondsSource) and falls back to this when that cannot be read; the writer
// (actions/writer.go) uses it for a decision that still carries no
// value, or one that does not parse (UC-040 BR-022). It equals the variant's baseline.
const DefaultShieldSeconds = 5

// ShieldSecondsSource returns the grant_shield length in seconds for a decision that
// carries no value (UC-040 BR-029, #2395). main.go wires actions.ShieldSecondsReader,
// which reads the interventions/shield_seconds "default" variant through the flag
// service at use time and fails closed to DefaultShieldSeconds, so it always returns a
// usable length.
type ShieldSecondsSource func(ctx context.Context) float64

// SetShieldSecondsSource injects the grant length source. nil (the construction
// default) keeps a grant without a value empty. Set during construction.
func (l *Loop) SetShieldSecondsSource(src ShieldSecondsSource) { l.shieldSeconds = src }

// withShieldSeconds stamps a targeted grant_shield that carries no value (every rules
// decision, a remote answer without a magnitude) with the configured length, so the
// coordinator and the writer use one value (UC-040
// BR-029). The source is read only for a grant the allow-list permits; a value the
// decision already carries (a remote magnitude) is kept.
func (l *Loop) withShieldSeconds(ctx context.Context, snapshot flags.Snapshot, d Decision) Decision {
	if l.shieldSeconds == nil || d.Value != "" || !grantsShieldLength(d) {
		return d
	}
	if !snapshot.Permits(d.Intervention) {
		return d
	}
	v := l.shieldSeconds(ctx)
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		v = DefaultShieldSeconds
	}
	d.Value = strconv.FormatFloat(v, 'g', -1, 64)
	return d
}

// grantsShieldLength reports whether d grants shields whose length the loop stamps:
// a targeted grant_shield, or a grant_group_shield with its set (#2467: the group's
// targets share one length, which the writer uses).
func grantsShieldLength(d Decision) bool {
	switch d.Intervention {
	case InterventionGrantShield:
		return d.TargetSerial != ""
	case InterventionGrantGroupShield:
		return len(d.TargetSerials) >= 2
	}
	return false
}

// stampShieldSeconds stamps the grant_shield length on the rules decisions of one
// cycle before they run (withShieldSeconds), so the writer and the record's answers
// use what is written, not the 5 s bounds default (UC-040 BR-029; #2395).
// runDecision keeps a value already stamped, so the source is read once per grant.
//
// It then clamps each decision's value into the request's magnitude ranges
// (clampRulesValue, #2696, UC-040 BR-045), so a range the operator narrowed holds on
// the rules path too; at the default ranges no rules value moves.
func (l *Loop) stampShieldSeconds(ctx context.Context, snapshot flags.Snapshot, ds []Decision, bounds *decide.MagnitudeTable) []Decision {
	if len(ds) == 0 || (l.shieldSeconds == nil && bounds == nil) {
		return ds
	}
	out := make([]Decision, len(ds))
	for i, d := range ds {
		out[i] = clampRulesValue(bounds, l.withShieldSeconds(ctx, snapshot, d))
	}
	return out
}

// shieldLimits are the live shield limits of one check (UC-040 A33, BR-022): the
// policy.max_shields_per_player_per_game and policy.shield_cooldown_seconds values of
// the snapshot. 0 = unlimited / off. Only the cap is compared here: the cooldown end
// comes from the coordinator, which read the cooldown when it applied the grant.
type shieldLimits struct {
	maxShields int
}

func limitsOf(p flags.Policy) shieldLimits {
	return shieldLimits{maxShields: p.MaxShieldsPerPlayerPerGame}
}

// shieldGateRefusal is the reason a targeted grant_shield to serial is refused at
// now, or "". The order is the coordinator's: in a hold game the budget then the
// pool; else a held charge (already_charged); then the cap, then the cooldown.
func shieldGateRefusal(c gamecontext.GameContext, serial string, lim shieldLimits, now time.Time) BlockReason {
	if r := holdPoolOf(c, serial).refusal(); r != "" {
		return r
	}
	g, ok := c.Session.ShieldGate[serial]
	if !ok {
		return ""
	}
	if g.Charged && !c.Session.IsShieldHold() {
		return ReasonAlreadyCharged
	}
	if lim.maxShields > 0 && g.Grants >= lim.maxShields {
		return ReasonShieldCapReached
	}
	if now.Before(g.CooldownUntil) {
		return ReasonShieldCooldown
	}
	return ""
}

// shieldGateLimits holds the latest cycle's limits for the views.
type shieldGateLimits struct {
	mu  sync.Mutex
	lim shieldLimits
}

func (g *shieldGateLimits) set(lim shieldLimits) {
	g.mu.Lock()
	g.lim = lim
	g.mu.Unlock()
}

func (g *shieldGateLimits) get() shieldLimits {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lim
}

// ShieldSerials is the gate's view of c, taken once per ranking and per request
// (UC-040 A31, A33, A37): charged are the players holding a charge (the decide
// state's shield_charged), noShield every player a targeted grant_shield would be
// refused for now (the rules' no-shield filter, the state's shield_limited). Either
// is nil when empty.
func (l *Loop) ShieldSerials(c gamecontext.GameContext) (charged, noShield map[string]bool) {
	lim, now := l.shieldLimits.get(), l.nowOrDefault()()
	for serial := range c.Players {
		r := shieldGateRefusal(c, serial, lim, now)
		if r == "" {
			continue
		}
		if noShield == nil {
			noShield = make(map[string]bool, len(c.Players))
		}
		noShield[serial] = true
		if r == ReasonAlreadyCharged {
			if charged == nil {
				charged = make(map[string]bool, len(c.Players))
			}
			charged[serial] = true
		}
	}
	return charged, noShield
}

// ShieldGrants is the rules engine's catch-up view (UC-040 A41, BR-030): the agent
// shields the coordinator applied per serial in c's game. nil when there are none.
func (l *Loop) ShieldGrants(c gamecontext.GameContext) map[string]int {
	var out map[string]int
	for serial, g := range c.Session.ShieldGate {
		if g.Grants > 0 {
			if out == nil {
				out = make(map[string]int, len(c.Session.ShieldGate))
			}
			out[serial] = g.Grants
		}
	}
	return out
}

// checkShield runs the gate step of the permission chain (decide-contract §4.5 step
// 4) for a targeted grant_shield: "" when the gate does not apply or passes.
func (l *Loop) checkShield(snapshot flags.Snapshot, c gamecontext.GameContext, d Decision, now time.Time) BlockReason {
	if d.Intervention != InterventionGrantShield || d.TargetSerial == "" {
		return ""
	}
	return shieldGateRefusal(c, d.TargetSerial, limitsOf(snapshot.Policy), now)
}

// holdPool is a hold-mode player's pool as the game context shows it (#2096).
type holdPool struct {
	energy, budgetLeft, cap, minBurst *float64
	active                            bool
}

// holdPoolOf returns serial's pool in c, nil outside a hold game or for a player the
// context does not show alive.
func holdPoolOf(c gamecontext.GameContext, serial string) *holdPool {
	if !c.Session.IsShieldHold() {
		return nil
	}
	p := c.Players[serial]
	if p == nil || p.Active == nil || !*p.Active {
		return nil
	}
	return &holdPool{
		energy:     p.ShieldEnergy,
		budgetLeft: p.ShieldBudgetRemaining,
		cap:        c.Session.ShieldSetting(gamecontext.ShieldSettingEnergyMax),
		minBurst:   c.Session.ShieldSetting(gamecontext.ShieldSettingMinBurst),
		active:     p.ShieldActive != nil && *p.ShieldActive,
	}
}

// refusal is the hold check of the pool.
func (h *holdPool) refusal() BlockReason {
	if h == nil {
		return ""
	}
	return BlockReason(decide.HoldShieldRefusal(h.energy, h.budgetLeft, h.active, h.cap, h.minBurst))
}
