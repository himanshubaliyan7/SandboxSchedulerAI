// Command sbxd is the single-node sandbox scheduler: the warm-pool
// controller, the egress proxy and the public REST API in one process.
//
//	sbxd serve      run the server
//	sbxd preflight  check that this host can run rootless sandboxes
//	sbxd admin      manage tenants, API keys, templates and webhooks; destroy a host's sandboxes
//	sbxd version    print the version
//
// Every serve flag has an environment fallback (SBX_DATABASE_URL,
// SBX_LISTEN, SBX_AGENT_BINARY, SBX_RUN_DIR, SBX_PODMAN_SOCKET, ...);
// run `sbxd serve -h` for the list.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman/runscshim"
	"github.com/himanshubaliyan7/warmbox/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: sbxd <command> [flags]

commands:
  serve [flags]                         run the scheduler and its API
  preflight [flags]                     check the host and print a report (exit status 1 on failure)
  admin <resource> <action> [id] [flags]
                                        manage tenants, API keys, templates and webhooks; destroy
                                        everything a host id owns (run "sbxd admin" for the list)
  version                               print the version

Run "sbxd <command> -h" (or "sbxd admin <resource> <action> -h") for the flags of a command.
Secrets (the database URL, the metrics token) are best given in the environment:
a flag is visible in the process list. Help output never prints their values.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = cmdServe(args[1:], stderr)
	case "preflight":
		err = cmdPreflight(args[1:], stdout, stderr)
	case "admin":
		err = cmdAdmin(args[1:], stdout, stderr)
	case "runsc-shim":
		// Not for people: Podman runs this, through a generated wrapper,
		// as the OCI runtime of the gVisor tier.
		return runscshim.Main(args[1:], stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "sbxd", version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "sbxd: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	}
	fmt.Fprintf(stderr, "sbxd: %v\n", err)
	return 1
}

// errUsage marks an error whose message has already been printed by the
// flag package.
var errUsage = errors.New("usage")

func parse(fs *flag.FlagSet, args []string) error {
	_, err := parsePositional(fs, args, 0)
	return err
}

// parsePositional parses flags and up to max positional arguments, which
// may come before, between or after the flags (the flag package alone
// stops at the first one, so "show python --json" would lose --json).
func parsePositional(fs *flag.FlagSet, args []string, max int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errUsage
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		if len(pos) == max {
			fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
			return nil, errUsage
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// secretValue is a string flag whose value must never be printed: it has
// no visible default, so `-h` shows the flag and not what the environment
// holds for it.
type secretValue struct{ p *string }

func (secretValue) String() string       { return "" }
func (s secretValue) Set(v string) error { *s.p = v; return nil }

// secretVar registers a flag whose fallback is an environment variable
// holding a secret (a database URL with its password, a token). The
// variable's value becomes the flag's value without ever being its
// printed default; the usage text only says whether the variable is set.
func secretVar(fs *flag.FlagSet, p *string, name, env, usage string) {
	*p = os.Getenv(env)
	state := "not set"
	if *p != "" {
		state = "set, value not shown"
	}
	fs.Var(secretValue{p}, name, fmt.Sprintf("%s [%s: %s]; prefer the variable, a flag is visible in the process list", usage, env, state))
}

// Environment fallbacks: a flag's default is the variable's value when it
// is set, so a flag on the command line always wins.

func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) bool {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		fmt.Fprintf(os.Stderr, "sbxd: ignoring %s=%q: not a boolean\n", name, v)
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Fprintf(os.Stderr, "sbxd: ignoring %s=%q: not a duration\n", name, v)
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		fmt.Fprintf(os.Stderr, "sbxd: ignoring %s=%q: not a number\n", name, v)
	}
	return def
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		fmt.Fprintf(os.Stderr, "sbxd: ignoring %s=%q: not an integer\n", name, v)
	}
	return def
}

// defaultAgentBinary is sbx-agent next to this executable, which is where
// `make build` puts it.
func defaultAgentBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "sbx-agent")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q (want debug, info, warn or error)", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q (want json or text)", format)
}

func cmdServe(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("sbxd serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfg                 server.Config
		logLevel, logFormat string
		permitPrivate       string
		maxUpload           string
		corsOrigins         string
		runtimes, runscArgs string
	)
	secretVar(fs, &cfg.DatabaseURL, "database-url", "SBX_DATABASE_URL", "Postgres `url` (required)")
	fs.StringVar(&cfg.Listen, "listen", envStr("SBX_LISTEN", "127.0.0.1:8080"), "API listen address [SBX_LISTEN]")
	fs.StringVar(&cfg.MetricsListen, "metrics-listen", envStr("SBX_METRICS_LISTEN", ""),
		"serve /metrics on this address instead of the API listener, e.g. 127.0.0.1:9090 [SBX_METRICS_LISTEN]")
	secretVar(fs, &cfg.MetricsToken, "metrics-token", "SBX_METRICS_TOKEN",
		"bearer `token` required by /metrics; without it and without --metrics-listen, sbxd refuses a non-loopback --listen")
	fs.StringVar(&cfg.AgentBinary, "agent-binary", envStr("SBX_AGENT_BINARY", defaultAgentBinary()),
		"path of the static sbx-agent binary [SBX_AGENT_BINARY] (default: sbx-agent next to sbxd)")
	fs.StringVar(&cfg.RunDir, "run-dir", envStr("SBX_RUN_DIR", server.DefaultRunDir()),
		"directory for per-sandbox sockets; keep it short [SBX_RUN_DIR]")
	fs.StringVar(&cfg.PodmanMode, "podman-mode", envStr("SBX_PODMAN_MODE", ""),
		"managed: sbxd starts and supervises its own Podman service under the run dir, with its own configuration "+
			"(no kernel key per sandbox, no default sysctls); external: use an existing Podman socket [SBX_PODMAN_MODE] "+
			"(default managed, or external when --podman-socket is given)")
	fs.StringVar(&cfg.PodmanSocket, "podman-socket", envStr("SBX_PODMAN_SOCKET", ""),
		"Podman API socket of an existing service; implies --podman-mode external [SBX_PODMAN_SOCKET] "+
			"(default in external mode: $XDG_RUNTIME_DIR/podman/podman.sock)")
	addRuntimeFlags(fs, &runtimes)
	fs.StringVar(&runscArgs, "runsc-flags", envStr("SBX_RUNSC_FLAGS", ""),
		"extra runsc flags for the gVisor tier, space-separated, e.g. \"--platform=kvm\" [SBX_RUNSC_FLAGS]")
	fs.StringVar(&cfg.HostID, "host-id", envStr("SBX_HOST_ID", ""),
		"identity of this scheduler: the host_id of its rows and the sbx.host_id label of its containers; it only touches "+
			"what carries it, so keep it stable across restarts and unique per server [SBX_HOST_ID] "+
			"(default: <hostname>-u<uid>-<hash of the run dir>)")
	fs.BoolVar(&cfg.SkipPreflight, "skip-preflight", envBool("SBX_SKIP_PREFLIGHT", false), "start even if a host check fails [SBX_SKIP_PREFLIGHT]")
	fs.BoolVar(&cfg.RequireLinger, "require-linger", envBool("SBX_REQUIRE_LINGER", false),
		"fail preflight when systemd lingering is off for this user [SBX_REQUIRE_LINGER]")
	fs.StringVar(&cfg.SeccompProfile, "seccomp-profile", envStr("SBX_SECCOMP_PROFILE", ""),
		"seccomp profile: empty for the embedded hardened profile, \""+server.SeccompDefault+"\", or an absolute path [SBX_SECCOMP_PROFILE]")
	fs.StringVar(&cfg.UsernsMode, "userns", envStr("SBX_USERNS", ""), "user namespace mapping: keep-id (default), nomap, host, auto [SBX_USERNS]")
	fs.StringVar(&cfg.ShutdownPolicy, "shutdown-policy", envStr("SBX_SHUTDOWN_POLICY", server.ShutdownDrain),
		"what a graceful stop leaves behind: drain (release tenant sandboxes, keep the warm ones for the next start), "+
			"leave (touch nothing; the next start recycles tenant sandboxes) or destroy (remove everything this host id owns) [SBX_SHUTDOWN_POLICY]")
	fs.StringVar(&corsOrigins, "cors-allowed-origins", envStr("SBX_CORS_ALLOWED_ORIGINS", ""),
		"comma-separated browser origins allowed to call the data-plane routes with a sandbox access token, e.g. "+
			"https://app.example.com, or * for any; empty disables CORS [SBX_CORS_ALLOWED_ORIGINS]")
	fs.BoolVar(&cfg.CORSAllowAPIKey, "cors-allow-api-key", envBool("SBX_CORS_ALLOW_API_KEY", false),
		"also accept a tenant API key on the data-plane routes from those origins (not recommended: keys do not belong in browsers) [SBX_CORS_ALLOW_API_KEY]")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", envDuration("SBX_SHUTDOWN_TIMEOUT", 30*time.Second), "budget of a graceful stop [SBX_SHUTDOWN_TIMEOUT]")
	fs.StringVar(&permitPrivate, "egress-permit-private-cidrs", envStr("SBX_EGRESS_PERMIT_PRIVATE_CIDRS", ""),
		"comma-separated private ranges sandboxes may reach when their policy allows it [SBX_EGRESS_PERMIT_PRIVATE_CIDRS]")
	finishWebhookFlags := webhookFlags(fs, &cfg.Webhooks)
	fs.StringVar(&maxUpload, "max-upload-size", envStr("SBX_MAX_UPLOAD_SIZE", "256m"), "largest file upload or stdin write, e.g. 64m, 1g [SBX_MAX_UPLOAD_SIZE]")
	fs.DurationVar(&cfg.TouchInterval, "touch-interval", envDuration("SBX_TOUCH_INTERVAL", 0),
		"debounce of the activity timestamp; keep it well below the shortest idle timeout (default 5s) [SBX_TOUCH_INTERVAL]")
	fs.DurationVar(&cfg.ClaimWait, "claim-wait", envDuration("SBX_CLAIM_WAIT", 0), "how long a create waits on an empty pool (default 5s) [SBX_CLAIM_WAIT]")
	fs.DurationVar(&cfg.AuthCacheTTL, "auth-cache-ttl", envDuration("SBX_AUTH_CACHE_TTL", 0),
		"how long API-key lookups are cached; bounds how long a revoked key works (default 5s, negative disables) [SBX_AUTH_CACHE_TTL]")
	fs.Float64Var(&cfg.CreateRatePerSec, "create-rate", envFloat("SBX_CREATE_RATE", 0),
		"sandbox creations per second per tenant (default: the tenant's own rate) [SBX_CREATE_RATE]")
	fs.IntVar(&cfg.CreateBurst, "create-burst", envInt("SBX_CREATE_BURST", 0), "burst of the creation bucket (default: the tenant's own burst) [SBX_CREATE_BURST]")
	fs.DurationVar(&cfg.SweepInterval, "sweep-interval", envDuration("SBX_SWEEP_INTERVAL", 0), "how often timers are enforced (default 1s) [SBX_SWEEP_INTERVAL]")
	fs.DurationVar(&cfg.ReconcileInterval, "reconcile-interval", envDuration("SBX_RECONCILE_INTERVAL", 0),
		"how often containers are reconciled with the store (default 10s) [SBX_RECONCILE_INTERVAL]")
	fs.StringVar(&logLevel, "log-level", envStr("SBX_LOG_LEVEL", "info"), "debug, info, warn or error [SBX_LOG_LEVEL]")
	fs.StringVar(&logFormat, "log-format", envStr("SBX_LOG_FORMAT", "text"), "text or json [SBX_LOG_FORMAT]")
	if err := parse(fs, args); err != nil {
		return err
	}
	logger, err := newLogger(stderr, logLevel, logFormat)
	if err != nil {
		return err
	}
	cfg.Logger = logger
	finishWebhookFlags()
	if cfg.Runtimes, err = podman.ParseRuntimes(runtimes); err != nil {
		return fmt.Errorf("--runtimes: %w", err)
	}
	cfg.RunscFlags = strings.Fields(runscArgs)
	if cfg.MaxUploadBytes, err = parseSize(maxUpload); err != nil {
		return fmt.Errorf("--max-upload-size: %w", err)
	}
	for _, o := range strings.Split(corsOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, o)
		}
	}
	for _, c := range strings.Split(permitPrivate, ",") {
		if c = strings.TrimSpace(c); c != "" {
			cfg.EgressPermitPrivateCIDRs = append(cfg.EgressPermitPrivateCIDRs, c)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// A second signal during the graceful stop kills the process the
	// default way.
	go func() { <-ctx.Done(); stop() }()
	return server.Run(ctx, cfg)
}

// addRuntimeFlags registers --runtimes, shared by serve, preflight and
// the admin commands that must agree with the server about it.
func addRuntimeFlags(fs *flag.FlagSet, runtimes *string) {
	fs.StringVar(runtimes, "runtimes", envStr("SBX_RUNTIMES", ""),
		"OCI runtimes (isolation tiers) templates may use, comma-separated, each with an optional path: "+
			"crun,runsc or crun,runsc=/opt/gvisor/runsc [SBX_RUNTIMES] (default crun)")
}

func cmdPreflight(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sbxd preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("podman-mode", envStr("SBX_PODMAN_MODE", ""),
		"check for a server in this mode: managed (the default without --podman-socket) or external [SBX_PODMAN_MODE]")
	socket := fs.String("podman-socket", envStr("SBX_PODMAN_SOCKET", ""), "Podman API socket of an existing service; implies external mode [SBX_PODMAN_SOCKET]")
	var runtimes string
	addRuntimeFlags(fs, &runtimes)
	linger := fs.Bool("require-linger", envBool("SBX_REQUIRE_LINGER", false), "treat missing lingering as a failure [SBX_REQUIRE_LINGER]")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	enabled, err := podman.ParseRuntimes(runtimes)
	if err != nil {
		return fmt.Errorf("--runtimes: %w", err)
	}
	report, err := server.PreflightWith(ctx, server.PreflightConfig{PodmanMode: *mode, PodmanSocket: *socket,
		Runtimes: enabled, RequireLinger: *linger, ProbeService: true})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprint(stdout, report.String())
	}
	if !report.OK() {
		return fmt.Errorf("%d host check(s) failed", len(report.Failed()))
	}
	return nil
}

// parseSize parses a byte count with an optional binary suffix: 4096,
// 64k, 512m, 2g (also kb/mb/gb and kib/mib/gib, case-insensitive).
func parseSize(s string) (int64, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, suf := range []struct {
		name string
		mult int64
	}{{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"b", 1}} {
		if strings.HasSuffix(s, suf.name) {
			s, mult = strings.TrimSuffix(s, suf.name), suf.mult
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || n < 0 || n*float64(mult) > float64(1<<62) {
		return 0, fmt.Errorf("invalid size %q (want e.g. 4096, 64k, 512m, 2g)", orig)
	}
	return int64(n * float64(mult)), nil
}
