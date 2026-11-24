package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
)

// Variant options (#2709, architecture note §2.3, UC-054 A6 / BR-012, UC-055
// BR-005): a synced feature whose flag offers two or more declared variants
// shows them as Flagsmith multivariate options; the operator picks one by
// setting it to 100 %. The stack applies exactly one value: any other split is
// refused. Since #2713 an option's title (key) is the variant name and its
// value the variant's value as text, so the operator sees what it does.

// percentSplitReason is the health text of a refused percentage split.
const percentSplitReason = "percentage splits are not applied by JoustMania; set one option to 100 % or every option to 0 %"

// optionValueLimit is Flagsmith's default FEATURE_VALUE_LIMIT (characters of
// an option's string_value); a longer variant value is shown as its name.
const optionValueLimit = 20000

// Option is one multivariate option of a feature: its id, the offered variant
// it is ("" none, §2.3), its title (key) and, for a text option, its value
// (Text false: an integer or boolean option, or none).
type Option struct {
	ID      int
	Variant string
	Key     string
	Value   string
	Text    bool
}

// fsOption is a multivariate option as the Admin API names it (the feature
// list's multivariate_options and the mv-options routes).
type fsOption struct {
	ID          int     `json:"id"`
	Key         *string `json:"key"`
	Type        string  `json:"type"`
	StringValue *string `json:"string_value"`
	// hasKey: the answer carried the key field at all (Flagsmith #7698). A
	// null key is an option without a title on a Flagsmith that has titles.
	hasKey bool
}

// UnmarshalJSON decodes an option and notes whether its key field is present
// (§2.3, a Flagsmith without option keys).
func (o *fsOption) UnmarshalJSON(b []byte) error {
	type plain fsOption
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	_, p.hasKey = fields["key"]
	*o = fsOption(p)
	return nil
}

func (o fsOption) key() string {
	if o.Key == nil {
		return ""
	}
	return *o.Key
}

func (o fsOption) option() Option {
	if o.Type != TypeUnicode || o.StringValue == nil {
		return Option{ID: o.ID, Key: o.key()}
	}
	return Option{ID: o.ID, Key: o.key(), Value: *o.StringValue, Text: true}
}

// OptionText is the value of a variant's option (#2713, §2.3): the variant's
// value as Flagsmith text (§2.1 without the name shortcut: a string itself,
// an object or array compact JSON, a number or boolean its JSON text); the
// name when the value cannot be encoded or is longer than optionValueLimit.
func OptionText(p *registry.Policy, b *baseline.Flag, name string) string {
	if b == nil {
		return name
	}
	fv, err := Encode(p, b, Value{Present: true, Value: b.Variants[name]})
	if err != nil {
		return name
	}
	text := string(fv.Value)
	if fv.Type == TypeUnicode && json.Unmarshal(fv.Value, &text) != nil {
		return name
	}
	if utf8.RuneCountInString(text) > optionValueLimit {
		return name
	}
	return text
}

// optionMatcher tells which offered variant an option is (§2.3): (1) its key
// names one; with no key, or a key that names none, (3) its value is a variant
// name (an option of #2711) or (4) its value is the text of exactly one
// offered variant. Rule (2), the option ids the link recorded, is the pull's
// (pickedVariant).
type optionMatcher struct {
	offered []string
	is      map[string]bool
	text    map[string]string
	byText  map[string]string // "" when two variants share the text
}

func newOptionMatcher(p *registry.Policy, b *baseline.Flag) *optionMatcher {
	m := &optionMatcher{offered: OfferedVariants(p, b), is: map[string]bool{}, text: map[string]string{}, byText: map[string]string{}}
	for _, n := range m.offered {
		m.is[n] = true
		t := OptionText(p, b, n)
		m.text[n] = t
		if _, dup := m.byText[normText(t)]; dup {
			m.byText[normText(t)] = ""
		} else {
			m.byText[normText(t)] = n
		}
	}
	return m
}

// normText is a JSON object or array in canonical form (keys sorted, no
// spaces), so a value Flagsmith or a person re-spaced or re-ordered matches.
func normText(s string) string {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return s
	}
	var v any
	if json.Unmarshal([]byte(t), &v) != nil {
		return s
	}
	out, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
}

// byKey is rule 1.
func (m *optionMatcher) byKey(key string) string {
	if m.is[key] {
		return key
	}
	return ""
}

// byValue is rules 3 and 4, for an option whose key names no offered variant
// (none, or a title a person renamed).
func (m *optionMatcher) byValue(o Option) string {
	if m.is[o.Key] || !o.Text {
		return ""
	}
	if m.is[o.Value] {
		return o.Value
	}
	return m.byText[normText(o.Value)]
}

// match is rules 1, 3 and 4 over a feature's options, one option per variant:
// every key first, then the options left over by value, so an option renamed
// in Flagsmith never takes a variant another option holds by its key.
func (m *optionMatcher) match(opts []Option) []string {
	out := make([]string, len(opts))
	taken := map[string]bool{}
	for i, o := range opts {
		if v := m.byKey(o.Key); v != "" && !taken[v] {
			out[i], taken[v] = v, true
		}
	}
	for i, o := range opts {
		if out[i] != "" {
			continue
		}
		if v := m.byValue(o); v != "" && !taken[v] {
			out[i], taken[v] = v, true
		}
	}
	return out
}

// knownOptions maps the options Flagsmith named to the variants they are.
func (l *Link) knownOptions(p *registry.Policy, have []fsOption) []Option {
	opts := make([]Option, 0, len(have))
	for _, fo := range have {
		opts = append(opts, fo.option())
	}
	if p == nil {
		return opts
	}
	for i, v := range newOptionMatcher(p, l.BaselineFlag(p.Set, p.Key)).match(opts) {
		opts[i].Variant = v
	}
	return opts
}

// OfferedVariants are the variant names a feature shows as options: the
// row's bounds.variants when it lists them (only names the baseline
// declares), else every declared variant in the baseline's order; none for an
// event list or fewer than two.
func OfferedVariants(p *registry.Policy, b *baseline.Flag) []string {
	if p == nil || b == nil || p.Kind == registry.KindEvent {
		return nil
	}
	var names []string
	if len(p.Bounds.Variants) > 0 {
		for _, n := range p.Bounds.Variants {
			if _, ok := b.Variants[n]; ok {
				names = append(names, n)
			}
		}
	} else {
		names = b.VariantNames()
	}
	if len(names) < 2 {
		return nil
	}
	return names
}

// optionsPath is the mv-options route of a feature (rest "" or "<id>/").
func (l *Link) optionsPath(featureID int, rest string) string {
	return "projects/" + strconv.Itoa(l.cfg.ProjectID) + "/features/" + strconv.Itoa(featureID) + "/mv-options/" + rest
}

// setOptions records a feature's options. A changed id -> variant pair makes
// the next pull unconditional (§2.3): the document may already show a pick of
// that option, which a 304 or an unchanged digest would never read again.
func (l *Link) setOptions(flag string, opts []Option) {
	l.ids.Lock()
	old := l.ids.Options[flag]
	l.ids.Options[flag] = opts
	l.ids.Unlock()
	if variantsChanged(old, opts) {
		l.fullPull.Store(true)
	}
}

// variantsChanged: an option of now is another variant than in old (a new
// option counts when it is a variant).
func variantsChanged(old, now []Option) bool {
	was := make(map[int]string, len(old))
	for _, o := range old {
		was[o.ID] = o.Variant
	}
	for _, o := range now {
		if was[o.ID] != o.Variant {
			return true
		}
	}
	return false
}

// options returns a feature's options as last known.
func (l *Link) options(flag string) []Option {
	l.ids.Lock()
	defer l.ids.Unlock()
	return append([]Option(nil), l.ids.Options[flag]...)
}

// optionJob is one feature whose options the start keeps in step.
type optionJob struct {
	k         Key
	p         *registry.Policy
	featureID int
	have      []fsOption
}

// queueOptions records the options the feature list named for a usable
// feature (the weight writes use them at once) and queues the feature for
// the option sync that runs once the start succeeded.
func (s *startup) queueOptions(k Key, p *registry.Policy, featureID int, have []fsOption) {
	s.l.setOptions(k.Flag, s.l.knownOptions(p, have))
	s.optionJobs = append(s.optionJobs, optionJob{k: k, p: p, featureID: featureID, have: have})
}

// scheduleOptions runs the option sync of the queued features in the
// background, paced like the rest of the start (§2.3): the link goes online
// and pulls without waiting for it. A push meanwhile names the options the
// feature list showed; one Flagsmith refuses lists them again.
func (s *startup) scheduleOptions(ctx context.Context) {
	jobs := s.optionJobs
	s.optionJobs = nil
	run := newOptionRun(jobs)
	s.l.optionsKeyless.Store(run.keyless)
	done := make(chan struct{})
	s.mu.Lock()
	s.options = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		s.runOptions(ctx, run, jobs)
	}()
}

// Option sync limits (§2.3, #2713 decision D2): a pass ends after
// optionTransientRun transient errors in a row and is retried like the stale
// identity cleanup.
const (
	optionTransientRun = 3
	optionRetries      = staleCleanupRetries
	optionRetryEvery   = staleCleanupRetryEvery
)

// optionRun is the option sync of one start: whether Flagsmith has option keys
// (decided once, §2.3) and the state of the current pass.
type optionRun struct {
	// keyless: write the #2711 shape (no key, value = the variant name).
	keyless bool
	// decided: keyless is known (from the feature list, else the first
	// create's answer).
	decided bool

	// Per pass: stopped after a 403 or 405, aborted after a run of
	// transient errors; transient counts the errors in a row, failed notes
	// one in the current feature.
	stopped, aborted bool
	transient        int
	failed           bool
}

// newOptionRun decides from the feature list, before any write, whether the
// options carry the key field: one option without it means a Flagsmith
// without option keys. A null key is an option of #2711, not a sign of it.
func newOptionRun(jobs []optionJob) *optionRun {
	r := &optionRun{}
	for _, j := range jobs {
		for _, o := range j.have {
			r.decided = true
			if !o.hasKey {
				r.keyless = true
			}
		}
	}
	return r
}

// note counts one option call's result; true ends the pass.
func (r *optionRun) note(err error) bool {
	st := httpStatus(err)
	switch {
	case err == nil:
		r.transient = 0
	case st == http.StatusForbidden || st == http.StatusMethodNotAllowed:
		r.stopped = true
	case isTransient(err) || st == 0:
		r.transient++
		r.failed = true
		r.aborted = r.transient >= optionTransientRun
	default:
		r.transient = 0
	}
	return r.stopped || r.aborted
}

// refusedAll: err stops every option write of the pass (logged once).
func refusedAll(err error) bool {
	st := httpStatus(err)
	return st == http.StatusForbidden || st == http.StatusMethodNotAllowed
}

// runOptions runs option passes until every feature is in step, the pass was
// stopped, the link is cancelled or the retries are used up.
func (s *startup) runOptions(ctx context.Context, run *optionRun, jobs []optionJob) {
	for retry := 0; ; retry++ {
		left, err := s.optionPass(ctx, run, jobs, retry > 0)
		if err != nil || len(left) == 0 {
			return
		}
		if retry == optionRetries {
			s.l.Logger().Warn("flagsmith options not synced", "detail", "the option sync failed after its retries", "retries", optionRetries, "features", len(left))
			return
		}
		if s.l.Sleep(ctx, optionRetryEvery) != nil {
			return
		}
		jobs = left
	}
}

// optionPass syncs the options of jobs (relist: read each feature's options
// again first, a retry) and returns the features to retry. Only a cancel is
// returned as an error.
func (s *startup) optionPass(ctx context.Context, run *optionRun, jobs []optionJob, relist bool) ([]optionJob, error) {
	run.stopped, run.aborted, run.transient = false, false, 0
	var retry []optionJob
	for i, j := range jobs {
		run.failed = false
		if relist {
			have, err := s.readOptions(ctx, j.featureID)
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case err != nil:
				if !refusedAll(err) { // a 403 or 405 is endPass's one WARN
					s.warnOption(j.k, "list", "", err)
				}
				if run.note(err) {
					return s.endPass(run, append(retry, jobs[i:]...)), nil
				}
				if run.failed {
					retry = append(retry, j)
				}
				continue
			}
			j.have = have
		}
		if err := s.syncOptions(ctx, run, j.k, j.p, j.featureID, j.have); err != nil {
			return nil, err
		}
		if run.stopped || run.aborted {
			return s.endPass(run, append(retry, jobs[i:]...)), nil
		}
		if run.failed {
			retry = append(retry, j)
		}
	}
	return retry, nil
}

// endPass logs the one WARN of a stopped or aborted pass; a stopped pass is
// not retried (the next start tries again).
func (s *startup) endPass(run *optionRun, left []optionJob) []optionJob {
	if run.stopped {
		s.l.Logger().Warn("flagsmith options not synced", "detail", "Flagsmith refused an option write (403 or 405); no option is changed until the next start")
		return nil
	}
	s.l.Logger().Warn("flagsmith options not synced", "detail", "transient errors in a row; the option sync is retried", "errors", optionTransientRun, "retry_in", optionRetryEvery.String(), "features", len(left))
	return left
}

// readOptions lists a feature's options (a retried pass).
func (s *startup) readOptions(ctx context.Context, featureID int) ([]fsOption, error) {
	var raw json.RawMessage
	if _, _, err := s.pace.call(ctx, http.MethodGet, s.l.optionsPath(featureID, ""), nil, &raw, true, nil); err != nil {
		return nil, err
	}
	return parseOptions(raw)
}

// parseOptions reads an mv-options listing (a JSON array or a DRF page).
func parseOptions(raw json.RawMessage) ([]fsOption, error) {
	var list []fsOption
	if err := json.Unmarshal(raw, &list); err != nil {
		var paged struct {
			Results []fsOption `json:"results"`
		}
		if err := json.Unmarshal(raw, &paged); err != nil {
			return nil, ReasonError{Reason: "malformed_response"}
		}
		list = paged.Results
	}
	return list, nil
}

// optionsDone is closed when the scheduled option sync ended (nil before
// Start succeeded).
func (s *startup) optionsDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.options
}

// syncOptions makes the options of k's feature equal to its offered variants
// (§2.3): each option is matched to a variant by key first, then by value; one
// that is no offered variant (a dropped variant, a person's, a duplicate) is
// removed, a matched one whose key, type or value differs is updated in place
// (PUT keeps its id, so its weights and a running pick stay: the migration of
// the #2711 options, #2713; one deleted meanwhile, 404, is created again), a
// missing one is created at 0 % in every environment. Deletes go first, then
// updates, then creates. A failed call is logged and counted (run); a 403 or
// 405 stops the pass and a run of transient errors aborts it, the options
// known so far recorded. Only a cancel is returned.
func (s *startup) syncOptions(ctx context.Context, run *optionRun, k Key, p *registry.Policy, featureID int, have []fsOption) error {
	m := newOptionMatcher(p, s.l.BaselineFlag(p.Set, p.Key))
	cur := make([]Option, len(have)) // the options as Flagsmith has them now
	for i, o := range have {
		cur[i] = o.option()
	}
	matched := m.match(cur)
	taken := map[string]bool{}
	for i, v := range matched {
		cur[i].Variant = v
		if v != "" {
			taken[v] = true
		}
	}
	gone := make([]bool, len(cur))
	var created []Option
	// want is the key and text of a variant's option: the #2711 shape on a
	// Flagsmith without option keys.
	want := func(name string) (string, string) {
		if run.keyless {
			return "", name
		}
		return name, m.text[name]
	}
	body := func(name string) map[string]any {
		key, text := want(name)
		b := map[string]any{"feature": featureID, "type": TypeUnicode, "string_value": text, "default_percentage_allocation": 0}
		if key != "" {
			b["key"] = key
		}
		return b
	}
	finish := func() error {
		final := make([]Option, 0, len(cur)+len(created))
		for i, o := range cur {
			if !gone[i] {
				final = append(final, o) // still there: a weight write names it
			}
		}
		s.l.setOptions(k.Flag, append(final, created...))
		return nil
	}
	path := func(id int) string { return s.l.optionsPath(featureID, strconv.Itoa(id)+"/") }

	for i, o := range cur {
		if matched[i] != "" {
			continue
		}
		err := s.optionCall(ctx, k, http.MethodDelete, path(o.ID), nil, nil)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil || httpStatus(err) == http.StatusNotFound {
			gone[i], err = true, nil
		} else if !refusedAll(err) {
			s.warnOption(k, "delete", optionLabel(o), err)
		}
		if run.note(err) {
			return finish()
		}
	}
	for i, o := range cur {
		name := matched[i]
		if name == "" {
			continue
		}
		key, text := want(name)
		if o.Text && o.Value == text && (run.keyless || o.Key == key) {
			continue // in step; on a Flagsmith without keys never updated for the key alone
		}
		err := s.optionCall(ctx, k, http.MethodPut, path(o.ID), body(name), nil)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err == nil:
			cur[i] = Option{ID: o.ID, Variant: name, Key: key, Value: text, Text: true}
		case httpStatus(err) == http.StatusNotFound:
			// Deleted since the listing: created below in this pass.
			gone[i], taken[name], err = true, false, nil
		case !refusedAll(err):
			s.warnOption(k, "update", name, err)
		}
		if run.note(err) {
			return finish()
		}
	}
	for _, name := range m.offered {
		if taken[name] {
			continue
		}
		b := body(name)
		_, sentKey := b["key"]
		var c fsOption
		err := s.optionCall(ctx, k, http.MethodPost, s.l.optionsPath(featureID, ""), b, &c)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err == nil:
			key, text := want(name)
			if !run.decided {
				s.decideKeyless(run, c, sentKey)
				if run.keyless {
					key = ""
				}
			}
			created = append(created, Option{ID: c.ID, Variant: name, Key: key, Value: text, Text: true})
		case !refusedAll(err):
			s.warnOption(k, "create", name, err)
		}
		if run.note(err) {
			return finish()
		}
	}
	return finish()
}

// decideKeyless decides from the first create's answer, when the feature list
// showed no option at all, whether Flagsmith has option keys: no key field, or
// null after one was sent, means it has none (§2.3, #2713 decision D1).
func (s *startup) decideKeyless(run *optionRun, c fsOption, sentKey bool) {
	run.decided = true
	if !c.hasKey || (sentKey && c.Key == nil) {
		run.keyless = true
		s.l.optionsKeyless.Store(true)
		s.l.Logger().Warn("flagsmith options without keys", "detail", "this Flagsmith keeps no option key; options are written as in #2709 (value = the variant name)")
	}
}

// optionLabel names an option in a log line: its key, else its id (never its
// value, which can be 20 000 characters).
func optionLabel(o Option) string {
	if o.Key != "" {
		return o.Key
	}
	return "id " + strconv.Itoa(o.ID)
}

// optionCall is one paced option request inside a push span (op option). A
// delete or update answered 404 is handled (the option is gone; an update's
// variant is created again) and counted ok.
func (s *startup) optionCall(ctx context.Context, k Key, method, path string, body, out any) error {
	sctx, span := s.pushSpan(ctx, k, OpOption)
	_, _, err := s.pace.call(sctx, method, path, body, out, true, nil)
	gone := (method == http.MethodDelete || method == http.MethodPut) && httpStatus(err) == http.StatusNotFound
	switch {
	case err == nil, gone:
		s.endPush(sctx, span, OpOption, ResultOK, "")
	case httpStatus(err) >= 400 && httpStatus(err) < 500:
		s.endPush(sctx, span, OpOption, ResultRejected, Reason(err))
	default:
		s.endPush(sctx, span, OpOption, ResultError, Reason(err))
	}
	return err
}

// warnOption logs a failed option call; option is the variant name or the
// option's key or id (optionLabel), never its value.
func (s *startup) warnOption(k Key, op, option string, err error) {
	s.l.Logger().Warn("flagsmith option not synced", "flag_set", k.Set, "flag", k.Flag, "op", op, "option", option, "reason", Reason(err))
}

// listOptions reads a feature's options again (a weight write Flagsmith
// refused because they changed since the start). An option the rules make no
// variant keeps the variant the link recorded for its id (rule 2: keyless
// options with equal values).
func (o *out) listOptions(ctx context.Context, k Key, featureID int) error {
	if err := o.pace(ctx); err != nil {
		return err
	}
	var raw json.RawMessage
	if _, err := o.l.Client().Admin(ctx, http.MethodGet, o.l.optionsPath(featureID, ""), nil, &raw); err != nil {
		return err
	}
	list, err := parseOptions(raw)
	if err != nil {
		return err
	}
	o.l.setOptions(k.Flag, keepRecorded(o.l.knownOptions(o.l.Policy(k.Set, k.Flag), list), o.l.options(k.Flag)))
	return nil
}

// keepRecorded gives an option of opts with no variant the variant prev
// recorded for its id, unless another option already is that variant.
func keepRecorded(opts, prev []Option) []Option {
	taken := map[string]bool{}
	for _, o := range opts {
		if o.Variant != "" {
			taken[o.Variant] = true
		}
	}
	for i := range opts {
		if opts[i].Variant != "" {
			continue
		}
		for _, p := range prev {
			if p.ID == opts[i].ID && p.Variant != "" && !taken[p.Variant] {
				opts[i].Variant, taken[p.Variant] = p.Variant, true
				break
			}
		}
	}
	return opts
}

// variantsRefused: Flagsmith refused the weights of a write because they do
// not name the feature's options (check_variants of the __future__ API).
func variantsRefused(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(he.Body))
	return strings.Contains(body, "variant") || strings.Contains(body, "multivariate")
}

// weightsFor are the option weights of a stack value (§2.3): 100 on the
// first option that is the variant sent (by name) while the overlay is on, 0
// on every other option.
func weightsFor(opts []Option, fv FSValue, enabled bool) []futureVariant {
	if len(opts) == 0 {
		return nil
	}
	var s string
	picked := enabled && fv.Type == TypeUnicode && json.Unmarshal(fv.Value, &s) == nil
	out := make([]futureVariant, 0, len(opts))
	for _, o := range opts {
		w := 0.0
		if picked && o.Variant != "" && o.Variant == s {
			w, picked = 100, false
		}
		out = append(out, futureVariant{ID: o.ID, Weight: w})
	}
	return out
}

// MVStateValue is one option's weight in a feature state of the document
// (multivariate_feature_state_values).
type MVStateValue struct {
	Option struct {
		ID    int             `json:"id"`
		Value json.RawMessage `json:"value"`
		// Key is the option's title (Flagsmith #7698; absent in older
		// documents): the variant name since #2713.
		Key *string `json:"key"`
	} `json:"multivariate_feature_option"`
	PercentageAllocation float64 `json:"percentage_allocation"`
}

// pickedValue reads a feature state's weights (§2.3): none or all 0 -> no
// pick (ok false); exactly one option at 100 and the others 0 -> that option;
// anything else is a percentage split (refused).
func pickedValue(pol *registry.Policy, mv []MVStateValue) (MVStateValue, bool, *fserr.Error) {
	var picked MVStateValue
	found, split := false, false
	for _, v := range mv {
		switch {
		case v.PercentageAllocation == 0:
		case v.PercentageAllocation == 100 && !found:
			picked, found = v, true
		default:
			split = true
		}
	}
	if split {
		return MVStateValue{}, false, fserr.Unprocessable(fserr.FlagsmithPercentageSplit, "%s: %s", pol.Key, percentSplitReason)
	}
	if !found || len(bytes.TrimSpace(picked.Option.Value)) == 0 {
		return MVStateValue{}, false, nil
	}
	return picked, true, nil
}

// pickedVariant is the offered variant a picked option is (§2.3, #2713):
// (1) its key, (2) the variant the link recorded for its id, (3, 4) with no
// key or one that names no offered variant, its value; "" when none matches
// (the value is then read as any value). On a Flagsmith without option keys
// rule 3 goes before rule 2, the read order of #2711.
func (l *Link) pickedVariant(pol *registry.Policy, mv MVStateValue) string {
	m := newOptionMatcher(pol, l.BaselineFlag(pol.Set, pol.Key))
	o := Option{ID: mv.Option.ID}
	if mv.Option.Key != nil {
		o.Key = *mv.Option.Key
	}
	if v := m.byKey(o.Key); v != "" {
		return v
	}
	var s string
	if json.Unmarshal(mv.Option.Value, &s) == nil {
		o.Value, o.Text = s, true
	}
	if l.optionsKeyless.Load() && o.Text && m.is[o.Value] {
		return o.Value
	}
	for _, known := range l.options(pol.Key) {
		if known.ID == o.ID && m.is[known.Variant] {
			return known.Variant
		}
	}
	return m.byValue(o)
}
