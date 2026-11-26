package state

import (
	"path/filepath"
	"testing"

	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
)

// recorder is an Observer that keeps every Change.
type recorder struct{ changes []Change }

func (r *recorder) StateChanged(c Change) { r.changes = append(r.changes, c) }

func (r *recorder) take() []Change {
	out := r.changes
	r.changes = nil
	return out
}

func observedFixture(t *testing.T, o fixtureOpts) (*fixture, *recorder) {
	t.Helper()
	f := newFixture(t, o)
	rec := &recorder{}
	f.state.SetObserver(rec)
	return f, rec
}

func wantOne(t *testing.T, got []Change, kind ChangeKind, flag string) Change {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d changes %+v, want one %s of %s", len(got), got, kind, flag)
	}
	if got[0].Kind != kind || got[0].Flag != flag {
		t.Fatalf("got %s of %q, want %s of %s", got[0].Kind, got[0].Flag, kind, flag)
	}
	return got[0]
}

// UC: UC-055 BR-001
// The Flagsmith link sees every change of the runtime state through the
// observer (services.md §7.3, #2588): each mutation path reports what the slot
// holds now, in order, and paths that change nothing report nothing.
func TestObserver_MutationPaths(t *testing.T) {
	t.Run("UC-055/BR-001 a stack Put reports the value in force with its writer and version", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		res := f.put(WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev", Writer: "stage"})
		c := wantOne(t, rec.take(), ChangeValue, "decision.backend")
		if c.ScopeName != registry.ScopeStack || c.Writer != "stage" || c.DeclaredVariant != "jev" || c.Version != res.SnapshotVersion {
			t.Fatalf("change %+v, want stack jev by stage at version %d", c, res.SnapshotVersion)
		}
		if c.SlotVersion <= firstSlotVersion {
			t.Fatalf("slot version %d, want above %d after a write", c.SlotVersion, firstSlotVersion)
		}
	})
	t.Run("UC-055/BR-001 two Puts are reported in order", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.put(WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev", Writer: "a"})
		f.put(WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "rules", Writer: "b"})
		got := rec.take()
		if len(got) != 2 || got[0].Writer != "a" || got[1].Writer != "b" || got[0].Version >= got[1].Version || got[0].SlotVersion >= got[1].SlotVersion {
			t.Fatalf("changes %+v, want a then b with rising versions", got)
		}
	})
	t.Run("UC-055/BR-001 a dry run, a rejected write and a no-op clear report nothing", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		if _, err := f.state.Put(WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev", DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.state.Put(WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "nope"}); err == nil {
			t.Fatal("want a rejected write")
		}
		if _, err := f.state.Clear(ClearRequest{Set: "agent", Flag: "decision.backend"}); err != nil {
			t.Fatal(err)
		}
		if got := rec.take(); len(got) != 0 {
			t.Fatalf("got %+v, want no change", got)
		}
	})
	t.Run("UC-055/BR-001 DELETE /{set}?writer= reports each cleared overlay with the selector", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.put(WriteRequest{Set: "agent", Flag: "enabled", Variant: "off", Writer: "stage"})
		rec.take()
		if _, err := f.state.ClearWriter("agent", "stage"); err != nil {
			t.Fatal(err)
		}
		c := wantOne(t, rec.take(), ChangeCleared, "enabled")
		if c.Writer != "stage" || c.Value != nil {
			t.Fatalf("change %+v, want cleared by the selector stage", c)
		}
	})
	t.Run("UC-055/BR-001 an append reports the event list, a clear reports an empty list", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.register("game_a", "c1", "MOCK0001")
		rec.take()
		if _, err := f.state.Append(AppendRequest{Set: "interventions", Flag: "audio_cue", Scope: Scope{GameID: "game_a"}, Payload: map[string]any{"sound": "agent_cue"}, Writer: "agent"}); err != nil {
			t.Fatal(err)
		}
		c := wantOne(t, rec.take(), ChangeEvents, "audio_cue")
		if c.Writer != "agent" || !containsStr(c.Events, "agent_cue") || c.Scope.GameID != "game_a" {
			t.Fatalf("change %+v (events %s), want the list with agent_cue", c, c.Events)
		}
		if _, err := f.state.Clear(ClearRequest{Set: "interventions", Flag: "audio_cue", Scope: Scope{GameID: "game_a"}}); err != nil {
			t.Fatal(err)
		}
		c = wantOne(t, rec.take(), ChangeEvents, "audio_cue")
		if string(c.Events) != "[]" {
			t.Fatalf("events %s, want []", c.Events)
		}
	})
	t.Run("UC-055/BR-001 a game end reports one ChangeGameEnded with the game, even with no values", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.register("game_a", "c1", "MOCK0001")
		c := wantOne(t, rec.take(), ChangeGameRegistered, "")
		if c.Game == nil || c.Game.ID != "game_a" || c.Prev != nil {
			t.Fatalf("change %+v, want game_a first registered", c)
		}
		f.state.EndGame("game_a", EndReasonEnded)
		c = wantOne(t, rec.take(), ChangeGameEnded, "")
		if c.Game == nil || len(c.Game.Serials) != 1 || c.End == nil || c.End.DroppedValues != 0 {
			t.Fatalf("change %+v, want game_a ended with its serials", c)
		}
	})
	t.Run("UC-055/BR-001 a reconcile refresh reports nothing, a serial drop reports the dropped value", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.register("game_a", "c1", "MOCK0001", "MOCK0002")
		f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("5"), Scope: Scope{GameID: "game_a", Serial: "MOCK0002"}})
		rec.take()
		if _, err := f.state.Reconcile("c1", []Game{{ID: "game_a", Kind: "shadow", Serials: []string{"MOCK0002", "MOCK0001"}}}); err != nil {
			t.Fatal(err)
		}
		if got := rec.take(); len(got) != 0 {
			t.Fatalf("got %+v, want nothing for a refresh", got)
		}
		f.register("game_a", "c1", "MOCK0001")
		got := rec.take()
		if len(got) != 2 || got[0].Kind != ChangeCleared || got[0].Scope.Serial != "MOCK0002" || got[1].Kind != ChangeGameRegistered || got[1].Prev == nil || len(got[1].Prev.Serials) != 2 {
			t.Fatalf("got %+v, want the dropped player value then the re-registration with its previous record", got)
		}
	})
	t.Run("UC-055/BR-001 experiment expiry reports each arm value cleared, then the arm ended", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.put(WriteRequest{Set: "game", Flag: "sensitivity", Variant: "slow", Scope: Scope{ExperimentID: "exp_1", Arm: "control"}})
		rec.take()
		f.clock.Advance(DefaultExperimentIdle)
		if n := f.state.ExpireExperiments(); n != 1 {
			t.Fatalf("expired %d, want 1", n)
		}
		got := rec.take()
		if len(got) != 2 || got[0].Kind != ChangeCleared || got[0].Scope.Arm != "control" || got[1].Kind != ChangeExperimentEnded || got[1].Scope.ExperimentID != "exp_1" || got[1].Scope.Arm != "control" {
			t.Fatalf("got %+v, want the arm value cleared then exp_1/control ended", got)
		}
	})
	t.Run("UC-055/BR-001 a failed persisted write that rolls back reports nothing, a kill switch reports its in-memory value", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "admin.json")
		f, rec := observedFixture(t, fixtureOpts{stateFile: path})
		f.put(WriteRequest{Set: "game", Flag: "sensitivity", Variant: "slow", Persist: true})
		breakStateFile(t, path)
		rec.take()
		if _, err := f.state.Put(WriteRequest{Set: "game", Flag: "num_teams", Variant: "four", Persist: true}); err == nil {
			t.Fatal("want persist_failed")
		}
		if got := rec.take(); len(got) != 0 {
			t.Fatalf("got %+v, want nothing for a rolled-back write", got)
		}
		if _, err := f.state.Put(WriteRequest{Set: "agent", Flag: "interventions_allowed", Variant: "none", Persist: true, Writer: "menu"}); err != nil {
			t.Fatal(err)
		}
		c := wantOne(t, rec.take(), ChangeValue, "interventions_allowed")
		if c.DeclaredVariant != "none" || c.Persisted {
			t.Fatalf("change %+v, want none in memory (not persisted)", c)
		}
	})
	t.Run("UC-055/BR-001 no observer is fine", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.put(WriteRequest{Set: "agent", Flag: "decision.backend", Variant: "jev"})
	})
}

// UC: UC-054 BR-005
// A pulled write carries the slot version it read: a local write landing in
// between makes it 412 slot_changed and nothing changes (opposer blocker 2).
func TestSlotVersion_CompareAndSet(t *testing.T) {
	t.Run("UC-054/BR-005 a local kill switch between read and write wins", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{})
		f.put(WriteRequest{Set: "agent", Flag: "enabled", Variant: "act", Writer: "menu"})
		cs, _ := f.state.Changes()
		read := findChange(t, cs, "enabled", registry.ScopeStack)
		f.put(WriteRequest{Set: "agent", Flag: "enabled", Variant: "off", Writer: "menu"})
		rec.take()
		_, err := f.state.Put(WriteRequest{Set: "agent", Flag: "enabled", Variant: "act", Writer: "flagsmith", IfSlotVersion: read.SlotVersion})
		wantReason(t, err, 412, fserr.SlotChanged)
		if got := rec.take(); len(got) != 0 {
			t.Fatalf("got %+v, want nothing changed", got)
		}
		if v := f.snapshotFlag("enabled")["defaultVariant"]; v != "off" {
			t.Fatalf("enabled = %v, want the local off", v)
		}
	})
	t.Run("UC-054/BR-005 an unchanged slot takes the write; a clear and an append check it too", func(t *testing.T) {
		f, _ := observedFixture(t, fixtureOpts{})
		cs, _ := f.state.Changes()
		read := findChange(t, cs, "enabled", registry.ScopeStack)
		res, err := f.state.Put(WriteRequest{Set: "agent", Flag: "enabled", Variant: "off", IfSlotVersion: read.SlotVersion})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.state.Clear(ClearRequest{Set: "agent", Flag: "enabled", IfSlotVersion: read.SlotVersion})
		wantReason(t, err, 412, fserr.SlotChanged)
		f.register("game_a", "c1", "MOCK0001")
		_, err = f.state.Append(AppendRequest{Set: "interventions", Flag: "audio_cue", Scope: Scope{GameID: "game_a"}, Payload: map[string]any{"sound": "agent_cue"}, IfSlotVersion: res.SnapshotVersion + 1000})
		wantReason(t, err, 412, fserr.SlotChanged)
	})
	t.Run("UC-054/BR-005 a Flagsmith clear of a persist row clears its persisted value in one change", func(t *testing.T) {
		f, rec := observedFixture(t, fixtureOpts{stateFile: filepath.Join(t.TempDir(), "admin.json")})
		f.put(WriteRequest{Set: "game", Flag: "sensitivity", Variant: "slow", Persist: true, Writer: "menu"})
		f.put(WriteRequest{Set: "game", Flag: "sensitivity", Variant: "fast", Writer: "stage"})
		rec.take()
		res, err := f.state.Clear(ClearRequest{Set: "game", Flag: "sensitivity", Writer: "flagsmith"})
		if err != nil || res.Cleared != 1 || !res.Persisted {
			t.Fatalf("res %+v err %v, want one persisted clear", res, err)
		}
		c := wantOne(t, rec.take(), ChangeCleared, "sensitivity")
		if c.Writer != "flagsmith" {
			t.Fatalf("change %+v, want cleared by flagsmith", c)
		}
	})
	// services.md §7.3 / UC-054 Pull: every pulled write is conditional on the
	// local slot, empty slots included (#2594 review finding 2).
	t.Run("UC-054/BR-005 a value slot cleared locally after the read refuses the pulled Put and Clear", func(t *testing.T) {
		cases := []struct {
			name, set, flag string
			scope           Scope
			write           WriteRequest
		}{
			{"stack", "agent", "enabled", Scope{}, WriteRequest{Variant: "act"}},
			{"game:<id>", "interventions", "volume_override", Scope{GameID: "game_a"}, WriteRequest{Value: raw("0.5")}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f, rec := observedFixture(t, fixtureOpts{})
				f.register("game_a", "c1", "MOCK0001")
				w := tc.write
				w.Set, w.Flag, w.Scope, w.Writer = tc.set, tc.flag, tc.scope, "menu"
				f.put(w)
				read := f.state.SlotVersion(tc.set, tc.flag, tc.scope)
				if read <= firstSlotVersion {
					t.Fatalf("read slot version %d, want a written slot", read)
				}
				if _, err := f.state.Clear(ClearRequest{Set: tc.set, Flag: tc.flag, Scope: tc.scope, Writer: "menu"}); err != nil {
					t.Fatal(err)
				}
				if now := f.state.SlotVersion(tc.set, tc.flag, tc.scope); now <= read {
					t.Fatalf("slot version %d after the clear, want above %d (versions survive clears)", now, read)
				}
				rec.take()
				pulled := tc.write
				pulled.Set, pulled.Flag, pulled.Scope, pulled.Writer, pulled.IfSlotVersion = tc.set, tc.flag, tc.scope, "flagsmith", read
				_, err := f.state.Put(pulled)
				wantReason(t, err, 412, fserr.SlotChanged)
				_, err = f.state.Clear(ClearRequest{Set: tc.set, Flag: tc.flag, Scope: tc.scope, Writer: "flagsmith", IfSlotVersion: read})
				wantReason(t, err, 412, fserr.SlotChanged)
				if got := rec.take(); len(got) != 0 {
					t.Fatalf("got %+v, want nothing changed", got)
				}
				if _, err := f.state.Put(WriteRequest{Set: tc.set, Flag: tc.flag, Scope: tc.scope, Variant: tc.write.Variant, Value: tc.write.Value, Writer: "flagsmith", IfSlotVersion: f.state.SlotVersion(tc.set, tc.flag, tc.scope)}); err != nil {
					t.Fatalf("a pulled write with the current version of the empty slot: %v", err)
				}
			})
		}
	})
}

// UC: UC-054 BR-005
// State.SlotVersion reads any slot, empty ones included (#2594 F3).
func TestSlotVersion_Read(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	game := Scope{GameID: "game_a"}
	serial := Scope{GameID: "game_a", Serial: "MOCK0001"}
	if v := f.state.SlotVersion("interventions", "volume_override", game); v != 0 {
		t.Fatalf("version %d of an unregistered game's slot, want 0", v)
	}
	f.register("game_a", "c1", "MOCK0001", "MOCK0002")
	for _, tc := range []struct {
		set, flag string
		scope     Scope
		want      uint64
	}{
		{"agent", "enabled", Scope{}, firstSlotVersion},
		{"interventions", "volume_override", game, firstSlotVersion},
		{"interventions", "shield_seconds", serial, firstSlotVersion},
		{"game", "enabled", Scope{}, 0},          // not a flag of that set
		{"agent", "no_such_flag", Scope{}, 0},    // unknown flag
		{"agent", "enabled", Scope{Arm: "x"}, 0}, // fits no scope
	} {
		if v := f.state.SlotVersion(tc.set, tc.flag, tc.scope); v != tc.want {
			t.Fatalf("SlotVersion(%s, %s, %+v) = %d, want %d", tc.set, tc.flag, tc.scope, v, tc.want)
		}
	}
	// A serial drop moves the slot version on (it survives the drop).
	two := Scope{GameID: "game_a", Serial: "MOCK0002"}
	f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("5"), Scope: two})
	before := f.state.SlotVersion("interventions", "shield_seconds", two)
	f.register("game_a", "c1", "MOCK0001")
	if after := f.state.SlotVersion("interventions", "shield_seconds", two); after <= before {
		t.Fatalf("slot version %d after the serial drop, want above %d", after, before)
	}
	// A game end forgets them: the slot has no version until the game is back.
	f.state.EndGame("game_a", EndReasonEnded)
	if v := f.state.SlotVersion("interventions", "shield_seconds", two); v != 0 {
		t.Fatalf("version %d after the game ended, want 0", v)
	}
}

// UC: UC-053 A3
// An experiment whose arm values were cleared by DELETE leaves no slot
// versions behind once no game carries it (NFR-051, #2594 review finding 7).
func TestSlotVersion_ClearedExperimentIsForgotten(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	arm := Scope{ExperimentID: "exp_1", Arm: "control"}
	if _, err := f.state.RegisterGame(Game{ID: "game_a", CoordinatorInstance: "c1", Kind: "shadow", ExperimentID: "exp_1", Arm: "control"}); err != nil {
		t.Fatal(err)
	}
	f.put(WriteRequest{Set: "game", Flag: "sensitivity", Variant: "slow", Scope: arm})
	if _, err := f.state.Clear(ClearRequest{Set: "game", Flag: "sensitivity", Scope: arm}); err != nil {
		t.Fatal(err)
	}
	f.state.ExpireExperiments()
	if f.state.slotVer["x:exp_1"] == nil {
		t.Fatal("the versions of an experiment a live game carries went")
	}
	f.state.EndGame("game_a", EndReasonEnded)
	f.state.ExpireExperiments() // last sweep's carrier still counts
	f.state.ExpireExperiments()
	if m := f.state.slotVer["x:exp_1"]; m != nil {
		t.Fatalf("slot versions %v of a cleared experiment no game carries, want none", m)
	}
}

// UC: UC-054 BR-005
func TestChanges_Snapshot(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.register("game_a", "c1", "MOCK0001")
	f.put(WriteRequest{Set: "interventions", Flag: "shield_seconds", Value: raw("5"), Scope: Scope{GameID: "game_a", Serial: "MOCK0001"}})
	cs, version := f.state.Changes()
	if version != f.state.Version() {
		t.Fatalf("version %d, want %d", version, f.state.Version())
	}
	if cs[0].Kind != ChangeGameRegistered || cs[0].Game.ID != "game_a" {
		t.Fatalf("first change %+v, want the game", cs[0])
	}
	c := findChange(t, cs, "shield_seconds", registry.ScopeGameSerial)
	if string(c.Value) != "5" || c.SlotVersion <= firstSlotVersion {
		t.Fatalf("change %+v, want 5 with a written slot version", c)
	}
	if c := findChange(t, cs, "decision.backend", registry.ScopeStack); c.Kind != ChangeCleared || c.SlotVersion != firstSlotVersion {
		t.Fatalf("change %+v, want an untouched cleared stack slot", c)
	}
}

func findChange(t *testing.T, cs []Change, flag, scope string) Change {
	t.Helper()
	for _, c := range cs {
		if c.Flag == flag && c.ScopeName == scope {
			return c
		}
	}
	t.Fatalf("no %s change of %s", scope, flag)
	return Change{}
}
