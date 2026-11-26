package experiment

import (
	"testing"
)

// fitness_store_test.go covers the #965 finalized-per-game fitness store: the
// real-versus-shadow instrumentation (#1017).

// TestFitnessStore_RecordAndGet: a recorded sample is read back by game_id.
func TestFitnessStore_RecordAndGet(t *testing.T) {
	s := NewFitnessStore(0) // default capacity
	s.Record(FitnessSample{GameID: "game_aaa", Fitness: 0.7, Objective: "endurance", GameKind: "shadow"})

	got, ok := s.Get("game_aaa")
	if !ok {
		t.Fatalf("Get(game_aaa) not found after Record")
	}
	if got.Fitness != 0.7 {
		t.Errorf("fitness = %v, want 0.7", got.Fitness)
	}
	if _, ok := s.Get("game_missing"); ok {
		t.Errorf("Get(game_missing) should be absent")
	}
}

// TestFitnessStore_EmptyGameIDDropped: a sample with no game_id is not stored
// (nothing could ever read it back).
func TestFitnessStore_EmptyGameIDDropped(t *testing.T) {
	s := NewFitnessStore(0)
	s.Record(FitnessSample{GameID: "", Fitness: 0.5, GameKind: "real"})
	if s.Len() != 0 {
		t.Fatalf("empty-game-id sample should be dropped, len = %d", s.Len())
	}
}

// TestFitnessStore_RingEviction: the ring evicts the oldest sample once capacity is
// exceeded, keeping memory bounded.
func TestFitnessStore_RingEviction(t *testing.T) {
	s := NewFitnessStore(2)
	s.Record(FitnessSample{GameID: "g1", Fitness: 0.1, GameKind: "real"})
	s.Record(FitnessSample{GameID: "g2", Fitness: 0.2, GameKind: "real"})
	s.Record(FitnessSample{GameID: "g3", Fitness: 0.3, GameKind: "real"}) // evicts g1

	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2 (ring capacity)", s.Len())
	}
	if _, ok := s.Get("g1"); ok {
		t.Errorf("g1 should have been evicted (oldest)")
	}
	if _, ok := s.Get("g3"); !ok {
		t.Errorf("g3 (newest) should be present")
	}
}

// TestFitnessStore_ReRecordKeepsBounded: re-Recording the same game_id updates in
// place without growing the order ring (so a duplicate cannot evict live samples).
func TestFitnessStore_ReRecordKeepsBounded(t *testing.T) {
	s := NewFitnessStore(2)
	s.Record(FitnessSample{GameID: "g1", Fitness: 0.1, GameKind: "real"})
	s.Record(FitnessSample{GameID: "g2", Fitness: 0.2, GameKind: "real"})
	s.Record(FitnessSample{GameID: "g1", Fitness: 0.9, GameKind: "real"}) // update, not new

	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2 after re-record", s.Len())
	}
	got, _ := s.Get("g1")
	if got.Fitness != 0.9 {
		t.Errorf("re-record fitness = %v, want 0.9", got.Fitness)
	}
	if _, ok := s.Get("g2"); !ok {
		t.Errorf("g2 must not be evicted by a re-record of g1")
	}
}
