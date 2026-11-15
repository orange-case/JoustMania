package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
)

// Authorship from the project audit log (UC-054 BR-006, architecture note
// §5.1): GET projects/{id}/audit/?environments=<env id>, newest first. The
// feature and identity come from the log text, pinned to Flagsmith's log
// constants (audit/constants.py, edge_api/identities/tasks.py), because
// related_object_id is not reliable for values. is_system_event is never sent
// (Flagsmith parses it wrongly): system events are filtered here.

// Audit paging (O2).
const (
	auditPageSize = 200
	// auditMaxPages bounds the newest-first scan.
	auditMaxPages = 50
	// auditSearchPages bounds the per-feature search of a candidate the scan
	// did not find.
	auditSearchPages = 5
)

// envLogPatterns match the audit rows of a change of a feature's environment
// value; group 1 is the feature name.
var envLogPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^Flag state updated for feature: (.+)$`),
	regexp.MustCompile(`^Remote config value updated for feature: (.+)$`),
	regexp.MustCompile(`^New Flag / Remote Config created: (.+)$`),
	regexp.MustCompile(`^New version published for feature: (.+)$`),
	regexp.MustCompile(`^Flag state / Remote config updated for feature: (.+?) by Change Request: .*$`),
	regexp.MustCompile(`^Scheduled change to Flag state / Remote config value went live for feature: (.+?) by Change Request: .*$`),
	releasePipelinePattern,
}

// releasePipelinePattern matches FEATURE_STATE_UPDATED_BY_RELEASE_PIPELINE_MESSAGE
// ("... for feature: %s by Release pipeline: %s (stage: %s)"). A configured
// release pipeline is a deliberate change: such a row counts as a person's,
// whatever its author field says.
var releasePipelinePattern = regexp.MustCompile(`^Flag state / Remote config updated for feature: (.+?) by Release pipeline: .*$`)

// overrideLogPattern matches an edge identity override row; group 1 is the
// feature name, group 2 the identity's identifier. Core-identity rows are not
// supported (edge identities are required, O8).
var overrideLogPattern = regexp.MustCompile(`^Feature override (?:created|updated|deleted) for feature '(.+?)' and identity '(.*)'$`)

// authorKey names one audited value: a feature's environment value
// (identity "") or its override on one identity.
type authorKey struct {
	feature, identity string
}

// auditRow is one audit log entry.
type auditRow struct {
	Log           string          `json:"log"`
	Author        json.RawMessage `json:"author"`
	IsSystemEvent bool            `json:"is_system_event"`
}

// author: a user is a person; null (not a system event) is an API key, which
// counts as the link (maintainer decision D1); a system event, or a row
// without the field, counts as a person (never an API key).
func (r auditRow) author() Author {
	a := bytes.TrimSpace(r.Author)
	if !r.IsSystemEvent && bytes.Equal(a, []byte("null")) {
		return AuthorAPIKey
	}
	return AuthorPerson
}

// attributed is the author the restart rule uses: a release pipeline's row is
// a person's, any other row author().
func (r auditRow) attributed() Author {
	if releasePipelinePattern.MatchString(r.Log) {
		return AuthorPerson
	}
	return r.author()
}

// key returns the value the row is about.
func (r auditRow) key() (authorKey, bool) {
	if m := overrideLogPattern.FindStringSubmatch(r.Log); m != nil {
		return authorKey{feature: m[1], identity: m[2]}, true
	}
	for _, re := range envLogPatterns {
		if m := re.FindStringSubmatch(r.Log); m != nil {
			return authorKey{feature: m[1]}, true
		}
	}
	return authorKey{}, false
}

// latestAuthors finds the author of the latest audit row of each wanted
// value: the newest-first scan (auditMaxPages pages), then one search per
// feature still missing. ok is false when an audit call did not answer 200
// (after retries; a 401 too: an audit error never takes the link down,
// architecture note §5.1); only a cancel is returned as the error.
// A value with no row is absent from the map.
func (s *startup) latestAuthors(ctx context.Context, want map[authorKey]bool) (map[authorKey]Author, bool, error) {
	found := map[authorKey]Author{}
	if len(want) == 0 {
		return found, true, nil
	}
	ids := s.l.ids
	ids.Lock()
	base := s.projectPath("audit/?environments=" + strconv.Itoa(ids.EnvironmentID) + "&page_size=" + strconv.Itoa(auditPageSize))
	ids.Unlock()
	ok, err := s.scanAudit(ctx, base, auditMaxPages, want, found)
	if err != nil || !ok {
		return found, ok, err
	}
	missing := map[string]bool{}
	for k := range want {
		if _, has := found[k]; !has {
			missing[k.feature] = true
		}
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if ok, err = s.scanAudit(ctx, base+"&search="+url.QueryEscape(name), auditSearchPages, want, found); err != nil || !ok {
			return found, ok, err
		}
	}
	return found, true, nil
}

// scanAudit pages through path newest first and records the first (latest)
// row of every wanted value not found yet, until all are found or the pages
// end.
func (s *startup) scanAudit(ctx context.Context, path string, pages int, want map[authorKey]bool, found map[authorKey]Author) (bool, error) {
	for page := 1; page <= pages; page++ {
		var p struct {
			Next    *string    `json:"next"`
			Results []auditRow `json:"results"`
		}
		_, _, err := s.pace.call(ctx, http.MethodGet, path+"&page="+strconv.Itoa(page), nil, &p, true, nil)
		if err != nil {
			if ctx.Err() != nil {
				return false, err
			}
			s.l.Logger().Warn("flagsmith audit log not readable", "reason", Reason(err))
			return false, nil
		}
		for _, r := range p.Results {
			k, ok := r.key()
			if !ok || !want[k] {
				continue
			}
			if _, has := found[k]; !has {
				found[k] = r.attributed()
			}
		}
		if len(found) >= len(want) || p.Next == nil || *p.Next == "" {
			return true, nil
		}
	}
	return true, nil
}
