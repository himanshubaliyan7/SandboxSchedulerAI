// Package server wires the single-node scheduler: preflight, the Postgres
// store, the egress proxy, the rootless Podman backend, the pool
// controller and the public API. cmd/sbxd is a thin command line over it.
package server

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
	"github.com/himanshubaliyan7/warmbox/internal/api"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/egress"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/preflight"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Shutdown policies: what a graceful stop (SIGTERM, SIGINT) leaves behind.
//
// Whatever the policy, the stop first closes the API (no new requests,
// open streams and WebSockets are ended with shutting_down), then stops
// the refill loop and waits for the creates in flight to finish, and only
// then touches a sandbox. So no create is ever started, or left half
// done, by a shutdown: every container that remains is a whole sandbox
// with a row that says what it is.
const (
	// ShutdownDrain (the default) releases every tenant-owned sandbox
	// with reason host_drain and leaves the warm ones running. A tenant
	// session cannot outlive the process: its egress proxy and its
	// gateway die with it, so it would be unreachable and its network
	// policy unenforced. A warm sandbox holds no tenant data and is
	// re-adopted by the next start (after one readiness probe), which
	// makes a restart cheap. After the stop, the containers of this host
	// id are exactly its warm sandboxes.
	ShutdownDrain = "drain"
	// ShutdownLeave exits without touching any sandbox. Tenant sandboxes
	// keep running with no way out (their proxy socket is gone) and no
	// way in until the next start, whose first reconcile pass recycles
	// them with reason host_drain. Useful only to make a stop fast.
	// After the stop, the containers of this host id are its warm and
	// its tenant sandboxes.
	ShutdownLeave = "leave"
	// ShutdownDestroy removes everything this host id owns: tenant
	// sandboxes (reason host_drain), warm sandboxes, any other container
	// carrying the host id, and the run dirs. For decommissioning a host
	// and for tests. After the stop nothing of this host id is left.
	// `sbxd admin host destroy` does the same for a server that is not
	// running.
	ShutdownDestroy = "destroy"
)

// SeccompDefault selects Podman's built-in seccomp profile instead of the
// hardened one embedded in this binary.
const SeccompDefault = "podman-default"

// Config configures Run. DatabaseURL and AgentBinary are required.
type Config struct {
	DatabaseURL string
	// Listen is the API listen address. Default 127.0.0.1:8080.
	Listen string
	// MetricsListen, when set, serves GET /metrics on a listener of its
	// own and removes the route from Listen. MetricsToken, when set, is
	// the bearer token GET /metrics requires, wherever it is served.
	//
	// The metrics are operational data about every tenant (template ids,
	// pool sizes, request and failure counts). With neither option set
	// they are served unauthenticated on Listen, which is fine for the
	// loopback default. Run refuses to start when that would expose them:
	// Listen is not a loopback address, /metrics is on it and there is no
	// token. Set MetricsToken, or move the metrics to MetricsListen.
	MetricsListen string
	MetricsToken  string
	// AgentBinary is the host path of the static sbx-agent binary that is
	// bind-mounted into every sandbox.
	AgentBinary string
	// RunDir holds one 0700 directory per sandbox (agent and egress
	// sockets) and the seccomp profile. Default $XDG_RUNTIME_DIR/sbx. It
	// must be short: Unix socket paths are limited to 107 bytes.
	RunDir string
	// PodmanSocket is the Libpod API socket of an existing Podman service
	// (PodmanExternal). Setting it selects external mode. In external
	// mode an empty value means $XDG_RUNTIME_DIR/podman/podman.sock.
	PodmanSocket string
	// PodmanMode is PodmanManaged or PodmanExternal. Empty means managed,
	// unless PodmanSocket is set. See the constants.
	PodmanMode string
	// Runtimes are the OCI runtimes (isolation tiers) templates may name,
	// each with an optional path of its binary. Default: crun. A template
	// whose runtime is not listed stops the server at start.
	Runtimes []podman.OCIRuntime
	// RunscFlags are extra runsc flags for the gVisor tier, for example
	// "--platform=kvm"; they follow, and so override, runscshim.DefaultFlags.
	RunscFlags []string
	// HostID is this scheduler's identity: the host_id of its rows in the
	// store and the sbx.host_id label of its containers. The pool
	// controller only ever touches rows and containers that carry it, so
	// it must be stable across restarts and different for every scheduler
	// that shares a Podman user or a database. Default: DefaultHostID.
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

	// ShutdownPolicy is ShutdownDrain (default), ShutdownLeave or
	// ShutdownDestroy.
	ShutdownPolicy string
	// ShutdownTimeout bounds the graceful stop. Default 30s.
	ShutdownTimeout time.Duration

	// EgressPermitPrivateCIDRs opens sub-ranges of private address space
	// to sandboxes whose policy allows them (egress.Config).
	EgressPermitPrivateCIDRs []string

	// CORSAllowedOrigins opens the data-plane routes to browsers on these
	// origins ("https://app.example.com", or "*"); empty means no CORS.
	// CORSAllowAPIKey additionally accepts a tenant API key from them.
	// See api.Config.
	CORSAllowedOrigins []string
	CORSAllowAPIKey    bool

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
	// OnMetricsListen is the same for the metrics listener.
	OnMetricsListen func(net.Addr)
}

// DefaultRunDir is $XDG_RUNTIME_DIR/sbx, or "" when XDG_RUNTIME_DIR is unset.
func DefaultRunDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "sbx")
	}
	return ""
}

// DefaultHostID is the host id used when none is configured:
//
//	<hostname>-u<uid>-<first 8 hex digits of SHA-256(absolute run dir)>
//
// It is derived, never random, so a restart finds its own sandboxes
// again. Machine and user are in it because one database may serve
// several hosts and several users of one host. The run dir is in it
// because that is what tells two schedulers of the same user apart: they
// cannot share a run dir (it holds the per-sandbox sockets, and each
// would delete the other's), and a warm sandbox is bound to the run dir
// it was created under (its sockets are bind-mounted from there), so a
// scheduler started on another run dir could not adopt the old one's
// sandboxes anyway. Consequently, moving the run dir without passing
// --host-id abandons the sandboxes of the old one.
func DefaultHostID(runDir string) (string, error) {
	abs, err := filepath.Abs(runDir)
	if err != nil {
		return "", fmt.Errorf("run dir: %w", err)
	}
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "", errors.New("could not determine the hostname; set --host-id")
	}
	// Keep the hostname within the host id alphabet and leave room for
	// the two suffixes (63 bytes in all).
	b := []byte(name)
	for i, ch := range b {
		ok := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-'
		if !ok || (i == 0 && (ch == '.' || ch == '_' || ch == '-')) {
			b[i] = 'x'
		}
	}
	if len(b) > 32 {
		b = b[:32]
	}
	sum := sha256.Sum256([]byte(abs))
	return fmt.Sprintf("%s-u%d-%s", b, os.Getuid(), hex.EncodeToString(sum[:4])), nil
}

// lockRunDir takes an exclusive advisory lock on RunDir/sbxd.lock for the
// life of the process. Two schedulers on one run dir would have the same
// default host id and would remove each other's sockets and containers;
// this turns that mistake into a start-up error. The kernel drops the
// lock when the process dies, however it dies.
func lockRunDir(dir string) (release func(), err error) {
	path := filepath.Join(dir, "sbxd.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("run dir %s is in use by another sbxd (lock %s is held); give each server its own --run-dir", dir, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// isLoopbackAddr reports whether a listen address can only be reached
// from this machine. A host name other than "localhost" is not resolved:
// it counts as reachable from elsewhere, as does an empty host (all
// interfaces).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		if c.HostID, err = DefaultHostID(c.RunDir); err != nil {
			return err
		}
	}
	if err := runtime.ValidHostID(c.HostID); err != nil {
		return fmt.Errorf("--host-id: %w", err)
	}
	if c.PodmanMode, err = ResolvePodmanMode(c.PodmanMode, c.PodmanSocket); err != nil {
		return err
	}
	if c.Runtimes, err = normaliseRuntimes(c.Runtimes); err != nil {
		return err
	}
	if c.PodmanMode == PodmanExternal && (len(c.Runtimes) != 1 || c.Runtimes[0].Path != "") {
		return errExternalRuntimes(c.Runtimes)
	}
	switch c.ShutdownPolicy {
	case "":
		c.ShutdownPolicy = ShutdownDrain
	case ShutdownDrain, ShutdownLeave, ShutdownDestroy:
	default:
		return fmt.Errorf("unknown shutdown policy %q (want %s, %s or %s)", c.ShutdownPolicy, ShutdownDrain, ShutdownLeave, ShutdownDestroy)
	}
	if c.MetricsListen == "" && c.MetricsToken == "" && !isLoopbackAddr(c.Listen) {
		return fmt.Errorf("refusing to serve unauthenticated /metrics on the non-loopback address %s: "+
			"set --metrics-token, or move the metrics to their own listener with --metrics-listen (for example 127.0.0.1:9090)", c.Listen)
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

// Preflight runs the host checks for a server that uses an existing
// Podman socket. PreflightWith takes the full configuration.
func Preflight(ctx context.Context, podmanSocket string, requireLinger bool) preflight.Report {
	return preflight.Run(ctx, preflight.Options{PodmanSocket: podmanSocket, RequireLinger: requireLinger})
}

// plannedSandboxes is how many sandboxes the templates allow to exist at
// once (the sum of max_total), and which templates set no cap at all.
func plannedSandboxes(templates []core.Template) (planned int64, uncapped []string) {
	for _, t := range templates {
		if t.MaxTotal > 0 {
			planned += int64(t.MaxTotal)
		} else {
			uncapped = append(uncapped, t.ID)
			planned += int64(t.WarmTarget) // the least such a pool holds
		}
	}
	return planned, uncapped
}

// checkCapacity warns when the configured pools can outgrow a per-user
// kernel quota (kernel keys, inotify instances). It never fails the
// start: the pool works up to the ceiling, and creates beyond it fail,
// are retried and trip the circuit breaker like any other create error.
func checkCapacity(ctx context.Context, log *slog.Logger, podmanSocket string, managed bool, templates []core.Template) {
	planned, uncapped := plannedSandboxes(templates)
	if len(uncapped) > 0 {
		log.Warn("templates without max_total: their pools are bounded only by tenant quotas, so the host's per-user limits "+
			"(kernel keys, inotify instances) cannot be checked against them; set max_total", "templates", uncapped)
	}
	if planned == 0 {
		return
	}
	report := preflight.Capacity(ctx, preflight.Options{PodmanSocket: podmanSocket, PlannedSandboxes: planned,
		ManagedPodman: managed, SkipServiceProbe: true})
	for _, c := range report.Checks {
		if c.Status == preflight.StatusWarn {
			log.Warn("capacity warning", "check", c.Name, "detail", c.Detail, "remediation", c.Remediation)
		} else {
			log.Debug("capacity check", "check", c.Name, "detail", c.Detail)
		}
	}
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
		// In managed mode the service is not probed here: the real one is
		// started below, and its failure is the better error.
		report, err := PreflightWith(ctx, PreflightConfig{PodmanMode: cfg.PodmanMode, PodmanSocket: cfg.PodmanSocket,
			Runtimes: cfg.Runtimes, RequireLinger: cfg.RequireLinger})
		if err != nil {
			return err
		}
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

	// A template that names a runtime this server has not enabled is a
	// start-up error, not a pool that fails every create. The per-user
	// kernel quotas that each running sandbox draws on can only be judged
	// against the size of the pools, which is known now too.
	templates, err := st.ListTemplates(ctx)
	if err != nil {
		return fmt.Errorf("read the templates: %w", err)
	}
	if err := CheckTemplateRuntimes(templates, podman.RuntimeNames(cfg.Runtimes)); err != nil {
		return err
	}
	if !cfg.SkipPreflight {
		checkCapacity(ctx, log, cfg.PodmanSocket, cfg.PodmanMode == PodmanManaged, templates)
	}

	// 3. Run dir and seccomp profile.
	if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	if err := os.Chmod(cfg.RunDir, 0o700); err != nil {
		return fmt.Errorf("chmod run dir: %w", err)
	}
	unlock, err := lockRunDir(cfg.RunDir)
	if err != nil {
		return err
	}
	defer unlock()
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

	// 5. Isolation backend: in managed mode a Podman service of our own
	// (see podman.go), in both modes the backend on its socket. The
	// service is stopped last, after the pool: sandboxes that stay (the
	// warm pool, with the drain policy) do not need it to keep running.
	pstack, err := startPodman(ctx, podmanParams{Mode: cfg.PodmanMode, Socket: cfg.PodmanSocket, RunDir: cfg.RunDir,
		Runtimes: cfg.Runtimes, RunscFlags: cfg.RunscFlags, Seccomp: seccomp, UsernsMode: cfg.UsernsMode, Logger: log})
	if err != nil {
		return err
	}
	defer pstack.Close(context.Background())
	backend := pstack.Backend
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
		RuntimeInfo:  runtimeInfo(backend.Capabilities()),
		ExtraMetrics: em.write, MetricsToken: cfg.MetricsToken, NoMetricsRoute: cfg.MetricsListen != "",
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
		CORSAllowedOrigins: cfg.CORSAllowedOrigins, CORSAllowAPIKey: cfg.CORSAllowAPIKey,
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
	serveErr := make(chan error, 2)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	var metricsSrv *http.Server
	if cfg.MetricsListen != "" {
		mln, err := net.Listen("tcp", cfg.MetricsListen)
		if err != nil {
			_ = httpSrv.Close()
			return fmt.Errorf("listen on %s (metrics): %w", cfg.MetricsListen, err)
		}
		metricsSrv = &http.Server{
			Handler:           apiSrv.MetricsHandler(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
			IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
			ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		defer metricsSrv.Close()
		go func() {
			if err := metricsSrv.Serve(mln); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("metrics listener: %w", err)
			}
		}()
		log.Info("metrics listening", "addr", mln.Addr().String(), "token_required", cfg.MetricsToken != "")
		if cfg.MetricsToken == "" && !isLoopbackAddr(cfg.MetricsListen) {
			log.Warn("/metrics is served without authentication on a non-loopback address; set --metrics-token unless the network already restricts who can reach it",
				"addr", mln.Addr().String())
		}
		if cfg.OnMetricsListen != nil {
			cfg.OnMetricsListen(mln.Addr())
		}
	}
	log.Info("sbxd listening", "addr", ln.Addr().String(), "host", cfg.HostID, "run_dir", cfg.RunDir,
		"podman_api", apiVersion, "podman_mode", cfg.PodmanMode, "podman_socket", backend.Config().SocketPath,
		"runtimes", podman.RuntimeNames(cfg.Runtimes), "seccomp", orDefault(seccomp, SeccompDefault), "agent_binary", agentBinary,
		"shutdown_policy", cfg.ShutdownPolicy, "cors_origins", cfg.CORSAllowedOrigins)
	if cfg.OnListen != nil {
		cfg.OnListen(ln.Addr())
	}

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	// Graceful stop, in this order (see the Shutdown* constants):
	//  1. stop accepting and end streams;
	//  2. stop the refill loop and wait for in-flight creates;
	//  3. apply the shutdown policy;
	//  4. stop the controller (waits for teardowns in flight).
	// Whatever is left is labelled and recorded, so the next start
	// reconciles it.
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
	if metricsSrv != nil {
		_ = metricsSrv.Close()
	}
	if err := ctrl.Quiesce(sctx); err != nil {
		log.Warn("in-flight creates were cancelled: the shutdown budget ran out", "err", err)
	}
	switch cfg.ShutdownPolicy {
	case ShutdownDrain:
		drain(sctx, log, st, ctrl, cfg.HostID)
	case ShutdownDestroy:
		// Not bounded by what is left of the budget alone: a destroy that
		// stops half-way defeats its purpose.
		dctx, dcancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		if _, err := ctrl.DestroyAll(dctx); err != nil {
			log.Error("shutdown policy destroy: not everything could be destroyed; run `sbxd admin host destroy`", "err", err)
		}
		dcancel()
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
