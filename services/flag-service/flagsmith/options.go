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
// names one; with no key, (3) its value is a variant name (an option of #2711)
// or (4) its value is the text of exactly one offered variant. Rule (2), the
// option ids the link recorded, is the pull's (pickedVariant).
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

// byValue is rules 3 and 4, for an option without a key.
func (m *optionMatcher) byValue(o Option) string {
	if o.Key != "" || !o.Text {
		return ""
	}
	if m.is[o.Value] {
		return o.Value
	}
	return m.byText[normText(o.Value)]
}

// variant is the variant an option is by rules 1, 3 and 4.
func (m *optionMatcher) variant(o Option) string {
	if o.Key != "" {
		return m.byKey(o.Key)
	}
	return m.byValue(o)
}

// knownOptions maps the options Flagsmith named to the variants they are.
func (l *Link) knownOptions(p *registry.Policy, have []fsOption) []Option {
	var m *optionMatcher
	if p != nil {
		m = newOptionMatcher(p, l.BaselineFlag(p.Set, p.Key))
	}
	opts := make([]Option, 0, len(have))
	for _, fo := range have {
		o := fo.option()
		if m != nil {
			o.Variant = m.variant(o)
		}
		opts = append(opts, o)
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

// setOptions records a feature's options.
func (l *Link) setOptions(flag string, opts []Option) {
	l.ids.Lock()
	defer l.ids.Unlock()
	l.ids.Options[flag] = opts
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
	done := make(chan struct{})
	s.mu.Lock()
	s.options = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		for _, j := range jobs {
			if s.syncOptions(ctx, j.k, j.p, j.featureID, j.have) != nil {
				return
			}
		}
	}()
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
// the #2711 options, #2713), a missing one is created at 0 % in every
// environment. Deletes go first, then updates, then creates, so a key is free
// before it is set. A failed call is logged and counted; only a cancel is
// returned.
func (s *startup) syncOptions(ctx context.Context, k Key, p *registry.Policy, featureID int, have []fsOption) error {
	m := newOptionMatcher(p, s.l.BaselineFlag(p.Set, p.Key))
	matched := make([]string, len(have))
	taken := map[string]bool{}
	for i, o := range have { // rule 1: by key
		if v := m.byKey(o.key()); v != "" && !taken[v] {
			matched[i], taken[v] = v, true
		}
	}
	for i, o := range have { // rules 3 and 4: a keyless option by value
		if v := m.byValue(o.option()); matched[i] == "" && v != "" && !taken[v] {
			matched[i], taken[v] = v, true
		}
	}
	body := func(name string) map[string]any {
		return map[string]any{"feature": featureID, "key": name, "type": TypeUnicode, "string_value": m.text[name], "default_percentage_allocation": 0}
	}
	var final []Option
	for i, o := range have {
		if matched[i] != "" {
			continue
		}
		opt := o.option()
		err := s.optionCall(ctx, k, http.MethodDelete, s.l.optionsPath(featureID, strconv.Itoa(o.ID)+"/"), nil, nil)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && httpStatus(err) != http.StatusNotFound:
			s.warnOption(k, "delete", opt.Value, err)
			final = append(final, opt) // still there: a weight write names it
		}
	}
	for i, o := range have {
		name := matched[i]
		if name == "" {
			continue
		}
		opt := o.option()
		opt.Variant = name
		if o.key() != name || !opt.Text || opt.Value != m.text[name] {
			err := s.optionCall(ctx, k, http.MethodPut, s.l.optionsPath(featureID, strconv.Itoa(o.ID)+"/"), body(name), nil)
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case err != nil:
				s.warnOption(k, "update", name, err)
			default:
				opt = Option{ID: o.ID, Variant: name, Key: name, Value: m.text[name], Text: true}
			}
		}
		final = append(final, opt)
	}
	for _, name := range m.offered {
		if taken[name] {
			continue
		}
		var created fsOption
		err := s.optionCall(ctx, k, http.MethodPost, s.l.optionsPath(featureID, ""), body(name), &created)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			s.warnOption(k, "create", name, err)
		default:
			final = append(final, Option{ID: created.ID, Variant: name, Key: name, Value: m.text[name], Text: true})
		}
	}
	s.l.setOptions(k.Flag, final)
	return nil
}

// optionCall is one paced option request inside a push span (op option).
func (s *startup) optionCall(ctx context.Context, k Key, method, path string, body, out any) error {
	sctx, span := s.pushSpan(ctx, k, OpOption)
	_, _, err := s.pace.call(sctx, method, path, body, out, true, nil)
	switch {
	case err == nil, method == http.MethodDelete && httpStatus(err) == http.StatusNotFound:
		s.endPush(sctx, span, OpOption, ResultOK, "")
	case httpStatus(err) >= 400 && httpStatus(err) < 500:
		s.endPush(sctx, span, OpOption, ResultRejected, Reason(err))
	default:
		s.endPush(sctx, span, OpOption, ResultError, Reason(err))
	}
	return err
}

func (s *startup) warnOption(k Key, op, value string, err error) {
	s.l.Logger().Warn("flagsmith option not synced", "flag_set", k.Set, "flag", k.Flag, "op", op, "option", value, "reason", Reason(err))
}

// listOptions reads a feature's options again (a weight write Flagsmith
// refused because they changed since the start).
func (o *out) listOptions(ctx context.Context, k Key, featureID int) error {
	if err := o.pace(ctx); err != nil {
		return err
	}
	var raw json.RawMessage
	if _, err := o.l.Client().Admin(ctx, http.MethodGet, o.l.optionsPath(featureID, ""), nil, &raw); err != nil {
		return err
	}
	var list []fsOption
	if err := json.Unmarshal(raw, &list); err != nil {
		var paged struct {
			Results []fsOption `json:"results"`
		}
		if err := json.Unmarshal(raw, &paged); err != nil {
			return ReasonError{Reason: "malformed_response"}
		}
		list = paged.Results
	}
	o.l.setOptions(k.Flag, o.l.knownOptions(o.l.Policy(k.Set, k.Flag), list))
	return nil
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
// key, its value; "" when none matches (the value is then read as any value).
func (l *Link) pickedVariant(pol *registry.Policy, mv MVStateValue) string {
	m := newOptionMatcher(pol, l.BaselineFlag(pol.Set, pol.Key))
	o := Option{ID: mv.Option.ID}
	if mv.Option.Key != nil {
		o.Key = *mv.Option.Key
	}
	if v := m.byKey(o.Key); v != "" {
		return v
	}
	for _, known := range l.options(pol.Key) {
		if known.ID == o.ID && m.is[known.Variant] {
			return known.Variant
		}
	}
	var s string
	if json.Unmarshal(mv.Option.Value, &s) == nil {
		o.Value, o.Text = s, true
	}
	return m.byValue(o)
}
