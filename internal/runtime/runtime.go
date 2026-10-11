// Package runtime defines the isolation backend interface. It is defined
// above Podman on purpose: nothing here may assume bind mounts, `podman
// exec` or cgroup files, so that a microVM driver can implement it too.
// All exec, PTY and file traffic goes through the in-sandbox agent
// (package agentapi), never through the backend.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

// Labels every instance carries, so a host agent can rebuild its view from
// the runtime alone after a crash.
const (
	LabelManaged = "sbx.managed" // always "true"
	// LabelHostID is the identity of the scheduler that owns the instance
	// (Spec.HostID). One Podman user may serve several schedulers (two
	// servers, a server and a test run), so sbx.managed alone does not say
	// whose an instance is: a scheduler acts only on instances carrying
	// its own host id and leaves every other one strictly alone.
	LabelHostID     = "sbx.host_id"
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

// hostIDRE is what a host id may look like: it is a label value, a log
// field and a database key, so it is kept to a conservative alphabet.
var hostIDRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// ValidHostID checks a host id (Spec.HostID, LabelHostID).
func ValidHostID(id string) error {
	if !hostIDRE.MatchString(id) {
		return fmt.Errorf("invalid host id %q: want 1 to 63 letters, digits, '_', '.' or '-', starting with a letter or digit", id)
	}
	return nil
}

var (
	ErrNotFound    = errors.New("runtime: instance not found")
	ErrUnsupported = errors.New("runtime: operation not supported by this backend")
	// ErrNotRunning is returned by Stats for an instance that still
	// exists but whose processes are gone, so it has no counters left to
	// read. Inspect then says how it ended (Instance.ExitCode, OOMKilled).
	ErrNotRunning = errors.New("runtime: instance is not running")
)

// Capabilities describes what a backend can do. Backends are not uniform
// and callers must check instead of assuming.
//
// Pause and SharedKernel are the answer for the backend as a whole, and
// so the pessimistic one when its runtimes differ: SharedKernel is true
// as soon as one enabled runtime shares the host kernel, Pause only if
// every one can pause. Runtimes has the answer per runtime, which is the
// one that applies to a template.
type Capabilities struct {
	Pause         bool
	Snapshot      bool
	SharedKernel  bool // true for crun/runc: not a hostile multi-tenant boundary
	NeedsKVM      bool
	Rootless      bool
	NetworkModes  []core.NetworkMode
	RuntimeLabels []string // e.g. "crun", "runsc"
	// Runtimes describes each enabled runtime (isolation tier), in the
	// order of RuntimeLabels.
	Runtimes []RuntimeInfo
}

// RuntimeInfo is what differs between the isolation tiers of one backend.
type RuntimeInfo struct {
	// Name is what a template's Runtime field says: "crun", "runsc".
	Name string `json:"name"`
	// SharedKernel is true when sandboxes of this runtime run directly on
	// the host kernel (crun, runc), false when a kernel of their own
	// stands in between (gVisor's user-space kernel, a microVM's guest).
	SharedKernel bool `json:"shared_kernel"`
	// Pause says whether Pause and Unpause work for this runtime.
	Pause bool `json:"pause"`
}

// Spec is everything needed to create one instance.
type Spec struct {
	SandboxID string
	// HostID is the owning scheduler's host id; it is required and ends
	// up in LabelHostID.
	HostID     string
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
	ID        string // backend id (container id)
	SandboxID string
	// HostID is the instance's LabelHostID; empty for an instance created
	// before the label existed.
	HostID     string
	Generation int64
	AgentEpoch string
	TemplateID string
	Running    bool
	CreatedAt  time.Time
	// ExitCode and OOMKilled describe how an instance that is no longer
	// running ended. They are zero while Running, and best effort
	// otherwise: List may leave OOMKilled unset where only Inspect can
	// tell. OOMKilled means the backend has evidence, possibly indirect
	// (see the backend's documentation), that the kernel OOM killer ended
	// the instance.
	ExitCode  int
	OOMKilled bool
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
// LeftoverReaper is implemented by a backend whose instances can leave
// host processes behind that the backend's own listing no longer shows
// (Podman: a conmon whose container was removed while it was between
// "created" and "started"; see podman.Leftover). The pool's reconciler
// calls it on every pass.
type LeftoverReaper interface {
	// ReapLeftovers ends the leftovers of the sandboxes for which owned
	// returns true and reports how many it ended. Leftovers of other
	// sandboxes are not touched: they may be another scheduler's.
	ReapLeftovers(ctx context.Context, owned func(sandboxID string) bool) (int, error)
}

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
	// List returns every instance carrying LabelManaged, running or not,
	// whichever scheduler owns it. Callers must compare Instance.HostID
	// with their own host id before acting on an instance.
	List(ctx context.Context) ([]Instance, error)
	Inspect(ctx context.Context, instanceID string) (Instance, error)
	Stats(ctx context.Context, instanceID string) (Stats, error)
	// Pause and Unpause return ErrUnsupported when Capabilities().Pause is false.
	Pause(ctx context.Context, instanceID string) error
	Unpause(ctx context.Context, instanceID string) error
}
