package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// createGeneration is the generation an instance was labelled with when
// it was created. A row is inserted at generation 0 and the claim bumps
// it exactly once (sandbox ids are never reused), so the label of a
// claimed sandbox's instance is one behind the row.
func createGeneration(sb core.Sandbox) int64 {
	if sb.AssignedAt == nil {
		return sb.Generation
	}
	return sb.Generation - 1
}

// Reconcile runs one level-triggered pass that converges what the runtime
// has with what the store says this host should have. Desired is the
// host's non-terminal rows; actual is the part of Backend.List that
// carries this controller's host id (see ownInstances). Instances of
// another host id, and instances with no host id at all, are never
// inspected, adopted or destroyed here.
//
// Instances:
//   - no row, or the row is terminal or belongs to another host: destroyed
//     as an orphan (it carries this host id, so it is this host's to reap);
//   - generation label or instance id disagrees with the row: destroyed as
//     an orphan (the row is then handled as having no instance);
//   - not running while its row is WARM, ASSIGNED or RUNNING: the row is
//     failed with instance_lost, or oom_killed if the stats say so.
//
// Rows:
//   - no instance: FAILED (instance_lost) -> RECYCLING -> DESTROYED;
//   - FAILED or UNKNOWN: RECYCLING -> DESTROYED;
//   - RECYCLING with no teardown in flight (a crash interrupted it): the
//     teardown is finished.
//
// Agent epochs. After a restart the controller has a new epoch and the
// instances it finds carry the old one. The decision is:
//   - a WARM row whose instance is running with the matching generation
//     is re-adopted, after one readiness probe of its agent. It holds no
//     tenant data and no per-process state, so it is as good as new. It
//     keeps its old epoch label. If the probe fails it is failed with
//     instance_lost.
//   - every other old-epoch instance is destroyed and its row recycled
//     with reason host_drain. That includes RUNNING and PAUSED sessions:
//     the egress attachment and the streams of the previous process are
//     gone, and silently keeping a session whose network policy is no
//     longer enforced would be worse than ending it. It also includes
//     CREATING and ASSIGNED rows, whose owner died mid-operation.
//
// Adoption is remembered for the life of this process, so an adopted
// sandbox that is later claimed here is treated like any other session of
// this epoch even though its instance label still names the old one. (It
// would be drained by the next restart, like every session.)
//
// An instance is never adopted as WARM on the strength of its labels
// alone; the row must say WARM with the matching generation.
//
// Rows and instances this process is creating or tearing down right now
// are skipped; an instance with no row in the snapshot is re-checked
// against the store before it is destroyed, because it may have been
// created after the snapshot. The pass also removes leftover run dirs.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()

	err := c.reconcile(ctx)
	c.mu.Lock()
	c.m.LastReconcileAt = c.clock.Now()
	if err != nil {
		c.m.ReconcileFailures++
	}
	c.mu.Unlock()
	return err
}

func (c *Controller) reconcile(ctx context.Context) error {
	rows, err := c.store.ListActiveByHost(ctx, c.cfg.HostID)
	if err != nil {
		return fmt.Errorf("list rows: %w", err)
	}
	instances, err := c.ownInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	byID := make(map[string]core.Sandbox, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	hasInstance := map[string]bool{}
	handled := map[string]bool{}
	changed := false

	for _, inst := range instances {
		row, ok := byID[inst.SandboxID]
		if !ok {
			cur, err := c.store.GetSandbox(ctx, inst.SandboxID)
			switch {
			case err == nil && !cur.State.Terminal() && cur.HostID == c.cfg.HostID:
				continue // created after the snapshot
			case err != nil && !errors.Is(err, ErrNotFound):
				c.log.Warn("reconcile: could not check instance", "instance", inst.ID, "err", err)
				continue
			}
			c.destroyOrphan(ctx, inst, "no_live_row")
			continue
		}
		if c.busyRow(row) {
			hasInstance[row.ID] = true
			continue
		}
		if inst.Generation != createGeneration(row) || (row.InstanceID != "" && row.InstanceID != inst.ID) {
			c.destroyOrphan(ctx, inst, "generation_or_instance_mismatch")
			continue
		}
		hasInstance[row.ID] = true
		switch row.State {
		case core.StateFailed, core.StateUnknown, core.StateRecycling:
			continue // handled with the rows below
		}
		switch {
		case inst.AgentEpoch != c.cfg.AgentEpoch && !c.isAdopted(row.ID):
			if row.State == core.StateWarm && inst.Running {
				if c.adopt(ctx, row) {
					continue
				}
				c.lose(ctx, row, core.ReasonInstanceLost, map[string]any{"cause": "agent_not_ready_after_restart"})
			} else if err := c.recycle(ctx, row, core.ReasonHostDrain, map[string]any{
				"cause": "agent_epoch_changed", "instance_epoch": inst.AgentEpoch}); err != nil {
				c.log.Warn("reconcile: recycle old-epoch sandbox", "sandbox", row.ID, "err", err)
			}
			handled[row.ID], changed = true, true
		case !inst.Running && (row.State == core.StateWarm || row.State == core.StateAssigned || row.State == core.StateRunning):
			reason, data := c.diedOf(ctx, inst)
			c.lose(ctx, row, reason, data)
			handled[row.ID], changed = true, true
		}
	}

	for _, row := range rows {
		if handled[row.ID] || c.busyRow(row) {
			continue
		}
		switch row.State {
		case core.StateFailed, core.StateUnknown:
			if err := c.recycle(ctx, row, "", nil); err != nil {
				c.log.Warn("reconcile: recycle failed row", "sandbox", row.ID, "err", err)
			}
			changed = true
		case core.StateRecycling:
			if err := c.teardown(row); err != nil {
				c.log.Warn("reconcile: finish teardown", "sandbox", row.ID, "err", err)
			}
			changed = true
		default:
			if !hasInstance[row.ID] {
				c.lose(ctx, row, core.ReasonInstanceLost, map[string]any{"cause": "no_instance"})
				changed = true
			}
		}
	}

	c.removeStaleRunDirs(ctx, byID)
	if changed {
		c.Kick()
	}
	return nil
}

// diedOf says why an instance that List reported as not running ended.
// List may not know about an OOM kill, so the instance is inspected.
func (c *Controller) diedOf(ctx context.Context, inst runtime.Instance) (core.TerminationReason, map[string]any) {
	if stats, err := c.backend.Stats(ctx, inst.ID); err == nil && stats.OOMKills > 0 {
		return core.ReasonOOMKilled, map[string]any{"cause": "instance_not_running", "oom_kills": stats.OOMKills}
	}
	if full, err := c.backend.Inspect(ctx, inst.ID); err == nil && !full.Running {
		inst = full
	}
	return deathReason(inst, core.ReasonInstanceLost)
}

// CheckInstance looks at one tenant-owned sandbox's instance right now,
// without waiting for the next reconcile pass, and if the instance is
// gone or no longer running fails the sandbox with instance_lost or
// oom_killed and recycles it. It reports whether the sandbox was lost.
// The request path calls it when the in-sandbox agent stops answering, so
// that a sandbox killed by the OOM killer is reported as such within
// milliseconds. It is fenced by generation like every other transition.
func (c *Controller) CheckInstance(ctx context.Context, sandboxID string, generation int64) (lost bool, err error) {
	row, err := c.store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	if row.Generation != generation {
		return false, fmt.Errorf("%w: sandbox %s", ErrStaleGeneration, sandboxID)
	}
	switch row.State {
	case core.StateAssigned, core.StateRunning, core.StatePaused:
	default:
		return false, nil
	}
	if row.InstanceID == "" || c.busyRow(row) {
		return false, nil
	}
	inst, err := c.backend.Inspect(ctx, row.InstanceID)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		c.lose(ctx, row, core.ReasonInstanceLost, map[string]any{"cause": "no_instance"})
	case err != nil:
		return false, err
	case inst.Running:
		return false, nil
	default:
		reason, data := c.diedOf(ctx, inst)
		c.lose(ctx, row, reason, data)
	}
	c.Kick()
	return true, nil
}

// ownInstances lists the backend's instances and returns those labelled
// with this controller's host id. It is the only way this package reads
// Backend.List: several schedulers may share one backend (two servers of
// the same Podman user, or a server and a test run), each with its own
// store, and an instance of another scheduler has no row here, so without
// this filter it would look exactly like an orphan.
//
// Everything else is left strictly alone and reported once per instance:
// at debug level when it names another host id, and as a warning when it
// has no host id at all. The latter is a container made by a version that
// did not label ownership (or by hand); nobody can tell whose it is, so it
// is not destroyed, and the operator is told how to remove it.
func (c *Controller) ownInstances(ctx context.Context) ([]runtime.Instance, error) {
	all, err := c.backend.List(ctx)
	if err != nil {
		return nil, err
	}
	own := make([]runtime.Instance, 0, len(all))
	var foreign []runtime.Instance
	for _, inst := range all {
		if inst.HostID == c.cfg.HostID {
			own = append(own, inst)
		} else {
			foreign = append(foreign, inst)
		}
	}
	// Remember what was reported, and forget instances that are gone so
	// the set stays as small as the backend's list.
	c.mu.Lock()
	seen := make(map[string]struct{}, len(foreign))
	var fresh []runtime.Instance
	for _, inst := range foreign {
		if _, ok := c.foreign[inst.ID]; !ok {
			fresh = append(fresh, inst)
		}
		seen[inst.ID] = struct{}{}
	}
	c.foreign = seen
	c.mu.Unlock()
	for _, inst := range fresh {
		if inst.HostID == "" {
			c.log.Warn("instance has no "+runtime.LabelHostID+" label; leaving it alone (if it is a leftover of an older version, remove it by hand)",
				"instance", inst.ID, "sandbox", inst.SandboxID, "running", inst.Running)
		} else {
			c.log.Debug("instance belongs to another host id; leaving it alone",
				"instance", inst.ID, "sandbox", inst.SandboxID, "instance_host", inst.HostID)
		}
	}
	return own, nil
}

// destroyOrphan destroys an instance of this host id that no live row
// accounts for.
func (c *Controller) destroyOrphan(ctx context.Context, inst runtime.Instance, why string) {
	if inst.HostID != c.cfg.HostID {
		// Not reachable through ownInstances; kept so that no future
		// caller can destroy somebody else's instance by mistake.
		c.log.Error("reconcile: refusing to destroy an instance of another host id", "instance", inst.ID, "instance_host", inst.HostID)
		return
	}
	if err := c.backend.Destroy(ctx, inst.ID); err != nil {
		c.log.Warn("reconcile: destroy orphan", "instance", inst.ID, "sandbox", inst.SandboxID, "err", err)
		return
	}
	c.mu.Lock()
	c.m.OrphansDestroyed++
	c.m.terminations[core.ReasonOrphan]++
	c.mu.Unlock()
	c.log.Warn("reconcile: destroyed orphan instance", "instance", inst.ID, "sandbox", inst.SandboxID,
		"generation", inst.Generation, "instance_epoch", inst.AgentEpoch, "why", why)
	if inst.SandboxID == "" {
		return
	}
	if err := c.store.AppendEvent(ctx, store.Event{
		SandboxID: inst.SandboxID, Type: "orphan_destroyed", Reason: core.ReasonOrphan,
		Data: map[string]any{"instance_id": inst.ID, "why": why, "instance_generation": inst.Generation, "instance_epoch": inst.AgentEpoch},
	}); err != nil {
		c.log.Warn("reconcile: record orphan event", "instance", inst.ID, "err", err)
	}
}

// lose fails a row whose instance is gone or dead and recycles it.
func (c *Controller) lose(ctx context.Context, row core.Sandbox, reason core.TerminationReason, data map[string]any) {
	failed, err := c.store.Transition(ctx, store.TransitionParams{
		ID: row.ID, Generation: row.Generation, From: row.State, To: core.StateFailed, Reason: reason, Data: data,
	})
	if err != nil {
		if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrStaleGeneration) && !errors.Is(err, ErrNotFound) {
			c.log.Warn("reconcile: mark failed", "sandbox", row.ID, "err", err)
		}
		return
	}
	c.mu.Lock()
	c.m.InstancesLost++
	c.mu.Unlock()
	c.log.Warn("reconcile: sandbox lost", "sandbox", row.ID, "was", row.State, "tenant", row.TenantID, "reason", reason)
	if err := c.recycle(ctx, failed, "", nil); err != nil {
		c.log.Warn("reconcile: recycle lost sandbox", "sandbox", row.ID, "err", err)
	}
}

func (c *Controller) isAdopted(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.adopted[id]
	return ok
}

// adopt probes a WARM instance inherited from an older agent epoch once
// and remembers the verdict for the life of this process.
func (c *Controller) adopt(ctx context.Context, row core.Sandbox) bool {
	pctx, cancel := c.withTimeout(ctx, c.cfg.AdoptProbeTimeout)
	err := c.agent.WaitReady(pctx, c.RunDir(row.ID))
	cancel()
	if err != nil {
		c.log.Warn("reconcile: inherited warm sandbox is not ready", "sandbox", row.ID, "err", err)
		return false
	}
	c.mu.Lock()
	c.adopted[row.ID] = struct{}{}
	c.m.Adopted++
	c.mu.Unlock()
	c.log.Info("reconcile: re-adopted warm sandbox from an older epoch", "sandbox", row.ID, "instance", row.InstanceID)
	return true
}

// removeStaleRunDirs deletes run dirs whose sandbox is gone.
func (c *Controller) removeStaleRunDirs(ctx context.Context, live map[string]core.Sandbox) {
	entries, err := os.ReadDir(c.cfg.RunDirBase)
	if err != nil {
		return
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !strings.HasPrefix(id, sandboxIDPrefix) {
			continue
		}
		if _, ok := live[id]; ok || c.isBusy(id) {
			continue
		}
		cur, err := c.store.GetSandbox(ctx, id)
		if (err == nil && !cur.State.Terminal()) || (err != nil && !errors.Is(err, ErrNotFound)) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.cfg.RunDirBase, id)); err != nil {
			c.log.Warn("reconcile: remove stale run dir", "dir", id, "err", err)
		} else {
			c.log.Info("reconcile: removed stale run dir", "dir", id)
		}
	}
}
