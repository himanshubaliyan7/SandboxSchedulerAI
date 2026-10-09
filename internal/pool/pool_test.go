package pool_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/pool"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/pool/pooltest"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime/fake"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

const (
	tmplID = "py"
	tenant = "t1"
	host   = "host-a"
)

type harness struct {
	t      *testing.T
	clock  *pooltest.FakeClock
	st     *pooltest.MemStore
	be     *fake.Backend
	agent  *pooltest.FakeAgent
	egress *pooltest.FakeEgress
	dir    string
	c      *pool.Controller
}

func baseTemplate() core.Template {
	return core.Template{
		ID: tmplID, Version: 1, Image: "img", Runtime: "crun",
		Resources:  core.Resources{CPUs: 1, MemoryBytes: 256 << 20, PidsLimit: 128, WorkdirBytes: 64 << 20, TmpBytes: 32 << 20, NofileLimit: 1024},
		Network:    core.NetworkAllowlist,
		Timeouts:   core.Timeouts{Idle: 10 * time.Minute, MaxLifetime: time.Hour, WarmTTL: 30 * time.Minute},
		WarmTarget: 3, MaxTotal: 10, RefillRate: 2,
	}
}

// newHarness builds a controller on a fake clock, an in-memory store and
// the fake backend. It does not start it.
func newHarness(t *testing.T, edit func(*core.Template)) *harness {
	t.Helper()
	clock := pooltest.NewFakeClock()
	h := &harness{t: t, clock: clock, st: pooltest.NewMemStore(clock.Now), be: fake.New(),
		agent: &pooltest.FakeAgent{}, egress: &pooltest.FakeEgress{}, dir: t.TempDir()}
	tmpl := baseTemplate()
	if edit != nil {
		edit(&tmpl)
	}
	h.st.PutTemplate(tmpl)
	h.st.PutTenant(tenant, 10)
	h.c = h.controller("epoch-1")
	return h
}

func (h *harness) controller(epoch string) *pool.Controller {
	h.t.Helper()
	c, err := pool.New(pool.Config{
		Store: h.st, Backend: h.be, Agent: h.agent, Egress: h.egress, Clock: h.clock,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HostID: host, AgentEpoch: epoch, RunDirBase: h.dir, AgentBinaryPath: "/opt/sbx-agent",
	})
	if err != nil {
		h.t.Fatalf("pool.New: %v", err)
	}
	h.t.Cleanup(c.Stop)
	return c
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.c.Start(context.Background()); err != nil {
		h.t.Fatalf("Start: %v", err)
	}
}

func (h *harness) inState(s core.State) []core.Sandbox {
	var out []core.Sandbox
	for _, sb := range h.st.Sandboxes() {
		if sb.State == s {
			out = append(out, sb)
		}
	}
	return out
}

func (h *harness) get(id string) core.Sandbox {
	h.t.Helper()
	sb, err := h.st.GetSandbox(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetSandbox(%s): %v", id, err)
	}
	return sb
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settled waits until exactly n sandboxes are WARM and nothing is in a
// transient or failed state.
func (h *harness) settled(n int) {
	h.t.Helper()
	eventually(h.t, "pool to settle", func() bool {
		return len(h.inState(core.StateWarm)) == n && len(h.inState(core.StateCreating)) == 0 &&
			len(h.inState(core.StateRecycling)) == 0 && len(h.inState(core.StateFailed)) == 0
	})
}

func (h *harness) claim() core.Sandbox {
	h.t.Helper()
	sb, err := h.c.Claim(context.Background(), tenant, tmplID, pool.ClaimOptions{})
	if err != nil {
		h.t.Fatalf("Claim: %v", err)
	}
	return sb
}

// neverRewarmed fails the test if a sandbox's history shows WARM after
// its first assignment.
func (h *harness) neverRewarmed(id string) {
	h.t.Helper()
	assigned := false
	for _, e := range h.st.Events(id) {
		if e.ToState == core.StateAssigned {
			assigned = true
		}
		if assigned && e.ToState == core.StateWarm {
			h.t.Fatalf("sandbox %s went back to WARM after assignment: %+v", id, h.st.Events(id))
		}
	}
	if !assigned {
		h.t.Fatalf("sandbox %s was never assigned", id)
	}
}

func hasEvent(events []store.Event, to core.State, reason core.TerminationReason) bool {
	for _, e := range events {
		if e.ToState == to && e.Reason == reason {
			return true
		}
	}
	return false
}

func ctxBg() context.Context { return context.Background() }

// ---------------------------------------------------------------------------

func TestPoolFillsToTarget(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)

	if n := h.be.Len(); n != 3 {
		t.Fatalf("backend has %d instances, want 3", n)
	}
	for _, sb := range h.inState(core.StateWarm) {
		if sb.InstanceID == "" || sb.Generation != 0 || sb.TenantID != "" || sb.DeadlineAt != nil || sb.HostID != host {
			t.Fatalf("warm row is wrong: %+v", sb)
		}
		inst, ok := h.be.BySandbox(sb.ID)
		if !ok || inst.ID != sb.InstanceID || inst.AgentEpoch != "epoch-1" || inst.Generation != 0 {
			t.Fatalf("instance of %s is wrong: %+v", sb.ID, inst)
		}
		fi, err := os.Stat(h.c.RunDir(sb.ID))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("run dir of %s: %v, mode %v", sb.ID, err, fi)
		}
	}
	if got := h.agent.ReadyCalls(); got != 3 {
		t.Fatalf("readiness was checked %d times, want once per sandbox", got)
	}
	// Level-triggered: more passes at target create nothing.
	for i := 0; i < 5; i++ {
		if err := h.c.Refill(ctxBg()); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.Advance(3 * time.Second)
	h.settled(3)
	if c := h.be.Counters(); c.Creates != 3 {
		t.Fatalf("%d creates for a target of 3", c.Creates)
	}
	m := h.c.Metrics()
	if tm := m.Templates[tmplID]; tm.Warm != 3 || tm.WarmTarget != 3 || m.Creates != 3 {
		t.Fatalf("metrics: %+v creates=%d", tm, m.Creates)
	}
}

func TestRefillRespectsMaxTotalAndRefillRate(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget, tm.MaxTotal, tm.RefillRate = 5, 4, 2 })
	h.be.SetCreateHang(true)
	if err := h.c.Refill(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Refill(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.inState(core.StateCreating)); n != 2 {
		t.Fatalf("%d concurrent creates with RefillRate 2", n)
	}
	h.be.SetCreateHang(false)
	h.clock.Advance(core.CreatingDeadline + time.Second) // fail the two hung creates
	h.start()
	h.settled(4) // MaxTotal 4 caps a target of 5
	for i := 0; i < 3; i++ {
		_ = h.c.Refill(ctxBg())
	}
	h.settled(4)
}

func TestClaimTriggersRefill(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	oldest := h.inState(core.StateWarm)[0]

	sb, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{
		Env:             map[string]string{"SESSION": "abc"},
		Network:         &core.NetworkPolicy{AllowDomains: []string{"pypi.org"}},
		AccessTokenHash: []byte("hash"),
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if sb.ID != oldest.ID {
		t.Fatalf("claimed %s, want the oldest warm sandbox %s", sb.ID, oldest.ID)
	}
	if sb.State != core.StateRunning || sb.TenantID != tenant || sb.Generation != 1 || sb.DeadlineAt != nil ||
		sb.AssignedAt == nil || sb.ExpiresAt == nil || !sb.ExpiresAt.Equal(h.clock.Now().Add(time.Hour)) {
		t.Fatalf("claimed row is wrong: %+v", sb)
	}
	inits := h.agent.Inits()
	if len(inits) != 1 {
		t.Fatalf("%d agent inits", len(inits))
	}
	if in := inits[0]; in.HostRunDir != h.c.RunDir(sb.ID) || in.Req.SandboxID != sb.ID || in.Req.Generation != 1 ||
		in.Req.Env["SESSION"] != "abc" || !in.Req.EgressProxy {
		t.Fatalf("init request is wrong: %+v", in)
	}
	att, ok := h.egress.Attached(sb.ID)
	if !ok || att.SocketPath != filepath.Join(h.c.RunDir(sb.ID), runtime.EgressSocket) ||
		att.Policy.Mode != core.NetworkAllowlist || len(att.Policy.AllowDomains) != 1 {
		t.Fatalf("egress attachment is wrong: %+v (attached=%v)", att, ok)
	}
	if p := h.st.NetworkPolicy(sb.ID); p == nil || p.Mode != core.NetworkAllowlist {
		t.Fatalf("policy was not stored: %+v", p)
	}

	// The claimed sandbox is replaced without any clock movement.
	h.settled(3)
	if c := h.be.Counters(); c.Creates != 4 {
		t.Fatalf("%d creates after one claim, want 4", c.Creates)
	}
	for _, w := range h.inState(core.StateWarm) {
		if w.ID == sb.ID {
			t.Fatal("the claimed sandbox is in the warm pool")
		}
	}
	if m := h.c.Metrics(); m.Claims != 1 || m.ClaimWait.Count != 1 {
		t.Fatalf("metrics: claims=%d wait=%+v", m.Claims, m.ClaimWait)
	}

	// Touch is fenced.
	if err := h.c.Touch(ctxBg(), sb.ID, sb.Generation-1); !errors.Is(err, pool.ErrStaleGeneration) {
		t.Fatalf("stale touch: %v", err)
	}
	if err := h.c.Touch(ctxBg(), sb.ID, sb.Generation); err != nil {
		t.Fatal(err)
	}
	// Policy updates are fenced and cannot exceed the template.
	if err := h.c.UpdateNetwork(ctxBg(), sb.ID, sb.Generation, core.NetworkPolicy{Mode: core.NetworkFull}); !errors.Is(err, pool.ErrNetworkNotAllowed) {
		t.Fatalf("loosening the policy: %v", err)
	}
	if err := h.c.UpdateNetwork(ctxBg(), sb.ID, sb.Generation, core.NetworkPolicy{AllowDomains: []string{"a.example", "b.example"}}); err != nil {
		t.Fatal(err)
	}
	if att, _ := h.egress.Attached(sb.ID); len(att.Policy.AllowDomains) != 2 {
		t.Fatalf("egress policy not updated: %+v", att)
	}
}

func TestClaimErrors(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.Network = core.NetworkNone })
	h.st.PutTenant("small", 1)
	h.start()
	h.settled(3)

	if _, err := h.c.Claim(ctxBg(), tenant, "no-such-template", pool.ClaimOptions{}); !errors.Is(err, pool.ErrNotFound) {
		t.Fatalf("unknown template: %v", err)
	}
	if _, err := h.c.Claim(ctxBg(), "no-such-tenant", tmplID, pool.ClaimOptions{}); !errors.Is(err, pool.ErrNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	_, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Network: &core.NetworkPolicy{Mode: core.NetworkFull}})
	if !errors.Is(err, pool.ErrNetworkNotAllowed) {
		t.Fatalf("network escalation: %v", err)
	}
	if n := len(h.inState(core.StateWarm)); n != 3 {
		t.Fatalf("rejected claims consumed sandboxes: %d warm", n)
	}

	sb, err := h.c.Claim(ctxBg(), "small", tmplID, pool.ClaimOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.egress.Attached(sb.ID); ok {
		t.Fatal("a network=none sandbox must not be attached to the egress proxy")
	}
	if in := h.agent.Inits(); len(in) != 1 || in[0].Req.EgressProxy {
		t.Fatalf("init for network=none: %+v", in)
	}
	if _, err := h.c.Claim(ctxBg(), "small", tmplID, pool.ClaimOptions{}); !errors.Is(err, pool.ErrQuotaExceeded) {
		t.Fatalf("over quota: %v", err)
	}
	if err := h.c.Release(ctxBg(), sb.ID, sb.Generation, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Claim(ctxBg(), "small", tmplID, pool.ClaimOptions{}); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if m := h.c.Metrics(); m.ClaimsQuota != 1 || m.ClaimsFailed != 3 {
		t.Fatalf("metrics: quota=%d failed=%d", m.ClaimsQuota, m.ClaimsFailed)
	}
}

func TestReleaseDestroysAndNeverRewarms(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	sb := h.claim()
	h.settled(3)
	instance := sb.InstanceID

	// A stale actor (holding the pre-claim generation) is rejected and nothing changes.
	if err := h.c.Release(ctxBg(), sb.ID, sb.Generation-1, core.ReasonReleased); !errors.Is(err, pool.ErrStaleGeneration) {
		t.Fatalf("stale release: %v", err)
	}
	if got := h.get(sb.ID); got.State != core.StateRunning {
		t.Fatalf("stale release changed the row: %+v", got)
	}
	if err := h.c.Release(ctxBg(), "sb-missing", 1, ""); !errors.Is(err, pool.ErrNotFound) {
		t.Fatalf("release of a missing sandbox: %v", err)
	}
	warm := h.inState(core.StateWarm)[0]
	if err := h.c.Release(ctxBg(), warm.ID, warm.Generation, ""); !errors.Is(err, pool.ErrInvalidTransition) {
		t.Fatalf("release of a WARM sandbox: %v", err)
	}

	if err := h.c.Release(ctxBg(), sb.ID, sb.Generation, ""); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got := h.get(sb.ID)
	if got.State != core.StateDestroyed || got.Reason != core.ReasonReleased || got.DeadlineAt != nil {
		t.Fatalf("released row: %+v", got)
	}
	if !h.be.WasDestroyed(instance) {
		t.Fatal("instance was not destroyed")
	}
	if _, ok := h.be.BySandbox(sb.ID); ok {
		t.Fatal("instance still listed")
	}
	if _, ok := h.egress.Attached(sb.ID); ok || h.egress.Detaches(sb.ID) != 1 {
		t.Fatalf("egress not detached (detaches=%d)", h.egress.Detaches(sb.ID))
	}
	if _, err := os.Stat(h.c.RunDir(sb.ID)); !os.IsNotExist(err) {
		t.Fatalf("run dir still present: %v", err)
	}
	h.neverRewarmed(sb.ID)
	var path []core.State
	for _, e := range h.st.Events(sb.ID) {
		path = append(path, e.ToState)
	}
	want := []core.State{core.StateCreating, core.StateWarm, core.StateAssigned, core.StateRunning, core.StateRecycling, core.StateDestroyed}
	if len(path) != len(want) {
		t.Fatalf("history %v, want %v", path, want)
	}
	for i := range want {
		if path[i] != want[i] {
			t.Fatalf("history %v, want %v", path, want)
		}
	}

	// Releasing again is a no-op, and the id never comes back from a claim.
	if err := h.c.Release(ctxBg(), sb.ID, sb.Generation, ""); err != nil {
		t.Fatalf("second release: %v", err)
	}
	h.settled(3)
	seen := map[string]bool{sb.ID: true}
	for i := 0; i < 6; i++ {
		next := h.claim()
		if seen[next.ID] {
			t.Fatalf("claim returned sandbox %s twice", next.ID)
		}
		seen[next.ID] = true
		if err := h.c.Release(ctxBg(), next.ID, next.Generation, ""); err != nil {
			t.Fatal(err)
		}
		h.neverRewarmed(next.ID)
		h.settled(3)
	}
	if m := h.c.Metrics(); m.Releases != 7 || m.Terminations[core.ReasonReleased] != 7 || m.Destroyed != 7 {
		t.Fatalf("metrics: %+v", m)
	}
}

func TestClaimFailureAfterAssignmentRecycles(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	victim := h.inState(core.StateWarm)[0]

	boom := errors.New("agent refused init")
	h.agent.SetInitError(boom)
	_, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("Claim error = %v, want the init error", err)
	}
	got := h.get(victim.ID)
	if got.State != core.StateDestroyed || got.Reason != core.ReasonError {
		t.Fatalf("failed claim left the row as %+v", got)
	}
	h.neverRewarmed(victim.ID)
	if !h.be.WasDestroyed(victim.InstanceID) || h.egress.Detaches(victim.ID) == 0 {
		t.Fatal("instance or egress attachment survived a failed claim")
	}

	// Same for an egress failure.
	h.agent.SetInitError(nil)
	h.settled(3)
	victim = h.inState(core.StateWarm)[0]
	h.egress.SetAttachError(errors.New("proxy down"))
	if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{}); err == nil {
		t.Fatal("claim succeeded without egress")
	}
	if got := h.get(victim.ID); got.State != core.StateDestroyed || got.Reason != core.ReasonError {
		t.Fatalf("row after egress failure: %+v", got)
	}
	h.egress.SetAttachError(nil)
	h.settled(3)
	if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{}); err != nil {
		t.Fatalf("claim after recovery: %v", err)
	}
}

func TestClaimInitHangHitsAssignedDeadline(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	victim := h.inState(core.StateWarm)[0]
	h.agent.SetInitHang(true)

	done := make(chan error, 1)
	go func() {
		_, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{})
		done <- err
	}()
	eventually(t, "agent init to start", func() bool { return len(h.agent.Inits()) == 1 })
	if got := h.get(victim.ID); got.State != core.StateAssigned || got.DeadlineAt == nil {
		t.Fatalf("row during init: %+v", got)
	}
	h.clock.Advance(core.AssignedDeadline + time.Second)
	if err := <-done; err == nil {
		t.Fatal("claim succeeded although init hung")
	}
	eventually(t, "hung claim to be destroyed", func() bool { return h.get(victim.ID).State == core.StateDestroyed })
	// The claim's own timeout and the sweeper race for the row; both are valid owners.
	if r := h.get(victim.ID).Reason; r != core.ReasonError && r != core.ReasonDeadlineExceeded {
		t.Fatalf("reason = %q", r)
	}
	h.neverRewarmed(victim.ID)
}

func TestCreateFailureCleansUpAndBreakerEngages(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate = 1 })
	h.be.SetCreateError(errors.New("image pull failed"))
	h.start()

	// Threshold is 3: three creates fail back to back, then the breaker opens.
	eventually(t, "three failed creates to be cleaned up", func() bool {
		return len(h.st.Sandboxes()) == 3 && len(h.inState(core.StateDestroyed)) == 3
	})
	for _, sb := range h.st.Sandboxes() {
		if sb.Reason != core.ReasonCreateFailed {
			t.Fatalf("reason = %q, want create_failed", sb.Reason)
		}
		ev := h.st.Events(sb.ID)
		if !hasEvent(ev, core.StateFailed, core.ReasonCreateFailed) || !hasEvent(ev, core.StateRecycling, core.ReasonCreateFailed) {
			t.Fatalf("history lacks FAILED -> RECYCLING: %+v", ev)
		}
		for _, e := range ev {
			if e.ToState == core.StateWarm {
				t.Fatal("a failed create was marked WARM")
			}
		}
		if _, err := os.Stat(h.c.RunDir(sb.ID)); !os.IsNotExist(err) {
			t.Fatalf("run dir of failed create remains: %v", err)
		}
	}
	// No hot loop: passes while the breaker is open create nothing.
	for i := 0; i < 20; i++ {
		_ = h.c.Refill(ctxBg())
	}
	if n := len(h.st.Sandboxes()); n != 3 {
		t.Fatalf("breaker open but %d sandboxes were attempted", n)
	}
	m := h.c.Metrics()
	if tm := m.Templates[tmplID]; !tm.BreakerOpen || tm.ConsecutiveCreateFailures != 3 || m.CreateFailures != 3 {
		t.Fatalf("metrics: %+v failures=%d", tm, m.CreateFailures)
	}

	// After the 1s backoff exactly one probe is allowed; it fails and the backoff doubles.
	// (The explicit Kick stands in for the loop's interval tick, which on a
	// fake clock only fires if the loop was already waiting when time moved.)
	h.clock.Advance(time.Second)
	h.c.Kick()
	eventually(t, "probe create to fail", func() bool { return len(h.inState(core.StateDestroyed)) == 4 })
	for i := 0; i < 20; i++ {
		_ = h.c.Refill(ctxBg())
	}
	h.clock.Advance(time.Second) // 1s into a 2s backoff
	for i := 0; i < 20; i++ {
		_ = h.c.Refill(ctxBg())
	}
	if n := len(h.st.Sandboxes()); n != 4 {
		t.Fatalf("%d attempts, want 4 (backoff should have doubled)", n)
	}

	// The backend recovers: the next probe succeeds, the breaker closes, the pool fills.
	h.be.SetCreateError(nil)
	h.clock.Advance(time.Second)
	h.c.Kick()
	h.settled(3)
	if tm := h.c.Metrics().Templates[tmplID]; tm.BreakerOpen || tm.ConsecutiveCreateFailures != 0 {
		t.Fatalf("breaker did not close: %+v", tm)
	}
	if n := h.be.Len(); n != 3 {
		t.Fatalf("backend has %d instances", n)
	}
}

func TestReadinessFailureIsACreateFailure(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate, tm.WarmTarget = 1, 1 })
	h.agent.SetReadyError(errors.New("agent never came up"))
	h.start()
	eventually(t, "unready sandboxes to be destroyed", func() bool { return len(h.inState(core.StateDestroyed)) == 3 })
	if n := len(h.inState(core.StateWarm)); n != 0 {
		t.Fatalf("%d sandboxes marked WARM without passing readiness", n)
	}
	// The instance existed, so it must have been destroyed, not leaked.
	if n := h.be.Len(); n != 0 {
		t.Fatalf("%d instances leaked", n)
	}
	if c := h.be.Counters(); c.DestroyedLive != 3 {
		t.Fatalf("destroyed %d live instances, want 3", c.DestroyedLive)
	}
}

func TestHungCreateIsCaughtByDeadline(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.RefillRate, tm.WarmTarget = 1, 1 })
	h.be.HangNextCreates(1)
	h.start()
	eventually(t, "create to start", func() bool { return h.be.Counters().Creates == 1 })
	hung := h.st.Sandboxes()[0]
	if hung.State != core.StateCreating || hung.DeadlineAt == nil {
		t.Fatalf("hung row: %+v", hung)
	}

	h.clock.Advance(core.CreatingDeadline - time.Second)
	_ = h.c.Sweep(ctxBg())
	if got := h.get(hung.ID); got.State != core.StateCreating {
		t.Fatalf("create was failed before its deadline: %+v", got)
	}

	h.clock.Advance(2 * time.Second)
	eventually(t, "hung create to be destroyed", func() bool { return h.get(hung.ID).State == core.StateDestroyed })
	if got := h.get(hung.ID); got.Reason != core.ReasonDeadlineExceeded {
		t.Fatalf("reason = %q, want deadline_exceeded", got.Reason)
	}
	if !hasEvent(h.st.Events(hung.ID), core.StateFailed, core.ReasonDeadlineExceeded) {
		t.Fatalf("no FAILED event: %+v", h.st.Events(hung.ID))
	}
	// A replacement is created.
	h.settled(1)
	if w := h.inState(core.StateWarm)[0]; w.ID == hung.ID {
		t.Fatal("the hung sandbox became WARM")
	}
}

func TestSweeperFailsOrphanedCreatingRow(t *testing.T) {
	// A CREATING row with no create in flight: the process that owned it died.
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget = 0 })
	now := h.clock.Now()
	deadline := now.Add(core.CreatingDeadline)
	h.st.InsertRaw(core.Sandbox{ID: "sb-stuck", TemplateID: tmplID, TemplateVersion: 1, HostID: host,
		State: core.StateCreating, CreatedAt: now, DeadlineAt: &deadline})
	half := h.be.AddInstance(runtime.Instance{SandboxID: "sb-stuck", AgentEpoch: "epoch-1", Running: true})

	if err := h.c.Sweep(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if got := h.get("sb-stuck"); got.State != core.StateCreating {
		t.Fatalf("swept before the deadline: %+v", got)
	}
	h.clock.Advance(core.CreatingDeadline + time.Second)
	if err := h.c.Sweep(ctxBg()); err != nil {
		t.Fatal(err)
	}
	got := h.get("sb-stuck")
	if got.State != core.StateDestroyed || got.Reason != core.ReasonDeadlineExceeded {
		t.Fatalf("row: %+v", got)
	}
	if !h.be.WasDestroyed(half.ID) {
		t.Fatal("the half-created instance was not found by label and destroyed")
	}
}

func TestTimersFireWithTheRightReason(t *testing.T) {
	t.Run("idle_timeout", func(t *testing.T) {
		h := newHarness(t, nil)
		h.start()
		h.settled(3)
		idle, busy := h.claim(), h.claim()
		h.settled(3)

		h.clock.Advance(6 * time.Minute)
		if err := h.c.Touch(ctxBg(), busy.ID, busy.Generation); err != nil {
			t.Fatal(err)
		}
		_ = h.c.Sweep(ctxBg())
		if h.get(idle.ID).State != core.StateRunning {
			t.Fatal("recycled before the idle timeout")
		}
		h.clock.Advance(5 * time.Minute) // idle: 11 min; busy: 5 min since touch
		if err := h.c.Sweep(ctxBg()); err != nil {
			t.Fatal(err)
		}
		eventually(t, "idle sandbox to be destroyed", func() bool { return h.get(idle.ID).State == core.StateDestroyed })
		if got := h.get(idle.ID); got.Reason != core.ReasonIdleTimeout {
			t.Fatalf("reason = %q, want idle_timeout", got.Reason)
		}
		if got := h.get(busy.ID); got.State != core.StateRunning {
			t.Fatalf("active sandbox was recycled: %+v", got)
		}
		if !h.be.WasDestroyed(idle.InstanceID) || h.be.WasDestroyed(busy.InstanceID) {
			t.Fatal("wrong instance destroyed")
		}
		if h.be.Counters().StatsCalls == 0 {
			t.Fatal("stats were not checked before recycling an idle sandbox")
		}
		if m := h.c.Metrics(); m.Terminations[core.ReasonIdleTimeout] != 1 {
			t.Fatalf("terminations: %v", m.Terminations)
		}
	})

	t.Run("oom_killed_instead_of_idle", func(t *testing.T) {
		h := newHarness(t, nil)
		h.start()
		h.settled(3)
		sb := h.claim()
		h.be.SetStats(sb.InstanceID, runtime.Stats{OOMKills: 2})
		h.clock.Advance(11 * time.Minute)
		_ = h.c.Sweep(ctxBg())
		eventually(t, "sandbox to be destroyed", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
		if got := h.get(sb.ID); got.Reason != core.ReasonOOMKilled {
			t.Fatalf("reason = %q, want oom_killed", got.Reason)
		}
	})

	t.Run("max_lifetime", func(t *testing.T) {
		h := newHarness(t, func(tm *core.Template) { tm.Timeouts.WarmTTL = 0 })
		h.start()
		h.settled(3)
		sb := h.claim()
		// Keep it active so only the lifetime cap can end it.
		for i := 0; i < 6; i++ {
			h.clock.Advance(9 * time.Minute)
			if err := h.c.Touch(ctxBg(), sb.ID, sb.Generation); err != nil {
				t.Fatalf("touch %d: %v (row %+v)", i, err, h.get(sb.ID))
			}
		}
		_ = h.c.Sweep(ctxBg()) // 54 minutes in, active throughout
		if got := h.get(sb.ID); got.State != core.StateRunning {
			t.Fatalf("recycled before the lifetime cap: %+v", got)
		}
		h.clock.Advance(7 * time.Minute) // 61 minutes; idle for only 7
		_ = h.c.Sweep(ctxBg())
		eventually(t, "sandbox to be destroyed", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
		if got := h.get(sb.ID); got.Reason != core.ReasonMaxLifetime {
			t.Fatalf("reason = %q, want max_lifetime", got.Reason)
		}
	})

	t.Run("oom_killed_instead_of_max_lifetime", func(t *testing.T) {
		h := newHarness(t, func(tm *core.Template) { tm.Timeouts.WarmTTL, tm.Timeouts.Idle = 0, 0 })
		h.start()
		h.settled(3)
		sb := h.claim()
		h.be.SetStats(sb.InstanceID, runtime.Stats{OOMKills: 1})
		h.clock.Advance(61 * time.Minute)
		_ = h.c.Sweep(ctxBg())
		eventually(t, "sandbox to be destroyed", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
		if got := h.get(sb.ID); got.Reason != core.ReasonOOMKilled {
			t.Fatalf("reason = %q, want oom_killed", got.Reason)
		}
	})

	t.Run("warm_ttl", func(t *testing.T) {
		h := newHarness(t, nil)
		h.start()
		h.settled(3)
		first := h.inState(core.StateWarm)
		h.clock.Advance(29 * time.Minute)
		_ = h.c.Sweep(ctxBg())
		if n := len(h.inState(core.StateDestroyed)); n != 0 {
			t.Fatalf("%d warm sandboxes recycled before the TTL", n)
		}
		h.clock.Advance(2 * time.Minute)
		_ = h.c.Sweep(ctxBg())
		for _, sb := range first {
			eventually(t, "aged warm sandbox to be destroyed", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
			if got := h.get(sb.ID); got.Reason != core.ReasonWarmTTL {
				t.Fatalf("reason = %q, want warm_ttl", got.Reason)
			}
			if !h.be.WasDestroyed(sb.InstanceID) {
				t.Fatal("aged instance not destroyed")
			}
		}
		h.settled(3) // replaced by fresh ones
	})

	t.Run("template_changed", func(t *testing.T) {
		h := newHarness(t, nil)
		h.start()
		h.settled(3)
		old := h.inState(core.StateWarm)
		running := h.claim()
		h.settled(3)
		old = append(old[1:], h.inState(core.StateWarm)[2])

		v2 := baseTemplate()
		v2.Version, v2.Image = 2, "img:v2"
		h.st.PutTemplate(v2)
		// A claim never receives an instance of the superseded version.
		if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: -1}); !errors.Is(err, pool.ErrPoolEmpty) {
			t.Fatalf("claim against v2 with only v1 warm: %v", err)
		}
		if err := h.c.Sweep(ctxBg()); err != nil {
			t.Fatal(err)
		}
		for _, sb := range old {
			eventually(t, "old-version sandbox to be destroyed", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
			if got := h.get(sb.ID); got.Reason != core.ReasonTemplateChanged {
				t.Fatalf("reason = %q, want template_changed", got.Reason)
			}
		}
		h.settled(3)
		for _, sb := range h.inState(core.StateWarm) {
			if sb.TemplateVersion != 2 {
				t.Fatalf("refill used version %d", sb.TemplateVersion)
			}
		}
		// A tenant's running session is not touched by a template change.
		if got := h.get(running.ID); got.State != core.StateRunning {
			t.Fatalf("running sandbox: %+v", got)
		}
	})

	t.Run("assigned_deadline", func(t *testing.T) {
		h := newHarness(t, nil)
		h.start()
		h.settled(3)
		// A claim whose owner died between the store claim and RUNNING.
		sb, err := h.st.Claim(ctxBg(), store.ClaimParams{TenantID: tenant, TemplateID: tmplID, HostID: host})
		if err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(core.AssignedDeadline - time.Second)
		_ = h.c.Sweep(ctxBg())
		if h.get(sb.ID).State != core.StateAssigned {
			t.Fatal("swept before the deadline")
		}
		h.clock.Advance(2 * time.Second)
		_ = h.c.Sweep(ctxBg())
		eventually(t, "stuck claim to be destroyed", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
		if got := h.get(sb.ID); got.Reason != core.ReasonDeadlineExceeded {
			t.Fatalf("reason = %q, want deadline_exceeded", got.Reason)
		}
		h.neverRewarmed(sb.ID)
	})

	t.Run("recycling_deadline_retries_teardown", func(t *testing.T) {
		h := newHarness(t, nil)
		h.start()
		h.settled(3)
		sb := h.claim()
		h.be.SetDestroyError(errors.New("podman rm failed"))
		if err := h.c.Release(ctxBg(), sb.ID, sb.Generation, ""); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if got := h.get(sb.ID); got.State != core.StateRecycling || got.Reason != core.ReasonReleased {
			t.Fatalf("row after a failed teardown: %+v", got)
		}
		h.be.SetDestroyError(nil)
		h.clock.Advance(core.RecyclingDeadline + time.Second)
		_ = h.c.Sweep(ctxBg())
		eventually(t, "teardown to be retried", func() bool { return h.get(sb.ID).State == core.StateDestroyed })
		if got := h.get(sb.ID); got.Reason != core.ReasonReleased {
			t.Fatalf("reason changed on retry: %q", got.Reason)
		}
		if !h.be.WasDestroyed(sb.InstanceID) {
			t.Fatal("instance not destroyed on retry")
		}
	})
}

func TestReconcilerDestroysOrphansAndFailsLostRows(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	running := h.claim()
	h.settled(3)
	warm := h.inState(core.StateWarm)
	keep, lost, dead := warm[0], warm[1], warm[2]

	// Orphans: no row at all; a row that is already DESTROYED; a second
	// instance claiming a live sandbox id with the wrong generation.
	ghost := h.be.AddInstance(runtime.Instance{SandboxID: "sb-ghost", AgentEpoch: "epoch-0", Running: true})
	h.st.InsertRaw(core.Sandbox{ID: "sb-gone", TemplateID: tmplID, TemplateVersion: 1, HostID: host, State: core.StateDestroyed})
	zombie := h.be.AddInstance(runtime.Instance{SandboxID: "sb-gone", AgentEpoch: "epoch-1", Running: true})
	imposter := h.be.AddInstance(runtime.Instance{SandboxID: keep.ID, Generation: 5, AgentEpoch: "epoch-1", Running: true})
	// Lost rows: instance removed out of band (one WARM, one RUNNING); one exited after an OOM kill.
	h.be.Vanish(lost.InstanceID)
	h.be.Vanish(running.InstanceID)
	h.be.SetStats(dead.InstanceID, runtime.Stats{OOMKills: 1})
	h.be.StopInstance(dead.InstanceID)

	if err := h.c.Reconcile(ctxBg()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	for name, inst := range map[string]runtime.Instance{"ghost": ghost, "zombie": zombie, "imposter": imposter} {
		if !h.be.WasDestroyed(inst.ID) {
			t.Errorf("orphan %s was not destroyed", name)
		}
	}
	if ev := h.st.Events("sb-ghost"); len(ev) != 1 || ev[0].Type != "orphan_destroyed" || ev[0].Reason != core.ReasonOrphan {
		t.Errorf("orphan event: %+v", ev)
	}
	if got := h.get(keep.ID); got.State != core.StateWarm || h.be.WasDestroyed(keep.InstanceID) {
		t.Errorf("healthy warm sandbox was disturbed: %+v", got)
	}
	for _, c := range []struct {
		sb     core.Sandbox
		reason core.TerminationReason
	}{{lost, core.ReasonInstanceLost}, {running, core.ReasonInstanceLost}, {dead, core.ReasonOOMKilled}} {
		got := h.get(c.sb.ID)
		if got.State != core.StateDestroyed || got.Reason != c.reason {
			t.Errorf("row %s (was %s): %+v, want DESTROYED/%s", c.sb.ID, c.sb.State, got, c.reason)
		}
		ev := h.st.Events(c.sb.ID)
		if !hasEvent(ev, core.StateFailed, c.reason) || !hasEvent(ev, core.StateRecycling, c.reason) {
			t.Errorf("row %s did not go FAILED -> RECYCLING -> DESTROYED: %+v", c.sb.ID, ev)
		}
	}
	if !h.be.WasDestroyed(dead.InstanceID) {
		t.Error("the exited instance was not removed")
	}
	m := h.c.Metrics()
	if m.OrphansDestroyed != 3 || m.InstancesLost != 3 {
		t.Errorf("metrics: orphans=%d lost=%d", m.OrphansDestroyed, m.InstancesLost)
	}

	// The pool heals, and a second pass finds nothing to do.
	h.settled(3)
	before := h.be.Counters()
	if err := h.c.Reconcile(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if after := h.be.Counters(); after.Destroys != before.Destroys || after.Creates != before.Creates {
		t.Errorf("an idle reconcile pass changed something: %+v -> %+v", before, after)
	}
	if n := h.be.Len(); n != 3 {
		t.Errorf("backend has %d instances, want 3", n)
	}
}

func TestReconcilerLeavesInflightCreatesAlone(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget, tm.RefillRate = 2, 2 })
	h.be.SetCreateLatency(50 * time.Millisecond)
	h.start()
	eventually(t, "creates to start", func() bool { return h.be.Counters().Creates == 2 })
	for i := 0; i < 20; i++ {
		if err := h.c.Reconcile(ctxBg()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.settled(2)
	if c := h.be.Counters(); c.Creates != 2 || c.DestroyedLive != 0 {
		t.Fatalf("reconciler interfered with in-flight creates: %+v", c)
	}
	if n := len(h.inState(core.StateDestroyed)); n != 0 {
		t.Fatalf("%d rows destroyed", n)
	}
}

func TestReconcilerRemovesStaleRunDirs(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	stale := filepath.Join(h.dir, "sb-0123456789abcdef")
	other := filepath.Join(h.dir, "not-a-sandbox")
	for _, d := range []string{stale, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.c.Reconcile(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale run dir remains: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a directory that is not a sandbox run dir was removed: %v", err)
	}
	for _, sb := range h.inState(core.StateWarm) {
		if _, err := os.Stat(h.c.RunDir(sb.ID)); err != nil {
			t.Fatalf("live run dir removed: %v", err)
		}
	}
}

func TestRestartWithExistingWarmRows(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	session := h.claim()
	h.settled(3)
	warm := h.inState(core.StateWarm)
	h.c.Stop()

	// The stop left everything in place.
	if n := h.be.Len(); n != 4 {
		t.Fatalf("Stop destroyed instances: %d left", n)
	}
	if n := len(h.inState(core.StateWarm)); n != 3 {
		t.Fatalf("Stop changed warm rows: %d", n)
	}
	// While the agent is down one warm instance disappears.
	h.be.Vanish(warm[2].InstanceID)
	createsBefore := h.be.Counters().Creates
	readyBefore := h.agent.ReadyCalls()

	// A new agent process: same store, same runtime, new epoch.
	h.c = h.controller("epoch-2")
	h.start()

	// Start reconciles before returning.
	for _, sb := range warm[:2] {
		got := h.get(sb.ID)
		if got.State != core.StateWarm || got.Generation != 0 || got.InstanceID != sb.InstanceID {
			t.Fatalf("warm row %s after restart: %+v", sb.ID, got)
		}
		if h.be.WasDestroyed(sb.InstanceID) {
			t.Fatalf("matching warm instance %s was destroyed instead of re-adopted", sb.InstanceID)
		}
	}
	if got := h.get(warm[2].ID); got.State != core.StateDestroyed || got.Reason != core.ReasonInstanceLost {
		t.Fatalf("warm row without an instance: %+v", got)
	}
	// The tenant session of the old epoch is ended, not adopted.
	if got := h.get(session.ID); got.State != core.StateDestroyed || got.Reason != core.ReasonHostDrain {
		t.Fatalf("old-epoch session: %+v", got)
	}
	if !h.be.WasDestroyed(session.InstanceID) {
		t.Fatal("old-epoch session instance still running")
	}
	h.neverRewarmed(session.ID)
	if m := h.c.Metrics(); m.Adopted != 2 || m.InstancesLost != 1 {
		t.Fatalf("metrics: adopted=%d lost=%d", m.Adopted, m.InstancesLost)
	}
	if got := h.agent.ReadyCalls() - readyBefore; got < 2 {
		t.Fatalf("adopted instances were not probed (%d readiness calls)", got)
	}

	// Only the missing one is created.
	h.settled(3)
	if got := h.be.Counters().Creates - createsBefore; got != 1 {
		t.Fatalf("%d creates after restart, want 1", got)
	}
	// Adoption is remembered: further passes do not probe again or destroy.
	probes := h.agent.ReadyCalls()
	for i := 0; i < 3; i++ {
		if err := h.c.Reconcile(ctxBg()); err != nil {
			t.Fatal(err)
		}
	}
	if h.agent.ReadyCalls() != probes {
		t.Fatal("adopted sandboxes are probed on every pass")
	}
	// An adopted sandbox is claimable (oldest first) and works.
	sb := h.claim()
	if sb.ID != warm[0].ID || sb.Generation != 1 {
		t.Fatalf("claimed %+v, want adopted %s", sb, warm[0].ID)
	}
	// Its instance still carries the old epoch label. This process adopted
	// and claimed it, so the reconciler must not drain it as an old-epoch
	// session, however often it runs.
	for i := 0; i < 3; i++ {
		if err := h.c.Reconcile(ctxBg()); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.get(sb.ID); got.State != core.StateRunning {
		t.Fatalf("a sandbox adopted and claimed in this epoch was disturbed: %+v", got)
	}
	if h.be.WasDestroyed(sb.InstanceID) {
		t.Fatal("instance of the adopted, claimed sandbox was destroyed")
	}
	// It still ends normally.
	if err := h.c.Release(ctxBg(), sb.ID, sb.Generation, ""); err != nil {
		t.Fatal(err)
	}
	if got := h.get(sb.ID); got.State != core.StateDestroyed || got.Reason != core.ReasonReleased {
		t.Fatalf("released adopted sandbox: %+v", got)
	}
}

func TestRestartDoesNotAdoptUnreadyOrMismatchedWarm(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget = 2 })
	h.start()
	h.settled(2)
	warm := h.inState(core.StateWarm)
	h.c.Stop()

	// warm[0]: its agent no longer answers. warm[1]: the instance carrying
	// its id is not the one the row recorded.
	h.agent.SetReadyHook(func(ctx context.Context, dir string) error {
		if filepath.Base(dir) == warm[0].ID {
			return errors.New("agent dead")
		}
		return nil
	})
	h.be.Vanish(warm[1].InstanceID)
	fakeInst := h.be.AddInstance(runtime.Instance{SandboxID: warm[1].ID, Generation: 0, AgentEpoch: "epoch-1", Running: true})

	h.c = h.controller("epoch-2")
	h.start()
	for _, sb := range warm {
		if got := h.get(sb.ID); got.State != core.StateDestroyed || got.Reason != core.ReasonInstanceLost {
			t.Fatalf("row %s: %+v, want DESTROYED/instance_lost", sb.ID, got)
		}
	}
	if !h.be.WasDestroyed(warm[0].InstanceID) || !h.be.WasDestroyed(fakeInst.ID) {
		t.Fatal("unadoptable instances were left running")
	}
	h.settled(2)
}

func TestClaimWaitsForRefill(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget, tm.RefillRate = 1, 1 })
	h.start()
	h.settled(1)
	h.be.SetCreateLatency(40 * time.Millisecond)

	first := h.claim()
	// The pool is empty and its replacement takes 40ms: this claim queues.
	second, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: time.Minute})
	if err != nil {
		t.Fatalf("waiting claim: %v", err)
	}
	if second.ID == first.ID || second.State != core.StateRunning {
		t.Fatalf("second claim: %+v", second)
	}
	m := h.c.Metrics()
	if m.Claims != 2 || m.ClaimsWaited != 1 {
		t.Fatalf("metrics: claims=%d waited=%d", m.Claims, m.ClaimsWaited)
	}
	h.settled(1)
}

func TestClaimOnDemandWithZeroWarmTarget(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget = 0 })
	h.start()
	if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: -1}); !errors.Is(err, pool.ErrPoolEmpty) {
		t.Fatalf("no-wait claim on an empty pool: %v", err)
	}
	if n := len(h.st.Sandboxes()); n != 0 {
		t.Fatalf("a no-wait claim caused %d creates", n)
	}
	sb, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: time.Minute})
	if err != nil {
		t.Fatalf("on-demand claim: %v", err)
	}
	if sb.State != core.StateRunning {
		t.Fatalf("%+v", sb)
	}
	h.settled(0)
	if c := h.be.Counters(); c.Creates != 1 {
		t.Fatalf("%d creates for one on-demand claim", c.Creates)
	}
}

func TestClaimWaitDeadlineReturnsPoolEmpty(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget, tm.RefillRate = 1, 1 })
	h.be.SetCreateHang(true)
	h.start()

	done := make(chan error, 1)
	go func() {
		_, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: 2 * time.Second})
		done <- err
	}()
	eventually(t, "claim to queue", func() bool { return h.c.Metrics().Templates[tmplID].Waiters == 1 })
	select {
	case err := <-done:
		t.Fatalf("claim returned before its deadline: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	h.clock.Advance(2 * time.Second)
	if err := <-done; !errors.Is(err, pool.ErrPoolEmpty) {
		t.Fatalf("err = %v, want ErrPoolEmpty", err)
	}
	if w := h.c.Metrics().Templates[tmplID].Waiters; w != 0 {
		t.Fatalf("%d waiters left in the queue", w)
	}
	if m := h.c.Metrics(); m.ClaimsPoolEmpty != 1 {
		t.Fatalf("metrics: %+v", m)
	}

	// A cancelled context and a stopping controller also end the wait.
	ctx, cancel := context.WithCancel(ctxBg())
	go func() {
		_, err := h.c.Claim(ctx, tenant, tmplID, pool.ClaimOptions{Wait: time.Hour})
		done <- err
	}()
	eventually(t, "claim to queue", func() bool { return h.c.Metrics().Templates[tmplID].Waiters == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	go func() {
		_, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: time.Hour})
		done <- err
	}()
	eventually(t, "claim to queue", func() bool { return h.c.Metrics().Templates[tmplID].Waiters == 1 })
	h.c.Stop()
	if err := <-done; !errors.Is(err, pool.ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{}); !errors.Is(err, pool.ErrStopped) {
		t.Fatalf("claim on a stopped controller: %v", err)
	}
}

func TestWaitQueueIsBounded(t *testing.T) {
	clock := pooltest.NewFakeClock()
	st := pooltest.NewMemStore(clock.Now)
	tmpl := baseTemplate()
	tmpl.WarmTarget = 0
	st.PutTemplate(tmpl)
	st.PutTenant(tenant, 100)
	be := fake.New()
	be.SetCreateHang(true)
	c, err := pool.New(pool.Config{Store: st, Backend: be, Agent: &pooltest.FakeAgent{}, Clock: clock,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), HostID: host, RunDirBase: t.TempDir(), MaxWaiters: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.Start(ctxBg()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: time.Hour})
			done <- err
		}()
	}
	eventually(t, "two claims to queue", func() bool { return c.Metrics().Templates[tmplID].Waiters == 2 })
	if _, err := c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: time.Hour}); !errors.Is(err, pool.ErrPoolEmpty) {
		t.Fatalf("claim on a full queue: %v", err)
	}
	c.Stop()
	for i := 0; i < 2; i++ {
		if err := <-done; !errors.Is(err, pool.ErrStopped) {
			t.Fatalf("queued claim: %v", err)
		}
	}
}

func TestConcurrentClaimsNoDoubleAssignment(t *testing.T) {
	h := newHarness(t, func(tm *core.Template) { tm.WarmTarget, tm.MaxTotal, tm.RefillRate = 5, 0, 5 })
	h.st.PutTenant(tenant, 1000)
	h.start()
	h.settled(5)

	const n = 40
	type res struct {
		sb  core.Sandbox
		err error
	}
	out := make(chan res, n)
	for i := 0; i < n; i++ {
		go func() {
			sb, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Wait: time.Hour})
			out <- res{sb, err}
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		r := <-out
		if r.err != nil {
			t.Fatalf("claim: %v", r.err)
		}
		if seen[r.sb.ID] {
			t.Fatalf("sandbox %s assigned twice", r.sb.ID)
		}
		seen[r.sb.ID] = true
	}
	h.settled(5)
	if got := len(h.inState(core.StateRunning)); got != n {
		t.Fatalf("%d running, want %d", got, n)
	}
}

func TestStoreOutageDoesNotCrashLoops(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	h.st.SetError(errors.New("connection refused"))
	if err := h.c.Refill(ctxBg()); err == nil {
		t.Fatal("Refill hid a store error")
	}
	if err := h.c.Sweep(ctxBg()); err == nil {
		t.Fatal("Sweep hid a store error")
	}
	if err := h.c.Reconcile(ctxBg()); err == nil {
		t.Fatal("Reconcile hid a store error")
	}
	if _, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{}); err == nil {
		t.Fatal("Claim hid a store error")
	}
	h.clock.Advance(5 * time.Second)
	// Nothing was destroyed on the strength of a failed read.
	if n := h.be.Len(); n != 3 {
		t.Fatalf("store outage destroyed instances: %d left", n)
	}
	h.st.SetError(nil)
	sb := h.claim()
	if sb.State != core.StateRunning {
		t.Fatalf("%+v", sb)
	}
	h.settled(3)
}
