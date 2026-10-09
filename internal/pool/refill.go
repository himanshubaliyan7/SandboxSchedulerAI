package pool

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// breaker is a per-template circuit breaker on creates.
type breaker struct {
	failures  int // consecutive
	openUntil time.Time
}

// allowance is how many more creates may start now.
func (b *breaker) allowance(now time.Time, threshold, inflight int) int {
	switch {
	case b.failures < threshold:
		return math.MaxInt
	case now.Before(b.openUntil):
		return 0 // open
	case inflight > 0:
		return 0 // half-open and the probe is still running
	default:
		return 1 // half-open: one probe
	}
}

func (c *Controller) breakerResult(templateID string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.breakers[templateID]
	if b == nil {
		b = &breaker{}
		c.breakers[templateID] = b
	}
	if ok {
		if b.failures >= c.cfg.BreakerThreshold {
			c.log.Info("create breaker closed", "template", templateID)
		}
		*b = breaker{}
		return
	}
	b.failures++
	if over := b.failures - c.cfg.BreakerThreshold; over >= 0 {
		backoff := c.cfg.BreakerMaxBackoff
		if over < 30 && c.cfg.BreakerBaseBackoff<<over < backoff {
			backoff = c.cfg.BreakerBaseBackoff << over
		}
		b.openUntil = c.clock.Now().Add(backoff)
		c.log.Warn("create breaker open", "template", templateID, "consecutive_failures", b.failures, "backoff", backoff)
	}
}

// Refill runs one refill pass over every template. It is level-triggered:
// it reads the current counts from the store and starts as many creates
// as are missing, so a lost wake-up or a crash only delays it until the
// next pass. For each template
//
//	missing = WarmTarget + queued claims - WARM - CREATING
//
// capped by MaxTotal (all non-destroyed sandboxes; 0 means no cap), by
// RefillRate concurrent creates (at least 1) and by the circuit breaker.
// The CREATING row is inserted before the pass moves on, so the next pass
// already counts it. The pass also wakes queued claims while WARM
// sandboxes exist. Start runs it on every Kick and every RefillInterval.
func (c *Controller) Refill(ctx context.Context) error {
	c.refillMu.Lock()
	defer c.refillMu.Unlock()

	templates, err := c.store.ListTemplates(ctx)
	if err != nil {
		return fmt.Errorf("list templates: %w", err)
	}
	counts, err := c.store.CountByTemplateState(ctx, c.cfg.HostID)
	if err != nil {
		return fmt.Errorf("count sandboxes: %w", err)
	}
	now := c.clock.Now()

	type plan struct {
		tmpl core.Template
		n    int
	}
	var plans []plan
	c.mu.Lock()
	c.templates, c.counts = templates, counts
	c.m.LastRefillAt = now
	c.m.RefillPasses++
	for _, t := range templates {
		cs := counts[t.ID]
		warm, total := cs[core.StateWarm], 0
		for _, n := range cs {
			total += n
		}
		waiters := 0
		if q := c.queues[t.ID]; q != nil {
			for i := 0; i < warm && q.grant(); i++ {
			}
			waiters = q.len()
		}
		need := t.WarmTarget + waiters - warm - cs[core.StateCreating]
		if t.MaxTotal > 0 {
			need = min(need, t.MaxTotal-total)
		}
		inflight := 0
		for _, op := range c.creating {
			if op.templateID == t.ID {
				inflight++
			}
		}
		need = min(need, max(t.RefillRate, 1)-inflight)
		if b := c.breakers[t.ID]; b != nil {
			need = min(need, b.allowance(now, c.cfg.BreakerThreshold, inflight))
		}
		if need > 0 {
			plans = append(plans, plan{t, need})
		}
	}
	c.mu.Unlock()

	var firstErr error
	for _, p := range plans {
		for i := 0; i < p.n; i++ {
			if err := c.startCreate(ctx, p.tmpl); err != nil {
				c.log.Warn("could not start create", "template", p.tmpl.ID, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				break
			}
		}
	}
	return firstErr
}

// startCreate inserts the CREATING row and hands the rest to a goroutine.
func (c *Controller) startCreate(ctx context.Context, tmpl core.Template) error {
	id, err := newSandboxID()
	if err != nil {
		return err
	}
	cctx, cancel := c.withTimeout(c.rootCtx, c.cfg.CreateTimeout)
	// Register before the row exists so the reconciler never sees a
	// CREATING row of this process that it does not know is in flight.
	c.mu.Lock()
	c.creating[id] = &createOp{templateID: tmpl.ID, cancel: cancel}
	c.mu.Unlock()
	unregister := func() {
		cancel()
		c.mu.Lock()
		delete(c.creating, id)
		c.mu.Unlock()
	}

	sb, err := c.store.InsertCreating(ctx, store.NewSandbox{
		ID: id, TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, HostID: c.cfg.HostID, Deadline: c.cfg.CreateTimeout,
	})
	if err != nil {
		unregister()
		return fmt.Errorf("insert creating row: %w", err)
	}
	// The kick comes after unregistering: a pass that still counted this
	// create as in flight would start nothing under RefillRate, and no
	// later event would wake the loop before the next interval.
	if !c.goAsync(func() { c.runCreate(cctx, sb, tmpl); unregister(); c.Kick() }) {
		unregister()
		// Stopping: do not leave the row for the next start-up to find.
		dctx, dcancel := c.cleanupCtx()
		defer dcancel()
		if failed, err := c.store.Transition(dctx, store.TransitionParams{ID: sb.ID, Generation: sb.Generation,
			From: core.StateCreating, To: core.StateFailed, Reason: core.ReasonHostDrain}); err == nil {
			_ = c.recycle(dctx, failed, "", nil)
		}
		return ErrStopped
	}
	return nil
}

// runCreate makes the run dir, creates the instance, waits for the agent
// and only then marks the row WARM. Any failure ends in FAILED and an
// immediate teardown.
func (c *Controller) runCreate(ctx context.Context, sb core.Sandbox, tmpl core.Template) {
	var (
		start  = c.clock.Now()
		runDir = c.RunDir(sb.ID)
		inst   runtime.Instance
	)
	err := func() (err error) {
		if err = os.MkdirAll(c.cfg.RunDirBase, 0o700); err != nil {
			return fmt.Errorf("create run dir base: %w", err)
		}
		if err = os.Mkdir(runDir, 0o700); err != nil {
			return fmt.Errorf("create run dir: %w", err)
		}
		if err = os.Chmod(runDir, 0o700); err != nil { // Mkdir is subject to the umask
			return fmt.Errorf("chmod run dir: %w", err)
		}
		inst, err = c.backend.Create(ctx, runtime.Spec{
			SandboxID: sb.ID, Generation: sb.Generation, AgentEpoch: c.cfg.AgentEpoch, Template: tmpl,
			HostRunDir: runDir, AgentBinaryPath: c.cfg.AgentBinaryPath,
		})
		if err != nil {
			return fmt.Errorf("backend create: %w", err)
		}
		if err = c.agent.WaitReady(ctx, runDir); err != nil {
			return fmt.Errorf("agent readiness: %w", err)
		}
		return nil
	}()
	if err == nil {
		dctx, cancel := c.cleanupCtx()
		_, err = c.store.MarkWarm(dctx, sb.ID, sb.Generation, inst.ID)
		cancel()
		if err == nil {
			c.breakerResult(tmpl.ID, true)
			c.mu.Lock()
			c.m.Creates++
			if q := c.queues[tmpl.ID]; q != nil {
				q.grant()
			}
			c.mu.Unlock()
			c.log.Debug("sandbox warm", "sandbox", sb.ID, "template", tmpl.ID, "instance", inst.ID, "took", c.clock.Now().Sub(start))
			c.Kick()
			return
		}
		err = fmt.Errorf("mark warm: %w", err)
	}
	c.failCreate(ctx, sb, inst, err)
}

func (c *Controller) failCreate(ctx context.Context, sb core.Sandbox, inst runtime.Instance, cause error) {
	reason := core.ReasonCreateFailed
	switch {
	case timedOut(ctx):
		reason = core.ReasonDeadlineExceeded
	case c.rootCtx.Err() != nil:
		reason = core.ReasonHostDrain
	}
	if reason != core.ReasonHostDrain {
		c.breakerResult(sb.TemplateID, false)
	}
	c.mu.Lock()
	c.m.CreateFailures++
	c.mu.Unlock()
	c.log.Warn("sandbox create failed", "sandbox", sb.ID, "template", sb.TemplateID, "reason", reason, "err", cause)
	defer c.Kick()

	dctx, cancel := c.cleanupCtx()
	defer cancel()
	failed, err := c.store.Transition(dctx, store.TransitionParams{
		ID: sb.ID, Generation: sb.Generation, From: core.StateCreating, To: core.StateFailed,
		Reason: reason, InstanceID: inst.ID, Data: map[string]any{"error": cause.Error()},
	})
	if err != nil {
		// The row is no longer ours to fail: the sweeper already took it at
		// the deadline (its teardown may have run before our instance
		// existed), or the store is down. Either way this goroutine is the
		// only one that knows the instance id, so release it directly.
		if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrNotFound) {
			c.log.Warn("could not mark failed create", "sandbox", sb.ID, "err", err)
		}
		if inst.ID != "" {
			if derr := c.backend.Destroy(dctx, inst.ID); derr != nil {
				c.log.Warn("destroy after failed create", "sandbox", sb.ID, "instance", inst.ID, "err", derr)
			}
		}
		if cur, gerr := c.store.GetSandbox(dctx, sb.ID); gerr != nil || cur.State != core.StateCreating {
			_ = os.RemoveAll(c.RunDir(sb.ID))
		}
		return
	}
	if err := c.recycle(dctx, failed, "", nil); err != nil {
		c.log.Warn("could not recycle failed create", "sandbox", sb.ID, "err", err)
	}
}

// cancelCreate cancels an in-flight create and reports whether there was one.
func (c *Controller) cancelCreate(id string) bool {
	c.mu.Lock()
	op := c.creating[id]
	c.mu.Unlock()
	if op != nil {
		op.cancel()
	}
	return op != nil
}
