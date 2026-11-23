package flagsmith

import "testing"

// UC: UC-054 BR-005
// The header fallback is a float epoch, a few hundred ns off the body
// updated_at of the same document: the two are the same update time, so an
// abandoned time or the newest time read in a row is recognised whichever of
// the two a copy carries (#2602).
func TestSameTimeBodyAndHeader(t *testing.T) {
	body, ok := documentUpdated(&EnvironmentDocument{UpdatedAt: "2026-11-15T12:00:00.123457Z"})
	if !ok {
		t.Fatal("setup: body updated_at not read")
	}
	header, ok := documentUpdated(&EnvironmentDocument{HeaderUpdatedAt: "1794744000.123457"})
	if !ok {
		t.Fatal("setup: header update time not read")
	}
	if body.Equal(header) {
		t.Fatalf("setup: body %v and header %v are exactly equal; pick a fraction float64 cannot hold", body, header)
	}
	if !sameTime(body, header) {
		t.Fatalf("UC-054/BR-005 body %v and header %v of one document are not the same update time", body, header)
	}
	if sameTime(body, body.Add(1500)) {
		t.Fatalf("UC-054/BR-005 times 1.5 µs apart are the same update time, want different")
	}
}
