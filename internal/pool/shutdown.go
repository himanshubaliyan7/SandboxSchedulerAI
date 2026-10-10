package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

// Shutdown is two steps before Stop, in this order:
//
//  1. Quiesce: no create is started from now on, and the ones in flight
//     are over (WARM, or failed and cleaned up) when it returns. Until
//     this has happened nothing else may be torn down: every release
//     frees a slot and wakes the refill loop, which would answer with a
//     new create that the shutdown then has to interrupt half-way.
//  2. Whatever the shutdown policy asks for: release the tenant sandboxes
//     (Release), or DestroyAll.
//
// Stop alone still works and leaves the same things behind as before; it
// cancels in-flight creates rather than waiting for them.

// pollInterval is how often Quiesce and DestroyAll look whether the
// goroutines they wait for are done. It is real time on purpose: they are
// called on shutdown, outside anything a test clock drives.
const pollInterval = 5 * time.Millisecond

func (c *Controller) quiesced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quiesce
}

func (c *Controller) inflight() (creating, recycling int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.creating), len(c.recycling)
}

// Quiesce stops the refill for good and returns when no create is in
// flight any more. Creates that are running are awaited, not cancelled: a
// create that is allowed to finish leaves a WARM sandbox or nothing,
// whereas one interrupted between "container created" and "container
// known to us" is exactly how a half-made container gets left behind. If
// ctx ends first the remaining creates are cancelled, their cleanup is
// awaited, and ctx's error is returned.
//
// After Quiesce, Refill is a no-op and Claim returns ErrStopped. Sweep,
// Reconcile, Release and the teardowns keep working.
func (c *Controller) Quiesce(ctx context.Context) error {
	c.mu.Lock()
	c.quiesce = true
	c.mu.Unlock()
	// A refill pass that was already running has seen the flag or
	// finishes inserting its rows; either way it is over after this.
	c.refillMu.Lock()
	//nolint:staticcheck // empty critical section: wait for the pass in progress
	c.refillMu.Unlock()

	cancelled := false
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		n, _ := c.inflight()
		if n == 0 {
			if cancelled {
				return ctx.Err()
			}
			c.log.Info("refill stopped; no create in flight")
			return nil
		}
		if !cancelled {
			select {
			case <-ctx.Done():
				// Out of time: cancel them. Each cancelled create fails its
				// row and tears down what it made, on its own context.
				c.mu.Lock()
				for _, op := range c.creating {
					op.cancel()
				}
				c.mu.Unlock()
				cancelled = true
				c.log.Warn("shutdown budget spent while creates were in flight; cancelled them", "creates", n)
			case <-t.C:
			}
			continue
		}
		<-t.C
	}
}

// DestroyAll destroys everything this host id owns: every sandbox row of
// the host that is not terminal is recycled (reason host_drain unless it
// already has one) and torn down, then every instance still labelled with
// the host id is destroyed and every sandbox run dir removed. It returns
// how many sandboxes it destroyed, and an error if something of this
// host's is still there afterwards. Call Quiesce first; it is for
// decommissioning a host and for tests.
func (c *Controller) DestroyAll(ctx context.Context) (int, error) {
	if !c.quiesced() {
		return 0, errors.New("pool: DestroyAll needs Quiesce first")
	}
	rows, err := c.store.ListActiveByHost(ctx, c.cfg.HostID)
	if err != nil {
		return 0, fmt.Errorf("list rows: %w", err)
	}
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, c.cfg.SweepConcurrency)
	)
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			var err error
			switch row.State {
			case core.StateRecycling:
				err = c.teardown(row)
			case core.StateFailed, core.StateUnknown:
				err = c.recycle(ctx, row, "", nil)
			default:
				err = c.recycle(ctx, row, core.ReasonHostDrain, map[string]any{"cause": "shutdown_policy_destroy"})
			}
			if err != nil && !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrStaleGeneration) && !errors.Is(err, ErrNotFound) {
				c.log.Warn("destroy all: sandbox", "sandbox", row.ID, "err", err)
			}
		}()
	}
	wg.Wait()
	// Teardowns started elsewhere (an asynchronous delete, the sweeper)
	// are skipped above as "already in flight": wait for those too.
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		if _, n := c.inflight(); n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return len(rows), fmt.Errorf("teardowns still in flight: %w", ctx.Err())
		case <-t.C:
		}
	}

	// Whatever still carries the host id has no live row by now.
	left, err := c.ownInstances(ctx)
	if err != nil {
		return len(rows), fmt.Errorf("list instances: %w", err)
	}
	for _, inst := range left {
		c.destroyOrphan(ctx, inst, "shutdown_policy_destroy")
	}
	if entries, err := os.ReadDir(c.cfg.RunDirBase); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), sandboxIDPrefix) {
				if err := os.RemoveAll(filepath.Join(c.cfg.RunDirBase, e.Name())); err != nil {
					c.log.Warn("destroy all: remove run dir", "dir", e.Name(), "err", err)
				}
			}
		}
	}
	if left, err = c.ownInstances(ctx); err != nil {
		return len(rows), fmt.Errorf("list instances: %w", err)
	}
	if len(left) > 0 {
		return len(rows), fmt.Errorf("%d instance(s) of host %s could not be destroyed", len(left), c.cfg.HostID)
	}
	if still, err := c.store.ListActiveByHost(ctx, c.cfg.HostID); err == nil && len(still) > 0 {
		return len(rows), fmt.Errorf("%d sandbox row(s) of host %s are not destroyed yet", len(still), c.cfg.HostID)
	}
	c.log.Info("destroyed everything owned by this host", "sandboxes", len(rows), "orphan_instances", len(left))
	return len(rows), nil
}
