package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Response body caps (GOMEMLIMIT is 48 MiB, NFR-051).
const (
	MaxDocumentBytes = 4 << 20
	MaxResponseBytes = 1 << 20
)

// Client calls Flagsmith: the environment document (server-side key), the
// Admin API (admin key), the feature-health webhook and a realtime stream.
// Outbound HTTPS only (C-037). It records no URL anywhere: no otelhttp
// transport, no URL in an error.
type Client struct {
	cfg    Config
	api    *url.URL
	doc    *url.URL
	http   *http.Client
	stream *http.Client
}

// NewClient builds a client for cfg. hc (nil: a client with cfg.Timeout) is
// used for every call but Stream, which keeps hc's transport without a total
// timeout.
func NewClient(cfg Config, hc *http.Client) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	c := &Client{cfg: cfg}
	c.api, _ = url.Parse(orDefault(cfg.APIURL, DefaultAPIURL))
	c.doc, _ = url.Parse(orDefault(cfg.DocumentURL, DefaultDocumentURL))
	if hc == nil {
		c.http = &http.Client{Timeout: cfg.Timeout}
		c.stream = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   cfg.Timeout,
			ResponseHeaderTimeout: cfg.Timeout,
		}}
	} else {
		c.http = hc
		c.stream = &http.Client{Transport: hc.Transport}
	}
	return c
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// EnvironmentDocument pulls the environment document. ifModifiedSince (the
// Last-Modified of the previous pull, "" for a full pull) makes it
// conditional: notModified is true on a 304 and doc is nil.
func (c *Client) EnvironmentDocument(ctx context.Context, ifModifiedSince string) (doc *EnvironmentDocument, lastModified string, notModified bool, err error) {
	u := c.doc.ResolveReference(&url.URL{Path: "environment-document/"})
	req, err := newRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("X-Environment-Key", c.cfg.ServerKey)
	if ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}
	status, header, body, err := c.do(c.http, req, MaxDocumentBytes)
	if err != nil {
		return nil, "", false, err
	}
	if status == http.StatusNotModified {
		return nil, ifModifiedSince, true, nil
	}
	if status < 200 || status > 299 {
		return nil, "", false, httpError(status, header, body)
	}
	doc = &EnvironmentDocument{}
	if err := json.Unmarshal(body, doc); err != nil {
		return nil, "", false, ReasonError{Reason: "malformed_document"}
	}
	return doc, header.Get("Last-Modified"), false, nil
}

// Admin calls the Admin API with the admin key. path is relative to the API
// base ("projects/1/features/?q=x", the query kept), or absolute from the host
// root when it starts with "/" ("/api/__future__/..."). body (nil: none) is
// sent as JSON on every method, DELETE included; a 2xx JSON answer is decoded
// into out (nil: ignored).
func (c *Client) Admin(ctx context.Context, method, path string, body, out any) (status int, err error) {
	ref, err := url.Parse(path)
	if err != nil {
		return 0, ReasonError{Reason: "bad_path"}
	}
	req, err := newRequest(ctx, method, c.api.ResolveReference(ref).String(), body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Api-Key "+c.cfg.AdminKey)
	status, header, raw, err := c.do(c.http, req, MaxResponseBytes)
	if err != nil {
		return status, err
	}
	if status < 200 || status > 299 {
		return status, httpError(status, header, raw)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, ReasonError{Reason: "malformed_response"}
		}
	}
	return status, nil
}

// PostJSON posts body to an absolute URL (the feature-health webhook) without
// the admin key. The URL is a secret: no error names it.
func (c *Client) PostJSON(ctx context.Context, rawURL string, body any) (status int, err error) {
	req, err := newRequest(ctx, http.MethodPost, rawURL, body)
	if err != nil {
		return 0, err
	}
	status, header, raw, err := c.do(c.http, req, MaxResponseBytes)
	if err != nil {
		return status, err
	}
	if status < 200 || status > 299 {
		return status, httpError(status, header, raw)
	}
	return status, nil
}

// Stream opens a long-lived GET (the realtime SSE stream): only the TLS
// handshake and the response headers are bounded by the timeout. The caller
// closes the body.
func (c *Client) Stream(ctx context.Context, rawURL string, headers map[string]string) (io.ReadCloser, error) {
	req, err := newRequest(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.stream.Do(req) //nolint:bodyclose // the caller closes the stream
	if err != nil {
		return nil, transportError(req.Method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
		_ = resp.Body.Close()
		return nil, httpError(resp.StatusCode, resp.Header, raw)
	}
	return resp.Body, nil
}

func newRequest(ctx context.Context, method, rawURL string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, ReasonError{Reason: "bad_body"}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		// The parse error would quote the URL.
		return nil, ReasonError{Reason: "bad_url"}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends req and reads at most limit bytes of the answer.
func (c *Client) do(hc *http.Client, req *http.Request, limit int64) (int, http.Header, []byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, transportError(req.Method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, transportError(req.Method, err)
	}
	if int64(len(raw)) > limit {
		return resp.StatusCode, resp.Header, nil, ReasonError{Reason: ReasonResponseTooLarge}
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// transportError drops the URL of a *url.Error: it may carry a token.
func transportError(op string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &TransportError{Op: op, Err: err}
}

func httpError(status int, header http.Header, body []byte) *HTTPError {
	e := &HTTPError{Status: status, RetryAfter: retryAfter(header.Get("Retry-After"))}
	if len(body) > MaxErrorBody {
		body = body[:MaxErrorBody]
	}
	e.Body = append([]byte(nil), body...)
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		_, e.JSONDetail = obj["detail"]
	}
	return e
}

// retryAfter parses a Retry-After in seconds or as an HTTP date.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
