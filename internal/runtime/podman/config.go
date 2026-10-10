// Package podman is the rootless Podman isolation backend. It implements
// runtime.Backend by talking to the Libpod REST API on the per-user Unix
// socket with a small hand-written net/http client; the official Go
// bindings are deliberately not used (cgo and a very large dependency
// tree).
//
// The Podman API is used for lifecycle only (create, start, pause, remove,
// list, inspect). Exec, PTY and file traffic go through the in-sandbox
// agent, and resource counters are read straight from cgroup v2 files.
//
// Threat model, stated once: with crun (or runc) every sandbox shares the
// host kernel. This backend applies every documented hardening control, but
// it is defence in depth for trusted or single-tenant workloads, not a
// hostile multi-tenant boundary. Capabilities().SharedKernel says so.
package podman

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// UsernsMode selects how container UIDs map to host UIDs. It is one fixed
// mapping per pool (per Backend), never Podman's per-container "auto".
//
// Why not auto: with native overlayfs, the first container created from an
// image under a UID mapping that image has not been used with before pays
// for an ID-shifted copy of the image layers. "auto" may pick a different
// mapping on every create, so it would pay that on the hot path, and each
// container also consumes its own slice of the 65,536 subordinate IDs a
// user gets by default. A fixed mapping pays the cost once per image and
// has no per-sandbox subuid budget. The price is that all sandboxes of a
// pool share one UID range; isolation between pools then comes from giving
// each pool its own host user (and its own Podman service and Backend).
type UsernsMode string

const (
	// UsernsKeepID (default) maps the host user running Podman to
	// Config.ContainerUID/GID inside the container, and container root to
	// the first subordinate UID. The sandbox process therefore runs as the
	// pool's host user: it owns the bind-mounted run directory and the
	// agent socket it creates there with no extra chown or loosened
	// permissions, which is why this is the default. The cost: a process
	// that escapes the container is the pool user on the host, who owns
	// the Podman socket and every other sandbox of the pool.
	UsernsKeepID UsernsMode = "keep-id"

	// UsernsNoMap does not map the host user into the container at all;
	// every container UID is a subordinate UID. An escaped process owns
	// nothing on the host, which makes this the strongest fixed mapping.
	// The cost is operational: the run directory, owned by the host user,
	// appears as nobody:nogroup inside, so the sandbox user can only
	// create the agent socket if the caller makes the directory writable
	// for it (mode 0777, or Config.RunDirChown, which hands the directory
	// to a subordinate UID that the host user can then only clean up with
	// `podman unshare`).
	UsernsNoMap UsernsMode = "nomap"

	// UsernsHost puts the container in the rootless Podman user namespace
	// itself: container root is the host user, container UID n is
	// subordinate UID n-1. It needs no ID-shifted image layers at all, so
	// it is the fastest, and it has the same run-directory caveat as
	// UsernsNoMap for a non-root container user.
	UsernsHost UsernsMode = "host"

	// UsernsAuto is Podman's per-container automatic range. It is accepted
	// so the trade-off can be benchmarked, and is not recommended; see the
	// type comment.
	UsernsAuto UsernsMode = "auto"
)

// Defaults.
const (
	DefaultRuntime        = "crun"
	DefaultContainerUID   = 1000
	DefaultContainerGID   = 1000
	DefaultShmBytes       = 16 << 20
	DefaultCgroupRoot     = "/sys/fs/cgroup"
	DefaultDialTimeout    = 5 * time.Second
	DefaultRequestTimeout = 30 * time.Second
	DefaultHostname       = "sandbox"
)

// AgentCommand is the argv of the sandbox's long-lived process: the
// in-sandbox agent serving on the Unix socket in the shared run directory.
// It is the single place to change if the agent's invocation changes.
var AgentCommand = []string{
	"/usr/local/bin/sbx-agent", // == runtime.AgentBinary
	"serve",
	"--socket", "/run/sbx/agent.sock", // == runtime.RunDir + "/" + runtime.AgentSocket
}

// Config configures one Backend, which is one Podman service of one host
// user, which is one pool's worth of UID mapping.
type Config struct {
	// SocketPath is the Libpod API socket. Default:
	// $XDG_RUNTIME_DIR/podman/podman.sock. Whoever can open this socket
	// can run code as the owning user.
	SocketPath string

	// DefaultRuntime is the OCI runtime used when Template.Runtime is
	// empty. Default "crun".
	DefaultRuntime string
	// Runtimes is the set of OCI runtimes this backend accepts in
	// Template.Runtime, e.g. {"crun", "runsc"}. Default: {DefaultRuntime}.
	// It also drives Capabilities().SharedKernel.
	Runtimes []string

	// RuntimeSockets maps each runtime other than DefaultRuntime to the
	// API socket of a Podman service whose default OCI runtime it is.
	//
	// A Podman service runs every container it creates with its own
	// default runtime: the create request has an "oci_runtime" field, but
	// Podman 4.9.3 ignores it (pkg/specgen/generate never reads it; only
	// the global --runtime option and containers.conf select a runtime).
	// A second tier therefore needs a second service, and sandboxes of a
	// runtime are created and started through that runtime's service.
	// Everything else (inspect, list, remove, pause) goes through the
	// default service, which works because the services share one
	// container store and one runtime table. A runtime listed in Runtimes
	// without a socket here cannot be used and is rejected by New, and
	// Create checks that Podman really used the runtime that was asked.
	RuntimeSockets map[string]string

	// UsernsMode is the fixed user-namespace mapping; see UsernsMode.
	// Default UsernsKeepID.
	UsernsMode UsernsMode
	// ContainerUID and ContainerGID are the non-root identity the agent
	// and all user code run as inside the container. Default 1000:1000.
	// Zero is not accepted: the sandbox never runs as container root.
	ContainerUID int
	ContainerGID int
	// RunDirChown adds the "U" option to the run-directory bind mount, so
	// Podman chowns Spec.HostRunDir to the container user. Only useful
	// with UsernsNoMap or UsernsHost; see UsernsNoMap for the cost.
	RunDirChown bool

	// SeccompProfilePath is the host path of the seccomp profile applied
	// to every sandbox. Use WriteSeccompProfile to materialise the
	// embedded hardened profile and put its result here. Empty means
	// Podman's built-in default profile, which is looser (it allows
	// unshare, clone3, io_uring, keyctl and every socket family).
	// "unconfined" is rejected.
	SeccompProfilePath string

	// ShmBytes caps /dev/shm. Default 16 MiB. It is charged to the memory
	// cgroup like the other tmpfs mounts.
	ShmBytes int64

	// OOMEvidenceDirs are the working directories of the Podman services
	// that start this backend's containers, and nobody else's (managed
	// services: Service.WorkDir). When set, an exited container is
	// reported as OOM-killed only if conmon left its "oom" file in one of
	// them around the time the container ended, and the backend consumes
	// those files (call Close when done). When empty, exit code 137 alone
	// is taken as an OOM kill. They must never be directories other
	// programs use.
	OOMEvidenceDirs []string

	// CgroupRoot is where cgroup v2 is mounted; injectable for tests.
	// Default /sys/fs/cgroup.
	CgroupRoot string

	// DialTimeout bounds connecting to the socket; RequestTimeout bounds
	// each API request that has no earlier context deadline.
	DialTimeout    time.Duration
	RequestTimeout time.Duration

	// Command overrides AgentCommand as the container's argv. It exists
	// for tests and benchmarks that run an image without the agent (for
	// example {"sleep", "600"}); when set, Spec.AgentBinaryPath may be
	// empty and is then not mounted.
	Command []string
}

// DefaultSocketPath returns the per-user Podman socket path, or "" when
// XDG_RUNTIME_DIR is not set.
func DefaultSocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "podman", "podman.sock")
}

// withDefaults returns a validated copy of cfg with defaults filled in.
func (cfg Config) withDefaults() (Config, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
		if cfg.SocketPath == "" {
			return cfg, errors.New("podman: SocketPath is empty and XDG_RUNTIME_DIR is not set")
		}
	}
	if cfg.DefaultRuntime == "" {
		cfg.DefaultRuntime = DefaultRuntime
	}
	if len(cfg.Runtimes) == 0 {
		cfg.Runtimes = []string{cfg.DefaultRuntime}
	}
	found := false
	for _, r := range cfg.Runtimes {
		if r == "" {
			return cfg, errors.New("podman: empty entry in Runtimes")
		}
		if r == cfg.DefaultRuntime {
			found = true
		}
	}
	if !found {
		return cfg, fmt.Errorf("podman: DefaultRuntime %q is not in Runtimes %v", cfg.DefaultRuntime, cfg.Runtimes)
	}
	for _, r := range cfg.Runtimes {
		if r != cfg.DefaultRuntime && cfg.RuntimeSockets[r] == "" {
			return cfg, fmt.Errorf("podman: runtime %q has no service of its own (RuntimeSockets): one Podman service runs one OCI runtime, "+
				"so a second runtime needs a second service; sbxd starts one per runtime in managed mode", r)
		}
	}
	switch cfg.UsernsMode {
	case "":
		cfg.UsernsMode = UsernsKeepID
	case UsernsKeepID, UsernsNoMap, UsernsHost, UsernsAuto:
	default:
		return cfg, fmt.Errorf("podman: unknown UsernsMode %q (want keep-id, nomap, host or auto)", cfg.UsernsMode)
	}
	if cfg.ContainerUID == 0 && cfg.ContainerGID == 0 {
		cfg.ContainerUID, cfg.ContainerGID = DefaultContainerUID, DefaultContainerGID
	}
	if cfg.ContainerUID <= 0 || cfg.ContainerGID <= 0 {
		return cfg, fmt.Errorf("podman: ContainerUID/GID must be non-root and positive, got %d:%d", cfg.ContainerUID, cfg.ContainerGID)
	}
	if strings.EqualFold(cfg.SeccompProfilePath, "unconfined") {
		return cfg, errors.New("podman: SeccompProfilePath \"unconfined\" is not allowed")
	}
	if cfg.SeccompProfilePath != "" && !filepath.IsAbs(cfg.SeccompProfilePath) {
		return cfg, fmt.Errorf("podman: SeccompProfilePath %q must be absolute", cfg.SeccompProfilePath)
	}
	if cfg.ShmBytes == 0 {
		cfg.ShmBytes = DefaultShmBytes
	}
	if cfg.ShmBytes < 0 {
		return cfg, errors.New("podman: ShmBytes must be positive")
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = DefaultCgroupRoot
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.Command != nil && len(cfg.Command) == 0 {
		return cfg, errors.New("podman: Command override must not be an empty argv")
	}
	return cfg, nil
}

// sharedKernelRuntimes are the OCI runtimes whose containers run directly
// on the host kernel. Anything not listed in isolatedRuntimes is treated as
// shared-kernel, so an unknown runtime is never over-claimed.
var isolatedRuntimes = map[string]bool{
	"runsc": true, // gVisor: user-space kernel
	"krun":  true, // libkrun microVM
	"kata":  true, // Kata Containers VM
}

// RuntimeInfo describes one OCI runtime as this backend runs it.
//
// Pause: crun freezes the container's cgroup. runsc pauses the sandbox
// from inside the sentry (`runsc pause`), which Podman drives the same
// way; both were checked on Podman 4.9.3 (integration tests).
func RuntimeInfo(ociRuntime string) runtime.RuntimeInfo {
	return runtime.RuntimeInfo{Name: ociRuntime, SharedKernel: SharedKernel(ociRuntime), Pause: true}
}

// SharedKernel reports whether sandboxes run by the given OCI runtime share
// the host kernel. The runtime may be a bare name or a path.
func SharedKernel(ociRuntime string) bool {
	return !isolatedRuntimes[filepath.Base(ociRuntime)]
}
