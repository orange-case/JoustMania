package main

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/joustmania/agent/decision"
	"github.com/joustmania/agent/gamecontext"
)

const (
	httpLogTraceHex = "0af7651916cd43dd8448eb211c80319c"
	httpLogRootHex  = "b7ad6b7169203331"
)

func newHTTPTestPipeline(t *testing.T) (*pipeline, *gamecontext.Multiplexer) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := gamecontext.NewMultiplexer(func(string) *gamecontext.Store {
		return gamecontext.NewStore(time.Hour, time.Hour, nil)
	})
	loops := decision.NewLoopSet(func(string) *decision.Loop {
		return decision.NewLoop(nil, logger)
	})
	return newPipeline(mux, loops, time.Hour), mux
}

// gameStartLog is the coordinator's agent-only game.start record with a sampled trace context.
func gameStartLog(t *testing.T, gameID string) plog.Logs {
	t.Helper()
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "game-coordinator")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetEventName("game.start")
	lr.Attributes().PutStr("game.id", gameID)
	lr.Attributes().PutStr("game.kind", "real")
	lr.Attributes().PutStr("orangecase.audience", "agent")
	var tid pcommon.TraceID
	var sid pcommon.SpanID
	mustHex(t, httpLogTraceHex, tid[:])
	mustHex(t, httpLogRootHex, sid[:])
	lr.SetTraceID(tid)
	lr.SetSpanID(sid)
	lr.SetFlags(plog.DefaultLogRecordFlags.WithIsSampled(true))
	return ld
}

func mustHex(t *testing.T, s string, dst []byte) {
	t.Helper()
	if _, err := hex.Decode(dst, []byte(s)); err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
}

func gzipped(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func post(t *testing.T, h http.Handler, path, contentType, encoding string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// UC: UC-041 A6
// NFR: NFR-024
func TestOTLPHTTP_DeliversLikeGRPC(t *testing.T) {
	t.Run("UC-041/A6 a gzip protobuf game.start over OTLP/HTTP gives the agent the game's trace ids", func(t *testing.T) {
		pipe, mux := newHTTPTestPipeline(t)
		h := newOTLPHTTPHandler(pipe)
		raw, err := plogotlp.NewExportRequestFromLogs(gameStartLog(t, "game-H")).MarshalProto()
		if err != nil {
			t.Fatal(err)
		}
		rec := post(t, h, "/v1/logs", "application/x-protobuf", "gzip", gzipped(t, raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != otlpProtobuf {
			t.Fatalf("response content type = %q, want %q", ct, otlpProtobuf)
		}
		if err := plogotlp.NewExportResponse().UnmarshalProto(rec.Body.Bytes()); err != nil {
			t.Fatalf("response is no ExportLogsServiceResponse: %v", err)
		}
		snap, ok := mux.Snapshot("game-H")
		if !ok {
			t.Fatal("game.start opened no context for game-H")
		}
		if snap.GameTraceID != httpLogTraceHex || snap.GameTraceSpanID != httpLogRootHex {
			t.Fatalf("trace ids = %s/%s, want %s/%s", snap.GameTraceID, snap.GameTraceSpanID, httpLogTraceHex, httpLogRootHex)
		}
	})

	t.Run("NFR-024 traces and metrics are accepted uncompressed", func(t *testing.T) {
		pipe, _ := newHTTPTestPipeline(t)
		h := newOTLPHTTPHandler(pipe)
		td := ptrace.NewTraces()
		td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("x")
		traces, _ := ptraceotlp.NewExportRequestFromTraces(td).MarshalProto()
		metrics, _ := pmetricotlp.NewExportRequestFromMetrics(liveGameMetrics("game-M", "PA")).MarshalProto()
		for path, body := range map[string][]byte{"/v1/traces": traces, "/v1/metrics": metrics} {
			if rec := post(t, h, path, "application/x-protobuf", "", body); rec.Code != http.StatusOK {
				t.Fatalf("%s status = %d (%s), want 200", path, rec.Code, rec.Body.String())
			}
		}
	})
}

// NFR: NFR-024
func TestOTLPHTTP_RejectsWhatItCannotRead(t *testing.T) {
	pipe, mux := newHTTPTestPipeline(t)
	h := newOTLPHTTPHandler(pipe)
	raw, _ := plogotlp.NewExportRequestFromLogs(gameStartLog(t, "game-R")).MarshalProto()
	huge := make([]byte, otlpHTTPMaxBody+1)

	cases := []struct {
		name, method, path, contentType, encoding string
		body                                      []byte
		want                                      int
	}{
		{"JSON is not served", http.MethodPost, "/v1/logs", "application/json", "", []byte("{}"), http.StatusUnsupportedMediaType},
		{"an unknown encoding", http.MethodPost, "/v1/logs", "application/x-protobuf", "br", raw, http.StatusUnsupportedMediaType},
		{"a body that is not gzip", http.MethodPost, "/v1/logs", "application/x-protobuf", "gzip", raw, http.StatusBadRequest},
		{"a body that is not OTLP", http.MethodPost, "/v1/logs", "application/x-protobuf", "", []byte{0xff, 0xff, 0xff}, http.StatusBadRequest},
		{"a body above the limit", http.MethodPost, "/v1/logs", "application/x-protobuf", "", huge, http.StatusRequestEntityTooLarge},
		{"a gzip body inflating above the limit", http.MethodPost, "/v1/logs", "application/x-protobuf", "gzip", gzipped(t, huge), http.StatusRequestEntityTooLarge},
		{"GET", http.MethodGet, "/v1/logs", "", "", nil, http.StatusMethodNotAllowed},
		{"an unknown path", http.MethodPost, "/v1/profiles", "application/x-protobuf", "", raw, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run("NFR-024 "+tc.name+" is refused", func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if _, ok := mux.Snapshot("game-R"); ok {
		t.Fatal("a refused request reached the game context")
	}
}
