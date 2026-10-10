package pool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// ClaimOptions is what a claim may vary. The template itself is immutable.
type ClaimOptions struct {
	// Env is injected into the session through the in-sandbox agent.
	Env map[string]string
	// Network is the egress policy. Nil means the template's mode with no
	// allow or deny entries. Its mode may be the template's or a stricter
	// one (none < allowlist < full), never a looser one.
	Network *core.NetworkPolicy
	// AccessTokenHash is stored with the sandbox (SHA-256 of the
	// per-sandbox access token), if the caller issues one.
	AccessTokenHash []byte
	// Wait is how long to queue when the pool is empty. Zero means
	// Config.ClaimWait; negative means do not wait.
	Wait time.Duration
	// Lifetime overrides the template's max lifetime for this sandbox. It
	// can only shorten it: zero, or a value above the template's (when
	// the template has one), means the template's max lifetime.
	Lifetime time.Duration
}

// lifetime is the max lifetime a claim with opts gets from tmpl.
func lifetime(tmpl core.Template, opts ClaimOptions) time.Duration {
	max := tmpl.Timeouts.MaxLifetime
	if opts.Lifetime > 0 && (max <= 0 || opts.Lifetime < max) {
		return opts.Lifetime
	}
	return max
}

func networkRank(m core.NetworkMode) int {
	switch m {
	case core.NetworkNone, "":
		return 0
	case core.NetworkAllowlist:
		return 1
	case core.NetworkFull:
		return 2
	}
	return 3
}

func resolvePolicy(tmpl core.Template, want *core.NetworkPolicy) (core.NetworkPolicy, error) {
	mode := tmpl.Network
	if mode == "" {
		mode = core.NetworkNone
	}
	if want == nil {
		return core.NetworkPolicy{Mode: mode}, nil
	}
	p := *want
	if p.Mode == "" {
		p.Mode = mode
	}
	if networkRank(p.Mode) > networkRank(mode) {
		return core.NetworkPolicy{}, fmt.Errorf("%w: template %s allows %q, claim asked for %q", ErrNetworkNotAllowed, tmpl.ID, mode, p.Mode)
	}
	return p, nil
}

// Claim assigns a warm sandbox of a template to a tenant and returns it
// RUNNING. The steps are: the store's atomic claim (which also enforces
// the tenant quota), egress attach when the policy has a network, agent
// init with the session env, then ASSIGNED -> RUNNING. If anything fails
// after the store claim the sandbox is recycled, never returned to the
// pool, and the error is returned.
//
// When the pool is empty the claim waits in the template's bounded queue
// until a sandbox turns WARM or the wait expires, then returns
// ErrPoolEmpty. The queue serves tenants round-robin. To keep that fair a
// new claim does not try the store while others are queued; a consequence
// is that a tenant over its quota learns so only when its turn comes.
// Every claim kicks the refill loop, so the claimed sandbox is replaced
// immediately.
//
// Errors: ErrNotFound (template or tenant), ErrQuotaExceeded,
// ErrPoolEmpty, ErrNetworkNotAllowed, ErrStopped, ctx's error, or the
// wrapped cause of a post-assignment failure.
func (c *Controller) Claim(ctx context.Context, tenantID, templateID string, opts ClaimOptions) (core.Sandbox, error) {
	if c.rootCtx.Err() != nil || c.quiesced() {
		return core.Sandbox{}, ErrStopped
	}
	tmpl, err := c.store.GetTemplate(ctx, templateID)
	if err != nil {
		c.countClaim(err, false, 0)
		return core.Sandbox{}, err
	}
	policy, err := resolvePolicy(tmpl, opts.Network)
	if err != nil {
		c.countClaim(err, false, 0)
		return core.Sandbox{}, err
	}
	wait := opts.Wait
	if wait == 0 {
		wait = c.cfg.ClaimWait
	}

	start := c.clock.Now()
	sb, waited, err := c.claimRow(ctx, tenantID, tmpl, policy, opts, wait)
	c.Kick()
	if err != nil {
		c.countClaim(err, waited, c.clock.Now().Sub(start))
		return core.Sandbox{}, err
	}
	running, err := c.activate(ctx, sb, policy, opts)
	c.countClaim(err, waited, c.clock.Now().Sub(start))
	if err != nil {
		return core.Sandbox{}, err
	}
	c.log.Info("sandbox claimed", "sandbox", running.ID, "template", templateID, "tenant", tenantID,
		"generation", running.Generation, "waited", waited)
	return running, nil
}

// claimRow runs the store claim, queueing while the pool is empty.
func (c *Controller) claimRow(ctx context.Context, tenantID string, tmpl core.Template, policy core.NetworkPolicy,
	opts ClaimOptions, wait time.Duration) (sb core.Sandbox, waited bool, err error) {
	params := store.ClaimParams{
		TenantID: tenantID, TemplateID: tmpl.ID, TemplateVersion: tmpl.Version, HostID: c.cfg.HostID,
		AssignedDeadline: c.cfg.InitTimeout, MaxLifetime: lifetime(tmpl, opts),
		AccessTokenHash: opts.AccessTokenHash, NetworkPolicy: &policy,
	}
	var (
		deadline <-chan time.Time
		granted  bool // we were woken for a WARM sandbox and have not used the wake-up
	)
	if wait > 0 {
		deadline = c.clock.After(wait) // the wait is measured from the start of the claim
	}
	for {
		if granted || wait <= 0 || c.waiting(tmpl.ID) == 0 {
			sb, err = c.store.Claim(ctx, params)
			hadGrant := granted
			granted = false
			if err == nil {
				return sb, waited, nil
			}
			if !errors.Is(err, ErrPoolEmpty) {
				if hadGrant {
					c.grant(tmpl.ID) // let the next waiter have the sandbox we could not take
				}
				return core.Sandbox{}, waited, err
			}
			if wait <= 0 {
				return core.Sandbox{}, waited, err
			}
		}
		w := c.enqueue(tmpl.ID, tenantID)
		if w == nil {
			return core.Sandbox{}, waited, fmt.Errorf("%w: template %s: wait queue is full", ErrPoolEmpty, tmpl.ID)
		}
		waited = true
		// The refill pass creates for queued claims and wakes them if a
		// sandbox turned WARM between our failed claim and the enqueue.
		c.Kick()
		select {
		case <-w.ch:
			granted = true
			continue
		case <-deadline:
			err = fmt.Errorf("%w: template %s: waited %s", ErrPoolEmpty, tmpl.ID, wait)
		case <-ctx.Done():
			err = ctx.Err()
		case <-c.rootCtx.Done():
			err = ErrStopped
		}
		c.dequeue(tmpl.ID, w)
		return core.Sandbox{}, waited, err
	}
}

// activate finishes a claim: egress, agent init, RUNNING.
func (c *Controller) activate(ctx context.Context, sb core.Sandbox, policy core.NetworkPolicy, opts ClaimOptions) (core.Sandbox, error) {
	runDir := c.RunDir(sb.ID)
	ictx, cancel := c.withTimeout(ctx, c.cfg.InitTimeout)
	defer cancel()

	running, err := func() (core.Sandbox, error) {
		proxied := policy.Mode != core.NetworkNone && policy.Mode != ""
		if proxied {
			if err := c.egress.Attach(sb.ID, filepath.Join(runDir, runtime.EgressSocket), policy); err != nil {
				return core.Sandbox{}, fmt.Errorf("attach egress: %w", err)
			}
			c.mu.Lock()
			c.proxied[sb.ID] = struct{}{}
			c.mu.Unlock()
		}
		err := c.agent.Init(ictx, runDir, agentapi.InitRequest{
			SandboxID: sb.ID, Generation: sb.Generation, Env: opts.Env, EgressProxy: proxied,
		})
		if err != nil {
			if timedOut(ictx) {
				err = fmt.Errorf("%w (init timeout %s)", err, c.cfg.InitTimeout)
			}
			return core.Sandbox{}, fmt.Errorf("agent init: %w", err)
		}
		return c.store.Transition(ictx, store.TransitionParams{
			ID: sb.ID, Generation: sb.Generation, From: core.StateAssigned, To: core.StateRunning,
		})
	}()
	if err == nil {
		return running, nil
	}
	c.log.Warn("claim failed after assignment; recycling", "sandbox", sb.ID, "tenant", sb.TenantID, "err", err)
	dctx, dcancel := c.cleanupCtx()
	defer dcancel()
	if rerr := c.recycle(dctx, sb, core.ReasonError, map[string]any{"error": err.Error()}); rerr != nil &&
		!errors.Is(rerr, ErrInvalidTransition) {
		c.log.Warn("could not recycle after failed claim", "sandbox", sb.ID, "err", rerr)
	}
	c.Kick()
	return core.Sandbox{}, fmt.Errorf("pool: claim %s: %w", sb.ID, err)
}

func (c *Controller) countClaim(err error, waited bool, took time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil:
		c.m.Claims++
		if waited {
			c.m.ClaimsWaited++
		}
		c.m.ClaimWait.observe(took)
	case errors.Is(err, ErrPoolEmpty):
		c.m.ClaimsPoolEmpty++
	case errors.Is(err, ErrQuotaExceeded):
		c.m.ClaimsQuota++
	default:
		c.m.ClaimsFailed++
	}
}

func (c *Controller) waiting(templateID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q := c.queues[templateID]; q != nil {
		return q.len()
	}
	return 0
}

func (c *Controller) enqueue(templateID, tenantID string) *waiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queues[templateID]
	if q == nil {
		q = newWaitQueue(c.cfg.MaxWaiters)
		c.queues[templateID] = q
	}
	return q.push(tenantID)
}

// dequeue removes a waiter that gave up. If it had been granted in the
// meantime the grant is passed to the next waiter.
func (c *Controller) dequeue(templateID string, w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q := c.queues[templateID]; q != nil && !q.remove(w) {
		q.grant()
	}
}

func (c *Controller) grant(templateID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q := c.queues[templateID]; q != nil {
		q.grant()
	}
}

// Release ends a tenant's sandbox. It is fenced by generation: a caller
// holding an old generation gets ErrStaleGeneration and nothing happens.
// The row goes to RECYCLING with the reason (default "released"), then
// the instance is destroyed, egress detached and the run dir removed, and
// the row becomes DESTROYED. There is no path back to WARM. Releasing a
// sandbox that is already RECYCLING or DESTROYED is a no-op; releasing
// one that was never assigned is ErrInvalidTransition.
//
// Release returns once the row is RECYCLING and one teardown attempt has
// run. If the teardown failed the row stays RECYCLING and the sweeper
// retries; Release still returns nil because the release itself is durable.
func (c *Controller) Release(ctx context.Context, sandboxID string, generation int64, reason core.TerminationReason) error {
	return c.release(ctx, sandboxID, generation, reason, true)
}

// ReleaseAsync is Release without the wait for the teardown. It returns
// as soon as the sandbox is fenced: the row is RECYCLING (durably, so the
// access token is dead and the release cannot be lost) and the egress
// proxy is detached, so the sandbox has no way out any more. Killing and
// removing the instance, which is the slow part (about 90 ms for a
// Podman container, more under load), and removing the run dir then
// happen on a background goroutine of the controller. If that goroutine
// fails or the process dies first, the sweeper retries the teardown at
// the RECYCLING deadline and the reconciler finishes it after a restart,
// exactly as for a synchronous release whose teardown failed. Stop waits
// for teardowns that are in flight.
func (c *Controller) ReleaseAsync(ctx context.Context, sandboxID string, generation int64, reason core.TerminationReason) error {
	return c.release(ctx, sandboxID, generation, reason, false)
}

func (c *Controller) release(ctx context.Context, sandboxID string, generation int64, reason core.TerminationReason, wait bool) error {
	sb, err := c.store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if sb.Generation != generation {
		return fmt.Errorf("%w: sandbox %s is at generation %d, caller has %d", ErrStaleGeneration, sandboxID, sb.Generation, generation)
	}
	switch sb.State {
	case core.StateRecycling, core.StateDestroyed:
		return nil
	case core.StateCreating, core.StateWarm:
		return fmt.Errorf("%w: sandbox %s is %s and belongs to no tenant", ErrInvalidTransition, sandboxID, sb.State)
	}
	if reason == "" {
		reason = core.ReasonReleased
	}
	rec, err := c.fence(context.WithoutCancel(ctx), sb, reason, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.m.Releases++
	c.mu.Unlock()
	finish := func() {
		if err := c.teardown(rec); err != nil {
			c.log.Warn("teardown failed; the sweeper will retry", "sandbox", rec.ID, "err", err)
		}
		c.Kick()
	}
	if wait {
		finish()
		return nil
	}
	// Cut the network before answering; teardown detaches again, which is
	// a no-op.
	if err := c.egress.Detach(rec.ID); err != nil {
		c.log.Debug("egress detach failed", "sandbox", rec.ID, "err", err)
	}
	if !c.goAsync(finish) {
		finish() // stopping: no background work is accepted any more
	}
	return nil
}

// Touch records activity on a sandbox, resetting its idle timer. Callers
// on a hot path should throttle it.
func (c *Controller) Touch(ctx context.Context, sandboxID string, generation int64) error {
	return c.store.Touch(ctx, sandboxID, generation)
}

// UpdateNetwork replaces the egress policy of a live sandbox, in the
// proxy and in the store. The mode must stay within the template's. It
// may be "none", which locks a sandbox down after it has, say, installed
// its dependencies: the proxy stays attached and refuses everything, and
// a later update can open it again. A sandbox that was claimed with mode
// "none" has no proxy (and no bridge to one inside it), so it cannot be
// given a network afterwards: that is ErrNoEgress. Attachments are
// remembered in memory only, which is sound because a restarted
// controller ends every tenant session it inherits (see Reconcile).
func (c *Controller) UpdateNetwork(ctx context.Context, sandboxID string, generation int64, policy core.NetworkPolicy) error {
	sb, err := c.store.GetSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}
	if sb.Generation != generation {
		return fmt.Errorf("%w: sandbox %s", ErrStaleGeneration, sandboxID)
	}
	tmpl, err := c.store.GetTemplate(ctx, sb.TemplateID)
	if err != nil {
		return err
	}
	resolved, err := resolvePolicy(tmpl, &policy)
	if err != nil {
		return err
	}
	c.mu.Lock()
	_, attached := c.proxied[sandboxID]
	c.mu.Unlock()
	if !attached {
		return fmt.Errorf("%w: sandbox %s", ErrNoEgress, sandboxID)
	}
	if err := c.egress.Update(sandboxID, resolved); err != nil {
		return fmt.Errorf("pool: update egress of %s: %w", sandboxID, err)
	}
	return c.store.SetNetworkPolicy(ctx, sandboxID, generation, resolved)
}
