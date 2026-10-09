// Package server wires the single-node scheduler: preflight, the Postgres
// store, the egress proxy, the rootless Podman backend, the pool
// controller and the public API. cmd/sbxd is a thin command line over it.
package server

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentclient"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/api"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/egress"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/pool"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/preflight"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime/podman"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// Shutdown policies: what a graceful stop does with tenant-owned
// sandboxes. Warm sandboxes are always left running; they hold no tenant
// data and the next start re-adopts them.
const (
	// ShutdownDrain releases every tenant-owned sandbox with reason
	// host_drain before exiting. This is the default: the egress proxy
	// and the gateway die with the process, so a session left behind
	// would be unreachable and its network policy unenforced.
	ShutdownDrain = "drain"
	// ShutdownLeave exits without touching tenant-owned sandboxes. They
	// keep running with no way out (their proxy socket is gone) and no
	// way in until the next start, whose first reconcile pass recycles
	// them with reason host_drain. Useful only to make a stop fast.
	ShutdownLeave = "leave"
)

// SeccompDefault selects Podman's built-in seccomp profile instead of the
// hardened one embedded in this binary.
const SeccompDefault = "podman-default"

// Config configures Run. DatabaseURL and AgentBinary are required.
type Config struct {
	DatabaseURL string
	// Listen is the API listen address. Default 127.0.0.1:8080.
	Listen string
	// AgentBinary is the host path of the static sbx-agent binary that is
	// bind-mounted into every sandbox.
	AgentBinary string
	// RunDir holds one 0700 directory per sandbox (agent and egress
	// sockets) and the seccomp profile. Default $XDG_RUNTIME_DIR/sbx. It
	// must be short: Unix socket paths are limited to 107 bytes.
	RunDir string
	// PodmanSocket is the Libpod API socket. Default
	// $XDG_RUNTIME_DIR/podman/podman.sock.
	PodmanSocket string
	// HostID names this host in the store. Default: the hostname.
	HostID string

	SkipPreflight bool
	// RequireLinger makes a missing systemd linger setting a preflight
	// failure rather than a warning.
	RequireLinger bool
	// SeccompProfile is "" for the embedded hardened profile (written
	// under RunDir), SeccompDefault for Podman's own, or the absolute
	// path of a profile.
	SeccompProfile string
	// UsernsMode is the fixed user-namespace mapping: keep-id (default),
	// nomap, host or auto. Only keep-id lets the host reach the agent
	// socket without extra configuration.
	UsernsMode string

	// ShutdownPolicy is ShutdownDrain (default) or ShutdownLeave.
	ShutdownPolicy string
	// ShutdownTimeout bounds the graceful stop. Default 30s.
	ShutdownTimeout time.Duration

	// EgressPermitPrivateCIDRs opens sub-ranges of private address space
	// to sandboxes whose policy allows them (egress.Config).
	EgressPermitPrivateCIDRs []string

	// API settings; zero values take the api package's defaults.
	MaxUploadBytes   int64
	TouchInterval    time.Duration
	ClaimWait        time.Duration
	AuthCacheTTL     time.Duration
	CreateRatePerSec float64
	CreateBurst      int

	// Pool intervals; zero values take the pool package's defaults.
	SweepInterval     time.Duration
	ReconcileInterval time.Duration

	Logger *slog.Logger
	// OnListen, if set, is called with the bound address once the API is
	// accepting connections.
	OnListen func(net.Addr)
}

// DefaultRunDir is $XDG_RUNTIME_DIR/sbx, or "" when XDG_RUNTIME_DIR is unset.
func DefaultRunDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "sbx")
	}
	return ""
}

// maxRunDirLen keeps RunDir + "/sb-<16 hex>/egress.sock" within the 107
// bytes a Unix socket address can hold.
const maxRunDirLen = 107 - len("/sb-0123456789abcdef/egress.sock")

func (c *Config) defaults() error {
	if c.DatabaseURL == "" {
		return errors.New("a database URL is required (--database-url or SBX_DATABASE_URL)")
	}
	if c.AgentBinary == "" {
		return errors.New("the agent binary path is required (--agent-binary or SBX_AGENT_BINARY); build it with `make agent`")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.RunDir == "" {
		if c.RunDir = DefaultRunDir(); c.RunDir == "" {
			return errors.New("no run dir: set --run-dir or SBX_RUN_DIR (XDG_RUNTIME_DIR is not set)")
		}
	}
	abs, err := filepath.Abs(c.RunDir)
	if err != nil {
		return fmt.Errorf("run dir: %w", err)
	}
	c.RunDir = abs
	if len(c.RunDir) > maxRunDirLen {
		return fmt.Errorf("run dir %q is too long: Unix socket paths are limited to 107 bytes, so it may be at most %d",
			c.RunDir, maxRunDirLen)
	}
	if c.HostID == "" {
		if c.HostID, err = os.Hostname(); err != nil || c.HostID == "" {
			return errors.New("could not determine the hostname; set --host-id")
		}
	}
	switch c.ShutdownPolicy {
	case "":
		c.ShutdownPolicy = ShutdownDrain
	case ShutdownDrain, ShutdownLeave:
	default:
		return fmt.Errorf("unknown shutdown policy %q (want %s or %s)", c.ShutdownPolicy, ShutdownDrain, ShutdownLeave)
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return nil
}

// CheckAgentBinary verifies that path is an executable, statically linked
// ELF file. The agent is bind-mounted into arbitrary images, so a
// dynamically linked build would fail to start in most of them, and it
// would do so once per sandbox create rather than once here.
func CheckAgentBinary(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("agent binary: %w (build it with `make agent` and pass --agent-binary)", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("agent binary %s is not an executable file", abs)
	}
	f, err := elf.Open(abs)
	if err != nil {
		return "", fmt.Errorf("agent binary %s is not an ELF executable: %w", abs, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "", fmt.Errorf("agent binary %s is dynamically linked; build it with CGO_ENABLED=0 (`make agent`)", abs)
		}
	}
	return abs, nil
}

// Preflight runs the host checks for cfg.
func Preflight(ctx context.Context, podmanSocket string, requireLinger bool) preflight.Report {
	return preflight.Run(ctx, preflight.Options{PodmanSocket: podmanSocket, RequireLinger: requireLinger})
}

// Run starts the server and blocks until ctx is cancelled, then stops
// gracefully according to cfg.ShutdownPolicy. It returns an error if the
// server could not start or stopped for another reason than ctx.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	log := cfg.Logger

	// 1. Host checks.
	if cfg.SkipPreflight {
		log.Warn("preflight skipped")
	} else {
		report := Preflight(ctx, cfg.PodmanSocket, cfg.RequireLinger)
		for _, c := range report.Checks {
			switch c.Status {
			case preflight.StatusFail:
				log.Error("preflight failed", "check", c.Name, "detail", c.Detail, "remediation", c.Remediation)
			case preflight.StatusWarn:
				log.Warn("preflight warning", "check", c.Name, "detail", c.Detail, "remediation", c.Remediation)
			}
		}
		if err := report.Err(); err != nil {
			return fmt.Errorf("preflight: %w (run `sbxd preflight` for the full report, or pass --skip-preflight)", err)
		}
	}
	agentBinary, err := CheckAgentBinary(cfg.AgentBinary)
	if err != nil {
		return err
	}

	// 2. State store (applies pending migrations).
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	// 3. Run dir and seccomp profile.
	if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	if err := os.Chmod(cfg.RunDir, 0o700); err != nil {
		return fmt.Errorf("chmod run dir: %w", err)
	}
	seccomp := cfg.SeccompProfile
	switch seccomp {
	case "":
		if seccomp, err = podman.WriteSeccompProfile(filepath.Join(cfg.RunDir, "seccomp")); err != nil {
			return fmt.Errorf("write seccomp profile: %w", err)
		}
	case SeccompDefault:
		seccomp = ""
		log.Warn("using Podman's default seccomp profile, which is looser than the embedded one")
	}

	// 4. Egress proxy.
	em := newEgressMetrics()
	egressMgr, err := egress.New(egress.Config{
		Logger: log.With("component", "egress"), OnDecision: em.observe,
		PermitPrivateCIDRs: cfg.EgressPermitPrivateCIDRs,
	})
	if err != nil {
		return err
	}
	defer egressMgr.Close()

	// 5. Isolation backend.
	backend, err := podman.New(podman.Config{
		SocketPath: cfg.PodmanSocket, SeccompProfilePath: seccomp, UsernsMode: podman.UsernsMode(cfg.UsernsMode),
	})
	if err != nil {
		return err
	}
	apiVersion, err := backend.Ping(ctx)
	if err != nil {
		return fmt.Errorf("podman socket %s: %w", backend.Config().SocketPath, err)
	}

	// 6. Pool controller. Start reconciles first: orphans of a previous
	// run are destroyed and warm sandboxes re-adopted before any claim.
	ctrl, err := pool.New(pool.Config{
		Store: st, Backend: backend, Agent: agentclient.Sandboxes{}, Egress: egressMgr,
		Logger: log, HostID: cfg.HostID, RunDirBase: cfg.RunDir, AgentBinaryPath: agentBinary,
		SweepInterval: cfg.SweepInterval, ReconcileInterval: cfg.ReconcileInterval,
	})
	if err != nil {
		return err
	}
	if err := ctrl.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	defer ctrl.Stop()

	// 7. API.
	apiSrv, err := api.New(api.Config{
		Store: st, Pool: ctrl, Stats: backend, ValidatePolicy: egress.ValidatePolicy, Logger: log,
		ExtraMetrics: em.write,
		Ready: func(ctx context.Context) error {
			if err := st.Ping(ctx); err != nil {
				return fmt.Errorf("store: %w", err)
			}
			if _, err := backend.Ping(ctx); err != nil {
				return fmt.Errorf("podman: %w", err)
			}
			return nil
		},
		MaxUploadBytes: cfg.MaxUploadBytes, TouchInterval: cfg.TouchInterval, ClaimWait: cfg.ClaimWait,
		AuthCacheTTL: cfg.AuthCacheTTL, CreateRatePerSec: cfg.CreateRatePerSec, CreateBurst: cfg.CreateBurst,
	})
	if err != nil {
		return err
	}
	defer apiSrv.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	httpSrv := &http.Server{
		Handler: apiSrv.Handler(),
		// Streams and WebSockets are long-lived, so there is no overall
		// read or write timeout; slow headers and idle keep-alive
		// connections are bounded, and handlers bound themselves.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Info("sbxd listening", "addr", ln.Addr().String(), "host", cfg.HostID, "run_dir", cfg.RunDir,
		"podman_api", apiVersion, "seccomp", orDefault(seccomp, SeccompDefault), "agent_binary", agentBinary,
		"shutdown_policy", cfg.ShutdownPolicy)
	if cfg.OnListen != nil {
		cfg.OnListen(ln.Addr())
	}

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	// Graceful stop: stop accepting, end streams, apply the shutdown
	// policy, stop the controller. Whatever is left (warm sandboxes, and
	// with ShutdownLeave the tenant ones) is labelled and recorded, so
	// the next start reconciles it.
	log.Info("shutting down", "policy", cfg.ShutdownPolicy, "timeout", cfg.ShutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- httpSrv.Shutdown(sctx) }()
	apiSrv.Close()
	if err := <-shutdownDone; err != nil {
		log.Warn("http shutdown did not finish cleanly", "err", err)
		_ = httpSrv.Close()
	}
	if cfg.ShutdownPolicy == ShutdownDrain {
		drain(sctx, log, st, ctrl, cfg.HostID)
	}
	ctrl.Stop()
	egressMgr.Close()
	log.Info("sbxd stopped")
	return nil
}

// drain releases every tenant-owned sandbox of this host.
func drain(ctx context.Context, log *slog.Logger, st *store.Store, ctrl *pool.Controller, hostID string) {
	rows, err := st.ListActiveByHost(ctx, hostID)
	if err != nil {
		log.Warn("drain: could not list sandboxes; the next start will recycle them", "err", err)
		return
	}
	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, 8)
		n   int
	)
	for _, sb := range rows {
		switch sb.State {
		case core.StateAssigned, core.StateRunning, core.StatePaused:
		default:
			continue
		}
		n++
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := ctrl.Release(ctx, sb.ID, sb.Generation, core.ReasonHostDrain); err != nil {
				log.Warn("drain: release failed; the next start will recycle it", "sandbox", sb.ID, "err", err)
			}
		}()
	}
	wg.Wait()
	if n > 0 {
		log.Info("drained tenant sandboxes", "count", n)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// egressMetrics counts proxy decisions for GET /metrics.
type egressMetrics struct {
	mu        sync.Mutex
	decisions map[[2]string]uint64 // verdict, reason
	up, down  int64
}

func newEgressMetrics() *egressMetrics {
	return &egressMetrics{decisions: map[[2]string]uint64{}}
}

func (m *egressMetrics) observe(d egress.Decision) {
	m.mu.Lock()
	m.decisions[[2]string{string(d.Verdict), string(d.Reason)}]++
	m.up += d.BytesUp
	m.down += d.BytesDown
	m.mu.Unlock()
}

func (m *egressMetrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	totals := map[string]uint64{string(egress.VerdictAllowed): 0, string(egress.VerdictDenied): 0, string(egress.VerdictError): 0}
	for k, n := range m.decisions {
		totals[k[0]] += n
	}
	fmt.Fprint(w, "# HELP sbx_egress_requests_total Egress proxy decisions by verdict (allowed, denied, error).\n# TYPE sbx_egress_requests_total counter\n")
	for _, v := range []string{string(egress.VerdictAllowed), string(egress.VerdictDenied), string(egress.VerdictError)} {
		fmt.Fprintf(w, "sbx_egress_requests_total{verdict=%q} %d\n", v, totals[v])
	}
	fmt.Fprint(w, "# HELP sbx_egress_decisions_total Egress proxy decisions by verdict and reason.\n# TYPE sbx_egress_decisions_total counter\n")
	keys := make([][2]string, 0, len(m.decisions))
	for k := range m.decisions {
		keys = append(keys, k)
	}
	sortPairs(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "sbx_egress_decisions_total{verdict=%q,reason=%q} %d\n", k[0], k[1], m.decisions[k])
	}
	fmt.Fprint(w, "# HELP sbx_egress_bytes_total Bytes relayed by the egress proxy for finished connections.\n# TYPE sbx_egress_bytes_total counter\n")
	fmt.Fprintf(w, "sbx_egress_bytes_total{direction=\"up\"} %d\nsbx_egress_bytes_total{direction=\"down\"} %d\n", m.up, m.down)
}

func sortPairs(keys [][2]string) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && (keys[j][0] < keys[j-1][0] || (keys[j][0] == keys[j-1][0] && keys[j][1] < keys[j-1][1])); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}
