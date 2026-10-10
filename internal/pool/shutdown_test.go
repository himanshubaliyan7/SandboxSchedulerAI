package pool_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/pool"
)

// Quiesce waits for the creates in flight and starts no new one, also
// when a release frees a slot afterwards.
func TestQuiesceAwaitsCreatesAndStopsRefill(t *testing.T) {
	h := newHarness(t, nil) // warm target 3, refill rate 2
	h.start()
	h.settled(3)
	sb := h.claim()
	h.settled(3) // the claim was refilled

	// Two creates in flight: held at the readiness probe.
	gate := make(chan struct{})
	h.agent.SetReadyHook(func(ctx context.Context, _ string) error {
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	a, b := h.claim(), h.claim()
	eventually(t, "two creates in flight", func() bool { return len(h.inState(core.StateCreating)) == 2 })

	done := make(chan error, 1)
	go func() { done <- h.c.Quiesce(ctxBg()) }()
	select {
	case err := <-done:
		t.Fatalf("Quiesce returned (%v) while creates were in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	// The creates were allowed to finish: WARM, not failed.
	if got := len(h.inState(core.StateWarm)); got != 3 {
		t.Fatalf("%d warm after Quiesce, want 3 (1 left + 2 awaited)", got)
	}
	if n := len(h.inState(core.StateCreating)) + len(h.inState(core.StateFailed)); n != 0 {
		t.Fatalf("%d rows creating or failed after Quiesce", n)
	}
	creates := h.be.Counters().Creates

	// Draining now frees three slots and kicks the refill: nothing starts.
	for _, s := range []core.Sandbox{sb, a, b} {
		if err := h.c.Release(ctxBg(), s.ID, s.Generation, core.ReasonHostDrain); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
	_ = h.c.Refill(ctxBg())
	h.c.Kick()
	time.Sleep(50 * time.Millisecond)
	if got := h.be.Counters().Creates; got != creates {
		t.Fatalf("%d create(s) started after Quiesce", got-creates)
	}
	if n := len(h.inState(core.StateCreating)); n != 0 {
		t.Fatalf("%d CREATING rows after Quiesce", n)
	}
	if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{}); !errors.Is(err, pool.ErrStopped) {
		t.Fatalf("Claim after Quiesce: %v, want ErrStopped", err)
	}
	// The warm ones are still there for the next start to adopt.
	if got, live := len(h.inState(core.StateWarm)), h.be.Len(); got != 3 || live != 3 {
		t.Fatalf("after the drain: %d warm rows, %d instances, want 3 and 3", got, live)
	}
}

// When the budget runs out, the creates in flight are cancelled and
// nothing of them is left: no instance, no run dir, no CREATING row.
func TestQuiesceCancelsCreatesWhenOutOfTime(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget, tm.RefillRate = 2, 2 })
	h.agent.SetReadyHook(func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() })
	h.start()
	eventually(t, "two creates in flight", func() bool {
		return len(h.inState(core.StateCreating)) == 2 && h.be.Len() == 2
	})
	ctx, cancel := context.WithTimeout(ctxBg(), 30*time.Millisecond)
	defer cancel()
	if err := h.c.Quiesce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Quiesce: %v, want the deadline", err)
	}
	if n := h.be.Len(); n != 0 {
		t.Fatalf("%d instance(s) left by cancelled creates", n)
	}
	for _, sb := range h.st.Sandboxes() {
		if sb.State != core.StateDestroyed || sb.Reason != core.ReasonHostDrain {
			t.Errorf("row %s: %s (%s), want DESTROYED (host_drain)", sb.ID, sb.State, sb.Reason)
		}
	}
	if entries, _ := os.ReadDir(h.dir); len(entries) != 0 {
		t.Fatalf("%d run dir(s) left", len(entries))
	}
}

func TestDestroyAllLeavesNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	claimed := h.claim()
	h.settled(3)
	if _, err := h.c.DestroyAll(ctxBg()); err == nil {
		t.Fatal("DestroyAll worked without Quiesce")
	}
	if err := h.c.Quiesce(ctxBg()); err != nil {
		t.Fatal(err)
	}
	// A stray instance and run dir of this host that no row accounts for.
	if err := os.Mkdir(h.c.RunDir("sb-00000000deadbeef"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := h.c.DestroyAll(ctxBg())
	if err != nil || n != 4 {
		t.Fatalf("DestroyAll = %d, %v; want 4 sandboxes", n, err)
	}
	if h.be.Len() != 0 {
		t.Fatalf("%d instance(s) left", h.be.Len())
	}
	for _, sb := range h.st.Sandboxes() {
		if sb.State != core.StateDestroyed {
			t.Errorf("row %s is %s", sb.ID, sb.State)
		}
	}
	if got := h.get(claimed.ID); got.Reason != core.ReasonHostDrain {
		t.Errorf("tenant sandbox reason %q, want host_drain", got.Reason)
	}
	if entries, _ := os.ReadDir(h.dir); len(entries) != 0 {
		t.Fatalf("%d run dir(s) left", len(entries))
	}
}
