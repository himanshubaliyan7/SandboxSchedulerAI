// Package runtime defines the isolation backend interface. It is defined
// above Podman on purpose: nothing here may assume bind mounts, `podman
// exec` or cgroup files, so that a microVM driver can implement it too.
// All exec, PTY and file traffic goes through the in-sandbox agent
// (package agentapi), never through the backend.
package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

// Labels every instance carries, so a host agent can rebuild its view from
// the runtime alone after a crash.
const (
	LabelManaged    = "sbx.managed" // always "true"
	LabelSandboxID  = "sbx.sandbox_id"
	LabelGeneration = "sbx.generation"
	LabelAgentEpoch = "sbx.agent_epoch"
	LabelTemplateID = "sbx.template_id"
)

// Paths inside the sandbox. The host directory Spec.HostRunDir is exposed
// at RunDir; the in-sandbox agent listens on AgentSocket and reaches the
// egress proxy through EgressSocket.
const (
	RunDir       = "/run/sbx"
	AgentSocket  = "agent.sock"
	EgressSocket = "egress.sock"
	AgentBinary  = "/usr/local/bin/sbx-agent"
	Workdir      = "/workspace"
)

var (
	ErrNotFound    = errors.New("runtime: instance not found")
	ErrUnsupported = errors.New("runtime: operation not supported by this backend")
)

// Capabilities describes what a backend can do. Backends are not uniform
// and callers must check instead of assuming.
type Capabilities struct {
	Pause         bool
	Snapshot      bool
	SharedKernel  bool // true for crun/runc: not a hostile multi-tenant boundary
	NeedsKVM      bool
	Rootless      bool
	NetworkModes  []core.NetworkMode
	RuntimeLabels []string // e.g. "crun", "runsc"
}

// Spec is everything needed to create one instance.
type Spec struct {
	SandboxID  string
	Generation int64
	AgentEpoch string
	Template   core.Template
	// HostRunDir is a per-sandbox host directory, created by the caller,
	// exposed inside the sandbox at RunDir. The agent socket appears at
	// HostRunDir/AgentSocket once the agent is up.
	HostRunDir string
	// AgentBinaryPath is the host path of the static sbx-agent binary; it
	// is exposed read-only at AgentBinary and run as the sandbox's
	// long-lived process.
	AgentBinaryPath string
}

// Instance is a backend instance as seen by the host agent.
type Instance struct {
	ID         string // backend id (container id)
	SandboxID  string
	Generation int64
	AgentEpoch string
	TemplateID string
	Running    bool
	CreatedAt  time.Time
}

// Stats are the per-sandbox signals used for termination reasons and
// noisy-neighbour detection.
type Stats struct {
	MemoryBytes    int64
	OOMKills       int64
	CPUUsageUsec   int64
	NrThrottled    int64
	ThrottledUsec  int64
	PidsCurrent    int64
	PidsLimitHits  int64
	MemoryHighHits int64
}

// Backend is one isolation engine on one host.
type Backend interface {
	Name() string
	Capabilities() Capabilities
	// Create creates and starts an instance and returns once the runtime
	// reports it running. Agent readiness is the caller's check.
	Create(ctx context.Context, spec Spec) (Instance, error)
	// Destroy kills (SIGKILL, no grace) and removes the instance and all
	// of its writable storage. It is idempotent: destroying a missing
	// instance returns nil.
	Destroy(ctx context.Context, instanceID string) error
	// List returns every instance carrying LabelManaged, running or not.
	List(ctx context.Context) ([]Instance, error)
	Inspect(ctx context.Context, instanceID string) (Instance, error)
	Stats(ctx context.Context, instanceID string) (Stats, error)
	// Pause and Unpause return ErrUnsupported when Capabilities().Pause is false.
	Pause(ctx context.Context, instanceID string) error
	Unpause(ctx context.Context, instanceID string) error
}
