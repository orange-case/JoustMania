package flagsmith

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
)

func testConfig(f *flagsmithtest.Fake) Config {
	return Config{
		Enabled: true, APIURL: f.URL(), DocumentURL: f.URL(),
		ServerKey: "ser.secretserverkey", AdminKey: "secretadminkey", EnvironmentKey: "envkey123", ProjectID: 42,
		Poll: DefaultPoll, PushRate: DefaultPushRate, Timeout: DefaultTimeout,
	}
}

func noSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{"secretserverkey", "secretadminkey", "token=hush"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q leaks %q", err, s)
		}
	}
}

// UC: UC-054 A1
// UC: UC-055 BR-001
func TestClient(t *testing.T) {
	ctx := context.Background()
	t.Run("UC-054/A1 the document pull sends the server key and is conditional", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.ServerKey = "ser.secretserverkey"
		f.SetDocument(map[string]any{"name": "stage", "feature_states": []any{}}, "Wed, 11 Nov 2026 10:00:00 GMT")
		c := NewClient(testConfig(f), nil)
		doc, lm, notModified, err := c.EnvironmentDocument(ctx, "")
		if err != nil || notModified || doc.Name != "stage" || lm != "Wed, 11 Nov 2026 10:00:00 GMT" {
			t.Fatalf("doc %+v lm %q nm %v err %v", doc, lm, notModified, err)
		}
		_, _, notModified, err = c.EnvironmentDocument(ctx, lm)
		if err != nil || !notModified {
			t.Fatalf("second pull: not modified %v err %v, want a 304", notModified, err)
		}
		reqs := f.RequestsTo(http.MethodGet, flagsmithtest.APIPath+"environment-document/")
		if len(reqs) != 2 || reqs[1].Header.Get("If-Modified-Since") != lm || reqs[0].Header.Get("If-Modified-Since") != "" {
			t.Fatalf("requests %+v, want a full then a conditional pull", reqs)
		}
	})
	t.Run("UC-054/A1 the fake's default document has identity overrides in local evaluation on, with a knob", func(t *testing.T) {
		f := flagsmithtest.New(t)
		c := NewClient(testConfig(f), nil)
		if doc, _, _, err := c.EnvironmentDocument(ctx, ""); err != nil || !doc.UseIdentityOverridesInLocalEval {
			t.Fatalf("doc %+v err %v, want use_identity_overrides_in_local_eval on", doc, err)
		}
		f.SetIdentityOverridesInLocalEval(false)
		if doc, _, _, err := c.EnvironmentDocument(ctx, ""); err != nil || doc.UseIdentityOverridesInLocalEval {
			t.Fatalf("doc %+v err %v, want it off", doc, err)
		}
	})
	t.Run("UC-054/A1 the document carries the X-Flagsmith-Document-Updated-At header, empty when absent", func(t *testing.T) {
		for _, want := range []string{"2026-11-11T10:00:00.123456Z", ""} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if want != "" {
					w.Header().Set("X-Flagsmith-Document-Updated-At", want)
				}
				_, _ = w.Write([]byte(`{"name":"stage"}`))
			}))
			cfg := Config{Enabled: true, APIURL: srv.URL + "/api/v1/", DocumentURL: srv.URL + "/api/v1/", ServerKey: "ser.secretserverkey",
				Poll: DefaultPoll, PushRate: DefaultPushRate, Timeout: DefaultTimeout}
			doc, _, _, err := NewClient(cfg, nil).EnvironmentDocument(ctx, "")
			srv.Close()
			if err != nil || doc.HeaderUpdatedAt != want {
				t.Fatalf("doc %+v err %v, want HeaderUpdatedAt %q", doc, err, want)
			}
		}
	})
	t.Run("UC-054/A1 a wrong server key is unauthorized and the link goes down", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.ServerKey = "other"
		_, _, _, err := NewClient(testConfig(f), nil).EnvironmentDocument(ctx, "")
		if Reason(err) != ReasonUnauthorized || !IsLinkDown(err) {
			t.Fatalf("err %v reason %q, want unauthorized, link down", err, Reason(err))
		}
		noSecret(t, err)
	})
	t.Run("UC-055/BR-001 Admin sends the admin key, resolves relative and host-root paths, keeps the query", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.Handle("GET /api/v1/projects/42/features/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"results":[{"id":7}]}`))
		})
		f.Handle("DELETE /api/__future__/x/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		c := NewClient(testConfig(f), nil)
		var out struct {
			Results []struct{ ID int } `json:"results"`
		}
		if _, err := c.Admin(ctx, http.MethodGet, "projects/42/features/?q=enabled", nil, &out); err != nil || len(out.Results) != 1 || out.Results[0].ID != 7 {
			t.Fatalf("out %+v err %v", out, err)
		}
		if _, err := c.Admin(ctx, http.MethodDelete, "/api/__future__/x/", map[string]string{"a": "b"}, nil); err != nil {
			t.Fatal(err)
		}
		reqs := f.Requests()
		if reqs[0].Header.Get("Authorization") != "Api-Key secretadminkey" || reqs[0].Query != "q=enabled" {
			t.Fatalf("request %+v, want the admin key and the query", reqs[0])
		}
		if reqs[1].Path != "/api/__future__/x/" || string(reqs[1].Body) != `{"a":"b"}` {
			t.Fatalf("request %+v, want the host-root path with a JSON body on DELETE", reqs[1])
		}
	})
	t.Run("UC-055/BR-001 429 carries Retry-After and is a per-key refusal", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.Handle("POST /api/v1/x/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"detail":"slow down"}`, http.StatusTooManyRequests)
		})
		_, err := NewClient(testConfig(f), nil).Admin(ctx, http.MethodPost, "x/", map[string]int{"a": 1}, nil)
		var he *HTTPError
		if !asHTTP(err, &he) || he.RetryAfter != 7*time.Second || !he.JSONDetail || Reason(err) != ReasonRateLimited || IsLinkDown(err) {
			t.Fatalf("err %v (%+v), want 429 rate_limited after 7 s, not link down", err, he)
		}
	})
	t.Run("UC-054/A1 a slow Flagsmith times out within the timeout", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.Delay(2 * time.Second)
		cfg := testConfig(f)
		cfg.Timeout = 200 * time.Millisecond
		start := time.Now()
		_, _, _, err := NewClient(cfg, nil).EnvironmentDocument(ctx, "")
		if Reason(err) != ReasonTimeout || !IsLinkDown(err) || time.Since(start) > time.Second {
			t.Fatalf("err %v reason %q after %v, want timeout within 1 s", err, Reason(err), time.Since(start))
		}
		noSecret(t, err)
	})
	t.Run("UC-054/A1 an oversized document is refused", func(t *testing.T) {
		f := flagsmithtest.New(t)
		f.SetDocument([]byte(`{"name":"`+strings.Repeat("x", MaxDocumentBytes)+`"}`), "")
		_, _, _, err := NewClient(testConfig(f), nil).EnvironmentDocument(ctx, "")
		if Reason(err) != ReasonResponseTooLarge {
			t.Fatalf("err %v, want response_too_large", err)
		}
	})
	t.Run("UC-054/A1 no error names the webhook URL or a key", func(t *testing.T) {
		f := flagsmithtest.New(t)
		url := f.Server.URL + "/hook?token=hush"
		f.Server.Close()
		_, err := NewClient(testConfig(f), nil).PostJSON(ctx, url, map[string]string{"status": "unhealthy"})
		if err == nil || Reason(err) != ReasonConnection {
			t.Fatalf("err %v reason %q, want a connection error", err, Reason(err))
		}
		noSecret(t, err)
	})
	// C-016 (#2594 review finding 4): a redirect would carry the keys to
	// another host.
	t.Run("UC-054/A1 a redirect is not followed: no key reaches another host, the 3xx is the error", func(t *testing.T) {
		var hits atomic.Int64
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer other.Close()
		f := flagsmithtest.New(t)
		f.Handle("GET /api/v1/moved/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/api/v1/moved/", http.StatusFound)
		})
		f.Handle("GET /sse-moved/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/sse/", http.StatusTemporaryRedirect)
		})
		for name, hc := range map[string]*http.Client{"default client": nil, "injected client": {Timeout: time.Second}} {
			c := NewClient(testConfig(f), hc)
			_, err := c.Admin(ctx, http.MethodGet, "moved/", nil, nil)
			if Reason(err) != "http_302" {
				t.Fatalf("%s: err %v reason %q, want http_302", name, err, Reason(err))
			}
			_, err = c.Stream(ctx, f.Server.URL+"/sse-moved/", map[string]string{"X-Environment-Key": "ser.secretserverkey"})
			if Reason(err) != "http_307" {
				t.Fatalf("%s: stream err %v reason %q, want http_307", name, err, Reason(err))
			}
			if hc != nil && hc.CheckRedirect != nil {
				t.Fatalf("%s: the injected client was changed", name)
			}
		}
		if n := hits.Load(); n != 0 {
			t.Fatalf("the other host got %d requests, want none", n)
		}
	})
}

func asHTTP(err error, he **HTTPError) bool {
	return errors.As(err, he)
}

// UC: UC-054 A1
func TestReasonClasses(t *testing.T) {
	cases := []struct {
		err      error
		reason   string
		linkDown bool
	}{
		{&HTTPError{Status: 401}, ReasonUnauthorized, true},
		{&HTTPError{Status: 404}, ReasonNotFound, false},
		{&HTTPError{Status: 400, Body: []byte(`{"detail":"A change request is required"}`)}, ReasonChangeRequestRequired, false},
		{&HTTPError{Status: 502}, "http_502", true},
		{&HTTPError{Status: 400}, "http_400", false},
		{ReasonError{Reason: ReasonIdentityOverridesDisabled}, ReasonIdentityOverridesDisabled, true},
		{ReasonError{Reason: ReasonProjectMismatch}, ReasonProjectMismatch, true},
		{context.DeadlineExceeded, ReasonTimeout, true},
	}
	for _, c := range cases {
		if got := Reason(c.err); got != c.reason {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.reason)
		}
		if got := IsLinkDown(c.err); got != c.linkDown {
			t.Errorf("IsLinkDown(%v) = %v, want %v", c.err, got, c.linkDown)
		}
	}
}
