// Package pool is the warm-pool controller for one host. It keeps a pool
// of pre-created sandboxes per template, hands them out with an atomic
// claim, and destroys every sandbox after use. The lifecycle is one-way:
// nothing in this package moves a sandbox back to WARM.
//
// Four level-triggered activities share one Controller:
//
//   - refill: per template, compare WARM+CREATING with the target and
//     start creates, bounded by MaxTotal, RefillRate and a circuit breaker;
//   - claim/release: the request path;
//   - sweep: enforce transient-state deadlines and the three timers;
//   - reconcile: converge the runtime's instances with the store's rows.
//
// Ownership of in-flight work is tracked in memory (creating, recycling)
// only to avoid duplicate effort; correctness rests on the store's
// conditional transitions, which make every stale actor lose.
package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Errors returned by the controller. The store's typed errors are
// re-exported so callers need not import the store package.
var (
	ErrPoolEmpty         = store.ErrPoolEmpty
	ErrQuotaExceeded     = store.ErrQuotaExceeded
	ErrStaleGeneration   = store.ErrStaleGeneration
	ErrNotFound          = store.ErrNotFound
	ErrInvalidTransition = store.ErrInvalidTransition
	// ErrStopped: the controller is stopped or stopping.
	ErrStopped = errors.New("pool: controller stopped")
	// ErrNetworkNotAllowed: the claim asked for a network mode the
	// template does not permit.
	ErrNetworkNotAllowed = errors.New("pool: network mode not allowed by template")
	// ErrNoEgress: the sandbox was claimed with network mode none, so it
	// has no egress proxy whose policy could be updated.
	ErrNoEgress = errors.New("pool: sandbox has no egress proxy attached")
)

// Backend is the isolation engine.
type Backend = runtime.Backend

// AgentConn talks to the in-sandbox agent through the socket in a
// sandbox's host run dir.
type AgentConn interface {
	// WaitReady blocks until the agent reports ready or ctx is done.
	WaitReady(ctx context.Context, hostRunDir string) error
	// Init injects the per-session configuration, once, at assignment.
	Init(ctx context.Context, hostRunDir string, req agentapi.InitRequest) error
}

// Egress is the per-sandbox egress proxy. Detach must be idempotent and
// must not fail for a sandbox that was never attached: the controller
// calls it for every sandbox it destroys.
type Egress interface {
	Attach(sandboxID, socketPath string, policy core.NetworkPolicy) error
	Update(sandboxID string, policy core.NetworkPolicy) error
	Detach(sandboxID string) error
}

// NopEgress is an Egress that does nothing, for deployments and tests
// without a proxy.
type NopEgress struct{}

func (NopEgress) Attach(string, string, core.NetworkPolicy) error { return nil }
func (NopEgress) Update(string, core.NetworkPolicy) error         { return nil }
func (NopEgress) Detach(string) error                             { return nil }

// Store is the part of the state store the controller needs. *store.Store
// satisfies it; pooltest.MemStore is an in-memory implementation.
type Store interface {
	ListTemplates(ctx context.Context) ([]core.Template, error)
	GetTemplate(ctx context.Context, id string) (core.Template, error)
	InsertCreating(ctx context.Context, n store.NewSandbox) (core.Sandbox, error)
	MarkWarm(ctx context.Context, id string, generation int64, instanceID string) (core.Sandbox, error)
	Claim(ctx context.Context, p store.ClaimParams) (core.Sandbox, error)
	Transition(ctx context.Context, p store.TransitionParams) (core.Sandbox, error)
	Touch(ctx context.Context, id string, generation int64) error
	SetNetworkPolicy(ctx context.Context, id string, generation int64, policy core.NetworkPolicy) error
	GetSandbox(ctx context.Context, id string) (core.Sandbox, error)
	CountByTemplateState(ctx context.Context, hostID string) (map[string]map[core.State]int, error)
	ListSweepable(ctx context.Context, hostID string) ([]store.SweepItem, error)
	ListActiveByHost(ctx context.Context, hostID string) ([]core.Sandbox, error)
	AppendEvent(ctx context.Context, e store.Event) error
	GCDestroyed(ctx context.Context, olderThan time.Duration) (int64, error)
}

var _ Store = (*store.Store)(nil)

// Config wires a Controller. Store, Backend, Agent, HostID and RunDirBase
// are required; everything else has a default.
type Config struct {
	Store   Store
	Backend Backend
	Agent   AgentConn
	Egress  Egress // default NopEgress
	Clock   Clock  // default RealClock
	Logger  *slog.Logger

	// HostID names this host in the sandboxes table and in every
	// instance's sbx.host_id label. It must be stable across restarts
	// (a new id orphans everything the old one made) and unique among the
	// schedulers that share a backend or a store: the controller only
	// touches instances and rows that carry it.
	HostID string
	// AgentEpoch identifies this run of the host agent; it is written to
	// every instance's sbx.agent_epoch label. Default: random per process.
	AgentEpoch string
	// RunDirBase is the directory holding one 0700 run dir per sandbox.
	// Keep it short: Unix socket paths are limited to 107 bytes and the
	// controller appends "/sb-<16 hex>/agent.sock" (30 bytes).
	RunDirBase string
	// AgentBinaryPath is passed through to runtime.Spec.
	AgentBinaryPath string

	RefillInterval    time.Duration // safety-net refill tick; default 1s
	SweepInterval     time.Duration // default 1s
	ReconcileInterval time.Duration // default 10s

	CreateTimeout  time.Duration // create + readiness budget; default core.CreatingDeadline
	InitTimeout    time.Duration // egress attach + agent init budget; default core.AssignedDeadline
	DestroyTimeout time.Duration // teardown budget; default core.RecyclingDeadline
	OpTimeout      time.Duration // one refill/sweep/reconcile pass; default 15s

	// CreateAttempts is how many times one create (instance create and
	// start, then agent readiness) is tried before the row is failed and
	// the circuit breaker counts a failure; default 3, 1 disables the
	// retry. All attempts share the row's CreateTimeout.
	// CreateRetryBackoff is the pause before the second attempt, doubling
	// for each further one; default 100ms.
	CreateAttempts     int
	CreateRetryBackoff time.Duration

	// ClaimWait is how long a claim waits on an empty pool when
	// ClaimOptions.Wait is zero; default 5s.
	ClaimWait time.Duration
	// MaxWaiters bounds each template's wait queue; default 256.
	MaxWaiters int

	// Circuit breaker: after BreakerThreshold consecutive create failures
	// of a template, creates stop for BreakerBaseBackoff, doubling on each
	// further failure up to BreakerMaxBackoff. After the backoff a single
	// probe create is allowed; one success closes the breaker.
	BreakerThreshold   int           // default 3
	BreakerBaseBackoff time.Duration // default 1s
	BreakerMaxBackoff  time.Duration // default 1m

	// AdoptProbeTimeout bounds the readiness probe of a WARM instance
	// inherited from a previous agent epoch; default 2s.
	AdoptProbeTimeout time.Duration
	// SweepConcurrency bounds parallel teardowns in one sweep; default 8.
	SweepConcurrency int
	// DestroyedRetention is how long DESTROYED rows are kept before the
	// sweeper garbage-collects them (events are kept); default 7 days.
	// GCInterval is how often it tries; default 1h.
	DestroyedRetention time.Duration
	GCInterval         time.Duration
}

func (c *Config) defaults() error {
	switch {
	case c.Store == nil:
		return errors.New("pool: Config.Store is required")
	case c.Backend == nil:
		return errors.New("pool: Config.Backend is required")
	case c.Agent == nil:
		return errors.New("pool: Config.Agent is required")
	case c.HostID == "":
		return errors.New("pool: Config.HostID is required")
	case runtime.ValidHostID(c.HostID) != nil:
		return fmt.Errorf("pool: %w", runtime.ValidHostID(c.HostID))
	case c.RunDirBase == "":
		return errors.New("pool: Config.RunDirBase is required")
	}
	if c.Egress == nil {
		c.Egress = NopEgress{}
	}
	if c.Clock == nil {
		c.Clock = RealClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.AgentEpoch == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("pool: generate agent epoch: %w", err)
		}
		c.AgentEpoch = hex.EncodeToString(b)
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.RefillInterval, time.Second)
	def(&c.SweepInterval, time.Second)
	def(&c.ReconcileInterval, 10*time.Second)
	def(&c.CreateTimeout, core.CreatingDeadline)
	def(&c.InitTimeout, core.AssignedDeadline)
	def(&c.DestroyTimeout, core.RecyclingDeadline)
	def(&c.OpTimeout, 15*time.Second)
	def(&c.ClaimWait, 5*time.Second)
	def(&c.CreateRetryBackoff, 100*time.Millisecond)
	if c.CreateAttempts <= 0 {
		c.CreateAttempts = 3
	}
	def(&c.BreakerBaseBackoff, time.Second)
	def(&c.BreakerMaxBackoff, time.Minute)
	def(&c.AdoptProbeTimeout, 2*time.Second)
	def(&c.DestroyedRetention, 7*24*time.Hour)
	def(&c.GCInterval, time.Hour)
	if c.MaxWaiters <= 0 {
		c.MaxWaiters = 256
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 3
	}
	if c.SweepConcurrency <= 0 {
		c.SweepConcurrency = 8
	}
	return nil
}

// Controller is the pool controller of one host. Create it with New.
type Controller struct {
	cfg     Config
	log     *slog.Logger
	clock   Clock
	store   Store
	backend Backend
	agent   AgentConn
	egress  Egress

	rootCtx context.Context // cancelled by Stop
	cancel  context.CancelFunc
	kick    chan struct{}

	lifeMu  sync.Mutex
	started bool
	stopped bool
	quiesce bool           // guarded by mu: no more creates (see Quiesce)
	wg      sync.WaitGroup // loops and every background goroutine

	refillMu    sync.Mutex // one refill pass at a time
	sweepMu     sync.Mutex
	reconcileMu sync.Mutex

	mu        sync.Mutex
	creating  map[string]*createOp // in-flight creates by sandbox id
	recycling map[string]struct{}  // in-flight teardowns by sandbox id
	breakers  map[string]*breaker  // by template id
	queues    map[string]*waitQueue
	adopted   map[string]struct{} // WARM sandboxes inherited from an older epoch and probed
	proxied   map[string]struct{} // sandboxes this process attached to the egress proxy
	foreign   map[string]struct{} // instance ids of other host ids already reported in the log
	counts    map[string]map[core.State]int
	templates []core.Template
	lastGC    time.Time
	m         counters
}

type createOp struct {
	templateID string
	cancel     context.CancelFunc
}

// New validates the configuration and returns a stopped controller.
func New(cfg Config) (*Controller, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{
		cfg: cfg, clock: cfg.Clock, store: cfg.Store, backend: cfg.Backend, agent: cfg.Agent, egress: cfg.Egress,
		log:     cfg.Logger.With("component", "pool", "host", cfg.HostID, "epoch", cfg.AgentEpoch),
		rootCtx: ctx, cancel: cancel,
		kick:      make(chan struct{}, 1),
		creating:  map[string]*createOp{},
		recycling: map[string]struct{}{},
		breakers:  map[string]*breaker{},
		queues:    map[string]*waitQueue{},
		adopted:   map[string]struct{}{},
		proxied:   map[string]struct{}{},
		foreign:   map[string]struct{}{},
		counts:    map[string]map[core.State]int{},
		m:         newCounters(),
	}, nil
}

// Start creates the run-dir base, runs one reconcile pass synchronously
// (so orphans from a previous run are gone and lost rows are failed
// before any claim is served), then starts the refill, sweep and
// reconcile loops. It returns an error if the first reconcile cannot read
// the store or the backend. Cancelling ctx is equivalent to Stop.
func (c *Controller) Start(ctx context.Context) error {
	c.lifeMu.Lock()
	if c.started || c.stopped {
		c.lifeMu.Unlock()
		return errors.New("pool: controller already started or stopped")
	}
	c.started = true
	c.lifeMu.Unlock()

	if err := os.MkdirAll(c.cfg.RunDirBase, 0o700); err != nil {
		return fmt.Errorf("pool: create run dir base: %w", err)
	}
	rctx, cancel := c.withTimeout(ctx, c.cfg.OpTimeout)
	err := c.Reconcile(rctx)
	cancel()
	if err != nil {
		return fmt.Errorf("pool: start-up reconcile: %w", err)
	}

	stop := context.AfterFunc(ctx, c.cancel)
	c.goAsync(func() { <-c.rootCtx.Done(); stop() })
	c.goAsync(func() { c.loop("refill", c.cfg.RefillInterval, c.kick, c.Refill) })
	c.goAsync(func() { c.loop("sweep", c.cfg.SweepInterval, nil, c.Sweep) })
	c.goAsync(func() { c.loop("reconcile", c.cfg.ReconcileInterval, nil, c.Reconcile) })
	c.Kick()
	c.log.Info("pool controller started", "run_dir_base", c.cfg.RunDirBase)
	return nil
}

// Stop cancels the loops and in-flight creates, lets in-flight teardowns
// finish, and waits for every background goroutine. WARM instances are
// left running; a later controller re-adopts them (see Reconcile). Claims
// waiting in a queue return ErrStopped. Stop is idempotent.
//
// A graceful shutdown calls Quiesce first (and DestroyAll, if nothing is
// to be left behind); see shutdown.go.
func (c *Controller) Stop() {
	c.lifeMu.Lock()
	already := c.stopped
	c.stopped = true
	c.lifeMu.Unlock()
	c.cancel()
	c.wg.Wait()
	if !already {
		c.log.Info("pool controller stopped")
	}
}

// goAsync runs f on a tracked goroutine unless the controller is stopped.
func (c *Controller) goAsync(f func()) bool {
	c.lifeMu.Lock()
	if c.stopped {
		c.lifeMu.Unlock()
		return false
	}
	c.wg.Add(1)
	c.lifeMu.Unlock()
	go func() {
		defer c.wg.Done()
		f()
	}()
	return true
}

func (c *Controller) loop(name string, every time.Duration, wake <-chan struct{}, pass func(context.Context) error) {
	for {
		select {
		case <-c.rootCtx.Done():
			return
		case <-wake:
		case <-c.clock.After(every):
		}
		ctx, cancel := c.withTimeout(c.rootCtx, c.cfg.OpTimeout)
		err := pass(ctx)
		cancel()
		if err != nil && c.rootCtx.Err() == nil {
			c.log.Warn("pool pass failed", "pass", name, "err", err)
		}
	}
}

// Kick wakes the refill loop. It never blocks.
func (c *Controller) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// RunDir is the host run dir of a sandbox; the agent socket is
// RunDir(id)/runtime.AgentSocket.
func (c *Controller) RunDir(sandboxID string) string {
	return filepath.Join(c.cfg.RunDirBase, sandboxID)
}

// HostID and AgentEpoch report the controller's identity.
func (c *Controller) HostID() string     { return c.cfg.HostID }
func (c *Controller) AgentEpoch() string { return c.cfg.AgentEpoch }

// withTimeout is context.WithTimeout on the injected clock. On expiry the
// context is cancelled with cause context.DeadlineExceeded.
func (c *Controller) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	stop := c.clock.AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	return ctx, func() { stop(); cancel(context.Canceled) }
}

func timedOut(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), context.DeadlineExceeded)
}

// cleanupCtx is for work that must finish even if the request or the
// controller is cancelled: marking rows failed and tearing instances down.
func (c *Controller) cleanupCtx() (context.Context, context.CancelFunc) {
	return c.withTimeout(context.Background(), c.cfg.DestroyTimeout)
}

const sandboxIDPrefix = "sb-"

func newSandboxID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pool: generate sandbox id: %w", err)
	}
	return sandboxIDPrefix + hex.EncodeToString(b), nil
}

// isBusy reports whether this process has a create or a teardown in
// flight for the sandbox.
func (c *Controller) isBusy(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, creating := c.creating[id]
	_, recycling := c.recycling[id]
	return creating || recycling
}

// busyRow is isBusy for a row read from the store. An in-flight create
// only shields a row that is still CREATING: once the row says WARM or
// later the create is over, even if its goroutine has not yet returned.
func (c *Controller) busyRow(sb core.Sandbox) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, creating := c.creating[sb.ID]
	_, recycling := c.recycling[sb.ID]
	return recycling || (creating && sb.State == core.StateCreating)
}

// recycle moves a row from its current state to RECYCLING with a reason
// (an empty reason keeps the one the row carries) and tears the instance
// down. A failed teardown is logged, not returned: the row stays
// RECYCLING and the sweeper retries it after the deadline. The returned
// error is the store's verdict on the transition, e.g. ErrStaleGeneration.
func (c *Controller) recycle(ctx context.Context, sb core.Sandbox, reason core.TerminationReason, data map[string]any) error {
	rec, err := c.fence(ctx, sb, reason, data)
	if err != nil {
		return err
	}
	if err := c.teardown(rec); err != nil {
		c.log.Warn("teardown failed; the sweeper will retry", "sandbox", rec.ID, "err", err)
	}
	return nil
}

// fence moves a row to RECYCLING. From that moment the sandbox is no
// longer its tenant's: the access token stops working, the row cannot be
// claimed or go back, and the RECYCLING deadline makes the sweeper finish
// the teardown if nobody else does.
func (c *Controller) fence(ctx context.Context, sb core.Sandbox, reason core.TerminationReason, data map[string]any) (core.Sandbox, error) {
	rec, err := c.store.Transition(ctx, store.TransitionParams{
		ID: sb.ID, Generation: sb.Generation, From: sb.State, To: core.StateRecycling,
		Reason: reason, Deadline: c.cfg.DestroyTimeout, Data: data,
	})
	if err != nil {
		return core.Sandbox{}, err
	}
	c.mu.Lock()
	c.m.terminations[rec.Reason]++
	c.mu.Unlock()
	c.log.Info("sandbox recycling", "sandbox", rec.ID, "template", rec.TemplateID, "tenant", rec.TenantID,
		"from", sb.State, "reason", rec.Reason)
	return rec, nil
}

// teardown releases everything a RECYCLING row holds on the host, in this
// order: destroy the instance, detach egress, remove the run dir; then it
// marks the row DESTROYED. It is idempotent and safe to retry. It runs on
// its own context so a cancelled request or a stopping controller does
// not leave a half-destroyed sandbox behind.
func (c *Controller) teardown(sb core.Sandbox) error {
	c.mu.Lock()
	if _, busy := c.recycling[sb.ID]; busy {
		c.mu.Unlock()
		return nil
	}
	c.recycling[sb.ID] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.recycling, sb.ID)
		delete(c.adopted, sb.ID)
		delete(c.proxied, sb.ID)
		c.mu.Unlock()
	}()

	ctx, cancel := c.cleanupCtx()
	defer cancel()

	var instances []string
	if sb.InstanceID != "" {
		instances = append(instances, sb.InstanceID)
	} else {
		// The create never reported an instance id (it failed or hung).
		// Look for a half-created instance by label.
		list, err := c.ownInstances(ctx)
		if err != nil {
			return fmt.Errorf("list instances: %w", err)
		}
		for _, inst := range list {
			if inst.SandboxID == sb.ID {
				instances = append(instances, inst.ID)
			}
		}
	}
	for _, id := range instances {
		if err := c.backend.Destroy(ctx, id); err != nil {
			return fmt.Errorf("destroy instance %s: %w", id, err)
		}
	}
	if err := c.egress.Detach(sb.ID); err != nil {
		c.log.Debug("egress detach failed", "sandbox", sb.ID, "err", err)
	}
	if err := os.RemoveAll(c.RunDir(sb.ID)); err != nil {
		c.log.Warn("remove run dir failed", "sandbox", sb.ID, "err", err)
	}
	_, err := c.store.Transition(ctx, store.TransitionParams{
		ID: sb.ID, Generation: sb.Generation, From: core.StateRecycling, To: core.StateDestroyed,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrNotFound) {
			return nil // already finished elsewhere
		}
		return fmt.Errorf("mark destroyed: %w", err)
	}
	c.mu.Lock()
	c.m.Destroyed++
	c.mu.Unlock()
	c.log.Info("sandbox destroyed", "sandbox", sb.ID, "reason", sb.Reason)
	c.Kick() // a slot under MaxTotal is free again
	return nil
}
