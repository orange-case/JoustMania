package flagsmith

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/joustmania/flag-service/baseline"
	"github.com/joustmania/flag-service/fserr"
	"github.com/joustmania/flag-service/registry"
	"github.com/joustmania/flag-service/state"
	"github.com/joustmania/flag-service/telemetry"
)

// WriterLabel is the writer label of every pulled change. It is reserved: an
// HTTP write that labels itself with it is 400 malformed (services.md §7.2).
const WriterLabel = "flagsmith"

// Mode is the link state.
type Mode string

// The link states (services.md §7.3).
const (
	ModeOff      Mode = "off"
	ModeStarting Mode = "starting"
	ModeOnline   Mode = "online"
	ModeOffline  Mode = "offline"
)

// Modes lists every link state (the link gauge's label values).
var Modes = []Mode{ModeOff, ModeStarting, ModeOnline, ModeOffline}

// Backoff of the reconnect loop (services.md §7.3 Offline): 1, 2, 4 ... 60 s.
// It starts again at BackoffMin only after the link stayed online for
// BackoffResetAfter: a side that fails right after each reconnect keeps
// backing off (each reconnect is a full pull and a reconcile).
const (
	BackoffMin        = time.Second
	BackoffMax        = 60 * time.Second
	BackoffResetAfter = 30 * time.Second
)

// LocalWriter is the flag service's in-process write entry (*api.API): the
// same validation, apply code, span and metrics as the HTTP writes, writer
// label WriterLabel.
type LocalWriter interface {
	FlagsmithPut(ctx context.Context, req state.WriteRequest) (state.WriteResult, *fserr.Error)
	FlagsmithAppend(ctx context.Context, req state.AppendRequest) (state.WriteResult, *fserr.Error)
	FlagsmithClear(ctx context.Context, req state.ClearRequest) (state.WriteResult, *fserr.Error)
}

// outSide sends local changes to Flagsmith (out.go, #2589).
type outSide interface {
	// Changed is called by the observer under State.mu for every Change: it
	// must not block and must not call State or the LocalWriter (map work on
	// LinkState only).
	Changed(c state.Change)
	// Run sends pending values until ctx ends or the link goes down (an
	// error that IsLinkDown).
	Run(ctx context.Context) error
}

// inSide applies Flagsmith's changes to the stack (in.go, #2590).
type inSide interface {
	// Reconcile runs at start (trigger "start", once Start succeeded) and on
	// every reconnect ("reconnect"): a full pull with an empty
	// If-Modified-Since, then the 3-way reconcile (FR-210). A key with no
	// remote value (never seen, LinkState.Remote ok false) means the
	// document wins.
	Reconcile(ctx context.Context, trigger string) error
	// Poll pulls until ctx ends or the link goes down; Link.WakeC asks for an
	// early pull (realtime, a webhook-less nudge).
	Poll(ctx context.Context) error
}

// starter runs the start sequence once per process (start.go, #2591): ping,
// resolve ids, create features and tags, schedule the identity cleanup. It must not call
// NotePull (the first Reconcile pulls in full).
type starter interface {
	Start(ctx context.Context) error
}

// Deps are the link's collaborators.
type Deps struct {
	State  *state.State
	Writer LocalWriter
	// Registry and Baseline default to the State's.
	Registry *registry.Registry
	Baseline *baseline.Baseline
	Tracer   trace.Tracer
	Meter    metric.Meter
	Logger   *slog.Logger
	// Links holds the flagservice.write span of each snapshot version (the
	// push span's link); a version older than its last 128 writes has none.
	Links *telemetry.Links
	// HTTPClient (nil: one with the configured timeout).
	HTTPClient *http.Client
	Now        func() time.Time
	Sleep      func(ctx context.Context, d time.Duration) error
}

// IDs are the Flagsmith ids the start sequence resolves. Lock it to read or
// write a field.
type IDs struct {
	sync.Mutex
	ProjectID       int
	EnvironmentID   int
	EnvironmentName string
	// FeatureID by flag key, TagID by flag set, IdentityUUID by identifier.
	FeatureID    map[string]int
	TagID        map[string]int
	IdentityUUID map[string]string
}

// Status is the flagsmith object of GET /state (services.md §7.3).
type Status struct {
	Link          Mode       `json:"link"`
	Environment   string     `json:"environment,omitempty"`
	LastPullAt    *time.Time `json:"last_pull_at,omitempty"`
	Pending       int        `json:"pending"`
	OutOfSync     int        `json:"out_of_sync"`
	Refused       int        `json:"refused"`
	OfflineReason string     `json:"offline_reason,omitempty"`
	ConfigError   []string   `json:"config_error,omitempty"`
}

// Link is the Flagsmith link of one stack.
type Link struct {
	cfg     Config
	deps    Deps
	client  *Client
	links   *LinkState
	metrics *Metrics
	ids     *IDs
	wake    chan struct{}

	out   outSide
	in    inSide
	start starter

	mu            sync.Mutex
	mode          Mode
	offlineReason string
	lastPullAt    time.Time
	lastModified  string
	started       bool
	// reconciled: a Reconcile succeeded once; until then every connect
	// reconciles with TriggerStart (the first round after start is special,
	// architecture note §5.1).
	reconciled bool
	// connected: the last pull reached Flagsmith and the link has not gone
	// offline since; a local change made while it is false is made offline
	// (UC-054 A2, #2621).
	connected atomic.Bool

	gamesSeenOnce sync.Once
	gamesSeen     chan struct{}
}

// New builds the link. It never fails in a way that should stop the service:
// a metric registration error is returned for the log, and the link works
// without metrics.
func New(cfg Config, d Deps) (*Link, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Tracer == nil {
		d.Tracer = tracenoop.NewTracerProvider().Tracer("flagsmith")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleepCtx
	}
	if d.State != nil {
		if d.Registry == nil {
			d.Registry = d.State.Registry()
		}
		if d.Baseline == nil {
			d.Baseline = d.State.Baseline()
		}
	}
	l := &Link{
		cfg:    cfg,
		deps:   d,
		client: NewClient(cfg, d.HTTPClient),
		links:  NewLinkState(d.Now),
		ids:    &IDs{FeatureID: map[string]int{}, TagID: map[string]int{}, IdentityUUID: map[string]string{}},
		wake:   make(chan struct{}, 1),
		mode:   ModeOff,

		gamesSeen: make(chan struct{}),
	}
	l.out, l.in, l.start = newOut(l), newIn(l), newStarter(l)
	m, err := NewMetrics(d.Meter, l.Mode, l.links.Counts)
	if err != nil {
		return l, err
	}
	l.metrics = m
	return l, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Enabled reports whether the link is configured on.
func (l *Link) Enabled() bool { return l != nil && l.cfg.Enabled }

// Run runs the link until ctx ends: it returns at once when the link is off.
// Start runs until it succeeds once, never again in this process; then
// Reconcile, online, and the out and in sides until one fails. A failure sets
// the link offline with its reason and retries after the backoff (1, 2, 4 ...
// 60 s, back to 1 s once the link stayed online for BackoffResetAfter) with
// Reconcile("reconnect"), or Reconcile("start") while no Reconcile succeeded.
func (l *Link) Run(ctx context.Context) {
	if !l.Enabled() {
		l.setMode(ModeOff, "")
		return
	}
	var backoff time.Duration
	for ctx.Err() == nil {
		err := l.connect(ctx)
		if err == nil {
			l.setMode(ModeOnline, "")
			onlineAt := l.deps.Now()
			err = l.runSides(ctx)
			if l.deps.Now().Sub(onlineAt) >= BackoffResetAfter {
				backoff = 0
			}
		}
		if ctx.Err() != nil {
			return
		}
		reason := Reason(err)
		l.setMode(ModeOffline, reason)
		l.deps.Logger.Warn("flagsmith link offline", "reason", reason, "error", err)
		backoff = nextBackoff(backoff)
		if l.deps.Sleep(ctx, backoff) != nil {
			return
		}
	}
}

func nextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return BackoffMin
	}
	d *= 2
	if d > BackoffMax {
		d = BackoffMax
	}
	return d
}

// connect runs Start (until it succeeded once) and the reconcile: trigger
// "start" until a Reconcile succeeded once, then "reconnect".
func (l *Link) connect(ctx context.Context) error {
	l.mu.Lock()
	started, reconciled := l.started, l.reconciled
	l.mu.Unlock()
	if !started {
		l.setMode(ModeStarting, "")
		if err := l.start.Start(ctx); err != nil {
			return err
		}
		l.mu.Lock()
		l.started = true
		l.mu.Unlock()
	}
	trigger := TriggerReconnect
	if !reconciled {
		trigger = TriggerStart
	}
	if err := l.in.Reconcile(ctx, trigger); err != nil {
		return err
	}
	l.mu.Lock()
	l.reconciled = true
	l.mu.Unlock()
	return nil
}

// runSides runs out.Run and in.Poll on one derived context: the first to
// return cancels the other, and both have returned before the error goes back.
func (l *Link) runSides(ctx context.Context) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- l.out.Run(sctx) }()
	go func() { errs <- l.in.Poll(sctx) }()
	err := <-errs
	cancel()
	<-errs
	if err == nil {
		err = ReasonError{Reason: ReasonStopped}
	}
	return err
}

func (l *Link) setMode(m Mode, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mode, l.offlineReason = m, reason
	if m != ModeOnline {
		l.connected.Store(false)
	}
}

// Mode returns the link state.
func (l *Link) Mode() Mode {
	if l == nil {
		return ModeOff
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode
}

// Status is GET /state's flagsmith object; a nil link is off.
func (l *Link) Status() Status {
	if l == nil {
		return Status{Link: ModeOff}
	}
	l.mu.Lock()
	st := Status{Link: l.mode, OfflineReason: l.offlineReason, ConfigError: append([]string(nil), l.cfg.ConfigError...)}
	if !l.lastPullAt.IsZero() {
		t := l.lastPullAt.UTC()
		st.LastPullAt = &t
	}
	l.mu.Unlock()
	if st.Link == ModeOff {
		st.OfflineReason = ""
		return st
	}
	l.ids.Lock()
	st.Environment = l.ids.EnvironmentName
	l.ids.Unlock()
	st.Pending, st.OutOfSync, st.Refused = l.links.Counts()
	return st
}

// StateChanged is the state.Observer: it hands every Change to the out side.
// A game registration also closes GamesSeen (the first one) and wakes the in
// side for a round right away (Wake, non-blocking). Called under State.mu.
func (l *Link) StateChanged(c state.Change) {
	if !l.Enabled() {
		return
	}
	l.out.Changed(c)
	if c.Kind == state.ChangeGameRegistered {
		l.gamesSeenOnce.Do(func() { close(l.gamesSeen) })
		l.Wake()
	}
}

// GamesSeen is closed once StateChanged observed the first
// ChangeGameRegistered (the start sequence's stale-identity cleanup waits for
// it, architecture note §5.1).
func (l *Link) GamesSeen() <-chan struct{} { return l.gamesSeen }

// Client returns the Flagsmith client.
func (l *Link) Client() *Client { return l.client }

// Links returns the link's per-key state.
func (l *Link) Links() *LinkState { return l.links }

// Config returns the configuration.
func (l *Link) Config() Config { return l.cfg }

// Metrics returns the instruments (nil-safe).
func (l *Link) Metrics() *Metrics { return l.metrics }

// Tracer returns the tracer.
func (l *Link) Tracer() trace.Tracer { return l.deps.Tracer }

// Logger returns the logger.
func (l *Link) Logger() *slog.Logger { return l.deps.Logger }

// State returns the flag service state.
func (l *Link) State() *state.State { return l.deps.State }

// Writer returns the in-process write entry.
func (l *Link) Writer() LocalWriter { return l.deps.Writer }

// IDs returns the resolved Flagsmith ids.
func (l *Link) IDs() *IDs { return l.ids }

// SpanLinks returns the write-span links by snapshot version.
func (l *Link) SpanLinks() *telemetry.Links { return l.deps.Links }

// WriteSpanContext returns the flagservice.write span that produced version,
// if it is still known.
func (l *Link) WriteSpanContext(version uint64) (trace.SpanContext, bool) {
	if l.deps.Links == nil || version == 0 {
		return trace.SpanContext{}, false
	}
	e, ok := l.deps.Links.Lookup(version)
	return e.SC, ok
}

// Policy returns the registry row of set/key, or nil.
func (l *Link) Policy(set, key string) *registry.Policy {
	if l.deps.Registry == nil {
		return nil
	}
	return l.deps.Registry.Get(set, key)
}

// BaselineFlag returns the baseline flag of set/key, or nil.
func (l *Link) BaselineFlag(set, key string) *baseline.Flag {
	if l.deps.Baseline == nil {
		return nil
	}
	return l.deps.Baseline.Flag(set, key)
}

// Now is the injected clock.
func (l *Link) Now() time.Time { return l.deps.Now() }

// Sleep waits d or until ctx ends (injected in tests).
func (l *Link) Sleep(ctx context.Context, d time.Duration) error { return l.deps.Sleep(ctx, d) }

// NotePull records a document fetch: its time and Last-Modified.
func (l *Link) NotePull(at time.Time, lastModified string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastPullAt = at
	if lastModified != "" {
		l.lastModified = lastModified
	}
}

// LastModified is the Last-Modified of the last pull ("" before one).
func (l *Link) LastModified() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastModified
}

// Wake asks the in side for an early pull (non-blocking).
func (l *Link) Wake() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// WakeC is the channel Wake feeds (capacity 1).
func (l *Link) WakeC() <-chan struct{} { return l.wake }

// wakeOut asks the out side to send what a pull round queued, the wake the
// observer path uses (non-blocking, #2608). While the out side does not run
// (the start and reconnect rounds run before it) the wake is kept: Run sends
// whatever is pending first anyway, so it costs no request.
func (l *Link) wakeOut() {
	if o, ok := l.out.(*out); ok {
		o.poke()
	}
}
