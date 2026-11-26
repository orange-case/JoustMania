package gamecontext

import (
	"testing"
	"time"
)

// UC: UC-040 A31 A33 A47
// TestApplyMetrics_ShieldGate: game_player_shield_gate{stat} and
// game_shield_group_grants land on the session, never on the player, so the
// series never keeps a silent player alive. A snapshot handed out before a later
// update keeps its values.
func TestApplyMetrics_ShieldGate(t *testing.T) {
	gate := func(serial, stat string) map[string]string {
		return map[string]string{"game_id": "g1", "game_kind": "real", "serial": serial, "stat": stat}
	}
	t.Run("UC-040/A33 grants, charge and cooldown end per serial", func(t *testing.T) {
		s := newTestStore()
		if !s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 2, gate("A", ShieldGateGrants))) {
			t.Fatal("grants not applied")
		}
		s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 1, gate("A", ShieldGateCharged)))
		s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 1_800_000_000.5, gate("A", ShieldGateCooldownUntil)))
		before := s.Snapshot()
		s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 0, gate("A", ShieldGateCharged)))
		g := before.Session.ShieldGate["A"]
		if g.Grants != 2 || !g.Charged || !g.CooldownUntil.Equal(time.UnixMilli(1_800_000_000_500)) {
			t.Fatalf("gate = %+v, want 2 grants, charged, cooldown end set", g)
		}
		if s.Snapshot().Session.ShieldGate["A"].Charged {
			t.Fatal("the charge did not end")
		}
		if _, ok := before.Players["A"]; ok {
			t.Fatal("a gate series created a player")
		}
		if before.SessionID != "g1" {
			t.Fatalf("session id = %q, want the adopted g1", before.SessionID)
		}
	})
	t.Run("UC-040/A33 a zero cooldown end clears it and an unknown stat is ignored", func(t *testing.T) {
		s := newTestStore()
		s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 100, gate("A", ShieldGateCooldownUntil)))
		s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 0, gate("A", ShieldGateCooldownUntil)))
		s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 9, gate("A", "bogus")))
		if g := s.Snapshot().Session.ShieldGate["A"]; !g.CooldownUntil.IsZero() || g.Grants != 0 {
			t.Fatalf("gate = %+v, want empty", g)
		}
		if s.ApplyMetrics(metricsWith(metricPlayerShieldGate, 1, map[string]string{"game_id": "g1", "serial": "A"})) {
			t.Fatal("a gate datapoint without its stat applied")
		}
	})
	t.Run("UC-040/A47 the game's group shields", func(t *testing.T) {
		s := newTestStore()
		if !s.ApplyMetrics(metricsWith(metricShieldGroupGrants, 1, map[string]string{"game_id": "g1", "game_kind": "shadow"})) {
			t.Fatal("group count not applied")
		}
		if n := s.Snapshot().Session.ShieldGroupGrants; n == nil || *n != 1 {
			t.Fatalf("group grants = %v, want 1", n)
		}
	})
}
