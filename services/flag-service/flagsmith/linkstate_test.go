package flagsmith

import (
	"encoding/json"
	"testing"
	"time"
)

func val(raw string) Value { return Value{Present: true, Value: json.RawMessage(raw)} }

// UC: UC-055 BR-001
func TestLinkState(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	stack := Key{Set: "game", Flag: "sensitivity", Scope: ScopeStack}
	other := Key{Set: "agent", Flag: "enabled", Scope: ScopeStack}
	t.Run("UC-055/BR-001 two offers merge: the latest wins, merged 2, the first-change order is kept", func(t *testing.T) {
		s := NewLinkState(func() time.Time { return t0 })
		s.Offer(stack, val(`1`), t0, 10)
		s.Offer(other, val(`2`), t0, 11)
		s.Offer(stack, val(`3`), t0, 12)
		k, v, merged, version, ok := s.Next()
		if !ok || k != stack || string(v.Value) != "3" || merged != 2 || version != 12 {
			t.Fatalf("next %v %s merged %d version %d, want sensitivity 3 merged 2 at 12", k, v.Value, merged, version)
		}
	})
	t.Run("UC-055/BR-001 a newer pending value survives ConfirmPushed of the older one", func(t *testing.T) {
		s := NewLinkState(nil)
		s.Offer(stack, val(`1`), t0, 1)
		s.Offer(stack, val(`2`), t0, 2)
		s.ConfirmPushed(stack, val(`1`))
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("pending %s ok %v, want 2 still pending", v.Value, ok)
		}
		s.ConfirmPushed(stack, val(`2`))
		if _, _, _, _, ok := s.Next(); ok {
			t.Fatal("still pending after the confirm")
		}
		if p, oos, _ := s.Counts(); p != 0 || oos != 0 {
			t.Fatalf("counts pending %d out of sync %d, want 0 0", p, oos)
		}
	})
	t.Run("UC-054/BR-005 an offer equal to the remote value drops the pending entry and is in sync", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`{"a":1,"b":2}`))
		s.Offer(stack, val(`5`), t0, 1)
		if queued := s.Offer(stack, val(`{"b":2,"a":1}`), t0, 2); queued {
			t.Fatal("an offer equal to the remote value was queued")
		}
		if p, oos, _ := s.Counts(); p != 0 || oos != 0 {
			t.Fatalf("counts pending %d out of sync %d, want 0 0", p, oos)
		}
	})
	t.Run("UC-055/BR-001 Equal: key order ignored, variants compared by name, writer ignored", func(t *testing.T) {
		if !val(`{"a":1,"b":2}`).Equal(val(`{"b":2, "a":1}`)) {
			t.Fatal("key order matters")
		}
		a := Value{Present: true, Variant: "slow", Value: json.RawMessage(`1`), Writer: "menu"}
		b := Value{Present: true, Variant: "slow", Value: json.RawMessage(`1`), Writer: "flagsmith"}
		if !a.Equal(b) || a.Equal(Value{Present: true, Variant: "fast"}) || a.Equal(Value{}) || !(Value{}).Equal(Value{Writer: "x"}) {
			t.Fatal("Equal is wrong")
		}
	})
	t.Run("UC-055/BR-001 ForgetGame drops game:<id> and game:<id>:*, a late confirm is ignored", func(t *testing.T) {
		s := NewLinkState(nil)
		g := Key{Set: "interventions", Flag: "volume_override", Scope: "game:game_a"}
		p := Key{Set: "interventions", Flag: "shield_seconds", Scope: "game:game_a:MOCK0001"}
		keep := Key{Set: "interventions", Flag: "shield_seconds", Scope: "game:game_b:MOCK0001"}
		s.Offer(g, val(`0.5`), t0, 1)
		s.Offer(p, val(`5`), t0, 2)
		s.Offer(keep, val(`5`), t0, 3)
		s.ForgetGame("game_a")
		s.ConfirmPushed(g, val(`0.5`))
		keys := s.Keys()
		if len(keys) != 1 || keys[0] != keep {
			t.Fatalf("keys %v, want only game_b", keys)
		}
		if p, _, _ := s.Counts(); p != 1 {
			t.Fatalf("pending %d, want 1", p)
		}
		s.ForgetIdentity("game:game_b:MOCK0001")
		if len(s.Keys()) != 0 {
			t.Fatalf("keys %v after ForgetIdentity", s.Keys())
		}
	})
	t.Run("UC-055/BR-001 a retried key waits, a remote refusal drops it and counts", func(t *testing.T) {
		now := t0
		s := NewLinkState(func() time.Time { return now })
		s.Offer(stack, val(`1`), t0, 1)
		s.Retry(stack, t0.Add(time.Second))
		if _, _, _, _, ok := s.Next(); ok {
			t.Fatal("a held key was handed out")
		}
		now = t0.Add(time.Second)
		if _, _, _, _, ok := s.Next(); !ok {
			t.Fatal("the key did not come back after its retry time")
		}
		s.Refuse(stack, SideRemote, ReasonNotFound)
		if p, oos, refused := s.Counts(); p != 0 || oos != 1 || refused != 1 {
			t.Fatalf("counts %d %d %d, want 0 1 1", p, oos, refused)
		}
		g0 := s.Gen(stack)
		s.SetRemote(stack, val(`1`))
		if s.RestoreRemote(stack, val(`2`), Value{}, false) {
			t.Fatal("RestoreRemote swapped a remote value it did not expect")
		}
		if !s.RestoreRemote(stack, val(`1`), val(`0`), true) {
			t.Fatal("RestoreRemote refused the expected value")
		}
		if v, _ := s.Remote(stack); string(v.Value) != "0" || s.Gens()[stack] != g0+2 {
			t.Fatalf("remote %s gen %d, want 0 at gen %d", v.Value, s.Gens()[stack], g0+2)
		}
		s.ForgetRemote(stack)
		if _, had := s.Remote(stack); had {
			t.Fatal("ForgetRemote kept the remote value")
		}
	})
}
