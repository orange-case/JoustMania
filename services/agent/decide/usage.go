package decide

import (
	"context"
	"sync"
)

// Usage of one decide invocation (#2584): the GenAI agent span
// (`invoke_agent joustmania-agent`, telemetry.md §4.2) carries the totals of the
// model calls the invocation made. A backend reports each call's usage with
// RecordCall at the point where it stamps that call's own `chat {model}` span, so
// the totals equal the sum of the chat spans, including the calls of a decision
// that then fell back (timeout, error, invalid answer): those calls were made and
// billed.
//
// The accumulator rides the call's context (WithUsage), so it is per invocation:
// two games deciding at once each have their own, and Decide's signature stays as
// it is. It is safe for concurrent use.

// Count is a token count a call reported. OK is false when the call reported no
// such value, so a missing value is never summed as 0.
type Count struct {
	N  int
	OK bool
}

// Tokens is a reported count.
func Tokens(n int) Count { return Count{N: n, OK: true} }

func (c *Count) add(o Count) {
	if o.OK {
		c.N += o.N
		c.OK = true
	}
}

// CallUsage is what one model call reported, the values its chat span carries.
type CallUsage struct {
	InputTokens           Count
	OutputTokens          Count
	CacheReadInputTokens  Count
	CacheWriteInputTokens Count
	ReasoningOutputTokens Count
	// FinishReasons of the call's choices, in order ("error" for a call that got
	// no response, as on its chat span). Empty when the call reported none.
	FinishReasons []string
}

// UsageTotals are an invocation's totals. A Count is OK when at least one call
// reported it. FinishReasons are the distinct reasons in call order.
type UsageTotals struct {
	Calls                 int
	InputTokens           Count
	OutputTokens          Count
	CacheReadInputTokens  Count
	CacheWriteInputTokens Count
	ReasoningOutputTokens Count
	FinishReasons         []string
}

// Usage accumulates the calls of one invocation.
type Usage struct {
	mu sync.Mutex
	t  UsageTotals
}

// Add sums one call into the totals.
func (u *Usage) Add(c CallUsage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.t.Calls++
	u.t.InputTokens.add(c.InputTokens)
	u.t.OutputTokens.add(c.OutputTokens)
	u.t.CacheReadInputTokens.add(c.CacheReadInputTokens)
	u.t.CacheWriteInputTokens.add(c.CacheWriteInputTokens)
	u.t.ReasoningOutputTokens.add(c.ReasoningOutputTokens)
	for _, r := range c.FinishReasons {
		if r != "" && !containsString(u.t.FinishReasons, r) {
			u.t.FinishReasons = append(u.t.FinishReasons, r)
		}
	}
}

// Totals is a copy of the totals so far.
func (u *Usage) Totals() UsageTotals {
	u.mu.Lock()
	defer u.mu.Unlock()
	t := u.t
	t.FinishReasons = append([]string(nil), u.t.FinishReasons...)
	return t
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

type usageKey struct{}

// WithUsage returns ctx carrying u; RecordCall under it adds to u.
func WithUsage(ctx context.Context, u *Usage) context.Context {
	return context.WithValue(ctx, usageKey{}, u)
}

// RecordCall adds one model call's usage to the invocation's accumulator in ctx.
// A no-op without one (a backend called outside the decide hook, e.g. in tests).
func RecordCall(ctx context.Context, c CallUsage) {
	if u, ok := ctx.Value(usageKey{}).(*Usage); ok && u != nil {
		u.Add(c)
	}
}

// RequestSettings are a backend's request settings that the invoke_agent span
// carries from its start (gen_ai.request.max_tokens, gen_ai.request.temperature).
// MaxTokens 0 and a nil Temperature mean the backend does not set them.
type RequestSettings struct {
	MaxTokens   int
	Temperature *float64
}

// RequestSettingsReporter is implemented by a backend whose request settings are
// known before the call (claude, openai). Jev sets neither.
type RequestSettingsReporter interface {
	RequestSettings() RequestSettings
}
