package pool_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/pool/pooltest"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/fake"
)

// syncBuffer is a log sink that is safe to read while the controller's
// goroutines write to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestReconcilerActsOnlyOnItsOwnHostID: an instance labelled with another
// host id, and one with no host id at all, look like orphans (no row
// here) and must still never be destroyed, however often the reconciler
// runs. The controller's own orphan is reaped as before.
func TestReconcilerActsOnlyOnItsOwnHostID(t *testing.T) {
	h := newHarness(t, nil)
	var logs syncBuffer
	c, err := pool.New(pool.Config{
		Store: h.st, Backend: h.be, Agent: h.agent, Egress: h.egress, Clock: h.clock,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		HostID: host, AgentEpoch: "epoch-1", RunDirBase: h.dir, AgentBinaryPath: "/opt/sbx-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	h.c = c
	h.start()
	h.settled(3)
	warm := h.inState(core.StateWarm)

	mine := h.be.AddInstance(runtime.Instance{HostID: host, SandboxID: "sb-mine", AgentEpoch: "epoch-0", Running: true})
	foreign := h.be.AddInstance(runtime.Instance{HostID: "host-b", SandboxID: "sb-theirs", AgentEpoch: "epoch-7", Running: true})
	legacy := h.be.AddInstance(runtime.Instance{SandboxID: "sb-legacy", AgentEpoch: "epoch-0", Running: true})
	// Another host's instance that even claims one of our sandbox ids,
	// with the right generation, and one that is not running.
	twin := h.be.AddInstance(runtime.Instance{HostID: "host-b", SandboxID: warm[0].ID, AgentEpoch: "epoch-1", Running: true})
	dead := h.be.AddInstance(runtime.Instance{HostID: "host-b", SandboxID: "sb-dead", Running: false, ExitCode: 137, OOMKilled: true})

	for i := 0; i < 5; i++ {
		if err := h.c.Reconcile(ctxBg()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if !h.be.WasDestroyed(mine.ID) {
		t.Error("the controller's own orphan was not destroyed")
	}
	for name, inst := range map[string]runtime.Instance{"other host": foreign, "no host label": legacy, "other host, our sandbox id": twin, "other host, exited": dead} {
		if h.be.WasDestroyed(inst.ID) {
			t.Errorf("instance (%s) was destroyed by a controller that does not own it", name)
		}
		if _, err := h.be.Inspect(ctxBg(), inst.ID); err != nil {
			t.Errorf("instance (%s) is gone: %v", name, err)
		}
	}
	if m := h.c.Metrics(); m.OrphansDestroyed != 1 || m.InstancesLost != 0 {
		t.Errorf("metrics: orphans=%d lost=%d, want 1 and 0", m.OrphansDestroyed, m.InstancesLost)
	}
	for _, id := range []string{"sb-theirs", "sb-legacy", "sb-dead"} {
		if ev := h.st.Events(id); len(ev) != 0 {
			t.Errorf("events were recorded for %s, which is not ours: %+v", id, ev)
		}
	}
	// Our warm sandbox whose id the twin borrowed is undisturbed.
	if got := h.get(warm[0].ID); got.State != core.StateWarm || h.be.WasDestroyed(warm[0].InstanceID) {
		t.Errorf("warm sandbox was disturbed by another host's instance with its id: %+v", got)
	}
	h.settled(3)

	// Reported once per instance, not once per pass: debug for another
	// host id, a warning for a missing one.
	out := logs.String()
	for inst, want := range map[string]string{foreign.ID: "level=DEBUG", twin.ID: "level=DEBUG", dead.ID: "level=DEBUG", legacy.ID: "level=WARN"} {
		n := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "leaving it alone") && strings.Contains(line, "instance="+inst+" ") {
				n++
				if !strings.Contains(line, want) {
					t.Errorf("log line for %s is not %s: %s", inst, want, line)
				}
			}
		}
		if n != 1 {
			t.Errorf("instance %s was reported %d times, want once:\n%s", inst, n, out)
		}
	}
}

// TestTeardownByLabelIgnoresOtherHosts: a row that never recorded an
// instance id is torn down by looking for its sandbox id among the
// instances. Another host's instance with the same sandbox id is not it.
func TestTeardownByLabelIgnoresOtherHosts(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget = 0 })
	now := h.clock.Now()
	deadline := now.Add(core.CreatingDeadline)
	h.st.InsertRaw(core.Sandbox{ID: "sb-stuck", TemplateID: tmplID, TemplateVersion: 1, HostID: host,
		State: core.StateCreating, CreatedAt: now, DeadlineAt: &deadline})
	half := h.be.AddInstance(runtime.Instance{HostID: host, SandboxID: "sb-stuck", Running: true})
	theirs := h.be.AddInstance(runtime.Instance{HostID: "host-b", SandboxID: "sb-stuck", Running: true})
	unlabelled := h.be.AddInstance(runtime.Instance{SandboxID: "sb-stuck", Running: true})

	h.clock.Advance(core.CreatingDeadline + time.Second)
	if err := h.c.Sweep(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if got := h.get("sb-stuck"); got.State != core.StateDestroyed {
		t.Fatalf("row: %+v", got)
	}
	if !h.be.WasDestroyed(half.ID) {
		t.Error("the half-created instance of this host was not destroyed")
	}
	if h.be.WasDestroyed(theirs.ID) || h.be.WasDestroyed(unlabelled.ID) {
		t.Error("teardown by label destroyed an instance of another host id")
	}
}

// TestTwoControllersShareOneBackend runs two controllers with different
// host ids and separate stores against one backend, the way two servers
// of one Podman user do. Each fills and keeps its own pool, reaps its own
// orphans and loses its own vanished instances; neither ever destroys an
// instance of the other.
func TestTwoControllersShareOneBackend(t *testing.T) {
	be := fake.New()
	type side struct {
		host  string
		clock *pooltest.FakeClock
		st    *pooltest.MemStore
		c     *pool.Controller
	}
	mk := func(hostID string) *side {
		s := &side{host: hostID, clock: pooltest.NewFakeClock()}
		s.st = pooltest.NewMemStore(s.clock.Now)
		s.st.PutTemplate(baseTemplate())
		s.st.PutTenant(tenant, 10)
		c, err := pool.New(pool.Config{
			Store: s.st, Backend: be, Agent: &pooltest.FakeAgent{}, Egress: &pooltest.FakeEgress{}, Clock: s.clock,
			Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
			HostID: hostID, RunDirBase: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Stop)
		s.c = c
		return s
	}
	a, b := mk("host-a"), mk("host-b")
	count := func(s *side, st core.State) (n int) {
		for _, sb := range s.st.Sandboxes() {
			if sb.State == st {
				n++
			}
		}
		return n
	}
	settled := func(s *side, warm int) {
		t.Helper()
		eventually(t, s.host+" to settle", func() bool {
			return count(s, core.StateWarm) == warm && count(s, core.StateCreating) == 0 &&
				count(s, core.StateRecycling) == 0 && count(s, core.StateFailed) == 0
		})
	}
	owned := func(hostID string) map[string]bool {
		list, err := be.List(ctxBg())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, inst := range list {
			if inst.HostID == hostID {
				out[inst.ID] = true
			}
		}
		return out
	}

	// B starts while A's pool is already full: B's start-up reconcile sees
	// three instances it has no rows for.
	if err := a.c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled(a, 3)
	aInstances := owned("host-a")
	if err := b.c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled(b, 3)
	bInstances := owned("host-b")
	if len(aInstances) != 3 || len(bInstances) != 3 {
		t.Fatalf("instances: host-a %d, host-b %d, want 3 and 3", len(aInstances), len(bInstances))
	}

	// Each has an orphan; A also loses one warm instance.
	orphanA := be.AddInstance(runtime.Instance{HostID: "host-a", SandboxID: "sb-orphan-a", Running: true})
	orphanB := be.AddInstance(runtime.Instance{HostID: "host-b", SandboxID: "sb-orphan-b", Running: true})
	var lostA core.Sandbox
	for _, sb := range a.st.Sandboxes() {
		if sb.State == core.StateWarm {
			lostA = sb
			break
		}
	}
	be.Vanish(lostA.InstanceID)
	delete(aInstances, lostA.InstanceID)

	for i := 0; i < 5; i++ {
		for _, s := range []*side{a, b} {
			if err := s.c.Reconcile(ctxBg()); err != nil {
				t.Fatalf("%s: Reconcile: %v", s.host, err)
			}
			if err := s.c.Sweep(ctxBg()); err != nil {
				t.Fatalf("%s: Sweep: %v", s.host, err)
			}
		}
	}
	settled(a, 3)
	settled(b, 3)
	if !be.WasDestroyed(orphanA.ID) || !be.WasDestroyed(orphanB.ID) {
		t.Errorf("orphans reaped: host-a %v, host-b %v, want both", be.WasDestroyed(orphanA.ID), be.WasDestroyed(orphanB.ID))
	}
	if ma, mb := a.c.Metrics(), b.c.Metrics(); ma.OrphansDestroyed != 1 || mb.OrphansDestroyed != 1 || ma.InstancesLost != 1 || mb.InstancesLost != 0 {
		t.Errorf("host-a orphans=%d lost=%d (want 1, 1); host-b orphans=%d lost=%d (want 1, 0)",
			ma.OrphansDestroyed, ma.InstancesLost, mb.OrphansDestroyed, mb.InstancesLost)
	}
	for id := range aInstances {
		if be.WasDestroyed(id) {
			t.Errorf("host-a's warm instance %s was destroyed", id)
		}
	}
	for id := range bInstances {
		if be.WasDestroyed(id) {
			t.Errorf("host-b's warm instance %s was destroyed", id)
		}
	}
	if got := a.st.Events("sb-orphan-b"); len(got) != 0 {
		t.Errorf("host-a recorded events for host-b's orphan: %+v", got)
	}

	// Claim and release on both; then stop A and check B is untouched by
	// A's restart (new agent epoch, start-up reconcile).
	for _, s := range []*side{a, b} {
		sb, err := s.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{})
		if err != nil {
			t.Fatalf("%s: Claim: %v", s.host, err)
		}
		if err := s.c.Release(ctxBg(), sb.ID, sb.Generation, ""); err != nil {
			t.Fatalf("%s: Release: %v", s.host, err)
		}
		settled(s, 3)
	}
	bBefore := owned("host-b")
	a.c.Stop()
	a2, err := pool.New(pool.Config{
		Store: a.st, Backend: be, Agent: &pooltest.FakeAgent{}, Egress: &pooltest.FakeEgress{}, Clock: a.clock,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		HostID: "host-a", RunDirBase: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a2.Stop)
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := a2.Metrics(); m.Adopted != 3 || m.OrphansDestroyed != 0 {
		t.Errorf("restarted host-a: adopted=%d orphans=%d, want 3 and 0", m.Adopted, m.OrphansDestroyed)
	}
	bAfter := owned("host-b")
	if len(bAfter) != 3 {
		t.Fatalf("host-b has %d instances after host-a restarted, want 3", len(bAfter))
	}
	for id := range bBefore {
		if !bAfter[id] {
			t.Errorf("host-b's instance %s disappeared when host-a restarted", id)
		}
	}
}

// TestCreateIsRetriedBeforeItCounts: a create that fails twice and then
// succeeds ends WARM. Nothing is failed, the breaker hears nothing, and
// what the failed attempts left behind is gone.
func TestCreateIsRetriedBeforeItCounts(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate, tm.WarmTarget = 1, 1 })
	h.attempts = 3
	h.c = h.controller("epoch-1")
	h.be.FailNextCreates(2, errors.New("crun: write to /proc/sys/net/ipv4/ping_group_range: Invalid argument"))
	h.start()
	h.advanceUntil("the retried create to be warm", func() bool { return len(h.inState(core.StateWarm)) == 1 })

	if all := h.st.Sandboxes(); len(all) != 1 {
		t.Fatalf("%d rows, want the one that was retried: %+v", len(all), all)
	}
	sb := h.inState(core.StateWarm)[0]
	for _, e := range h.st.Events(sb.ID) {
		if e.ToState == core.StateFailed || e.ToState == core.StateRecycling {
			t.Fatalf("a retried create left a failure in the history: %+v", h.st.Events(sb.ID))
		}
	}
	m := h.c.Metrics()
	if m.Creates != 1 || m.CreateFailures != 0 || m.CreateRetries != 2 {
		t.Errorf("creates=%d failures=%d retries=%d, want 1, 0, 2", m.Creates, m.CreateFailures, m.CreateRetries)
	}
	if tm := m.Templates[tmplID]; tm.ConsecutiveCreateFailures != 0 || tm.BreakerOpen {
		t.Errorf("the breaker counted a retried create: %+v", tm)
	}
	if c := h.be.Counters(); c.Creates != 3 {
		t.Errorf("backend saw %d creates, want 3", c.Creates)
	}
	if inst, ok := h.be.BySandbox(sb.ID); !ok || inst.ID != sb.InstanceID || h.be.Len() != 1 {
		t.Errorf("row instance %q, backend has %+v (%d instances)", sb.InstanceID, inst, h.be.Len())
	}
}

// TestRetryCleansUpTheFailedInstance: when the instance came up but its
// agent did not, the instance is destroyed and the run dir emptied before
// the next attempt, which gets a new instance.
func TestRetryCleansUpTheFailedInstance(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate, tm.WarmTarget = 1, 1 })
	h.attempts = 3
	h.c = h.controller("epoch-1")
	var (
		mu    sync.Mutex
		calls int
		stale []bool
	)
	h.agent.SetReadyHook(func(ctx context.Context, runDir string) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		// The marker of an earlier attempt must not be visible to a later one.
		_, err := os.Stat(runDir + "/marker")
		stale = append(stale, err == nil)
		if calls == 1 {
			if err := os.WriteFile(runDir+"/marker", nil, 0o600); err != nil {
				t.Error(err)
			}
			return errors.New("agent did not come up")
		}
		return nil
	})
	h.start()
	h.advanceUntil("the second attempt to be warm", func() bool { return len(h.inState(core.StateWarm)) == 1 })

	sb := h.inState(core.StateWarm)[0]
	c := h.be.Counters()
	if c.Creates != 2 || c.DestroyedLive != 1 || h.be.Len() != 1 {
		t.Fatalf("backend: creates=%d destroyed=%d live=%d, want 2, 1, 1", c.Creates, c.DestroyedLive, h.be.Len())
	}
	if inst, _ := h.be.BySandbox(sb.ID); inst.ID != sb.InstanceID || h.be.WasDestroyed(sb.InstanceID) {
		t.Errorf("the warm row does not point at the surviving instance: row %q, backend %q", sb.InstanceID, inst.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stale) != 2 || stale[0] || stale[1] {
		t.Errorf("run dir was not fresh for every attempt: %v", stale)
	}
}

// TestCreateRetriesAreBounded: a create that keeps failing is given up
// after CreateAttempts attempts, and only then fails the row and counts
// once against the breaker.
func TestCreateRetriesAreBounded(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate, tm.WarmTarget = 1, 1 })
	h.attempts = 3
	h.c = h.controller("epoch-1")
	h.be.SetCreateError(errors.New("image not found"))
	h.start()
	// Threshold 3: three rows fail, each after three attempts, then the breaker opens.
	h.advanceUntil("three rows to fail", func() bool {
		return len(h.inState(core.StateDestroyed)) == 3 && h.c.Metrics().Templates[tmplID].BreakerOpen
	})
	m := h.c.Metrics()
	if c := h.be.Counters(); c.Creates != 9 || m.CreateFailures != 3 || m.CreateRetries != 6 {
		t.Errorf("backend creates=%d, failures=%d, retries=%d, want 9, 3, 6", c.Creates, m.CreateFailures, m.CreateRetries)
	}
	if tm := m.Templates[tmplID]; tm.ConsecutiveCreateFailures != 3 {
		t.Errorf("breaker counted %d failures, want one per row (3)", tm.ConsecutiveCreateFailures)
	}
	for _, sb := range h.st.Sandboxes() {
		if sb.Reason != core.ReasonCreateFailed || !hasEvent(h.st.Events(sb.ID), core.StateFailed, core.ReasonCreateFailed) {
			t.Errorf("row %s: %+v", sb.ID, sb)
		}
		if _, err := os.Stat(h.c.RunDir(sb.ID)); !os.IsNotExist(err) {
			t.Errorf("run dir of %s remains: %v", sb.ID, err)
		}
	}
	if h.be.Len() != 0 {
		t.Errorf("%d instances leaked", h.be.Len())
	}
}

// TestNoRetryAfterTheDeadline: a create whose deadline has passed is not
// retried; the row fails with deadline_exceeded.
func TestNoRetryAfterTheDeadline(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate, tm.WarmTarget = 1, 1 })
	h.attempts = 3
	h.c = h.controller("epoch-1")
	h.be.HangNextCreates(1)
	h.start()
	eventually(t, "create to start", func() bool { return h.be.Counters().Creates == 1 })
	hung := h.st.Sandboxes()[0]
	h.clock.Advance(core.CreatingDeadline + time.Second)
	eventually(t, "hung create to be destroyed", func() bool { return h.get(hung.ID).State == core.StateDestroyed })
	if got := h.get(hung.ID); got.Reason != core.ReasonDeadlineExceeded {
		t.Errorf("reason = %q, want deadline_exceeded", got.Reason)
	}
	if m := h.c.Metrics(); m.CreateRetries != 0 {
		t.Errorf("a create past its deadline was retried %d times", m.CreateRetries)
	}
}

// advanceUntil moves the fake clock in small steps until cond holds, for
// code that waits on short clock timers (the create retry backoff).
func (h *harness) advanceUntil(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for: %s", what)
		}
		h.clock.Advance(time.Millisecond)
		time.Sleep(time.Millisecond)
	}
}

// A leftover host process (a conmon whose container Podman has forgotten)
// is ended by the reconciler only when the store says the sandbox was
// this scheduler's: a row with this host id, live or destroyed.
func TestReconcilerReapsOnlyItsOwnLeftovers(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	sb := h.claim()
	if err := h.c.Release(ctxBg(), sb.ID, sb.Generation, core.ReasonReleased); err != nil {
		t.Fatal(err)
	}
	if got := h.get(sb.ID); got.State != core.StateDestroyed {
		t.Fatalf("released sandbox is %s", got.State)
	}
	h.be.AddLeftover(sb.ID)       // ours: the row is kept after the sandbox is destroyed
	h.be.AddLeftover("sb-nobody") // no row at all: another database, another scheduler
	before := h.c.Metrics().OrphansDestroyed
	if err := h.c.Reconcile(ctxBg()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if left := h.be.Leftovers(); len(left) != 1 || left[0] != "sb-nobody" {
		t.Errorf("leftovers after the pass: %v, want only the one nobody owns", left)
	}
	if got := h.c.Metrics().OrphansDestroyed - before; got != 1 {
		t.Errorf("orphans destroyed went up by %d, want 1", got)
	}
}
