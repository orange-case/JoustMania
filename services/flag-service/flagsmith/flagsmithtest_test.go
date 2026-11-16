package flagsmith

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
)

// The fake answers the start-up Admin endpoints by default (#2594 F7).
func TestFakeAdminDefaults(t *testing.T) {
	f := flagsmithtest.New(t)
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, f.URL()+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}
	if code, m := do("GET", "environments/k/", ""); code != 200 || m["project"] != float64(42) || m["use_v2_feature_versioning"] != false || m["use_identity_overrides_in_local_eval"] != true {
		t.Fatalf("environment %d %v", code, m)
	}
	if code, m := do("GET", "projects/42/features/", ""); code != 200 || m["results"] == nil {
		t.Fatalf("features %d %v", code, m)
	}
	if code, m := do("POST", "projects/42/features/", `{"name":"a"}`); code != 201 || m["id"] == nil {
		t.Fatalf("create feature %d %v", code, m)
	}
	for i := 0; i < 12; i++ {
		do("POST", "projects/42/tags/", `{"label":"t"}`)
	}
	if code, m := do("GET", "projects/42/tags/", ""); code != 200 || len(m["results"].([]any)) != 10 || m["next"] == nil {
		t.Fatalf("tags page 1 %d %v", code, m)
	}
	if code, m := do("GET", "projects/42/tags/?page=2", ""); code != 200 || len(m["results"].([]any)) != 2 || m["next"] != nil {
		t.Fatalf("tags page 2 %d %v", code, m)
	}
	if code, m := do("GET", "environments/k/edge-identities/", ""); code != 200 || len(m["results"].([]any)) != 0 || m["last_evaluated_key"] != nil {
		t.Fatalf("edge identities %d %v", code, m)
	}
	if code, _ := do("DELETE", "environments/k/edge-identities/u-1/", ""); code != 204 {
		t.Fatalf("delete identity %d", code)
	}
	f.Handle("GET /api/v1/projects/42/tags/", func(w http.ResponseWriter, _ *http.Request) {
		flagsmithtest.WriteJSON(w, 418, map[string]any{})
	})
	if code, _ := do("GET", "projects/42/tags/", ""); code != 418 {
		t.Fatalf("override %d, want 418", code)
	}
}
