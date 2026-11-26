package flagsmith

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/flagsmith/flagsmithtest"
	"github.com/joustmania/flag-service/registry"
)

// Titled options (#2713, architecture note §2.3): the option sync's cases
// beyond the happy path (a renamed title, a Flagsmith without option keys,
// refused, deleted, failing and cancelled writes), the baseline's fit and the
// pull's read order.

// startOptions runs one start and waits for its option sync.
func startOptions(t *testing.T, e *startEnv) {
	t.Helper()
	if err := e.s.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	<-e.s.optionsDone()
}

// optionRequestsTo counts the mv-options requests of method on feature fid.
func optionRequestsTo(f *flagsmithtest.Fake, method string, fid int) int {
	n := 0
	for _, r := range f.RequestsTo(method, flagsmithtest.APIPath+"projects/") {
		if strings.Contains(r.Path, "/features/"+strconv.Itoa(fid)+"/mv-options/") {
			n++
		}
	}
	return n
}

// legacyOptions adds the options #2711 created for a flag (value = the
// variant name, no title) and returns their ids by variant.
func legacyOptions(e *startEnv, fid int, set, key string) map[string]int {
	ids := map[string]int{}
	for _, n := range OfferedVariants(e.l.Policy(set, key), e.l.BaselineFlag(set, key)) {
		ids[n] = e.admin.f.Options.Add(fid, n).ID
	}
	return ids
}

// optionWarns counts the start's WARN lines about options.
func optionWarns(e *startEnv) int { return strings.Count(e.logs.String(), `msg="flagsmith option`) }

func keysOf(opts []flagsmithtest.MVOption) []string {
	var out []string
	for _, o := range opts {
		k := "<none>"
		if o.Key != nil {
			k = *o.Key
		}
		out = append(out, k)
	}
	return out
}

// UC: UC-055 BR-005
func TestStartOptionsRenamedKey(t *testing.T) {
	chaos := `{"endurance":0.1,"balanced":0.1,"accelerate":0.1,"chaos":0.7}`
	titled := func(e *startEnv, skip string) map[string]int {
		p, b := e.l.Policy("agent", "objectives"), e.l.BaselineFlag("agent", "objectives")
		ids := map[string]int{}
		for _, n := range OfferedVariants(p, b) {
			if n != skip {
				ids[n] = e.admin.f.Options.AddKeyed(1, n, OptionText(p, b, n)).ID
			}
		}
		return ids
	}
	t.Run("UC-055/BR-005 #2713 an option whose title a person renamed is its variant by value and is updated in place", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		titled(e, "chaos_focused")
		renamed := e.admin.f.Options.AddKeyed(1, "chaos", chaos)
		startOptions(t, e)
		f := e.admin.f
		if p, d, u := optionRequestsTo(f, http.MethodPost, 1), optionRequestsTo(f, http.MethodDelete, 1), optionRequestsTo(f, http.MethodPut, 1); p != 0 || d != 0 || u != 1 {
			t.Fatalf("UC-055/BR-005 %d creates, %d deletes, %d updates; want one update", p, d, u)
		}
		for _, o := range f.Options.Of(1) {
			if o.ID == renamed.ID && (o.Key == nil || *o.Key != "chaos_focused") {
				t.Fatalf("UC-055/BR-005 renamed option titled %v, want chaos_focused", keysOf([]flagsmithtest.MVOption{o}))
			}
		}
		if got := keysOf(f.Options.Of(1)); len(got) != 5 {
			t.Fatalf("UC-055/BR-005 titles %v, want five", got)
		}
	})
	t.Run("UC-055/BR-005 #2713 a renamed option never takes a variant another option holds by its title: it is deleted", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		renamed := e.admin.f.Options.AddKeyed(1, "chaos", chaos) // listed first
		ids := titled(e, "")
		startOptions(t, e)
		f := e.admin.f
		if d, u := optionRequestsTo(f, http.MethodDelete, 1), optionRequestsTo(f, http.MethodPut, 1); d != 1 || u != 0 {
			t.Fatalf("UC-055/BR-005 %d deletes, %d updates; want the renamed option deleted, nothing updated", d, u)
		}
		for _, o := range f.Options.Of(1) {
			if o.ID == renamed.ID {
				t.Fatal("UC-055/BR-005 the renamed duplicate was kept")
			}
		}
		if got := e.l.options("objectives"); len(got) != 5 || got[3].ID != ids["chaos_focused"] || got[3].Variant != "chaos_focused" {
			t.Fatalf("UC-055/BR-005 known options %+v, want the titled chaos_focused kept", got)
		}
	})
}

// UC: UC-055 BR-005
func TestStartOptionsKeylessFlagsmith(t *testing.T) {
	names := func(e *startEnv, set, key string) []string {
		return OfferedVariants(e.l.Policy(set, key), e.l.BaselineFlag(set, key))
	}
	t.Run("UC-055/BR-005 #2713 options without the key field: the #2711 shape is kept, nothing updated, ids stay", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.f.Options.DropKeys()
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}, {ID: 2, Name: "tempo.ramp_down_seconds"}}
		legacyOptions(e, 1, "agent", "objectives")
		tempo := legacyOptions(e, 2, "game", "tempo.ramp_down_seconds")
		for i := 1; i <= 2; i++ {
			startOptions(t, e)
			if n := optionRequestsTo(e.admin.f, http.MethodPut, 1) + optionRequestsTo(e.admin.f, http.MethodPut, 2); n != 0 {
				t.Fatalf("UC-055/BR-005 start %d: %d updates, want none", i, n)
			}
		}
		if got, want := e.admin.f.Options.Values(2), names(e, "game", "tempo.ramp_down_seconds"); !reflect.DeepEqual(got, want) {
			t.Fatalf("UC-055/BR-005 tempo values %v, want the names %v", got, want)
		}
		for _, o := range e.admin.f.Options.Of(2) {
			if tempo[*o.StringValue] != o.ID {
				t.Fatalf("UC-055/BR-005 option %d (%s) replaced, ids %v", o.ID, *o.StringValue, tempo)
			}
		}
		if !e.l.optionsKeyless.Load() {
			t.Fatal("UC-055/BR-005 the link did not note a Flagsmith without option keys")
		}
		var backend int
		e.admin.mu.Lock()
		for _, f := range e.admin.features {
			if f.Name == "decision.backend" {
				backend = f.ID
			}
		}
		e.admin.mu.Unlock()
		if got, want := e.admin.f.Options.Values(backend), names(e, "agent", "decision.backend"); !reflect.DeepEqual(got, want) {
			t.Fatalf("UC-055/BR-005 created options %v, want the names %v", got, want)
		}
	})
	t.Run("UC-055/BR-005 #2713 positive control: the key field present and null on every option, so all are updated", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}, {ID: 2, Name: "tempo.ramp_down_seconds"}}
		legacyOptions(e, 1, "agent", "objectives")
		tempo := legacyOptions(e, 2, "game", "tempo.ramp_down_seconds")
		startOptions(t, e)
		if u1, u2 := optionRequestsTo(e.admin.f, http.MethodPut, 1), optionRequestsTo(e.admin.f, http.MethodPut, 2); u1 != 5 || u2 != 4 {
			t.Fatalf("UC-055/BR-005 %d and %d updates, want 5 and 4", u1, u2)
		}
		if got, want := e.admin.f.Options.Keys(2), names(e, "game", "tempo.ramp_down_seconds"); !reflect.DeepEqual(got, want) {
			t.Fatalf("UC-055/BR-005 tempo titles %v, want %v", got, want)
		}
		if got := e.admin.f.Options.Values(2); !reflect.DeepEqual(got, []string{"0.1", "1.5", "1.5", "3.0"}) {
			t.Fatalf("UC-055/BR-005 tempo values %v", got)
		}
		for _, o := range e.admin.f.Options.Of(2) {
			if tempo[*o.Key] != o.ID {
				t.Fatalf("UC-055/BR-005 option %s got id %d, ids %v", *o.Key, o.ID, tempo)
			}
		}
		if e.l.optionsKeyless.Load() {
			t.Fatal("UC-055/BR-005 a null key was taken for a Flagsmith without option keys")
		}
	})
	t.Run("UC-055/BR-005 #2713 a project without options decides from the first create's answer", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.f.Options.DropKeys()
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		startOptions(t, e)
		keyed := 0
		for _, r := range e.admin.f.RequestsTo(http.MethodPost, flagsmithtest.APIPath+"projects/") {
			if strings.Contains(r.Path, "/mv-options/") && strings.Contains(string(r.Body), `"key"`) {
				keyed++
			}
		}
		if keyed != 1 {
			t.Fatalf("UC-055/BR-005 %d creates sent a key, want only the first", keyed)
		}
		startOptions(t, e)
		if got, want := e.admin.f.Options.Values(1), names(e, "agent", "objectives"); !reflect.DeepEqual(got, want) {
			t.Fatalf("UC-055/BR-005 objectives values %v after the second start, want the names %v", got, want)
		}
		puts := optionRequests(e.admin.f, http.MethodPut)
		startOptions(t, e)
		if n := optionRequests(e.admin.f, http.MethodPut) - puts; n != 0 {
			t.Fatalf("UC-055/BR-005 third start: %d updates, want none", n)
		}
	})
	t.Run("UC-055/BR-005 #2713 a project without options whose first create answers the key as null has no option keys", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.f.Options.NullKeys()
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		startOptions(t, e)
		if !e.l.optionsKeyless.Load() {
			t.Fatal("UC-055/BR-005 a key sent and answered null was not taken for a Flagsmith without option keys")
		}
		keyed := 0
		for _, r := range e.admin.f.RequestsTo(http.MethodPost, flagsmithtest.APIPath+"projects/") {
			if strings.Contains(r.Path, "/mv-options/") && strings.Contains(string(r.Body), `"key"`) {
				keyed++
			}
		}
		if keyed != 1 {
			t.Fatalf("UC-055/BR-005 %d creates sent a key, want only the first", keyed)
		}
		// Only the first create (of whichever feature came first) carried the
		// variant's text; every later one is the #2711 shape.
		if got, want := e.admin.f.Options.Values(1), names(e, "agent", "objectives"); len(got) != len(want) || !reflect.DeepEqual(got[1:], want[1:]) {
			t.Fatalf("UC-055/BR-005 objectives values %v, want the names %v after the first", got, want)
		}
	})
}

// UC: UC-055 BR-005
func TestStartOptionsWriteRefused(t *testing.T) {
	for _, st := range []int{http.StatusForbidden, http.StatusMethodNotAllowed} {
		t.Run("UC-055/BR-005 #2713 an update answered "+strconv.Itoa(st)+" stops every option write of the pass", func(t *testing.T) {
			e := newStartEnv(t)
			e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}, {ID: 2, Name: "decision.backend"}}
			obj := legacyOptions(e, 1, "agent", "objectives")
			legacyOptions(e, 2, "agent", "decision.backend")
			e.admin.f.Options.Script(http.MethodPut, st)
			startOptions(t, e)
			refused, after := -1, 0
			for i, r := range e.admin.f.Requests() {
				if !strings.Contains(r.Path, "/mv-options/") || r.Method == http.MethodGet {
					continue
				}
				if refused >= 0 {
					after++
				} else if r.Method == http.MethodPut {
					refused = i
				}
			}
			if refused < 0 || after != 0 {
				t.Fatalf("UC-055/BR-005 %d option writes after the refused update, want none", after)
			}
			if n := optionWarns(e); n != 1 {
				t.Fatalf("UC-055/BR-005 %d option WARNs, want one:\n%s", n, e.logs.String())
			}
			for _, o := range e.admin.f.Options.Of(1) {
				if o.Key != nil || obj[*o.StringValue] != o.ID {
					t.Fatalf("UC-055/BR-005 objectives option changed: %+v", o)
				}
			}
			var mv MVStateValue
			mv.Option.ID, mv.Option.Value = obj["chaos_focused"], json.RawMessage(`"chaos_focused"`)
			if v := e.l.pickedVariant(e.l.Policy("agent", "objectives"), mv); v != "chaos_focused" {
				t.Fatalf("UC-055/BR-005 a pick of the #2711 option is %q, want chaos_focused", v)
			}
		})
	}
	t.Run("UC-055/BR-005 #2713 a retry's listing answered 403 stops the pass with one WARN", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		legacyOptions(e, 1, "agent", "objectives")
		e.admin.f.Options.Script(http.MethodPut, transientRun()...)
		e.admin.f.Options.Script(http.MethodGet, http.StatusForbidden)
		startOptions(t, e)
		logs := e.logs.String()
		if strings.Contains(logs, "op=list") {
			t.Fatalf("UC-055/BR-005 the refused listing was logged as a failed option call:\n%s", logs)
		}
		if n := strings.Count(logs, "Flagsmith refused an option write"); n != 1 {
			t.Fatalf("UC-055/BR-005 %d stop WARNs, want one:\n%s", n, logs)
		}
		if n := e.sleepsOf(optionRetryEvery); n != 1 {
			t.Fatalf("UC-055/BR-005 %d retry waits, want only the one before the refused listing", n)
		}
	})
}

// transientRun is one run of transient errors: optionTransientRun option
// calls failing after all their attempts.
func transientRun() []int {
	var out []int
	for i := 0; i < optionTransientRun*transientAttempts; i++ {
		out = append(out, http.StatusServiceUnavailable)
	}
	return out
}

// UC: UC-055 BR-005
func TestStartOptionsUpdateNotFound(t *testing.T) {
	e := newStartEnv(t)
	e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
	obj := legacyOptions(e, 1, "agent", "objectives")
	// A person deletes the mixed option between the listing and its update.
	e.admin.afterList = func() { e.admin.f.Options.Remove(1, obj["mixed"]) }
	startOptions(t, e)
	t.Run("UC-055/BR-005 #2713 an option deleted after the listing is created again in the same pass", func(t *testing.T) {
		posts := 0
		for _, r := range e.admin.f.RequestsTo(http.MethodPost, flagsmithtest.APIPath+"projects/") {
			if strings.Contains(r.Path, "/features/1/mv-options/") {
				posts++
				if !strings.Contains(string(r.Body), `"key":"mixed"`) {
					t.Fatalf("UC-055/BR-005 create body %s, want the mixed key", r.Body)
				}
			}
		}
		if posts != 1 {
			t.Fatalf("UC-055/BR-005 %d creates, want one", posts)
		}
		if got := keysOf(e.admin.f.Options.Of(1)); len(got) != 5 || got[4] != "mixed" {
			t.Fatalf("UC-055/BR-005 titles %v, want mixed created again", got)
		}
		for _, o := range e.l.options("objectives") {
			if o.ID == obj["mixed"] {
				t.Fatalf("UC-055/BR-005 the link kept the deleted option %d", o.ID)
			}
		}
	})
	t.Run("UC-055/BR-005 #2713 the update answered 404 is handled and counted ok, like a delete's 404", func(t *testing.T) {
		n := 0
		for _, s := range e.spans.Ended() {
			if s.Name() != SpanPush || spanAttr(s, AttrOp) != OpOption {
				continue
			}
			n++
			if r := spanAttr(s, AttrResult); r != ResultOK {
				t.Fatalf("UC-055/BR-005 an option call counted %s (%s), want every one ok", r, spanAttr(s, AttrReason))
			}
		}
		if n == 0 {
			t.Fatal("UC-055/BR-005 no option push span")
		}
	})
}

// UC: UC-055 BR-005
func TestStartOptionsRetryLimits(t *testing.T) {
	t.Run("UC-055/BR-005 #2713 a feature whose option call failed without a run of errors is retried", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		obj := legacyOptions(e, 1, "agent", "objectives")
		// One update fails after all its attempts; the next succeed.
		for i := 0; i < transientAttempts; i++ {
			e.admin.f.Options.Script(http.MethodPut, http.StatusServiceUnavailable)
		}
		startOptions(t, e)
		if strings.Contains(e.logs.String(), "transient errors in a row") {
			t.Fatalf("UC-055/BR-005 one failed call aborted the pass:\n%s", e.logs.String())
		}
		if n := e.sleepsOf(optionRetryEvery); n != 1 {
			t.Fatalf("UC-055/BR-005 %d retry waits, want one", n)
		}
		for _, o := range e.admin.f.Options.Of(1) {
			if o.Key == nil || obj[*o.Key] != o.ID {
				t.Fatalf("UC-055/BR-005 objectives titles %v after the retry, ids %v", keysOf(e.admin.f.Options.Of(1)), obj)
			}
		}
	})
	t.Run("UC-055/BR-005 #2713 an option sync failing every time stops after 10 retries with one WARN", func(t *testing.T) {
		e := newStartEnv(t)
		e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
		legacyOptions(e, 1, "agent", "objectives")
		// The spec's cap (§2.3 "at most 10 times"), not optionRetries, so a changed cap fails.
		const retries = 10
		for i := 0; i <= retries; i++ {
			e.admin.f.Options.Script(http.MethodPut, transientRun()...)
		}
		startOptions(t, e)
		if n := e.sleepsOf(optionRetryEvery); n != retries {
			t.Fatalf("UC-055/BR-005 %d retry waits, want %d", n, retries)
		}
		if n := strings.Count(e.logs.String(), "the option sync failed after its retries"); n != 1 {
			t.Fatalf("UC-055/BR-005 %d final WARNs, want one:\n%s", n, e.logs.String())
		}
		if n := optionRequestsTo(e.admin.f, http.MethodPut, 1); n != (retries+1)*optionTransientRun*transientAttempts {
			t.Fatalf("UC-055/BR-005 %d update attempts, want %d (one pass and %d retries)", n, (retries+1)*optionTransientRun*transientAttempts, retries)
		}
	})
}

// UC: UC-055 BR-005
func TestStartOptionsRetried(t *testing.T) {
	e := newStartEnv(t)
	e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}, {ID: 2, Name: "tempo.ramp_down_seconds"}}
	obj := legacyOptions(e, 1, "agent", "objectives")
	legacyOptions(e, 2, "game", "tempo.ramp_down_seconds")
	// Three updates fail after all their attempts: a run of transient errors.
	var fails []int
	for i := 0; i < optionTransientRun*transientAttempts; i++ {
		fails = append(fails, http.StatusServiceUnavailable)
	}
	e.admin.f.Options.Script(http.MethodPut, fails...)
	startOptions(t, e)
	t.Run("UC-055/BR-005 #2713 a run of transient errors ends the pass with one WARN", func(t *testing.T) {
		if n := strings.Count(e.logs.String(), "transient errors in a row"); n != 1 {
			t.Fatalf("UC-055/BR-005 %d abort WARNs, want one:\n%s", n, e.logs.String())
		}
		relist, tempoPut := -1, -1
		for i, r := range e.admin.f.Requests() {
			switch {
			case relist < 0 && r.Method == http.MethodGet && strings.Contains(r.Path, "/mv-options/"):
				relist = i
			case tempoPut < 0 && r.Method == http.MethodPut && strings.Contains(r.Path, "/features/2/mv-options/"):
				tempoPut = i
			}
		}
		if relist < 0 || tempoPut < relist {
			t.Fatalf("UC-055/BR-005 tempo updated at request %d, first listing of the retry at %d: the pass went on after the run", tempoPut, relist)
		}
	})
	t.Run("UC-055/BR-005 #2713 the pass is retried after 60 s and brings every option in step, ids kept", func(t *testing.T) {
		waits := 0
		for _, d := range e.clock.got() {
			if d == optionRetryEvery {
				waits++
			}
		}
		if waits != 1 {
			t.Fatalf("UC-055/BR-005 %d retry waits, want one", waits)
		}
		for _, o := range e.admin.f.Options.Of(1) {
			if o.Key == nil || obj[*o.Key] != o.ID {
				t.Fatalf("UC-055/BR-005 objectives titles %v after the retry", keysOf(e.admin.f.Options.Of(1)))
			}
		}
		if got := keysOf(e.admin.f.Options.Of(2)); !reflect.DeepEqual(got, []string{"ci", "original", "default", "slow"}) {
			t.Fatalf("UC-055/BR-005 tempo titles %v after the retry", got)
		}
	})
}

// UC: UC-055 BR-005
func TestStartOptionsCancelled(t *testing.T) {
	e := newStartEnv(t)
	e.admin.features = []fsFeature{{ID: 1, Name: "objectives"}}
	obj := legacyOptions(e, 1, "agent", "objectives")
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var puts atomic.Int32
	e.admin.f.Options.OnWrite(func(method string) {
		if method == http.MethodPut && puts.Add(1) == 3 {
			cancel()
		}
	})
	if err := e.s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-e.s.optionsDone()
	// The PUT that cancelled is still being served: count once it stored.
	e.admin.f.Options.WaitWrites()
	e.admin.f.Options.OnWrite(nil)
	inStep := 0
	for _, o := range e.admin.f.Options.Of(1) {
		if o.Key != nil {
			inStep++
		}
	}
	if inStep == 0 || inStep == 5 {
		t.Fatalf("%d options updated before the cancel, want some", inStep)
	}
	before := optionRequestsTo(e.admin.f, http.MethodPut, 1)
	startOptions(t, e)
	t.Run("UC-055/BR-005 #2713 after a cancelled start the next start updates the rest, ids unchanged", func(t *testing.T) {
		if n := optionRequestsTo(e.admin.f, http.MethodPut, 1) - before; n != 5-inStep {
			t.Fatalf("UC-055/BR-005 %d updates, want the %d left", n, 5-inStep)
		}
		for _, o := range e.admin.f.Options.Of(1) {
			if o.Key == nil || obj[*o.Key] != o.ID {
				t.Fatalf("UC-055/BR-005 titles %v, ids %v", keysOf(e.admin.f.Options.Of(1)), obj)
			}
		}
	})
}

// UC: UC-055 BR-005
func TestOptionBaseline(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..")
	slug := regexp.MustCompile(`^[-a-zA-Z0-9_]+$`)
	// Flags whose variants share a value (§2.3: rule 4 cannot tell them
	// apart; the key or rule 2 does).
	sameValue := map[string]bool{"tempo.ramp_down_seconds": true, "llm.max_requests_per_minute": true}
	for _, sub := range []string{"", "ci"} {
		t.Run("UC-055/BR-005 #2713 every offered variant of services/flagd/"+sub+" fits a titled option", func(t *testing.T) {
			base, err := baseline.Load(filepath.Join(root, "..", "flagd", sub), baseline.DefaultFiles)
			if err != nil {
				t.Fatal(err)
			}
			reg, err := registry.LoadFile(filepath.Join(root, "registry.yaml"), base)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range reg.Policies() {
				b := base.Flag(p.Set, p.Key)
				offered := OfferedVariants(p, b)
				is := map[string]bool{}
				for _, n := range offered {
					is[n] = true
				}
				seen := map[string]string{}
				for _, n := range offered {
					if !slug.MatchString(n) || len(n) > 255 || n == "control" {
						t.Errorf("%s: variant %q is no Flagsmith option key", p.Key, n)
					}
					text := OptionText(p, b, n)
					var own string
					if text == n && (json.Unmarshal(b.Variants[n], &own) != nil || own != n) {
						t.Errorf("%s/%s: the option text fell back to the name", p.Key, n)
					}
					if utf8.RuneCountInString(text) > optionValueLimit || strings.TrimSpace(text) != text {
						t.Errorf("%s/%s: text %q does not survive Flagsmith (limit, trimmed)", p.Key, n, text)
					}
					if is[text] && text != n {
						t.Errorf("%s/%s: text %q is another variant's name", p.Key, n, text)
					}
					if other, dup := seen[normText(text)]; dup && !sameValue[p.Key] {
						t.Errorf("%s: %s and %s share the text %q", p.Key, other, n, text)
					}
					seen[normText(text)] = n
				}
			}
		})
	}
}

// UC: UC-054 A6
func TestPickedVariantReadOrder(t *testing.T) {
	e := newStartEnv(t)
	pol := e.l.Policy("agent", "objectives")
	e.l.setOptions("objectives", []Option{{ID: 32, Variant: "mixed", Value: "mixed", Text: true}})
	var mv MVStateValue
	mv.Option.ID, mv.Option.Value = 32, json.RawMessage(`"chaos_focused"`)
	t.Run("UC-054/A6 #2713 with option keys the recorded id goes before the value", func(t *testing.T) {
		if v := e.l.pickedVariant(pol, mv); v != "mixed" {
			t.Fatalf("UC-054/A6 %q, want mixed by the recorded id", v)
		}
	})
	t.Run("UC-054/A6 #2713 on a Flagsmith without option keys a value naming a variant goes first (#2711 order)", func(t *testing.T) {
		e.l.optionsKeyless.Store(true)
		defer e.l.optionsKeyless.Store(false)
		if v := e.l.pickedVariant(pol, mv); v != "chaos_focused" {
			t.Fatalf("UC-054/A6 %q, want chaos_focused, the name the operator sees", v)
		}
	})
}

// UC: UC-054 A6
func TestSetOptionsForcesFullPull(t *testing.T) {
	e := newStartEnv(t)
	opts := []Option{{ID: 41, Variant: "original"}, {ID: 42, Variant: "default"}}
	e.l.setOptions("tempo.ramp_down_seconds", opts)
	e.l.fullPull.Store(false)
	t.Run("UC-054/A6 #2713 the same id -> variant pairs leave the pull conditional", func(t *testing.T) {
		e.l.setOptions("tempo.ramp_down_seconds", append([]Option(nil), opts...))
		if e.l.fullPull.Load() {
			t.Fatal("UC-054/A6 an unchanged mapping forced a full pull")
		}
	})
	t.Run("UC-054/A6 #2713 a changed id -> variant pair makes the next pull full", func(t *testing.T) {
		e.l.setOptions("tempo.ramp_down_seconds", []Option{{ID: 41, Variant: "original"}, {ID: 42}})
		if !e.l.fullPull.Load() {
			t.Fatal("UC-054/A6 a changed mapping left the pull conditional")
		}
	})
}
