package pooltest

import (
	"context"
	"sync"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

// FakeAgent is a pool.AgentConn. By default every call succeeds at once.
type FakeAgent struct {
	mu        sync.Mutex
	readyErr  error
	initErr   error
	initHang  bool
	readyHook func(ctx context.Context, hostRunDir string) error
	ready     []string
	inits     []InitCall
}

// InitCall records one Init.
type InitCall struct {
	HostRunDir string
	Req        agentapi.InitRequest
}

// SetReadyError makes WaitReady fail until cleared with nil.
func (a *FakeAgent) SetReadyError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.readyErr = err
}

// SetReadyHook replaces WaitReady's behaviour.
func (a *FakeAgent) SetReadyHook(h func(ctx context.Context, hostRunDir string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.readyHook = h
}

// SetInitError makes Init fail until cleared with nil.
func (a *FakeAgent) SetInitError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initErr = err
}

// SetInitHang makes Init block until its context is done.
func (a *FakeAgent) SetInitHang(hang bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initHang = hang
}

func (a *FakeAgent) WaitReady(ctx context.Context, hostRunDir string) error {
	a.mu.Lock()
	a.ready = append(a.ready, hostRunDir)
	err, hook := a.readyErr, a.readyHook
	a.mu.Unlock()
	if hook != nil {
		return hook(ctx, hostRunDir)
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (a *FakeAgent) Init(ctx context.Context, hostRunDir string, req agentapi.InitRequest) error {
	a.mu.Lock()
	a.inits = append(a.inits, InitCall{hostRunDir, req})
	err, hang := a.initErr, a.initHang
	a.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

// Inits returns the Init calls so far.
func (a *FakeAgent) Inits() []InitCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]InitCall(nil), a.inits...)
}

// ReadyCalls is the number of WaitReady calls so far.
func (a *FakeAgent) ReadyCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.ready)
}

// FakeEgress is a pool.Egress that records attachments.
type FakeEgress struct {
	mu        sync.Mutex
	attachErr error
	attached  map[string]Attachment
	detaches  map[string]int
	updates   int
}

// Attachment is one live egress attachment.
type Attachment struct {
	SocketPath string
	Policy     core.NetworkPolicy
}

// SetAttachError makes Attach fail until cleared with nil.
func (e *FakeEgress) SetAttachError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachErr = err
}

func (e *FakeEgress) Attach(sandboxID, socketPath string, policy core.NetworkPolicy) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.attachErr != nil {
		return e.attachErr
	}
	if e.attached == nil {
		e.attached = map[string]Attachment{}
	}
	e.attached[sandboxID] = Attachment{socketPath, policy}
	return nil
}

func (e *FakeEgress) Update(sandboxID string, policy core.NetworkPolicy) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a, ok := e.attached[sandboxID]; ok {
		a.Policy = policy
		e.attached[sandboxID] = a
	}
	e.updates++
	return nil
}

func (e *FakeEgress) Detach(sandboxID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.attached, sandboxID)
	if e.detaches == nil {
		e.detaches = map[string]int{}
	}
	e.detaches[sandboxID]++
	return nil
}

// Attached returns a sandbox's attachment, if it has one.
func (e *FakeEgress) Attached(sandboxID string) (Attachment, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.attached[sandboxID]
	return a, ok
}

// Detaches is how often Detach was called for a sandbox.
func (e *FakeEgress) Detaches(sandboxID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.detaches[sandboxID]
}
