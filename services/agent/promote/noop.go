package promote

import (
	"context"
	"errors"
	"log/slog"
)

// noop.go holds the INERT recording no-op client that is the DEFAULT GitHubClient
// (see NewPromoter). It is the safety rail's fail-closed floor: a Promoter built
// without an explicit real client — i.e. every test, and main.go whenever the env
// gate/token are not both present — can only RECORD that it "would have" acted,
// never touch github.com.
//
// errNoopGitHub is returned so issue mode degrades to a RECORDED no-op
// (OutcomeDiscarded + reason on the span), making an unreachable real path
// VISIBLE in the trace rather than silent (#936 safety rail).

// errNoopGitHub is returned by the no-op GitHub client. Its presence in a Result
// reason proves the promotion degraded to an inert recorded no-op because the real
// client was not enabled.
var errNoopGitHub = errors.New("github not enabled (no real client wired; set AGENT_CODE_IMPROVEMENT_ENABLED=true + GITHUB_TOKEN)")

// noopGitHubClient records the requests it WOULD have sent and declines them. It
// has ZERO network code by construction — there is no http client, no URL, no
// token anywhere in this type, so no bug or test can make it dial github.com.
type noopGitHubClient struct {
	log *slog.Logger
	// Issues records what would have been opened, for tests that assert "the agent
	// would open an issue with body X" against the DEFAULT (disabled) path.
	Issues []IssueReq
}

// NewNoopGitHubClient builds the inert recording GitHub client. This is the default
// the Promoter falls back to; it is also exported so tests can assert against the
// recorded requests. log nil uses slog.Default().
func NewNoopGitHubClient(log *slog.Logger) *noopGitHubClient {
	if log == nil {
		log = slog.Default()
	}
	return &noopGitHubClient{log: log}
}

// OpenIssue records the request and declines (no network).
func (c *noopGitHubClient) OpenIssue(_ context.Context, req IssueReq) (string, error) {
	c.Issues = append(c.Issues, req)
	c.log.Info("code_improvement github DISABLED: would open issue (recorded no-op)", "title", req.Title)
	return "", errNoopGitHub
}
