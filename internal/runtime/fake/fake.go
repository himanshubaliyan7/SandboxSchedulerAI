// Package fake is an in-memory runtime.Backend for tests. It creates no
// processes; it only keeps a table of instances and lets a test inject
// latency, errors, hangs and vanished instances. It is safe for
// concurrent use.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// ErrInjected is the default error returned by injected failures.
var ErrInjected = errors.New("fake: injected failure")

// Counters is a snapshot of how often each operation was called.
type Counters struct {
	Creates        int // calls to Create
	CreateFailures int // Create calls that returned an error (including hangs cut short by the context)
	Destroys       int // calls to Destroy
	DestroyedLive  int // Destroy calls that removed an existing instance
	Lists          int
	Inspects       int
	StatsCalls     int
	Pauses         int
	Unpauses       int
}

// Backend implements runtime.Backend in memory.
type Backend struct {
	mu        sync.Mutex
	seq       int
	instances map[string]*entry
	destroyed map[string]bool // instance ids that were destroyed
	counters  Counters

	createLatency time.Duration
	createErr     error // persistent create error
	failNext      int   // fail the next N creates
	failNextErr   error
	createHang    bool // Create blocks until its context is done
	hangNext      int  // hang only the next N creates
	destroyErr    error
	destroyGate   chan struct{} // while non-nil, Destroy blocks until it is closed
	listErr       error
	caps          runtime.Capabilities
	now           func() time.Time
	leftovers     []string // sandbox ids with a leftover host process
}

// AddLeftover records a leftover host process of a sandbox (see
// runtime.LeftoverReaper): something Destroy has already forgotten.
func (b *Backend) AddLeftover(sandboxID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.leftovers = append(b.leftovers, sandboxID)
}

// Leftovers returns the sandbox ids whose leftover is still there.
func (b *Backend) Leftovers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.leftovers...)
}

// ReapLeftovers implements runtime.LeftoverReaper.
func (b *Backend) ReapLeftovers(ctx context.Context, owned func(sandboxID string) bool) (int, error) {
	b.mu.Lock()
	all := append([]string(nil), b.leftovers...)
	b.mu.Unlock()
	var keep []string
	n := 0
	for _, id := range all {
		if owned(id) { // called without the lock: it reads the store
			n++
		} else {
			keep = append(keep, id)
		}
	}
	b.mu.Lock()
	b.leftovers = keep
	b.mu.Unlock()
	return n, nil
}

type entry struct {
	inst      runtime.Instance
	stats     runtime.Stats
	paused    bool
	statsGone bool // Stats returns runtime.ErrNotRunning
}

var _ runtime.Backend = (*Backend)(nil)

// New returns an empty fake backend.
func New() *Backend {
	return &Backend{
		instances: map[string]*entry{},
		destroyed: map[string]bool{},
		caps: runtime.Capabilities{
			Pause:         true,
			SharedKernel:  true,
			Rootless:      true,
			NetworkModes:  []core.NetworkMode{core.NetworkNone, core.NetworkAllowlist, core.NetworkFull},
			RuntimeLabels: []string{"crun"},
		},
		now: time.Now,
	}
}

// --- configuration -------------------------------------------------------

// SetCreateLatency makes every Create take d (real time) before returning.
func (b *Backend) SetCreateLatency(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createLatency = d
}

// SetCreateError makes every Create fail with err until it is cleared with nil.
func (b *Backend) SetCreateError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createErr = err
}

// FailNextCreates makes the next n Create calls fail. A nil err means ErrInjected.
func (b *Backend) FailNextCreates(n int, err error) {
	if err == nil {
		err = ErrInjected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failNext, b.failNextErr = n, err
}

// SetCreateHang makes every Create block until its context is cancelled.
func (b *Backend) SetCreateHang(hang bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createHang = hang
}

// HangNextCreates makes only the next n Create calls block until their
// context is cancelled.
func (b *Backend) HangNextCreates(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hangNext = n
}

// SetDestroyError makes every Destroy fail with err until cleared with nil.
func (b *Backend) SetDestroyError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroyErr = err
}

// HoldDestroys makes every Destroy block until the returned function is
// called (or the Destroy's context is done). It stands in for a slow
// container removal.
func (b *Backend) HoldDestroys() (release func()) {
	gate := make(chan struct{})
	b.mu.Lock()
	b.destroyGate = gate
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			if b.destroyGate == gate {
				b.destroyGate = nil
			}
			b.mu.Unlock()
			close(gate)
		})
	}
}

// SetListError makes every List fail with err until cleared with nil.
func (b *Backend) SetListError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listErr = err
}

// SetCapabilities replaces the reported capabilities.
func (b *Backend) SetCapabilities(c runtime.Capabilities) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.caps = c
}

// SetNow replaces the time source used for Instance.CreatedAt.
func (b *Backend) SetNow(now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
}

// --- fault injection on existing instances -------------------------------

// Vanish removes an instance behind the caller's back, as if the container
// had been removed out of band. It reports whether the instance existed.
func (b *Backend) Vanish(instanceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.instances[instanceID]
	delete(b.instances, instanceID)
	return ok
}

// VanishSandbox removes every instance of a sandbox and returns how many
// were removed.
func (b *Backend) VanishSandbox(sandboxID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for id, e := range b.instances {
		if e.inst.SandboxID == sandboxID {
			delete(b.instances, id)
			n++
		}
	}
	return n
}

// StopInstance leaves the instance listed but not running, like an exited
// container.
func (b *Backend) StopInstance(instanceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.instances[instanceID]
	if ok {
		e.inst.Running = false
	}
	return ok
}

// KillInstance leaves the instance listed but not running, with the given
// exit code and OOM verdict, and with its counters gone: Stats then
// returns runtime.ErrNotRunning, like a dead container whose cgroup was
// removed.
func (b *Backend) KillInstance(instanceID string, exitCode int, oomKilled bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.instances[instanceID]
	if ok {
		e.inst.Running = false
		e.inst.ExitCode = exitCode
		e.inst.OOMKilled = oomKilled
		e.statsGone = true
	}
	return ok
}

// SetStats sets what Stats returns for an instance.
func (b *Backend) SetStats(instanceID string, s runtime.Stats) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.instances[instanceID]
	if ok {
		e.stats = s
	}
	return ok
}

// AddInstance injects an instance that no Create call produced (an orphan
// from the point of view of a controller). An empty ID is assigned one.
func (b *Backend) AddInstance(inst runtime.Instance) runtime.Instance {
	b.mu.Lock()
	defer b.mu.Unlock()
	if inst.ID == "" {
		b.seq++
		inst.ID = fmt.Sprintf("fake-%06d", b.seq)
	}
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = b.now()
	}
	b.instances[inst.ID] = &entry{inst: inst}
	return inst
}

// --- inspection ----------------------------------------------------------

// Counters returns a snapshot of the call counters.
func (b *Backend) Counters() Counters {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counters
}

// Len is the number of live instances.
func (b *Backend) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.instances)
}

// WasDestroyed reports whether Destroy was called for a live instance with this id.
func (b *Backend) WasDestroyed(instanceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.destroyed[instanceID]
}

// BySandbox returns the live instance of a sandbox, if any.
func (b *Backend) BySandbox(sandboxID string) (runtime.Instance, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.instances {
		if e.inst.SandboxID == sandboxID {
			return e.inst, true
		}
	}
	return runtime.Instance{}, false
}

// --- runtime.Backend -----------------------------------------------------

func (b *Backend) Name() string { return "fake" }

func (b *Backend) Capabilities() runtime.Capabilities {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.caps
}

func (b *Backend) Create(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	b.mu.Lock()
	b.counters.Creates++
	latency := b.createLatency
	hang := b.createHang
	if b.hangNext > 0 {
		b.hangNext--
		hang = true
	}
	err := b.createErr
	if err == nil && b.failNext > 0 {
		b.failNext--
		err = b.failNextErr
	}
	b.mu.Unlock()

	fail := func(err error) (runtime.Instance, error) {
		b.mu.Lock()
		b.counters.CreateFailures++
		b.mu.Unlock()
		return runtime.Instance{}, err
	}

	if hang {
		<-ctx.Done()
		return fail(ctx.Err())
	}
	if latency > 0 {
		t := time.NewTimer(latency)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return fail(ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err != nil {
		return fail(err)
	}
	if spec.SandboxID == "" {
		return fail(errors.New("fake: spec has no sandbox id"))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	inst := runtime.Instance{
		ID:         fmt.Sprintf("fake-%06d", b.seq),
		SandboxID:  spec.SandboxID,
		HostID:     spec.HostID,
		Generation: spec.Generation,
		AgentEpoch: spec.AgentEpoch,
		TemplateID: spec.Template.ID,
		Running:    true,
		CreatedAt:  b.now(),
	}
	b.instances[inst.ID] = &entry{inst: inst}
	return inst, nil
}

func (b *Backend) Destroy(ctx context.Context, instanceID string) error {
	b.mu.Lock()
	gate := b.destroyGate
	b.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.Destroys++
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.destroyErr != nil {
		return b.destroyErr
	}
	if _, ok := b.instances[instanceID]; ok {
		delete(b.instances, instanceID)
		b.destroyed[instanceID] = true
		b.counters.DestroyedLive++
	}
	return nil // idempotent
}

func (b *Backend) List(ctx context.Context) ([]runtime.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.Lists++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.listErr != nil {
		return nil, b.listErr
	}
	out := make([]runtime.Instance, 0, len(b.instances))
	for _, e := range b.instances {
		out = append(out, e.inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (b *Backend) Inspect(ctx context.Context, instanceID string) (runtime.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.Inspects++
	e, ok := b.instances[instanceID]
	if !ok {
		return runtime.Instance{}, runtime.ErrNotFound
	}
	return e.inst, nil
}

func (b *Backend) Stats(ctx context.Context, instanceID string) (runtime.Stats, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.StatsCalls++
	e, ok := b.instances[instanceID]
	if !ok {
		return runtime.Stats{}, runtime.ErrNotFound
	}
	if e.statsGone {
		return runtime.Stats{}, runtime.ErrNotRunning
	}
	return e.stats, nil
}

func (b *Backend) Pause(ctx context.Context, instanceID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.Pauses++
	if !b.caps.Pause {
		return runtime.ErrUnsupported
	}
	e, ok := b.instances[instanceID]
	if !ok {
		return runtime.ErrNotFound
	}
	e.paused = true
	return nil
}

func (b *Backend) Unpause(ctx context.Context, instanceID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters.Unpauses++
	if !b.caps.Pause {
		return runtime.ErrUnsupported
	}
	e, ok := b.instances[instanceID]
	if !ok {
		return runtime.ErrNotFound
	}
	e.paused = false
	return nil
}

// Paused reports whether the instance is currently paused.
func (b *Backend) Paused(instanceID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.instances[instanceID]
	return ok && e.paused
}
