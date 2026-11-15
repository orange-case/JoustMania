package flagsmith

import (
	"bufio"
	"context"
	"strings"
	"time"
)

// The optional realtime stream (FLAGSMITH_REALTIME_URL, architecture note
// §4.2 step 1): any SSE data line wakes the poller for an early pull. The
// stream is a hint only: a failure never takes the link down, it reconnects
// after its own backoff (1, 2, 4 ... 60 s) and polling goes on.
//
// UNVERIFIED against the hosted service: the subscriber path below follows
// Flagsmith's SSE service (sse/environments/{key}/stream).

// maxSSELine bounds one line of the stream.
const maxSSELine = 64 << 10

// realtimeSleep waits between reconnects; a variable so a test can shorten it.
var realtimeSleep = sleepCtx

func (p *puller) realtimeURL() string {
	base := p.l.cfg.RealtimeURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + "sse/environments/" + p.l.cfg.EnvironmentKey + "/stream"
}

// realtime reads the stream until ctx ends.
func (p *puller) realtime(ctx context.Context) {
	var backoff time.Duration
	for ctx.Err() == nil {
		if p.readStream(ctx) {
			backoff = 0
		}
		if ctx.Err() != nil {
			return
		}
		backoff = nextBackoff(backoff)
		if realtimeSleep(ctx, backoff) != nil {
			return
		}
	}
}

// readStream opens the stream once and wakes the poller on every data line.
// It reports whether the stream was open (the backoff starts over).
func (p *puller) readStream(ctx context.Context) bool {
	body, err := p.l.client.Stream(ctx, p.realtimeURL(), map[string]string{"Accept": "text/event-stream"})
	if err != nil {
		if ctx.Err() == nil {
			p.l.Logger().Debug("flagsmith realtime stream not open", "reason", Reason(err))
		}
		return false
	}
	defer body.Close()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), maxSSELine)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			p.l.Wake()
		}
	}
	return true
}
