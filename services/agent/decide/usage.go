package decide

import (
	"context"
	"sync"
)

// Usage totals for the invoke_agent span (telemetry.md §4.2, #2584) are the sum of
// the invocation's chat spans, including calls of a decision that then fell back:
// they were billed. The accumulator rides the context, so it is per invocation.

// Count is a reported token count; OK false means unreported, never summed as 0.
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
	// FinishReasons in choice order; "error" for a call that got no response.
	FinishReasons []string
}

// UsageTotals are an invocation's totals; FinishReasons are positional, never de-duplicated.
type UsageTotals struct {
	Calls                 int
	InputTokens           Count
	OutputTokens          Count
	CacheReadInputTokens  Count
	CacheWriteInputTokens Count
	ReasoningOutputTokens Count
	FinishReasons         []string
}

// Usage accumulates the calls of one invocation; safe for concurrent use.
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
	// Skip empty reasons (some openai-compatible vendors send them).
	for _, r := range c.FinishReasons {
		if r != "" {
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

type usageKey struct{}

// WithUsage returns ctx carrying u; RecordCall under it adds to u.
func WithUsage(ctx context.Context, u *Usage) context.Context {
	return context.WithValue(ctx, usageKey{}, u)
}

// RecordCall adds one call's usage to the accumulator in ctx; a no-op without one.
func RecordCall(ctx context.Context, c CallUsage) {
	if u, ok := ctx.Value(usageKey{}).(*Usage); ok && u != nil {
		u.Add(c)
	}
}

// RequestSettings are a backend's request settings; 0 / nil means not set.
type RequestSettings struct {
	MaxTokens   int
	Temperature *float64
}

// RequestSettingsReporter is implemented by backends whose settings are known before the call.
type RequestSettingsReporter interface {
	RequestSettings() RequestSettings
}
