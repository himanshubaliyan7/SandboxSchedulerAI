package pool_test

import (
	"errors"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
)

// A dead instance has no counters left (Stats says ErrNotRunning), so the
// reason must come from how the backend says the instance ended.
func TestCheckInstanceReportsHowTheInstanceDied(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	oom, lost, healthy, gone := h.claim(), h.claim(), h.claim(), h.claim()
	h.settled(3)

	h.be.KillInstance(oom.InstanceID, 137, true)
	h.be.KillInstance(lost.InstanceID, 1, false)
	h.be.Vanish(gone.InstanceID)

	for _, c := range []struct {
		name   string
		sb     core.Sandbox
		lost   bool
		reason core.TerminationReason
	}{
		{"oom", oom, true, core.ReasonOOMKilled},
		{"exited", lost, true, core.ReasonInstanceLost},
		{"vanished", gone, true, core.ReasonInstanceLost},
		{"healthy", healthy, false, ""},
	} {
		got, err := h.c.CheckInstance(ctxBg(), c.sb.ID, c.sb.Generation)
		if err != nil || got != c.lost {
			t.Fatalf("%s: CheckInstance = %v, %v; want %v", c.name, got, err, c.lost)
		}
		row := h.get(c.sb.ID)
		if !c.lost {
			if row.State != core.StateRunning {
				t.Errorf("%s: healthy sandbox is %s", c.name, row.State)
			}
			continue
		}
		if row.State != core.StateDestroyed || row.Reason != c.reason {
			t.Errorf("%s: state %s reason %q, want DESTROYED %q", c.name, row.State, row.Reason, c.reason)
		}
		if !hasEvent(h.st.Events(c.sb.ID), core.StateFailed, c.reason) {
			t.Errorf("%s: no FAILED event with reason %q: %+v", c.name, c.reason, h.st.Events(c.sb.ID))
		}
	}
	for _, e := range h.st.Events(oom.ID) {
		if e.ToState == core.StateFailed && e.Data["exit_code"] != 137 {
			t.Errorf("oom event does not carry the exit code: %+v", e.Data)
		}
	}
	// Fenced: a stale generation changes nothing.
	if _, err := h.c.CheckInstance(ctxBg(), healthy.ID, healthy.Generation-1); !errors.Is(err, pool.ErrStaleGeneration) {
		t.Errorf("stale CheckInstance: %v, want ErrStaleGeneration", err)
	}
}

// The reconciler reaches the same verdict for an instance that died
// between passes.
func TestReconcilerReportsOOMOfDeadInstance(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.settled(3)
	sb := h.claim()
	h.be.KillInstance(sb.InstanceID, 137, true)
	if err := h.c.Reconcile(ctxBg()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if row := h.get(sb.ID); row.State != core.StateDestroyed || row.Reason != core.ReasonOOMKilled {
		t.Errorf("state %s reason %q, want DESTROYED oom_killed", row.State, row.Reason)
	}
}

func TestClaimLifetimeOverrideCanOnlyShorten(t *testing.T) {
	h := newHarness(t, nil) // template max lifetime: 1h
	h.start()
	h.settled(3)
	for _, c := range []struct {
		ask, want time.Duration
	}{{0, time.Hour}, {10 * time.Minute, 10 * time.Minute}, {2 * time.Hour, time.Hour}} {
		sb, err := h.c.Claim(ctxBg(), tenant, tmplID, pool.ClaimOptions{Lifetime: c.ask})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if sb.ExpiresAt == nil || sb.AssignedAt == nil || sb.ExpiresAt.Sub(*sb.AssignedAt) != c.want {
			t.Errorf("Lifetime %s: expires %v after assignment %v, want %s", c.ask, sb.ExpiresAt, sb.AssignedAt, c.want)
		}
		h.settled(3)
	}
}
