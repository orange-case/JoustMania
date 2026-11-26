package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// otlpHTTPMaxBody bounds a request body before and after gzip, like gRPC's
// default 4 MiB receive limit on the :4317 receiver.
const otlpHTTPMaxBody = 4 << 20

const otlpProtobuf = "application/x-protobuf"

var errBodyTooLarge = errors.New("request body too large")

// newOTLPHTTPHandler serves OTLP/HTTP (protobuf, identity or gzip) into the same
// receivers as the gRPC server. The collector exports to the agent over HTTP
// because net/http resolves the agent on every new connection, without the gRPC
// client's per-signal reconnect backoff (NFR-024).
func newOTLPHTTPHandler(pipe *pipeline) http.Handler {
	traces := &traceReceiver{pipe: pipe}
	metrics := &metricsReceiver{pipe: pipe}
	logs := &logsReceiver{pipe: pipe}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) {
		req := ptraceotlp.NewExportRequest()
		if !decodeOTLP(w, r, req.UnmarshalProto) {
			return
		}
		resp, _ := traces.Export(r.Context(), req)
		writeOTLP(w, resp.MarshalProto)
	})
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		req := pmetricotlp.NewExportRequest()
		if !decodeOTLP(w, r, req.UnmarshalProto) {
			return
		}
		resp, _ := metrics.Export(r.Context(), req)
		writeOTLP(w, resp.MarshalProto)
	})
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		req := plogotlp.NewExportRequest()
		if !decodeOTLP(w, r, req.UnmarshalProto) {
			return
		}
		resp, _ := logs.Export(r.Context(), req)
		writeOTLP(w, resp.MarshalProto)
	})
	return mux
}

// decodeOTLP reads and unmarshals the body, answering the error itself: 415 for
// a media type or encoding other than protobuf / gzip (the exporter treats it as
// permanent, so a misconfiguration drops loudly in the collector log), 413 above
// otlpHTTPMaxBody, 400 for a body that does not decode.
func decodeOTLP(w http.ResponseWriter, r *http.Request, unmarshal func([]byte) error) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != otlpProtobuf {
		http.Error(w, "content type must be "+otlpProtobuf, http.StatusUnsupportedMediaType)
		return false
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, otlpHTTPMaxBody)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return false
		}
		defer gz.Close()
		body = gz
	default:
		http.Error(w, "content encoding must be gzip or identity", http.StatusUnsupportedMediaType)
		return false
	}
	raw, err := readCapped(body, otlpHTTPMaxBody)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || errors.Is(err, errBodyTooLarge) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad body", http.StatusBadRequest)
		}
		return false
	}
	if err := unmarshal(raw); err != nil {
		http.Error(w, "bad OTLP protobuf", http.StatusBadRequest)
		return false
	}
	return true
}

// readCapped reads at most limit bytes and fails above it, so a small gzip body
// cannot inflate past the limit.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, errBodyTooLarge
	}
	return buf.Bytes(), nil
}

func writeOTLP(w http.ResponseWriter, marshal func() ([]byte, error)) {
	out, err := marshal()
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", otlpProtobuf)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
