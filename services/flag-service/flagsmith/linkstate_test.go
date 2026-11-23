package flagsmith

import (
	"encoding/json"
	"testing"
	"time"
)

func val(raw string) Value { return Value{Present: true, Value: json.RawMessage(raw)} }

// UC: UC-055 BR-001
// UC: UC-054 BR-005
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
	t.Run("UC-055/BR-001 back to the remote value while a newer one is in flight: its confirm keeps the key out of sync (#2611)", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		s.Offer(stack, val(`2`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("setup: in flight %s ok %v, want 2", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 2) // the operator goes back to 1, not held
		s.Offer(stack, val(`1`), t0, 3) // a re-save keeps it reverted
		s.ConfirmPushed(stack, val(`2`))
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 0: the stack runs 1, Flagsmith holds 2", p, oos, r)
		}
		if !s.Reoffer(stack, val(`1`), t0, 4) {
			t.Fatal("the next pull round did not queue the local 1")
		}
		s.Next()
		s.ConfirmPushed(stack, val(`1`))
		if p, oos, _ := s.Counts(); p != 0 || oos != 0 {
			t.Fatalf("pending %d out_of_sync %d after the local 1 is confirmed, want 0 0", p, oos)
		}
	})
	t.Run("UC-055/BR-001 back to the remote value, a new pending value and back again while one is in flight: its confirm keeps the key out of sync (#2611)", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		s.Offer(stack, val(`2`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("setup: in flight %s ok %v, want 2", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 2) // back to 1 while 2 is in flight
		s.Offer(stack, val(`3`), t0, 3) // a new pending value, never handed out
		s.Offer(stack, val(`1`), t0, 4) // back to 1 again
		s.ConfirmPushed(stack, val(`2`))
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 0: the stack runs 1, Flagsmith holds 2", p, oos, r)
		}
	})
	t.Run("UC-055/BR-001 back to the remote value, then a pull applies a newer Flagsmith value while one is in flight: its confirm keeps the key out of sync (#2611)", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		s.Offer(stack, val(`2`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("setup: in flight %s ok %v, want 2", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 2) // back to 1 while 2 is in flight
		s.SetRemoteGen(stack, val(`5`)) // a pull round applies Flagsmith's 5
		s.Offer(stack, val(`5`), t0, 3) // the apply's local write
		s.ConfirmPushed(stack, val(`2`))
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 0: the stack runs 5, Flagsmith holds 2", p, oos, r)
		}
	})
	t.Run("UC-055/BR-001 back to the remote value, then a pull's apply fails and is undone while one is in flight: its confirm keeps the key out of sync (#2611)", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		s.Offer(stack, val(`2`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("setup: in flight %s ok %v, want 2", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 2) // back to 1 while 2 is in flight
		prev, had, gen := s.SetRemoteGen(stack, val(`5`))
		if !s.RestoreRemoteGen(stack, gen, prev, had) { // the apply's local write failed
			t.Fatal("setup: the restore was refused")
		}
		s.ConfirmPushed(stack, val(`2`))
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 0 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 0: the stack runs 1, Flagsmith holds 2", p, oos, r)
		}
	})
	t.Run("UC-055/BR-001 back to the remote value, then a pull applies the value in flight: its confirm leaves the key in sync (#2611)", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		s.Offer(stack, val(`5`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "5" {
			t.Fatalf("setup: in flight %s ok %v, want 5", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 2) // back to 1 while 5 is in flight
		s.SetRemoteGen(stack, val(`5`)) // the push landed before the pull: Flagsmith holds 5
		s.Offer(stack, val(`5`), t0, 3) // the apply writes 5 locally
		s.ConfirmPushed(stack, val(`5`))
		if _, oos, _ := s.Counts(); oos != 0 {
			t.Fatalf("out_of_sync %d, want 0: the stack runs 5, Flagsmith holds 5", oos)
		}
	})
	t.Run("UC-055/BR-001 back to the remote value, then a pull's apply is undone and the value in flight equals the pulled one: its confirm keeps the key out of sync (#2611)", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		s.Offer(stack, val(`5`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "5" {
			t.Fatalf("setup: in flight %s ok %v, want 5", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 2)
		prev, had, gen := s.SetRemoteGen(stack, val(`5`))
		if !s.RestoreRemoteGen(stack, gen, prev, had) {
			t.Fatal("setup: the restore was refused")
		}
		s.ConfirmPushed(stack, val(`5`))
		if _, oos, _ := s.Counts(); oos != 1 {
			t.Fatalf("out_of_sync %d, want 1: the stack runs 1, Flagsmith holds 5", oos)
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
	// #2594 delta finding 1: Refuse without a value must not drop a fresh
	// pending entry that Next never handed out.
	t.Run("UC-055/BR-001 Refuse keeps a fresh pending entry Next never handed out", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		s.Offer(stack, val(`1`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "1" {
			t.Fatalf("in flight %s ok %v, want 1", v.Value, ok)
		}
		s.Offer(stack, val(`0`), t0, 2)             // equal to the remote value: drops pending
		s.Offer(stack, val(`3`), t0, 3)             // a fresh entry, never handed out
		s.Refuse(stack, SideRemote, ReasonNotFound) // the refusal is of 1
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "3" {
			t.Fatalf("pending %s ok %v, want 3 kept", v.Value, ok)
		}
		if r, side := s.Refusal(stack); r != ReasonNotFound || side != SideRemote {
			t.Fatalf("refusal %q %q, want recorded", r, side)
		}
	})
	// #2594 review finding 1: the mirror of "a newer pending value survives
	// ConfirmPushed of the older one".
	t.Run("UC-055/BR-001 a newer pending value survives a remote refusal of the older one", func(t *testing.T) {
		s := NewLinkState(nil)
		s.Offer(stack, val(`1`), t0, 1)
		if _, v, _, _, _ := s.Next(); string(v.Value) != "1" {
			t.Fatalf("in flight %s, want 1", v.Value)
		}
		s.Offer(stack, val(`2`), t0, 2) // offered while 1 is in flight
		s.Refuse(stack, SideRemote, ReasonChangeRequestRequired)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("pending %s ok %v, want 2 still pending after 1 was refused", v.Value, ok)
		}
		if _, side := s.Refusal(stack); side != SideRemote {
			t.Fatalf("refusal side %q, want remote", side)
		}
		s.Refuse(stack, SideRemote, ReasonNotFound) // 2 is in flight now
		if _, _, _, _, ok := s.Next(); ok {
			t.Fatal("the refused value is still pending")
		}
		s.Offer(stack, val(`3`), t0, 3)
		s.RefuseValue(stack, val(`1`), SideRemote, ReasonNotFound)
		if p, _, _ := s.Counts(); p != 1 {
			t.Fatalf("pending %d, want 3 kept after a refusal of 1", p)
		}
		s.RefuseValue(stack, val(`3`), SideRemote, ReasonNotFound)
		if p, _, refused := s.Counts(); p != 0 || refused != 1 {
			t.Fatalf("pending %d refused %d, want 0 1", p, refused)
		}
	})
	t.Run("UC-054/BR-005 a key first seen by a pull is in sync while nothing is pending", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`1`))
		if _, oos, _ := s.Counts(); oos != 0 {
			t.Fatalf("out of sync %d, want 0 after a pull of a key with nothing pending", oos)
		}
		s.Offer(other, val(`2`), t0, 1)
		s.SetRemote(other, val(`3`))
		if _, oos, _ := s.Counts(); oos != 1 {
			t.Fatalf("out of sync %d, want 1: a pending value is not in sync", oos)
		}
	})
	// #2594 review finding 6: the undo is exact by generation.
	t.Run("UC-054/BR-005 RestoreRemoteGen undoes only its own SetRemote, not a later one of the same value", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		prev, had, gen := s.SetRemoteGen(stack, val(`1`))
		s.SetRemote(stack, val(`1`)) // a second pull saw 1 too
		if s.RestoreRemoteGen(stack, gen, prev, had) {
			t.Fatal("the first write's undo restored over a later pull")
		}
		if v, _ := s.Remote(stack); string(v.Value) != "1" {
			t.Fatalf("remote %s, want 1", v.Value)
		}
		prev, had, gen = s.SetRemoteGen(stack, val(`2`))
		if !s.RestoreRemoteGen(stack, gen, prev, had) {
			t.Fatal("the undo of the latest SetRemote was refused")
		}
		if v, _ := s.Remote(stack); string(v.Value) != "1" {
			t.Fatalf("remote %s, want 1 restored", v.Value)
		}
	})
}

// UC: UC-055 A4
// A value Flagsmith refused is remembered per key: a pull round (Reoffer)
// never queues it again, a re-save of the same value does not either, and a
// different local value, a confirmed push, ClearRefusal or the key's deletion
// ends it (#2604).
func TestLinkStateRefusedValue(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	stack := Key{Set: "agent", Flag: "decision.backend", Scope: ScopeStack}
	game := Key{Set: "interventions", Flag: "volume_override", Scope: "game:g1"}
	// refused returns a state where Flagsmith (remote 0) refused 1 for k.
	refused := func(t *testing.T, k Key) *LinkState {
		t.Helper()
		s := NewLinkState(nil)
		s.SetRemote(k, val(`0`))
		s.Offer(k, val(`1`), t0, 1)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "1" {
			t.Fatalf("setup: in flight %s ok %v, want 1", v.Value, ok)
		}
		s.RefuseValue(k, val(`1`), SideRemote, ReasonChangeRequestRequired)
		return s
	}
	// heldWithInFlight returns a state where Flagsmith (remote 0) refused 1
	// while 2 was offered, and 2 is in flight now.
	heldWithInFlight := func(t *testing.T) *LinkState {
		t.Helper()
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		s.Offer(stack, val(`1`), t0, 1)
		s.Next()
		s.Offer(stack, val(`2`), t0, 2) // merged while 1 is in flight
		s.RefuseValue(stack, val(`1`), SideRemote, ReasonChangeRequestRequired)
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("setup: in flight %s ok %v, want 2", v.Value, ok)
		}
		return s
	}
	t.Run("UC-055/A4 back to the refused value while a newer one is in flight: its confirm keeps the refusal, out of sync", func(t *testing.T) {
		s := heldWithInFlight(t)
		s.Offer(stack, val(`1`), t0, 3) // the operator goes back to the refused 1
		s.ConfirmPushed(stack, val(`2`))
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1: the stack runs the refused 1, Flagsmith holds 2", p, oos, r)
		}
		if got, _ := s.Remote(stack); string(got.Value) != "2" {
			t.Fatalf("remote %s, want 2 (confirmed)", got.Value)
		}
		if s.Reoffer(stack, val(`1`), t0, 4) {
			t.Fatal("the next pull round queued the refused 1 again")
		}
	})
	t.Run("UC-055/A4 back to the refused value while a pull round holds the older local value: the round does not queue it", func(t *testing.T) {
		s := heldWithInFlight(t)
		s.Offer(stack, val(`1`), t0, 3) // the operator goes back to the refused 1
		if s.Reoffer(stack, val(`2`), t0, 2) {
			t.Fatal("a pull round queued the 2 it read before the local change to 1")
		}
		s.ConfirmPushed(stack, val(`2`))
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1: the stack runs the refused 1, Flagsmith holds 2", p, oos, r)
		}
	})
	t.Run("UC-055/A4 back to the refused value while a pull round finds the older local value in Flagsmith: the refusal stays", func(t *testing.T) {
		s := heldWithInFlight(t)
		s.Offer(stack, val(`1`), t0, 3) // the operator goes back to the refused 1
		s.SetRemote(stack, val(`2`))    // the round read local 2 (snapshot 2) and Flagsmith shows 2
		s.ClearRefusalRead(stack, 2)
		s.SetInSyncRead(stack, 2)
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1: the stale round must not end the refusal of the local 1", p, oos, r)
		}
		s.ClearRefusalRead(stack, 3)
		s.SetInSyncRead(stack, 3)
		if _, oos, r := s.Counts(); oos != 0 || r != 0 {
			t.Fatalf("out_of_sync %d refused %d, want 0 0: a round that read the latest change acts", oos, r)
		}
	})
	t.Run("UC-055/A4 back to the refused value while a newer one is in flight: its refusal keeps the held value", func(t *testing.T) {
		s := heldWithInFlight(t)
		s.Offer(stack, val(`1`), t0, 3) // the operator goes back to the refused 1
		s.RefuseValue(stack, val(`2`), SideRemote, ReasonChangeRequestRequired)
		if s.Reoffer(stack, val(`1`), t0, 4) {
			t.Fatal("the next pull round re-sends the refused 1")
		}
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1", p, oos, r)
		}
		if !s.Reoffer(stack, val(`2`), t0, 5) {
			t.Fatal("a later local 2 differs from the held 1 and must be sent (Q1)")
		}
	})
	t.Run("UC-055/A4 a pull round does not queue the refused value again and keeps the key out of sync", func(t *testing.T) {
		s := refused(t, stack)
		s.SetInSync(stack, true) // as SetRemote of a keep-local round does
		if s.Reoffer(stack, val(`1`), t0, 2) {
			t.Fatal("Reoffer queued the refused value")
		}
		if p, oos, r := s.Counts(); p != 0 || oos != 1 || r != 1 {
			t.Fatalf("pending %d out_of_sync %d refused %d, want 0 1 1", p, oos, r)
		}
		if !s.Reoffer(stack, val(`2`), t0, 3) {
			t.Fatal("Reoffer did not queue a different value")
		}
	})
	t.Run("UC-055/A4 a re-save of the refused value does not retry, a different value does", func(t *testing.T) {
		s := refused(t, stack)
		if s.Offer(stack, val(`1`), t0, 2) {
			t.Fatal("a re-save of the refused value was queued")
		}
		if _, _, _, _, ok := s.Next(); ok {
			t.Fatal("a re-save of the refused value is pending")
		}
		if !s.Offer(stack, val(`2`), t0, 3) {
			t.Fatal("a different value was not queued")
		}
		s.Offer(stack, val(`1`), t0, 4) // back to the refused value: no longer remembered
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "1" {
			t.Fatalf("pending %s ok %v, want 1 queued after a different value ended the refusal", v.Value, ok)
		}
	})
	t.Run("UC-055/A4 a re-save of the refused value drops an older pending value", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		s.Offer(stack, val(`1`), t0, 1)
		s.Next()
		s.Offer(stack, val(`2`), t0, 2) // offered while 1 is in flight
		s.RefuseValue(stack, val(`1`), SideRemote, ReasonChangeRequestRequired)
		if s.Reoffer(stack, val(`2`), t0, 3) {
			t.Fatal("Reoffer replaced or re-queued the pending entry")
		}
		if _, v, _, _, ok := s.Next(); !ok || string(v.Value) != "2" {
			t.Fatalf("pending %s ok %v, want 2 kept", v.Value, ok)
		}
		s.Offer(stack, val(`1`), t0, 4) // the operator goes back to the refused 1
		if p, _, _ := s.Counts(); p != 0 {
			t.Fatalf("pending %d, want 2 dropped: the local value is the refused 1", p)
		}
	})
	t.Run("UC-055/A4 Reoffer never replaces a pending entry", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		s.Offer(stack, val(`5`), t0, 1)
		if s.Reoffer(stack, val(`4`), t0, 2) {
			t.Fatal("Reoffer replaced a pending entry")
		}
		if _, v, _, _, _ := s.Next(); string(v.Value) != "5" {
			t.Fatalf("pending %s, want 5", v.Value)
		}
	})
	t.Run("UC-055/A4 a confirmed push, ClearRefusal and deletion end the refused value", func(t *testing.T) {
		s := heldWithInFlight(t)
		s.ConfirmPushed(stack, val(`2`)) // 2 was the local value
		if !s.Reoffer(stack, val(`1`), t0, 3) {
			t.Fatal("the refused value is still held back after a confirmed push")
		}
		s = refused(t, stack)
		s.ClearRefusal(stack)
		if !s.Reoffer(stack, val(`1`), t0, 2) {
			t.Fatal("the refused value is still held back after ClearRefusal")
		}
		s = refused(t, game)
		s.ForgetGame("g1")
		s.SetRemote(game, val(`0`))
		if !s.Reoffer(game, val(`1`), t0, 2) {
			t.Fatal("the refused value survived ForgetGame")
		}
	})
	t.Run("UC-055/A4 only a push refusal with its value is remembered", func(t *testing.T) {
		s := NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		s.Offer(stack, val(`1`), t0, 1)
		s.Refuse(stack, SideRemote, ReasonNotFound) // no value (start sequence)
		s.Offer(stack, val(`0`), t0, 2)
		if !s.Reoffer(stack, val(`1`), t0, 3) {
			t.Fatal("a refusal without a value held the value back")
		}
		s = NewLinkState(nil)
		s.SetRemote(stack, val(`0`))
		s.RefuseValue(stack, val(`1`), SideLocal, "slot_changed")
		if !s.Reoffer(stack, val(`1`), t0, 2) {
			t.Fatal("a local refusal held the value back")
		}
	})
	t.Run("UC-055/A4 ClearLocalRefusal keeps a push refusal and its value", func(t *testing.T) {
		s := refused(t, stack)
		s.ClearLocalRefusal(stack)
		if r, side := s.Refusal(stack); r != ReasonChangeRequestRequired || side != SideRemote {
			t.Fatalf("refusal %q %q, want the push refusal kept", r, side)
		}
		if s.Reoffer(stack, val(`1`), t0, 2) {
			t.Fatal("ClearLocalRefusal ended the refused value")
		}
		s.Refuse(stack, SideLocal, "slot_changed")
		s.ClearLocalRefusal(stack)
		if r, _ := s.Refusal(stack); r != "" {
			t.Fatalf("refusal %q, want a local refusal cleared", r)
		}
	})
	t.Run("UC-055/A4 the remembered value alone counts as refused", func(t *testing.T) {
		s := refused(t, stack)
		s.ClearLocalRefusal(stack)
		s.Refuse(stack, SideLocal, "slot_changed")
		s.ClearLocalRefusal(stack) // the reason is gone, the refused value stays
		if _, _, r := s.Counts(); r != 1 {
			t.Fatalf("refused %d, want 1 while the refused value is held back", r)
		}
	})
}

// UC: UC-054 A2
// An offline-made pending value is held for the in side (never handed to the
// sender); DropOffline drops it only while it is still the change of the
// snapshot version, so a write made online during the reconcile is kept and
// sent (#2621, review of #2627 finding 5).
func TestOfflinePendingHeldAndDroppedByVersion(t *testing.T) {
	k := Key{Set: "agent", Flag: "decision.min_interval_ms", Scope: ScopeStack}
	s := NewLinkState(nil)
	s.SetRemote(k, val(`700`))
	s.OfferFrom(k, val(`900`), time.Now(), 5, true)
	if _, _, _, _, ok := s.Next(); ok {
		t.Fatal("UC-054/A2 an offline-made value is handed to the sender")
	}
	s.OfferFrom(k, val(`800`), time.Now(), 6, false) // an online write merges
	s.DropOffline(k, 5)
	if !s.HasPending(k) || s.PendingOffline(k) {
		t.Fatalf("UC-054/A2 pending %v offline %v after an online write merged, want pending and online", s.HasPending(k), s.PendingOffline(k))
	}
	if gk, v, _, _, ok := s.Next(); !ok || gk != k || string(v.Value) != "800" {
		t.Fatalf("UC-055/A3 Next = %v %s %v, want the online 800 sent", gk, v.Value, ok)
	}
	s.OfferFrom(k, val(`950`), time.Now(), 7, true)
	s.DropOffline(k, 6)
	if !s.HasPending(k) {
		t.Fatal("UC-054/A2 DropOffline with an older version dropped a newer offline value")
	}
	s.DropOffline(k, 7)
	if s.HasPending(k) {
		t.Fatal("UC-054/A2 DropOffline of the current offline version kept it")
	}
}
