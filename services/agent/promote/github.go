package promote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joustmania/agent/internal/anthropichttp"
	"github.com/joustmania/agent/keysafe"
)

// github.go is the REAL GitHub API client — the consequential, network-touching
// path of the safety rail. It is built ONLY by NewGitHubClientFromEnv, which
// returns a real client ONLY when GITHUB_TOKEN and GITHUB_REPOSITORY are set and
// GITHUB_API_URL is key-safe. Opening an issue changes no running system
// (UC-043 BR-003), so the credential is the only opt-in.
//
// Otherwise NewGitHubClientFromEnv returns nil and main.go falls back to the
// inert recording no-op (NewNoopGitHubClient). There is NO other constructor for a
// real client — a test cannot instantiate one, so no unit/integration test can
// reach github.com (the safety rail's structural guarantee). Stdlib net/http only:
// one simple endpoint (issues) does not justify a new dependency (the M7-4
// testing-strategy doc §4 "skip Microcks / go-github").

// httpGitHubClient posts to the GitHub REST API. Constructed only via
// NewGitHubClientFromEnv behind the token and repository.
type httpGitHubClient struct {
	httpClient *http.Client
	baseURL    string // https://api.github.com (overridable for a sandbox/e2e host)
	token      string
	owner      string
	repo       string
	log        *slog.Logger
}

// NewGitHubClientFromEnv returns the REAL GitHub client IFF the env gate is fully
// satisfied, else nil (the caller substitutes the no-op). This is the ONLY place a
// real GitHub client is ever constructed.
//
// Conditions (ALL required):
//   - GITHUB_TOKEN non-empty                          (credential)
//   - GITHUB_REPOSITORY == "owner/repo"               (where to act)
//
// Returns (nil, nil) without a token — the EXPECTED, fail-closed default, not an
// error. Returns (nil, err) when a token is set but GITHUB_REPOSITORY is
// malformed, so a misconfiguration fails loudly rather than silently dialing nothing.
func NewGitHubClientFromEnv(log *slog.Logger) (*httpGitHubClient, error) {
	if log == nil {
		log = slog.Default()
	}
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		log.Info("code_improvement github real client NOT built: GITHUB_TOKEN absent (issue promotions are recorded no-ops)")
		return nil, nil
	}
	owner, repo, ok := splitRepo(os.Getenv("GITHUB_REPOSITORY"))
	if !ok {
		return nil, fmt.Errorf("GITHUB_TOKEN is set but GITHUB_REPOSITORY=%q is not owner/repo", os.Getenv("GITHUB_REPOSITORY"))
	}
	baseURL := strings.TrimRight(getenvDefault("GITHUB_API_URL", "https://api.github.com"), "/")
	// GITHUB_TOKEN travels to this URL: https, or plain http only to loopback
	// (C-016, #1886). The URL is never logged, only the reason.
	if reason, ok := keysafe.BaseURL(baseURL); !ok {
		log.Warn("code_improvement github real client NOT built: GITHUB_API_URL is not key-safe (https, or http to a loopback host, no credentials, query or fragment); fail-closed to no-op",
			"reason", reason)
		return nil, nil
	}
	log.Warn("code_improvement GitHub real client ENABLED (#936): promotions with target=github WILL open real issues/PRs",
		"repo", owner+"/"+repo)
	return &httpGitHubClient{
		httpClient: anthropichttp.NewHTTPClient(15 * time.Second),
		baseURL:    baseURL,
		token:      token,
		owner:      owner,
		repo:       repo,
		log:        log,
	}, nil
}

// OpenIssue POSTs to /repos/{owner}/{repo}/issues and returns the html_url.
func (c *httpGitHubClient) OpenIssue(ctx context.Context, req IssueReq) (string, error) {
	payload := map[string]any{"title": req.Title, "body": req.Body}
	if len(req.Labels) > 0 {
		payload["labels"] = req.Labels
	}
	return c.post(ctx, fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo), payload)
}

// post sends a JSON POST to the GitHub API and returns the response's html_url.
func (c *httpGitHubClient) post(ctx context.Context, path string, payload any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("github POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github POST %s: status %d: %s", path, resp.StatusCode, string(respBody))
	}
	var parsed struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse github response: %w", err)
	}
	return parsed.HTMLURL, nil
}

// splitRepo parses "owner/repo" into its parts.
func splitRepo(s string) (owner, repo string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// getenvDefault returns the env var or def if empty.
func getenvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
