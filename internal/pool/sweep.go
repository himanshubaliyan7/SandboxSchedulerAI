package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Sweep runs one sweeper pass: it asks the store which rows a timer has
// caught and terminates each with the matching core.TerminationReason.
//
//	CREATING past deadline  -> FAILED (deadline_exceeded) -> RECYCLING; an in-flight create is cancelled
//	ASSIGNED past deadline  -> RECYCLING (deadline_exceeded)
//	RECYCLING past deadline -> the teardown is retried
//	RUNNING idle            -> RECYCLING (idle_timeout, or oom_killed)
//	RUNNING/PAUSED expired  -> RECYCLING (max_lifetime, or oom_killed)
//	WARM past warm TTL      -> RECYCLING (warm_ttl)
//	WARM, old template      -> RECYCLING (template_changed)
//
// Before a tenant's sandbox is recycled for idleness or lifetime the
// backend's stats are read, so a sandbox that was in fact OOM-killed is
// reported as oom_killed. Every transition is conditional on the state
// and generation the store listed, so a row that moved on in the meantime
// (claimed, released) is simply skipped. It returns when all teardowns of
// the pass have finished. It also garbage-collects old DESTROYED rows.
func (c *Controller) Sweep(ctx context.Context) error {
	c.sweepMu.Lock()
	defer c.sweepMu.Unlock()

	items, err := c.store.ListSweepable(ctx, c.cfg.HostID)
	if err != nil {
		return fmt.Errorf("list sweepable: %w", err)
	}
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, c.cfg.SweepConcurrency)
	)
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := c.sweepOne(ctx, it); err != nil && !errors.Is(err, ErrInvalidTransition) &&
				!errors.Is(err, ErrStaleGeneration) && !errors.Is(err, ErrNotFound) {
				c.log.Warn("sweep failed", "sandbox", it.Sandbox.ID, "cause", it.Cause, "err", err)
			}
		}()
	}
	wg.Wait()
	if len(items) > 0 {
		c.Kick()
	}

	now := c.clock.Now()
	c.mu.Lock()
	c.m.LastSweepAt = now
	gc := c.lastGC.IsZero() || now.Sub(c.lastGC) >= c.cfg.GCInterval
	if gc {
		c.lastGC = now
	}
	c.mu.Unlock()
	if gc {
		if n, err := c.store.GCDestroyed(ctx, c.cfg.DestroyedRetention); err != nil {
			c.log.Warn("gc of destroyed rows failed", "err", err)
		} else if n > 0 {
			c.log.Info("garbage-collected destroyed rows", "rows", n)
		}
	}
	return nil
}

func (c *Controller) sweepOne(ctx context.Context, it store.SweepItem) error {
	sb := it.Sandbox
	switch it.Cause {
	case store.SweepDeadline:
		switch sb.State {
		case core.StateCreating:
			// Cancel the create if this process is running it. If the
			// backend ignores the cancellation the row is still failed
			// here; the create goroutine destroys whatever it ends up with.
			c.cancelCreate(sb.ID)
			failed, err := c.store.Transition(ctx, store.TransitionParams{
				ID: sb.ID, Generation: sb.Generation, From: core.StateCreating, To: core.StateFailed,
				Reason: core.ReasonDeadlineExceeded,
			})
			if err != nil {
				return err
			}
			return c.recycle(ctx, failed, "", nil)
		case core.StateAssigned:
			return c.recycle(ctx, sb, core.ReasonDeadlineExceeded, nil)
		case core.StateRecycling:
			return c.teardown(sb)
		}
	case store.SweepIdle:
		reason, data := c.oomOr(ctx, sb, core.ReasonIdleTimeout)
		return c.recycle(ctx, sb, reason, data)
	case store.SweepMaxLifetime:
		reason, data := c.oomOr(ctx, sb, core.ReasonMaxLifetime)
		return c.recycle(ctx, sb, reason, data)
	case store.SweepWarmTTL:
		return c.recycle(ctx, sb, core.ReasonWarmTTL, nil)
	case store.SweepTemplateChanged:
		return c.recycle(ctx, sb, core.ReasonTemplateChanged, nil)
	}
	return nil
}

// oomOr returns oom_killed when the backend reports OOM kills for the
// sandbox's instance, and the fallback reason otherwise.
func (c *Controller) oomOr(ctx context.Context, sb core.Sandbox, fallback core.TerminationReason) (core.TerminationReason, map[string]any) {
	if sb.InstanceID == "" {
		return fallback, nil
	}
	stats, err := c.backend.Stats(ctx, sb.InstanceID)
	if err == nil && stats.OOMKills > 0 {
		return core.ReasonOOMKilled, map[string]any{"oom_kills": stats.OOMKills, "would_have_been": string(fallback)}
	}
	if err == nil || !errors.Is(err, runtime.ErrNotRunning) {
		return fallback, nil
	}
	// The instance is dead and took its counters with it; ask the backend
	// how it ended.
	inst, err := c.backend.Inspect(ctx, sb.InstanceID)
	if err != nil || inst.Running {
		return fallback, nil
	}
	return deathReason(inst, fallback)
}

// deathReason is the termination reason of a sandbox whose instance is no
// longer running: oom_killed when the backend has evidence of an OOM
// kill, otherwise fallback. The exit code is recorded either way, so the
// evidence travels with the verdict.
func deathReason(inst runtime.Instance, fallback core.TerminationReason) (core.TerminationReason, map[string]any) {
	data := map[string]any{"cause": "instance_not_running", "exit_code": inst.ExitCode}
	if !inst.OOMKilled {
		return fallback, data
	}
	if fallback != core.ReasonInstanceLost {
		data["would_have_been"] = string(fallback)
	}
	return core.ReasonOOMKilled, data
}
