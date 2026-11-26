package state

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
)

// UC: UC-053 A2 BR-004
// A session-only variant of a persist row (interventions_allowed=shadow_group)
// lives in memory over the persisted value: never persisted, never sent to
// Flagsmith, gone after a restart.
func TestSessionOnlyVariant(t *testing.T) {
	const flag = "interventions_allowed"
	t.Run("UC-053/A2 shadow_group stays in memory and a restart returns the persisted level", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admin.json")
		f := newFixture(t, fixtureOpts{stateFile: path, epoch: "e1"})
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "standard", Persist: true, Writer: "menu"})
		res := f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "shadow_group", Writer: "group-ab"})
		if res.Persisted {
			t.Fatal("a session-only write answered persisted")
		}
		if v := f.snapshotFlag(flag)["defaultVariant"]; v != "shadow_group" {
			t.Fatalf("in force %v, want shadow_group", v)
		}
		// A killed run never clears it: the restart alone must heal it.
		g := newFixture(t, fixtureOpts{stateFile: path, epoch: "e2"})
		if v := g.snapshotFlag(flag)["defaultVariant"]; v != "standard" {
			t.Fatalf("after a restart %v, want the persisted standard", v)
		}
	})
	t.Run("UC-053/A2 a DELETE or a writer reset clears shadow_group and leaves the persisted level", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{stateFile: filepath.Join(t.TempDir(), "admin.json")})
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "standard", Persist: true, Writer: "menu"})
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "shadow_group", Writer: "group-ab"})
		res, err := f.state.Clear(ClearRequest{Set: "agent", Flag: flag, IfWriter: "group-ab"})
		if err != nil || res.Cleared != 1 || res.Persisted {
			t.Fatalf("res %+v err %v, want one in-memory clear", res, err)
		}
		if v := f.snapshotFlag(flag)["defaultVariant"]; v != "standard" {
			t.Fatalf("after the clear %v, want standard", v)
		}
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "shadow_group", Writer: "group-ab"})
		if n, err := f.state.ClearWriter("agent", "group-ab"); err != nil || n != 1 {
			t.Fatalf("writer reset cleared %d (%v), want 1", n, err)
		}
		if v := f.snapshotFlag(flag)["defaultVariant"]; v != "standard" {
			t.Fatalf("after the writer reset %v, want standard", v)
		}
	})
	t.Run("UC-053/A2 a persisted write replaces shadow_group", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{stateFile: filepath.Join(t.TempDir(), "admin.json")})
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "shadow_group", Writer: "group-ab"})
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "none", Writer: "menu"})
		if v := f.snapshotFlag(flag)["defaultVariant"]; v != "none" {
			t.Fatalf("in force %v, want the persisted none", v)
		}
	})
	t.Run("UC-053/A5 shadow_group with persist, or from Flagsmith, is 400 malformed", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		for _, req := range []WriteRequest{
			{Set: "agent", Flag: flag, Variant: "shadow_group", Persist: true, Writer: "group-ab"},
			{Set: "agent", Flag: flag, Variant: "shadow_group", Writer: registry.FlagsmithWriter},
		} {
			_, err := f.state.Put(req)
			wantReason(t, err, http.StatusBadRequest, fserr.Malformed)
		}
	})
	t.Run("UC-055/BR-001 the Flagsmith link never sees shadow_group, only the persisted level", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{stateFile: filepath.Join(t.TempDir(), "admin.json")})
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "standard", Persist: true, Writer: "menu"})
		rec.take()
		f.put(WriteRequest{Set: "agent", Flag: flag, Variant: "shadow_group", Writer: "group-ab"})
		c := wantOne(t, rec.take(), ChangeValue, flag)
		if c.DeclaredVariant != "standard" || c.Writer != "menu" {
			t.Fatalf("change %+v, want the persisted standard by menu", c)
		}
	})
	t.Run("UC-053/A2 the session-only set names declared variants of persist rows", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		for k, vs := range sessionOnlyForTest() {
			fl := f.state.flags[k.key]
			if fl == nil || fl.set != k.set || fl.policy == nil || !fl.policy.Persist {
				t.Fatalf("%s/%s is not a persist row", k.set, k.key)
			}
			for _, v := range vs {
				if _, ok := fl.base.Variants[v]; !ok {
					t.Fatalf("%s/%s has no variant %s", k.set, k.key, v)
				}
			}
		}
	})
}

type setKey struct{ set, key string }

func sessionOnlyForTest() map[setKey][]string {
	out := map[setKey][]string{}
	for _, e := range registry.SessionOnlyVariants() {
		sk, variant, _ := strings.Cut(e, "=")
		set, key, _ := strings.Cut(sk, "/")
		out[setKey{set, key}] = append(out[setKey{set, key}], variant)
	}
	return out
}
