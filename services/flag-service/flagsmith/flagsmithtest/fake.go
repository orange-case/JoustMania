// Package flagsmithtest is a fake hosted Flagsmith for the link's tests
// (#2588; shared by the tests of #2589, #2590 and #2591, in package flagsmith
// and flagsmith_test alike). It serves the environment document with
// Last-Modified / If-Modified-Since, records every request, lets a test add
// handlers for the Admin API and feeds a realtime SSE stream.
package flagsmithtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// APIPath is the path the fake serves the v1 API (and the document) under.
const APIPath = "/api/v1/"

// Request is one request the fake received.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// Fake is a fake Flagsmith.
type Fake struct {
	// Server is the running test server.
	Server *httptest.Server
	// ServerKey, when set, is the X-Environment-Key the document needs (else
	// 401).
	ServerKey string

	mux          *http.ServeMux
	mu           sync.Mutex
	doc          []byte
	lastModified string
	requests     []Request
	failStatus   int
	delay        time.Duration
	events       chan string
}

// DefaultDocument is the document a new fake serves: no features, no
// identities, identity overrides in local evaluation on (the link stays
// offline without it, identity_overrides_disabled).
const DefaultDocument = `{"name":"test","use_identity_overrides_in_local_eval":true,"feature_states":[],"identity_overrides":[]}`

// New starts a fake Flagsmith, closed when the test ends. It serves
// DefaultDocument until SetDocument.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{mux: http.NewServeMux(), doc: []byte(DefaultDocument), events: make(chan string, 16)}
	f.mux.HandleFunc("GET "+APIPath+"environment-document/", f.document)
	f.mux.HandleFunc("GET /sse/", f.sse)
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// URL is the API base, ending with "/" (FLAGSMITH_API_URL and
// FLAGSMITH_DOCUMENT_URL).
func (f *Fake) URL() string { return f.Server.URL + APIPath }

// SSEURL is the realtime stream's URL.
func (f *Fake) SSEURL() string { return f.Server.URL + "/sse/" }

// Handle adds a handler for a net/http ServeMux pattern ("PUT /api/v1/...").
// A pattern can be added once.
func (f *Fake) Handle(pattern string, h http.HandlerFunc) { f.mux.HandleFunc(pattern, h) }

// SetDocument sets the environment document (any JSON-encodable value or raw
// bytes) and its Last-Modified ("" = none).
func (f *Fake) SetDocument(doc any, lastModified string) {
	raw, ok := doc.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(doc); err != nil {
			panic(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doc, f.lastModified = raw, lastModified
}

// SetIdentityOverridesInLocalEval sets use_identity_overrides_in_local_eval
// in the current document (true in DefaultDocument).
func (f *Fake) SetIdentityOverridesInLocalEval(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(f.doc, &doc); err != nil {
		panic(err)
	}
	doc["use_identity_overrides_in_local_eval"] = on
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	f.doc = raw
}

// Fail makes every request answer status (0: serve normally).
func (f *Fake) Fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus = status
}

// Delay holds every request for d before it is served.
func (f *Fake) Delay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// SendEvent sends one SSE data line to the open realtime stream.
func (f *Fake) SendEvent(data string) { f.events <- data }

// Requests returns every request so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// RequestsTo returns the requests with method (""= any) whose path starts
// with prefix.
func (f *Fake) RequestsTo(method, prefix string) []Request {
	var out []Request
	for _, r := range f.Requests() {
		if (method == "" || r.Method == method) && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
	fail, delay := f.failStatus, f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if fail != 0 {
		http.Error(w, `{"detail":"failing"}`, fail)
		return
	}
	f.mux.ServeHTTP(w, r)
}

func (f *Fake) document(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	doc, lm, key := f.doc, f.lastModified, f.ServerKey
	f.mu.Unlock()
	if key != "" && r.Header.Get("X-Environment-Key") != key {
		http.Error(w, `{"detail":"Invalid or missing Environment Key"}`, http.StatusUnauthorized)
		return
	}
	if lm != "" && r.Header.Get("If-Modified-Since") == lm {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if lm != "" {
		w.Header().Set("Last-Modified", lm)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(doc)
}

func (f *Fake) sse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-f.events:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
