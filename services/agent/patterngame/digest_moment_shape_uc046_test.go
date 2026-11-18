package patterngame

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// UC: UC-046 BR-011
func TestFlagDigestCoversTheMomentShape(t *testing.T) {
	t.Run("UC-046/BR-011 observability/telemetry.moment_shape is covered, nothing else of the set", func(t *testing.T) {
		if !DigestKeysCovered("observability", "telemetry.moment_shape") {
			t.Fatal("observability/telemetry.moment_shape not covered (#2658 slice 3)")
		}
		if DigestKeysCovered("observability", "log_level") || DigestKeysCovered("game", "telemetry.moment_shape") {
			t.Error("the moment shape entry leaks into another key or set")
		}
	})

	t.Run("UC-046/BR-011 a moment shape change changes the digest", func(t *testing.T) {
		values := map[string]map[string]any{
			"game":          {"sensitivity": 2.0},
			"observability": {"telemetry.moment_shape": "span_event", "log_level": "info"},
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			set := strings.TrimPrefix(req.Header.Get("Flagd-Selector"), "flagSetId=")
			var flags []map[string]any
			for k, v := range values[set] {
				flags = append(flags, map[string]any{"key": k, "value": v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"flags": flags})
		}))
		defer srv.Close()
		evalCtx := map[string]any{"game_kind": "shadow"}
		before := ResolvedFlagDigest(context.Background(), srv.Client(), srv.URL, evalCtx)
		values["observability"]["log_level"] = "error"
		if ResolvedFlagDigest(context.Background(), srv.Client(), srv.URL, evalCtx) != before {
			t.Error("an uncovered observability key changed the digest")
		}
		values["observability"]["telemetry.moment_shape"] = "log_event"
		if ResolvedFlagDigest(context.Background(), srv.Client(), srv.URL, evalCtx) == before {
			t.Error("a telemetry.moment_shape change did not change the digest")
		}
	})
}
