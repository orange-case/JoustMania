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
}

func newHealth(l *Link) *health {
	return &health{l: l, keys: map[string]map[Key]string{}, posted: map[string]string{}, queue: map[string]healthPost{}}
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
	if !h.running {
		h.running = true
		go h.drain()
	}
}

// drain sends the queued posts, the latest per feature, then exits.
func (h *health) drain() {
	for {
		h.mu.Lock()
		if len(h.queue) == 0 {
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
		h.post(f, env, p)
	}
}

func (h *health) post(feature, env string, p healthPost) {
	body := healthBody{
		Feature: feature, Environment: env, Status: p.status,
		Reason: healthReason{TextBlocks: []healthText{{Title: healthTitle, Text: p.text}}, URLBlocks: []healthURL{}},
	}
	ctx := context.Background()
	if _, err := h.l.client.PostJSON(ctx, h.l.cfg.HealthWebhookURL, body); err != nil {
		h.l.Metrics().RecordHealthPost(ctx, p.status, ResultError)
		h.l.Logger().Warn("flagsmith feature health post failed", "feature", feature, "status", p.status, "reason", Reason(err))
		return
	}
	h.l.Metrics().RecordHealthPost(ctx, p.status, ResultOK)
}
