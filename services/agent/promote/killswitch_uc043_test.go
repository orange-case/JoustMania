package promote

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joustmania/agent/flagservice"
	"github.com/joustmania/agent/flagservice/fake"
)

// killswitch_uc043_test.go covers the stateless kill-switch revert against the
// flag service (UC-043 A1, UC-045 step 5, #1704): it reverts a promotion written
// before an agent restart, it never touches another writer's overlay, and a
// promotion write racing the kill switch is reverted on the same tick, with one
// reported revert.

func killSwitchProposal() (p Evidence, flag string) {
	ev := testEvidence()
	ev.RealDefaultValue = 3.0
	return ev, promotedFlag
}

func deleteCalls(svc *fake.Service) int {
	n := 0
	for _, c := range svc.Calls {
		if strings.HasPrefix(c, "DELETE ") {
			n++
		}
	}
	return n
}

// UC: UC-043 A1
// UC: UC-045 main
func TestUC043_A1_KillSwitchRevertsFromFlagService(t *testing.T) {
	ctx := context.Background()
	on := Config{Mode: ModeAutonomous, Enabled: true}

	t.Run("UC-043/A1 a promotion written before an agent restart is reverted", func(t *testing.T) {
		svc := newPromotionService(t)
		// The previous agent run wrote the promotion overlay; this run has no memory of it.
		if err := writerForTest(t, svc).SetRealDefault(ctx, promotedFlag, 3.0); err != nil {
			t.Fatalf("precondition: %v", err)
		}
		w := writerForTest(t, svc)
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, w, w)

		got := p.RevertOnKillSwitch(ctx)

		if len(got) != 1 || got[0].FlagKey != promotedFlag || got[0].Outcome != OutcomeReverted || got[0].Reason != KillSwitchReason {
			t.Fatalf("kill switch after restart = %+v, want one reverted / kill_switch for %s", got, promotedFlag)
		}
		if _, ok := svc.RealValue("game", promotedFlag); ok {
			t.Fatal("the promotion overlay survived the kill switch")
		}
		if again := p.RevertOnKillSwitch(ctx); len(again) != 0 {
			t.Fatalf("second tick reverted %d flags, want 0", len(again))
		}
	})

	t.Run("UC-043/A1 another writer's real overlay is never touched", func(t *testing.T) {
		svc := newPromotionService(t)
		if _, err := svc.Put(ctx, "game", promotedFlag, flagservice.Write{
			Value: 3.0, Scope: flagservice.Scope{GameKind: "real"}, Writer: "admin",
		}); err != nil {
			t.Fatalf("precondition: %v", err)
		}
		w := writerForTest(t, svc)
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, w, w)

		got := p.RevertOnKillSwitch(ctx)
		if err := w.RevertRealDefault(ctx, promotedFlag); err != nil {
			t.Fatalf("RevertRealDefault: %v", err)
		}

		if len(got) != 0 || deleteCalls(svc) != 0 {
			t.Fatalf("kill switch reported %v and issued %d deletes, want none for an admin overlay", got, deleteCalls(svc))
		}
		if _, ok := svc.RealValue("game", promotedFlag); !ok {
			t.Fatal("the admin real overlay was removed")
		}
	})

	t.Run("UC-043/A1 a promotion whose config predates the kill switch does not write", func(t *testing.T) {
		svc := newPromotionService(t)
		w := writerForTest(t, svc)
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, w, w)
		ev, flag := killSwitchProposal()
		prop := promoteProposal
		prop.FlagKey = flag
		cfg := on
		cfg.killEpoch, cfg.epochKnown = p.KillSwitchEpoch(), true // read with enabled still on

		p.RevertOnKillSwitch(ctx) // the switch flips; enabled may be back on by the write
		res := p.Promote(ctx, prop, promoteVerdict, cfg, ev)

		if res.Outcome != OutcomeDiscarded || len(svc.Calls) != 0 {
			t.Fatalf("stale promotion: outcome=%q calls=%v, want discarded and no write", res.Outcome, svc.Calls)
		}
	})

	t.Run("UC-043/A1 a write in flight during the kill switch is reverted once", func(t *testing.T) {
		svc := newPromotionService(t)
		w := writerForTest(t, svc)
		blocking := &blockingRealDefault{inner: w, entered: make(chan struct{}), release: make(chan struct{})}
		t.Cleanup(blocking.releaseNow) // never leave the blocked write hanging
		p, _ := promoterWithRecorder(t, &recordingGitHub{}, blocking, w)
		ev, flag := killSwitchProposal()
		prop := promoteProposal
		prop.FlagKey = flag

		var wg sync.WaitGroup
		var res Result
		var reported []KillSwitchRevert
		wg.Add(1)
		go func() {
			defer wg.Done()
			res = p.Promote(ctx, prop, promoteVerdict, on, ev)
		}()
		awaitEntered(t, blocking.entered)
		killDone := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(killDone)
			reported = p.RevertOnKillSwitch(ctx)
		}()
		// The kill switch must wait for the write; let the PUT land only once it has
		// either finished (no serialisation: the bug) or had time to block on it.
		select {
		case <-killDone:
		case <-time.After(200 * time.Millisecond):
		}
		blocking.releaseNow()
		wg.Wait()

		if _, ok := svc.RealValue("game", promotedFlag); ok {
			t.Fatal("the in-flight promotion survived the kill switch")
		}
		reverts := 0
		if res.Outcome == OutcomeReverted && res.Reason == KillSwitchReason {
			reverts++
		}
		for _, rv := range reported {
			if rv.Outcome == OutcomeReverted {
				reverts++
			}
		}
		if reverts != 1 || deleteCalls(svc) != 1 {
			t.Fatalf("reported reverts=%d (result %+v, kill switch %+v), deletes=%d; want exactly one of each", reverts, res, reported, deleteCalls(svc))
		}
	})
}

// blockingRealDefault holds the first SetRealDefault until release is closed.
type blockingRealDefault struct {
	inner   RealDefaultWriter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func (b *blockingRealDefault) releaseNow() { b.relOnce.Do(func() { close(b.release) }) }

func (b *blockingRealDefault) SetRealDefault(ctx context.Context, flag string, value any) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.inner.SetRealDefault(ctx, flag, value)
}

// awaitEntered fails the test instead of hanging when the blocked call is never reached.
func awaitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write never reached the flag service")
	}
}
