package flagsmith

import (
	"context"
	"sort"
	"sync"
)

// Feature health (architecture note §8, FR-213, UC-054 A4): a feature is
// unhealthy while at least one of its keys is refused (or deferred too often,
// or carries a segment override); the webhook gets one post per transition,
// asynchronously and coalesced per feature, never inline in a round.

// Health statuses.
const (
	HealthUnhealthy = "unhealthy"
	HealthHealthy   = "healthy"
)

const healthTitle = "Refused by JoustMania"

// healthBody is the payload of a Flagsmith "Webhook" feature-health provider
// (features/feature_health/providers/webhook/types.py).
type healthBody struct {
	Feature     string       `json:"feature"`
	Environment string       `json:"environment"`
	Status      string       `json:"status"`
	Reason      healthReason `json:"reason"`
}

type healthReason struct {
	TextBlocks []healthText `json:"text_blocks"`
	URLBlocks  []healthURL  `json:"url_blocks"`
}

type healthText struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type healthURL struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type healthPost struct {
	status, text string
}

// health tracks the refused keys per feature and posts the transitions.
type health struct {
	l *Link

	mu      sync.Mutex
	env     string
	keys    map[string]map[Key]string // feature -> key -> reason text
	posted  map[string]string         // feature -> last status queued
	queue   map[string]healthPost     // feature -> latest post not yet sent
	running bool
	// stopped: no drain starts (between stop and resume); ctx ends the
	// running drain's post; wg joins it.
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	// send posts one transition (h.post; a test double in tests).
	send func(ctx context.Context, feature, env string, p healthPost) error
}

func newHealth(l *Link) *health {
	h := &health{l: l, keys: map[string]map[Key]string{}, posted: map[string]string{}, queue: map[string]healthPost{}}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.send = h.post
	return h
}

// resume lets the drain run again after stop and sends what stop left queued.
func (h *health) resume() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		return
	}
	h.stopped = false
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.startLocked()
}

// stop ends the running post and joins the drain goroutine: the poller does
// not return while a post it started is still running. The posts not sent
// stay queued for resume.
func (h *health) stop() {
	h.mu.Lock()
	h.stopped = true
	h.cancel()
	h.mu.Unlock()
	h.wg.Wait()
}

// startLocked starts the drain when posts are queued and none runs.
func (h *health) startLocked() {
	if h.running || h.stopped || len(h.queue) == 0 {
		return
	}
	h.running = true
	h.wg.Add(1)
	go h.drain(h.ctx)
}

// setEnvironment records the environment name the posts carry.
func (h *health) setEnvironment(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.env = name
}

// refuse marks k of feature refused with text; the feature turns unhealthy
// when it was not.
func (h *health) refuse(feature string, k Key, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.keys[feature]
	if m == nil {
		m = map[Key]string{}
		h.keys[feature] = m
	}
	m[k] = text
	h.transitionLocked(feature)
}

// accept clears k of feature; the feature turns healthy when it was the last.
func (h *health) accept(feature string, k Key) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.keys[feature]; m != nil {
		delete(m, k)
		if len(m) == 0 {
			delete(h.keys, feature)
		}
	}
	h.transitionLocked(feature)
}

// prune drops the keys keep rejects (a game that ended, an identity dropped).
func (h *health) prune(keep func(Key) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for feature, m := range h.keys {
		for k := range m {
			if !keep(k) {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			delete(h.keys, feature)
		}
		h.transitionLocked(feature)
	}
}

func (h *health) transitionLocked(feature string) {
	want := HealthHealthy
	text := "the stack runs the Flagsmith value"
	if m := h.keys[feature]; len(m) > 0 {
		want = HealthUnhealthy
		// The text of the first key in a stable order.
		ks := make([]Key, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool { return ks[i].Scope < ks[j].Scope })
		text = m[ks[0]]
	}
	last := h.posted[feature]
	if want == last || (want == HealthHealthy && last == "") {
		return
	}
	h.posted[feature] = want
	if want == HealthHealthy {
		delete(h.posted, feature)
	}
	if h.l.cfg.HealthWebhookURL == "" {
		return
	}
	h.queue[feature] = healthPost{status: want, text: text}
	h.startLocked()
}

// drain sends the queued posts, the latest per feature, then exits; it exits
// early when ctx ends (stop), keeping the post it was sending queued unless a
// newer one replaced it.
func (h *health) drain(ctx context.Context) {
	defer h.wg.Done()
	for {
		h.mu.Lock()
		if len(h.queue) == 0 || ctx.Err() != nil {
			h.running = false
			h.mu.Unlock()
			return
		}
		features := make([]string, 0, len(h.queue))
		for f := range h.queue {
			features = append(features, f)
		}
		sort.Strings(features)
		f := features[0]
		p := h.queue[f]
		delete(h.queue, f)
		env := h.env
		h.mu.Unlock()
		if err := h.send(ctx, f, env, p); err != nil && ctx.Err() != nil {
			h.mu.Lock()
			if _, newer := h.queue[f]; !newer {
				h.queue[f] = p
			}
			h.mu.Unlock()
		}
	}
}

func (h *health) post(ctx context.Context, feature, env string, p healthPost) error {
	body := healthBody{
		Feature: feature, Environment: env, Status: p.status,
		Reason: healthReason{TextBlocks: []healthText{{Title: healthTitle, Text: p.text}}, URLBlocks: []healthURL{}},
	}
	if _, err := h.l.client.PostJSON(ctx, h.l.cfg.HealthWebhookURL, body); err != nil {
		if ctx.Err() != nil {
			return err // stopped: sent again after resume
		}
		h.l.Metrics().RecordHealthPost(ctx, p.status, ResultError)
		h.l.Logger().Warn("flagsmith feature health post failed", "feature", feature, "status", p.status, "reason", Reason(err))
		return err
	}
	h.l.Metrics().RecordHealthPost(ctx, p.status, ResultOK)
	return nil
}
