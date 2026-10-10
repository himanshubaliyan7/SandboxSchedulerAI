package podman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Files of a managed Podman service, all inside ServiceConfig.Dir.
const (
	// ServiceConfFile is the generated containers.conf. It must outlive
	// the service: conmon passes it on to the `podman container cleanup`
	// it runs when a container exits, which may be long after sbxd and
	// its service are gone.
	ServiceConfFile = "containers.conf"
	// ServicePidFile holds the pid (and process group) of the running
	// service, so that the next start can replace one that outlived its
	// sbxd.
	ServicePidFile = "service.pid"
	// ServiceLogFile receives the service's stdout and stderr. It is a
	// file and not a pipe on purpose: a pipe would be closed when sbxd
	// dies, and nothing that was started under the service may ever get a
	// SIGPIPE for that.
	ServiceLogFile = "service.log"
	// ServiceWorkDir is the service's working directory. conmon inherits
	// it, and conmon 2.1 creates a file named "oom" in its working
	// directory when the container it watches is OOM-killed (Podman 4.9
	// passes no --persist-dir). See OOMEvidence.
	ServiceWorkDir = "cwd"
)

// Defaults of a managed service.
const (
	DefaultServiceReadyTimeout = 30 * time.Second
	serviceStopGrace           = 5 * time.Second
	serviceBackoffMin          = 200 * time.Millisecond
	serviceBackoffMax          = 10 * time.Second
	// A service that stayed up this long is considered healthy again, so
	// the next death starts from the smallest backoff.
	serviceBackoffReset = 30 * time.Second
)

// ServiceConfig configures a managed Podman service: one `podman system
// service` process that belongs to one sbxd, with a configuration sbxd
// generates instead of the user's containers.conf.
type ServiceConfig struct {
	// Dir holds the generated configuration, the pid file, the log and
	// the working directory. It is created 0700. It must not be shared
	// by two services; the caller guarantees that (sbxd holds the run dir
	// lock).
	Dir string
	// SocketPath is the API socket the service listens on. Whoever can
	// open it can run code as this user, so it is created 0600 in a 0700
	// directory.
	SocketPath string
	// PodmanPath is the podman binary. Default: `podman` from PATH.
	PodmanPath string
	// Runtimes is the [engine.runtimes] table: the OCI runtimes the
	// service can run containers with, each with the absolute path of its
	// binary (for runsc: of the wrapper, see WriteRunscWrapper).
	Runtimes []OCIRuntime
	// DefaultRuntime is the service's default runtime. Default "crun".
	DefaultRuntime string
	// ConfPath, when set, is an existing containers.conf to run the
	// service with instead of generating one from Runtimes: the file an
	// earlier service of the same run dir was started with, for a tool
	// that must act on that service's containers with the same runtime
	// table. It is not modified.
	ConfPath string
	// ReadyTimeout bounds how long Start waits for the socket to answer.
	ReadyTimeout time.Duration
	Logger       *slog.Logger
}

var runtimeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

// OCIRuntime is one enabled OCI runtime: its name, as templates and
// Podman know it, and the absolute path of its binary.
type OCIRuntime struct {
	Name string
	Path string
}

// ParseRuntimes parses a runtime list such as "crun,runsc" or
// "crun,runsc=/opt/gvisor/runsc": comma-separated names, each with an
// optional =path. Names are unique; a path, when given, is absolute.
func ParseRuntimes(s string) ([]OCIRuntime, error) {
	var out []OCIRuntime
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, path, _ := strings.Cut(part, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !runtimeNameRE.MatchString(name) {
			return nil, fmt.Errorf("invalid runtime name %q (want letters, digits, '-' or '_')", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("runtime %q is listed twice", name)
		}
		if path != "" && !filepath.IsAbs(path) {
			return nil, fmt.Errorf("runtime %s: path %q must be absolute", name, path)
		}
		seen[name] = true
		out = append(out, OCIRuntime{Name: name, Path: path})
	}
	return out, nil
}

// RuntimeNames returns the names of rts, in order.
func RuntimeNames(rts []OCIRuntime) []string {
	out := make([]string, len(rts))
	for i, r := range rts {
		out[i] = r.Name
	}
	return out
}

// ContainersConf renders the containers.conf of a managed service. It is
// passed as CONTAINERS_CONF_OVERRIDE, so it is merged on top of the
// system's and the user's files: what is set here wins, and what is not
// (helper binary directories, a distribution's conmon path) keeps working
// as it does for the user's own Podman.
//
//   - keyring = false: the OCI runtime creates no session keyring per
//     container (Libpod passes --no-new-keyring), so a sandbox holds none
//     of the user's kernel.keys.maxkeys quota (200 by default), which was
//     the first density ceiling.
//   - default_sysctls = []: removes net.ipv4.ping_group_range, which every
//     container with a private network namespace otherwise gets and which
//     crun was seen to fail to write (EINVAL) under concurrent starts.
//   - log_driver = "none" and events_logger = "none": output is recorded
//     by the in-sandbox agent, and lifecycle events by the scheduler's own
//     event log; neither is written to the journal a second time.
//   - cgroup_manager = "systemd": limits are applied to a transient scope
//     of the user's service manager. Both the crun path and the runsc
//     wrapper depend on it, so it is pinned rather than inherited.
//   - [engine.runtimes]: exactly the enabled runtimes, by absolute path.
func ContainersConf(runtimes []OCIRuntime, defaultRuntime string) (string, error) {
	if defaultRuntime == "" {
		defaultRuntime = DefaultRuntime
	}
	var b strings.Builder
	b.WriteString("# Generated by sbxd for its private Podman service; rewritten on every start.\n")
	b.WriteString("# Used as CONTAINERS_CONF_OVERRIDE, also by the cleanup command of each container:\n")
	b.WriteString("# do not delete it while sandboxes of this run dir are alive.\n\n")
	b.WriteString("[containers]\nkeyring = false\ndefault_sysctls = []\nlog_driver = \"none\"\n\n")
	b.WriteString("[engine]\ncgroup_manager = \"systemd\"\nevents_logger = \"none\"\n")
	found := false
	for _, r := range runtimes {
		found = found || r.Name == defaultRuntime
	}
	if !found {
		return "", fmt.Errorf("podman: default runtime %q is not among the enabled runtimes %v", defaultRuntime, RuntimeNames(runtimes))
	}
	fmt.Fprintf(&b, "runtime = %s\n\n[engine.runtimes]\n", tomlString(defaultRuntime))
	for _, r := range runtimes {
		if !runtimeNameRE.MatchString(r.Name) {
			return "", fmt.Errorf("podman: invalid runtime name %q", r.Name)
		}
		if !filepath.IsAbs(r.Path) {
			return "", fmt.Errorf("podman: runtime %s: path %q must be absolute", r.Name, r.Path)
		}
		fmt.Fprintf(&b, "%s = [%s]\n", r.Name, tomlString(r.Path))
	}
	return b.String(), nil
}

// tomlString renders s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Service is a running managed Podman service and its supervisor. The
// supervisor restarts the process, with backoff, whenever it exits
// without Stop having been called. Containers are not children of the
// service (each has its own conmon, in its own systemd scope), so they
// survive a restart of the service and of sbxd.
type Service struct {
	cfg      ServiceConfig
	log      *slog.Logger
	confPath string
	workDir  string

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopping bool

	restarts atomic.Int64
	stopCh   chan struct{}
	done     chan struct{}
}

// StartService writes the configuration, replaces a service left behind
// by an earlier sbxd on the same directory, starts the service and waits
// until its socket answers.
func StartService(ctx context.Context, cfg ServiceConfig) (*Service, error) {
	if !filepath.IsAbs(cfg.Dir) || !filepath.IsAbs(cfg.SocketPath) {
		return nil, fmt.Errorf("podman service: Dir %q and SocketPath %q must be absolute", cfg.Dir, cfg.SocketPath)
	}
	if cfg.PodmanPath == "" {
		p, err := exec.LookPath("podman")
		if err != nil {
			return nil, fmt.Errorf("podman service: %w", err)
		}
		cfg.PodmanPath = p
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultServiceReadyTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Service{cfg: cfg, log: cfg.Logger.With("component", "podman-service"),
		confPath: filepath.Join(cfg.Dir, ServiceConfFile), workDir: filepath.Join(cfg.Dir, ServiceWorkDir),
		stopCh: make(chan struct{}), done: make(chan struct{})}

	for _, dir := range []string{cfg.Dir, s.workDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("podman service: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("podman service: %w", err)
		}
	}
	if cfg.ConfPath != "" {
		if _, err := os.Stat(cfg.ConfPath); err != nil {
			return nil, fmt.Errorf("podman service: %w", err)
		}
		s.confPath = cfg.ConfPath
	} else {
		conf, err := ContainersConf(cfg.Runtimes, cfg.DefaultRuntime)
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(s.confPath, []byte(conf), 0o600); err != nil {
			return nil, fmt.Errorf("podman service: write %s: %w", s.confPath, err)
		}
	}
	s.replaceStale()

	first := make(chan error, 1)
	go s.supervise(first)
	select {
	case err := <-first:
		if err != nil {
			<-s.done
			return nil, err
		}
	case <-ctx.Done():
		_ = s.Stop(context.Background())
		return nil, ctx.Err()
	}
	return s, nil
}

// SocketPath is the API socket of the service.
func (s *Service) SocketPath() string { return s.cfg.SocketPath }

// ConfPath is the generated containers.conf. The podman CLI sees the
// service's containers with all of their runtimes when it is run with
// CONTAINERS_CONF_OVERRIDE set to this path.
func (s *Service) ConfPath() string { return s.confPath }

// WorkDir is the service's working directory, where conmon leaves its
// "oom" files (see OOMEvidence).
func (s *Service) WorkDir() string { return s.workDir }

// Restarts is how many times the supervisor had to start the service
// again after it died.
func (s *Service) Restarts() int64 { return s.restarts.Load() }

// Pid is the pid of the running service process, or 0 while it is down.
func (s *Service) Pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Stop ends the service (SIGTERM to its process group, SIGKILL after a
// grace period) and the supervisor, and removes the socket and the pid
// file. The configuration, the log and the working directory stay: live
// containers still refer to them. Stop is idempotent.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		<-s.done
		return nil
	}
	s.stopping = true
	close(s.stopCh)
	cmd := s.cmd
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	grace := time.NewTimer(serviceStopGrace)
	defer grace.Stop()
	select {
	case <-s.done:
	case <-grace.C:
		s.killGroup(cmd)
		<-s.done
	case <-ctx.Done():
		s.killGroup(cmd)
		<-s.done
	}
	_ = os.Remove(s.cfg.SocketPath)
	_ = os.Remove(filepath.Join(s.cfg.Dir, ServicePidFile))
	return nil
}

func (s *Service) killGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// supervise owns the service process for the life of the Service. It runs
// on a locked OS thread that it never releases: the child is started with
// Pdeathsig, which the kernel delivers when the thread that forked the
// child exits, not when the process does. Go may otherwise retire the
// forking thread at any time and the service would get a spurious
// SIGTERM. The thread ends with this goroutine, after the last child has
// been reaped.
func (s *Service) supervise(first chan<- error) {
	goruntime.LockOSThread()
	defer close(s.done)
	backoff := serviceBackoffMin
	started := false
	for {
		cmd, err := s.spawn()
		if err == nil {
			err = s.waitReady(cmd)
		}
		if !started {
			if err != nil {
				s.reap(cmd)
				first <- err
				return
			}
			started = true
			first <- nil
		} else if err != nil {
			if s.isStopping() {
				s.reap(cmd)
				return
			}
			s.log.Error("the Podman service did not come back; retrying", "err", err, "backoff", backoff, "log", s.logPath())
			s.reap(cmd)
			if !s.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, serviceBackoffMax)
			continue
		} else {
			s.log.Info("the Podman service is back", "pid", cmd.Process.Pid, "restarts", s.restarts.Load())
		}

		up := time.Now()
		werr := cmd.Wait()
		s.mu.Lock()
		s.cmd = nil
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			return
		}
		// Whatever the dead process left in its group (a forked rootless
		// re-exec, a helper) must not hold the socket.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		s.restarts.Add(1)
		if time.Since(up) > serviceBackoffReset {
			backoff = serviceBackoffMin
		}
		s.log.Error("the Podman service died; restarting it (running sandboxes are not affected: each has its own conmon)",
			"exit", fmt.Sprint(werr), "backoff", backoff, "log_tail", s.logTail(5))
		if !s.sleep(backoff) {
			return
		}
		backoff = min(backoff*2, serviceBackoffMax)
	}
}

func (s *Service) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// processGone reports whether pid has exited (it no longer exists, or is
// a zombie waiting to be reaped).
func processGone(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// "pid (comm) S ...": the state follows the last ')'.
	i := bytes.LastIndexByte(raw, ')')
	return i < 0 || i+2 >= len(raw) || raw[i+2] == 'Z' || raw[i+2] == 'X'
}

// sleep waits for d, or returns false as soon as Stop is called.
func (s *Service) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.stopCh:
		return false
	}
}

// reap kills and waits for a process that failed to become ready.
func (s *Service) reap(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	s.mu.Lock()
	s.cmd = nil
	s.mu.Unlock()
}

func (s *Service) logPath() string { return filepath.Join(s.cfg.Dir, ServiceLogFile) }

// logTail returns the last n lines of the service log.
func (s *Service) logTail(n int) string {
	b, err := os.ReadFile(s.logPath())
	if err != nil {
		return ""
	}
	if len(b) > 16<<10 {
		b = b[len(b)-16<<10:]
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// spawn starts one service process. It must run on the supervisor's
// locked thread.
func (s *Service) spawn() (*exec.Cmd, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, errors.New("podman service: stopping")
	}
	// A socket file left by a dead service would make the new one fail
	// to bind (or, worse, make the readiness probe talk to nobody).
	if err := os.Remove(s.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("podman service: remove stale socket: %w", err)
	}
	// Keep one generation of the log.
	if fi, err := os.Stat(s.logPath()); err == nil && fi.Size() > 0 {
		_ = os.Rename(s.logPath(), s.logPath()+".old")
	}
	logf, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("podman service: %w", err)
	}
	defer logf.Close()

	cmd := exec.Command(s.cfg.PodmanPath, "system", "service", "--time=0", "unix://"+s.cfg.SocketPath)
	cmd.Dir = s.workDir
	cmd.Env = serviceEnv(os.Environ(), s.confPath)
	cmd.Stdin = nil // /dev/null
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Its own process group, so that Stop reaches a forked rootless
		// re-exec too and a terminal's Ctrl-C reaches only sbxd, which
		// then stops the service in order.
		Setpgid: true,
		// If sbxd is killed outright the service must not linger with a
		// socket nobody supervises. (The next start also replaces a
		// stale one through the pid file, should this not have worked.)
		Pdeathsig: syscall.SIGTERM,
	}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("podman service: start %s: %w", s.cfg.PodmanPath, err)
	}
	s.cmd = cmd
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := writeFileAtomic(filepath.Join(s.cfg.Dir, ServicePidFile), []byte(pid+"\n"), 0o600); err != nil {
		s.log.Warn("could not write the service pid file; a service that outlives this sbxd will not be replaced automatically", "err", err)
	}
	return cmd, nil
}

// serviceEnv is the environment of the service: the caller's, without the
// variables that would redirect Podman somewhere else or make it believe
// it was socket-activated, plus the generated configuration.
func serviceEnv(env []string, confPath string) []string {
	drop := map[string]bool{
		"CONTAINERS_CONF_OVERRIDE": true, // replaced below
		"CONTAINER_HOST":           true, // a remote connection is never wanted here
		"CONTAINER_CONNECTION":     true,
		"NOTIFY_SOCKET":            true, // sbxd's own readiness protocol, if any
		"LISTEN_FDS":               true, // sbxd's own socket activation, if any
		"LISTEN_PID":               true,
		"LISTEN_FDNAMES":           true,
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, "CONTAINERS_CONF_OVERRIDE="+confPath)
}

// waitReady polls the socket until the service answers /_ping, the
// process exits, the timeout passes or Stop is called. Podman creates the
// socket 0600 itself; that is checked, and repaired if it ever differs.
func (s *Service) waitReady(cmd *exec.Cmd) error {
	deadline := time.NewTimer(s.cfg.ReadyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := PingSocket(ctx, s.cfg.SocketPath)
		cancel()
		if err == nil {
			if fi, err := os.Stat(s.cfg.SocketPath); err == nil && fi.Mode().Perm()&0o077 != 0 {
				if err := os.Chmod(s.cfg.SocketPath, 0o600); err != nil {
					return fmt.Errorf("podman service: socket %s is accessible to others and cannot be fixed: %w", s.cfg.SocketPath, err)
				}
			}
			return nil
		}
		if processGone(cmd.Process.Pid) {
			return fmt.Errorf("podman service: `%s system service` exited before its socket answered: %s", s.cfg.PodmanPath, orText(s.logTail(8), "no output"))
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("podman service: socket %s did not answer within %s: %s", s.cfg.SocketPath, s.cfg.ReadyTimeout, orText(s.logTail(8), "no output"))
		case <-s.stopCh:
			return errors.New("podman service: stopped while starting")
		case <-tick.C:
		}
	}
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// replaceStale ends a service that an earlier sbxd left running on this
// directory (it was killed, and Pdeathsig did not reach the service).
// Only a process that still is `podman system service` on this very
// socket is touched; a recycled pid is not.
func (s *Service) replaceStale() {
	pidPath := filepath.Join(s.cfg.Dir, ServicePidFile)
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	defer os.Remove(pidPath)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return
	}
	if !isServiceProcess(pid, s.cfg.SocketPath) {
		return
	}
	s.log.Warn("replacing a Podman service left behind by an earlier sbxd", "pid", pid)
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(serviceStopGrace)
	for time.Now().Before(deadline) && isServiceProcess(pid, s.cfg.SocketPath) {
		time.Sleep(25 * time.Millisecond)
	}
	if isServiceProcess(pid, s.cfg.SocketPath) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(100 * time.Millisecond)
	}
}

// isServiceProcess reports whether pid is a `podman system service`
// listening on socket, going by its command line.
func isServiceProcess(pid int, socket string) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0})
	var hasService, hasSocket bool
	for _, a := range args {
		hasService = hasService || string(a) == "service"
		hasSocket = hasSocket || string(a) == "unix://"+socket
	}
	return hasService && hasSocket
}

// writeFileAtomic writes data to path through a temporary file in the
// same directory, so a reader never sees half a file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// PingSocket probes a Libpod API socket and returns the Libpod API
// version it reports.
func PingSocket(ctx context.Context, socket string) (string, error) {
	if fi, err := os.Stat(socket); err != nil {
		return "", err
	} else if fi.Mode()&os.ModeSocket == 0 {
		return "", errors.New("not a socket")
	}
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://d/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/_ping returned %s", resp.Status)
	}
	return resp.Header.Get("Libpod-API-Version"), nil
}
