package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/preflight"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman"
)

// Podman modes: whose Podman service the server talks to.
const (
	// PodmanManaged (the default) starts a `podman system service` that
	// belongs to this server, with a configuration the server generates
	// under its run dir, and supervises it. It shares the user's image
	// and container store, so images pulled with plain `podman pull` are
	// there and `podman ps` shows the sandboxes.
	//
	// It is the default because the two things a sandbox needs from the
	// service cannot be asked for per container: no session keyring (the
	// first density ceiling, about 190 sandboxes per user) and no default
	// sysctls (a write that fails now and then under concurrent starts).
	// With the user's service both depend on a file the operator has to
	// edit; with a managed one they are simply so. A managed service also
	// has a working directory of its own, which is what makes conmon's
	// OOM reports attributable, and it is what a runtime table with the
	// gVisor wrapper needs.
	PodmanManaged = "managed"
	// PodmanExternal talks to a Podman service somebody else runs: the
	// socket given with --podman-socket, or the user's systemd-activated
	// one. Nothing is generated and nothing is supervised; the service's
	// own containers.conf applies, with the limits described in
	// docs/operations.md. Giving --podman-socket selects this mode.
	PodmanExternal = "external"
)

// Files of managed mode inside the run dir. A Podman service runs every
// container with its one default OCI runtime (podman.Config.RuntimeSockets
// has the details), so there is one managed service per enabled runtime:
// the crun one on podman.sock with its files in podman/, and for another
// runtime <name> one on podman-<name>.sock with its files in
// podman-<name>/. They share the user's store and one runtime table.
const (
	// ManagedSocket is the API socket of the default runtime's service.
	ManagedSocket = "podman.sock"
	// ManagedDir holds that service's configuration, log, pid file and
	// working directory, and the runsc wrapper.
	ManagedDir = "podman"
)

// managedPaths returns the directory and socket of the managed service
// of a runtime.
func managedPaths(runDir, runtimeName string) (dir, socket string) {
	if runtimeName == podman.DefaultRuntime {
		return filepath.Join(runDir, ManagedDir), filepath.Join(runDir, ManagedSocket)
	}
	return filepath.Join(runDir, ManagedDir+"-"+runtimeName), filepath.Join(runDir, ManagedDir+"-"+runtimeName+".sock")
}

// ResolvePodmanMode returns the effective mode for a configured mode and
// socket: an explicit socket means an external service.
func ResolvePodmanMode(mode, socket string) (string, error) {
	switch mode {
	case "":
		if socket != "" {
			return PodmanExternal, nil
		}
		return PodmanManaged, nil
	case PodmanExternal:
		return PodmanExternal, nil
	case PodmanManaged:
		if socket != "" {
			return "", errors.New("--podman-socket names a Podman service that already exists, which is --podman-mode external; " +
				"with --podman-mode managed sbxd starts its own service on <run-dir>/" + ManagedSocket)
		}
		return PodmanManaged, nil
	}
	return "", fmt.Errorf("unknown podman mode %q (want %s or %s)", mode, PodmanManaged, PodmanExternal)
}

// DefaultRuntimes is the runtime set of a server that was not given one.
func DefaultRuntimes() []podman.OCIRuntime {
	return []podman.OCIRuntime{{Name: podman.DefaultRuntime}}
}

// normaliseRuntimes validates an enabled-runtime list: it is never empty,
// and a template without a runtime means crun, so crun cannot be left out.
func normaliseRuntimes(rts []podman.OCIRuntime) ([]podman.OCIRuntime, error) {
	if len(rts) == 0 {
		return DefaultRuntimes(), nil
	}
	for _, r := range rts {
		if r.Name == podman.DefaultRuntime {
			return rts, nil
		}
	}
	return nil, fmt.Errorf("--runtimes %s: the default runtime %s must be enabled", strings.Join(podman.RuntimeNames(rts), ","), podman.DefaultRuntime)
}

// CheckTemplateRuntimes returns an error naming every template whose
// runtime is not enabled. A template is a promise about isolation; one
// that cannot be kept must stop the server (or the apply) and not turn
// into create failures later.
func CheckTemplateRuntimes(templates []core.Template, enabled []string) error {
	ok := map[string]bool{}
	for _, name := range enabled {
		ok[name] = true
	}
	var bad []string
	for _, t := range templates {
		name := t.Runtime
		if name == "" {
			name = podman.DefaultRuntime
		}
		if !ok[name] {
			bad = append(bad, fmt.Sprintf("%s (runtime %s)", t.ID, name))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("templates name a runtime this server has not enabled: %s; enabled: %s. "+
		"Enable it with --runtimes (for example --runtimes crun,runsc or runsc=/path/to/runsc; SBX_RUNTIMES), or change the template's runtime",
		strings.Join(bad, ", "), strings.Join(enabled, ","))
}

// podmanStack is the server's connection to Podman: the backend and, in
// managed mode, the services it talks to (one per runtime).
type podmanStack struct {
	Mode     string
	Backend  *podman.Backend
	Services []*podman.Service // empty in external mode
	Runtimes []podman.OCIRuntime
}

// Close stops the OOM watcher and, in managed mode, the services. The
// sandboxes keep running: they are conmon's children, not a service's.
func (p *podmanStack) Close(ctx context.Context) {
	if p == nil {
		return
	}
	if p.Backend != nil {
		p.Backend.Close()
	}
	for _, svc := range p.Services {
		_ = svc.Stop(ctx)
	}
}

// podmanParams is what startPodman needs.
type podmanParams struct {
	Mode       string // resolved: PodmanManaged or PodmanExternal
	Socket     string // external mode; "" for the user's default socket
	RunDir     string // managed mode: an existing, locked run dir
	Runtimes   []podman.OCIRuntime
	RunscFlags []string
	Seccomp    string
	UsernsMode string
	Logger     *slog.Logger
}

// shimCommand is the argv prefix of the runsc shim: this very binary.
func shimCommand() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find sbxd's own path for the runsc wrapper: %w", err)
	}
	return []string{exe, "runsc-shim"}, nil
}

// errExternalRuntimes explains why an external service cannot serve a
// second tier.
func errExternalRuntimes(enabled []podman.OCIRuntime) error {
	return fmt.Errorf("--runtimes %s needs --podman-mode managed: a Podman service runs every container with its one default OCI runtime "+
		"(Podman ignores a per-container runtime in the API), so each tier needs a service of its own, which sbxd starts and configures "+
		"only in managed mode; with --podman-socket (external mode) only %s is available",
		strings.Join(podman.RuntimeNames(enabled), ","), podman.DefaultRuntime)
}

// startPodman brings up the Podman side of a server: in managed mode one
// private service per enabled runtime (with their configuration and the
// runsc wrapper), in both modes the backend, checked to answer.
func startPodman(ctx context.Context, p podmanParams) (*podmanStack, error) {
	enabled, err := normaliseRuntimes(p.Runtimes)
	if err != nil {
		return nil, err
	}
	stack := &podmanStack{Mode: p.Mode}
	bcfg := podman.Config{SeccompProfilePath: p.Seccomp, UsernsMode: podman.UsernsMode(p.UsernsMode),
		Runtimes: podman.RuntimeNames(enabled), RuntimeSockets: map[string]string{}}
	fail := func(err error) (*podmanStack, error) {
		stack.Close(context.WithoutCancel(ctx))
		return nil, err
	}

	switch p.Mode {
	case PodmanManaged:
		if p.RunDir == "" {
			return nil, errors.New("a run dir is required")
		}
		var shim []string
		for _, r := range enabled {
			if r.Name == podman.RunscRuntime {
				if shim, err = shimCommand(); err != nil {
					return nil, err
				}
			}
		}
		base, _ := managedPaths(p.RunDir, podman.DefaultRuntime)
		if err := os.MkdirAll(base, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", base, err)
		}
		// One table for all services, so that each of them can stop and
		// remove a container of any runtime.
		table, err := podman.PrepareRuntimes(base, enabled, shim, p.RunscFlags)
		if err != nil {
			return nil, err
		}
		stack.Runtimes = table
		for _, r := range enabled {
			dir, socket := managedPaths(p.RunDir, r.Name)
			svc, err := podman.StartService(ctx, podman.ServiceConfig{
				Dir: dir, SocketPath: socket, Runtimes: table, DefaultRuntime: r.Name, Logger: p.Logger.With("runtime", r.Name),
			})
			if err != nil {
				return fail(fmt.Errorf("runtime %s: %w", r.Name, err))
			}
			stack.Services = append(stack.Services, svc)
			bcfg.OOMEvidenceDirs = append(bcfg.OOMEvidenceDirs, svc.WorkDir())
			if r.Name == podman.DefaultRuntime {
				bcfg.SocketPath = svc.SocketPath()
			} else {
				bcfg.RuntimeSockets[r.Name] = svc.SocketPath()
			}
		}
	case PodmanExternal:
		if len(enabled) != 1 || enabled[0].Path != "" {
			return nil, errExternalRuntimes(enabled)
		}
		bcfg.SocketPath = p.Socket
		stack.Runtimes = enabled
	default:
		return nil, fmt.Errorf("unknown podman mode %q", p.Mode)
	}

	backend, err := podman.New(bcfg)
	if err != nil {
		return fail(err)
	}
	stack.Backend = backend
	if _, err := backend.Ping(ctx); err != nil {
		return fail(fmt.Errorf("podman socket %s: %w", backend.Config().SocketPath, err))
	}
	return stack, nil
}

// PreflightConfig says what `sbxd preflight` and the server's own start-up
// check should assume about Podman.
type PreflightConfig struct {
	PodmanMode    string // "", managed or external; see ResolvePodmanMode
	PodmanSocket  string
	Runtimes      []podman.OCIRuntime
	RequireLinger bool
	// ProbeService starts a throwaway managed service to prove that one
	// can be started with the generated configuration. The server skips
	// it: it is about to start the real one.
	ProbeService bool
}

// PreflightWith runs the host checks for a configuration.
func PreflightWith(ctx context.Context, pc PreflightConfig) (preflight.Report, error) {
	mode, err := ResolvePodmanMode(pc.PodmanMode, pc.PodmanSocket)
	if err != nil {
		return preflight.Report{}, err
	}
	enabled, err := normaliseRuntimes(pc.Runtimes)
	if err != nil {
		return preflight.Report{}, err
	}
	if mode == PodmanExternal && (len(enabled) != 1 || enabled[0].Path != "") {
		return preflight.Report{}, errExternalRuntimes(enabled)
	}
	opts := preflight.Options{PodmanSocket: pc.PodmanSocket, RequireLinger: pc.RequireLinger,
		ManagedPodman: mode == PodmanManaged, SkipServiceProbe: !pc.ProbeService}
	for _, r := range enabled {
		opts.Runtimes = append(opts.Runtimes, preflight.Runtime{Name: r.Name, Path: r.Path})
	}
	return preflight.Run(ctx, opts), nil
}

// runtimeInfo answers the API's question about one runtime name.
func runtimeInfo(caps runtime.Capabilities) func(name string) (runtime.RuntimeInfo, bool) {
	return func(name string) (runtime.RuntimeInfo, bool) {
		if name == "" {
			name = podman.DefaultRuntime
		}
		for _, r := range caps.Runtimes {
			if r.Name == name {
				return r, true
			}
		}
		return runtime.RuntimeInfo{}, false
	}
}
