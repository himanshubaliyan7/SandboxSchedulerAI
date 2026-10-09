package agentclient

import (
	"context"
	"path/filepath"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
)

// SocketPath is the host path of the agent socket of the sandbox whose
// host run directory (runtime.Spec.HostRunDir) is hostRunDir.
func SocketPath(hostRunDir string) string {
	return filepath.Join(hostRunDir, runtime.AgentSocket)
}

// ForRunDir returns a client for the sandbox whose host run directory is
// hostRunDir.
func ForRunDir(hostRunDir string) *Client { return New(SocketPath(hostRunDir)) }

// Sandboxes addresses agents by their host run directory. It is the
// method set the pool controller depends on:
//
//	WaitReady(ctx context.Context, hostRunDir string) error
//	Init(ctx context.Context, hostRunDir string, req agentapi.InitRequest) error
//
// The zero value is ready to use. It holds no state and no connections.
type Sandboxes struct{}

// WaitReady blocks until the agent of the sandbox at hostRunDir answers
// its health check as ready, or ctx is done.
func (Sandboxes) WaitReady(ctx context.Context, hostRunDir string) error {
	c := ForRunDir(hostRunDir)
	defer c.Close()
	return c.WaitReady(ctx)
}

// Init injects the session configuration into the sandbox at hostRunDir.
func (Sandboxes) Init(ctx context.Context, hostRunDir string, req agentapi.InitRequest) error {
	c := ForRunDir(hostRunDir)
	defer c.Close()
	return c.Init(ctx, req)
}
