package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
)

// Variant options (#2709, architecture note §2.3, UC-054 A6 / BR-012, UC-055
// BR-005): a synced feature whose flag offers two or more declared variants
// shows them as Flagsmith multivariate options; the operator picks one by
// setting it to 100 %. The stack applies exactly one value: any other split is
// refused.

// percentSplitReason is the health text of a refused percentage split.
const percentSplitReason = "percentage splits are not applied by JoustMania; set one option to 100 % or every option to 0 %"

// Option is one multivariate option of a feature: its id and, for a text
// option, its value (Text false: an integer or boolean option, or none).
type Option struct {
	ID    int
	Value string
	Text  bool
}

// fsOption is a multivariate option as the Admin API names it (the feature
// list's multivariate_options and the mv-options routes).
type fsOption struct {
	ID          int     `json:"id"`
	Type        string  `json:"type"`
	StringValue *string `json:"string_value"`
}

func (o fsOption) option() Option {
	if o.Type != TypeUnicode || o.StringValue == nil {
		return Option{ID: o.ID}
	}
	return Option{ID: o.ID, Value: *o.StringValue, Text: true}
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
	opts := make([]Option, 0, len(have))
	for _, o := range have {
		opts = append(opts, o.option())
	}
	s.l.setOptions(k.Flag, opts)
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
// (§2.3): a missing one is created at 0 % in every environment, one that
// names no offered variant (a dropped variant, a person's, a non-text option,
// a duplicate) is removed. A failed call is logged and counted; only a cancel
// is returned.
func (s *startup) syncOptions(ctx context.Context, k Key, p *registry.Policy, featureID int, have []fsOption) error {
	want := OfferedVariants(p, s.l.BaselineFlag(p.Set, p.Key))
	offered := map[string]bool{}
	for _, n := range want {
		offered[n] = true
	}
	kept := map[string]bool{}
	var final []Option
	for _, o := range have {
		opt := o.option()
		if opt.Text && offered[opt.Value] && !kept[opt.Value] {
			kept[opt.Value] = true
			final = append(final, opt)
			continue
		}
		err := s.optionCall(ctx, k, http.MethodDelete, s.l.optionsPath(featureID, strconv.Itoa(o.ID)+"/"), nil, nil)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && httpStatus(err) != http.StatusNotFound:
			s.warnOption(k, "delete", opt.Value, err)
			final = append(final, opt) // still there: a weight write names it
		}
	}
	for _, name := range want {
		if kept[name] {
			continue
		}
		body := map[string]any{"feature": featureID, "type": TypeUnicode, "string_value": name, "default_percentage_allocation": 0}
		var created fsOption
		err := s.optionCall(ctx, k, http.MethodPost, s.l.optionsPath(featureID, ""), body, &created)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			s.warnOption(k, "create", name, err)
		default:
			opt := created.option()
			if !opt.Text {
				opt = Option{ID: created.ID, Value: name, Text: true}
			}
			final = append(final, opt)
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
func (o *out) listOptions(ctx context.Context, flag string, featureID int) error {
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
	opts := make([]Option, 0, len(list))
	for _, fo := range list {
		opts = append(opts, fo.option())
	}
	o.l.setOptions(flag, opts)
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
// first text option whose value is the value sent while the overlay is on, 0
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
		if picked && o.Text && o.Value == s {
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
	} `json:"multivariate_feature_option"`
	PercentageAllocation float64 `json:"percentage_allocation"`
}

// pickedValue reads a feature state's weights (§2.3): none or all 0 -> the
// feature state's value (ok false); exactly one option at 100 and the others
// 0 -> that option's value; anything else is a percentage split (refused).
func pickedValue(pol *registry.Policy, mv []MVStateValue) (json.RawMessage, bool, *fserr.Error) {
	var picked json.RawMessage
	found, split := false, false
	for _, v := range mv {
		switch {
		case v.PercentageAllocation == 0:
		case v.PercentageAllocation == 100 && !found:
			picked, found = v.Option.Value, true
		default:
			split = true
		}
	}
	if split {
		return nil, false, fserr.Unprocessable(fserr.FlagsmithPercentageSplit, "%s: %s", pol.Key, percentSplitReason)
	}
	if !found || len(bytes.TrimSpace(picked)) == 0 {
		return nil, false, nil
	}
	return picked, true, nil
}
