package podman

// Integration tests against real rootless Podman. By default they start
// managed services of their own, as sbxd does (one per runtime), and run
// on the tier named by SBX_TEST_RUNTIME (crun, or runsc with SBX_TEST_RUNSC
// naming the gVisor binary); with SBX_PODMAN_SOCKET set they use that
// external service instead. They are skipped when Podman is not there,
// and need the test images to be present locally:
//
//	podman pull docker.io/library/alpine:3.20 docker.io/library/python:3.12-alpine
//
// The containers run `sleep` instead of the agent (Config.Command), and
// the in-container checks use the podman CLI's exec, which the product
// itself never does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman/runscshim"
)

const (
	itAlpine = "docker.io/library/alpine:3.20"
	itPython = "docker.io/library/python:3.12-alpine"
)

// itHostID labels every container of this test process. A scheduler
// running for the same Podman user at the same time (a dev server, the
// end-to-end test) acts only on its own host id, so it leaves these
// alone; the name carries the pid so that two test processes differ too.
var itHostID = fmt.Sprintf("it-podman-%d", os.Getpid())

// itWatchOOMFile cleans up after conmon. When a container is OOM-killed,
// conmon (2.1.10) creates an empty file named "oom" in its working
// directory, in addition to the one in --persist-dir when that is given;
// Podman 4.9.3 gives neither a persist dir nor a working directory, so
// conmon inherits the working directory of the process that started the
// container. With the API socket that is the Podman service, not this
// test: for the systemd-activated service it is the user's home (shown to
// be the service's directory by running a service elsewhere: the file
// followed it). No create option moves it, and this test cannot move the
// service, so it removes the file afterwards, and only if the file was
// not there before, is empty and is a regular file. An operator who minds
// the file can give podman.service a WorkingDirectory in a systemd
// drop-in.
func itWatchOOMFile(t *testing.T) {
	t.Helper()
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, home)
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd) // conmon started by the podman CLI on this test's behalf
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, "oom")
		if _, err := os.Lstat(path); err == nil {
			continue // not ours to remove
		}
		t.Cleanup(func() {
			if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
				if os.Remove(path) == nil {
					t.Logf("removed %s, left by conmon for an OOM-killed container", path)
				}
			}
		})
	}
}

func itResources() core.Resources {
	return core.Resources{
		CPUs:         0.5,
		MemoryBytes:  256 << 20,
		PidsLimit:    64,
		WorkdirBytes: 64 << 20,
		TmpBytes:     32 << 20,
		NofileLimit:  1024,
	}
}

// itRuntime is the OCI runtime (isolation tier) the integration tests
// run on: SBX_TEST_RUNTIME, crun by default. With runsc the gVisor binary
// is SBX_TEST_RUNSC, or runsc from PATH.
func itRuntime() string {
	if r := os.Getenv("SBX_TEST_RUNTIME"); r != "" {
		return r
	}
	return DefaultRuntime
}

func itGVisor() bool { return itRuntime() == RunscRuntime }

// itManaged is the managed Podman services of this test process: one per
// runtime, as sbxd runs them, started on first use and stopped by
// TestMain. With SBX_PODMAN_SOCKET set the tests use that external
// service instead (crun only).
var itManaged struct {
	once     sync.Once
	err      error
	dir      string
	services []*Service
	conf     string // CONTAINERS_CONF_OVERRIDE for the podman CLI
	sockets  map[string]string
	workDirs []string
}

func itStartManaged() {
	m := &itManaged
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		m.err = errors.New("XDG_RUNTIME_DIR is not set")
		return
	}
	if m.dir, m.err = os.MkdirTemp(base, "sbx-it-"); m.err != nil {
		return
	}
	enabled := []OCIRuntime{{Name: DefaultRuntime}}
	var shim []string
	if itGVisor() {
		self, err := os.Executable()
		if err != nil {
			m.err = err
			return
		}
		// TestMain makes the test binary act as the shim.
		shim = []string{self, "runsc-shim"}
		enabled = append(enabled, OCIRuntime{Name: RunscRuntime, Path: os.Getenv("SBX_TEST_RUNSC")})
	}
	table, err := PrepareRuntimes(m.dir, enabled, shim, strings.Fields(os.Getenv("SBX_TEST_RUNSC_FLAGS")))
	if err != nil {
		m.err = err
		return
	}
	m.sockets = map[string]string{}
	for _, r := range enabled {
		svc, err := StartService(context.Background(), ServiceConfig{
			Dir: filepath.Join(m.dir, "svc-"+r.Name), SocketPath: filepath.Join(m.dir, r.Name+".sock"),
			Runtimes: table, DefaultRuntime: r.Name, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			m.err = fmt.Errorf("runtime %s: %w", r.Name, err)
			return
		}
		m.services = append(m.services, svc)
		m.sockets[r.Name] = svc.SocketPath()
		m.workDirs = append(m.workDirs, svc.WorkDir())
		if r.Name == DefaultRuntime {
			m.conf = svc.ConfPath()
		}
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "runsc-shim" {
		os.Exit(runscshim.Main(os.Args[2:], os.Stderr))
	}
	if os.Getenv(fakeConmonEnv) != "" {
		fakeConmon() // never returns; see leftovers_test.go
	}
	code := m.Run()
	for _, svc := range itManaged.services {
		_ = svc.Stop(context.Background())
	}
	if itManaged.dir != "" {
		_ = os.RemoveAll(itManaged.dir)
	}
	os.Exit(code)
}

// itPodman builds a podman CLI command that sees the test's containers
// with the runtime table they were created with.
func itPodman(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "podman", args...)
	if itManaged.conf != "" {
		cmd.Env = append(os.Environ(), "CONTAINERS_CONF_OVERRIDE="+itManaged.conf)
	}
	return cmd
}

// itBackend returns a Backend on real Podman, or skips: on managed
// services of this test process, or on $SBX_PODMAN_SOCKET.
func itBackend(t *testing.T, mutate func(*Config)) *Backend {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman CLI not found")
	}
	// The profile must outlive the test's containers but need not outlive
	// the test, so the test's temp dir is the right place.
	profile, err := WriteSeccompProfile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{SeccompProfilePath: profile, Command: []string{"sleep", "600"}}
	if socket := os.Getenv("SBX_PODMAN_SOCKET"); socket != "" {
		if itGVisor() {
			t.Fatal("SBX_TEST_RUNTIME=runsc needs managed services: unset SBX_PODMAN_SOCKET")
		}
		if _, err := os.Stat(socket); err != nil {
			t.Skipf("no Podman socket at %s", socket)
		}
		cfg.SocketPath = socket
	} else {
		if os.Getenv("XDG_RUNTIME_DIR") == "" {
			t.Skip("XDG_RUNTIME_DIR is not set")
		}
		itManaged.once.Do(itStartManaged)
		if itManaged.err != nil {
			if itGVisor() {
				t.Fatalf("managed Podman services for runsc: %v", itManaged.err)
			}
			t.Skipf("no managed Podman service: %v", itManaged.err)
		}
		cfg.SocketPath = itManaged.sockets[DefaultRuntime]
		cfg.OOMEvidenceDirs = itManaged.workDirs
		if itGVisor() {
			cfg.Runtimes = []string{DefaultRuntime, RunscRuntime}
			cfg.RuntimeSockets = map[string]string{RunscRuntime: itManaged.sockets[RunscRuntime]}
		}
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, err := b.Ping(ctx)
	if err != nil {
		t.Skipf("Podman does not answer: %v", err)
	}
	t.Logf("Libpod API version %s, runtime %s", v, itRuntime())
	return b
}

// itCreate creates a sandbox and registers its destruction.
func itCreate(t *testing.T, b *Backend, image string, res core.Resources) runtime.Instance {
	t.Helper()
	id := fmt.Sprintf("it-%d", time.Now().UnixNano())
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	inst, err := b.Create(ctx, runtime.Spec{
		SandboxID:  id,
		HostID:     itHostID,
		Generation: 7,
		AgentEpoch: "epoch-it",
		Template:   core.Template{ID: "tpl-it", Image: image, Runtime: itRuntime(), Resources: res, Network: core.NetworkAllowlist},
		HostRunDir: runDir,
	})
	if IsStatus(err, http.StatusNotFound) {
		t.Skipf("image %s is not present locally (podman pull it to run this test): %v", image, err)
	}
	if err != nil && strings.Contains(err.Error(), "not enough unused IDs in user namespace") {
		// userns=auto needs a free block of subordinate IDs, and every
		// keep-id container of this user (a warm pool left by a server,
		// another test) holds the whole range.
		t.Skipf("userns auto cannot be tested while other containers of this user hold the subordinate ID range: %v", err)
	}
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := b.Destroy(ctx, inst.ID); err != nil {
			t.Errorf("cleanup Destroy: %v", err)
		}
	})
	if inst.HostID != itHostID {
		t.Fatalf("Create returned host id %q, want %q", inst.HostID, itHostID)
	}
	if inst.SandboxID != id || inst.Generation != 7 || inst.AgentEpoch != "epoch-it" || inst.TemplateID != "tpl-it" || !inst.Running {
		t.Fatalf("Create returned %+v", inst)
	}
	return inst
}

// itExec runs a shell snippet in the container and returns its combined
// output and exit error.
func itExec(t *testing.T, id, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := itPodman(ctx, "exec", id, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func itMustExec(t *testing.T, id, script string) string {
	t.Helper()
	out, err := itExec(t, id, script)
	if err != nil {
		t.Fatalf("exec %q: %v\n%s", script, err, out)
	}
	return out
}

func itCgroupFile(t *testing.T, b *Backend, id, name string) string {
	t.Helper()
	_, st, err := b.inspect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	dir := statsDir(b.cfg.CgroupRoot, st.CgroupPath)
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s (podman cgroup path %q): %v", name, st.CgroupPath, err)
	}
	return strings.TrimSpace(string(raw))
}

func TestIntegrationHardeningAndLimits(t *testing.T) {
	b := itBackend(t, nil)
	res := itResources()
	inst := itCreate(t, b, itAlpine, res)
	ctx := context.Background()

	// Limits, read from the host side of the cgroup. Under gVisor the
	// cgroup is the scope the runsc shim made, and its pids.max is the
	// sentry's backstop (gvisorSpec); the sandbox's own process limit is
	// RLIMIT_NPROC, checked below.
	wantPids := "64"
	if itGVisor() {
		wantPids = strconv.Itoa(64*gvisorTasksPerPid + gvisorTasksReserve)
	}
	for file, want := range map[string]string{
		"memory.max":       "268435456",
		"memory.swap.max":  "0",
		"memory.high":      "max", // deliberately unset, see buildCreateSpec
		"memory.oom.group": "1",
		"cpu.max":          "50000 100000",
		"pids.max":         wantPids,
	} {
		if got := itCgroupFile(t, b, inst.ID, file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		} else {
			t.Logf("%s = %s", file, got)
		}
	}

	// The runtime that was asked for is the one Podman used, and under
	// gVisor the kernel the sandbox sees is the sentry.
	if out, err := itPodman(ctx, "inspect", "-f", "{{.OCIRuntime}}", inst.ID).Output(); err != nil || strings.TrimSpace(string(out)) != itRuntime() {
		t.Errorf("OCIRuntime = %q (%v), want %q", strings.TrimSpace(string(out)), err, itRuntime())
	}
	kernel := itMustExec(t, inst.ID, "uname -r; cat /proc/version; dmesg 2>&1 | head -1")
	t.Logf("kernel as seen from the sandbox:\n%s", kernel)
	if gv := strings.Contains(kernel, "gvisor") && strings.Contains(kernel, "Starting gVisor"); gv != itGVisor() {
		t.Errorf("runtime %s, but the sandbox's kernel is:\n%s", itRuntime(), kernel)
	}
	if itGVisor() {
		if got := itMustExec(t, inst.ID, "ulimit -u"); got != "64" {
			t.Errorf("RLIMIT_NPROC = %q, want 64 (the sandbox's process limit under gVisor)", got)
		}
	}

	// Ownership label and default environment.
	if out, err := itPodman(context.Background(), "inspect", "-f", `{{index .Config.Labels "sbx.host_id"}}`, inst.ID).Output(); err != nil ||
		strings.TrimSpace(string(out)) != itHostID {
		t.Errorf("sbx.host_id label = %q (%v), want %q", strings.TrimSpace(string(out)), err, itHostID)
	}
	if got := itMustExec(t, inst.ID, `echo "$HOME $TMPDIR $PYTHONPYCACHEPREFIX"`); got != "/workspace /tmp /tmp/pycache" {
		t.Errorf("default environment = %q, want /workspace /tmp /tmp/pycache", got)
	}
	// What the API cannot switch off: the default sysctl of the service's
	// containers.conf is in the OCI spec although the network is "none",
	// and (unless the service runs with keyring = false) the container
	// holds a kernel key. Logged, not asserted: both depend on the host's
	// containers.conf.
	if out, err := itPodman(context.Background(), "inspect", "-f", "{{.OCIConfigPath}}", inst.ID).Output(); err == nil {
		// The bundle belongs to a subordinate uid under keep-id, hence unshare.
		if raw, err := itPodman(context.Background(), "unshare", "cat", strings.TrimSpace(string(out))).Output(); err == nil {
			var oci struct {
				Linux struct {
					Sysctl map[string]string `json:"sysctl"`
				} `json:"linux"`
			}
			if json.Unmarshal(raw, &oci) == nil {
				t.Logf("sysctls in the generated OCI spec (netns none): %v", oci.Linux.Sysctl)
			}
		} else {
			t.Logf("OCI spec not readable: %v", err)
		}
	} else {
		t.Logf("podman inspect OCIConfigPath: %v", err)
	}

	// Identity and privilege.
	if got := itMustExec(t, inst.ID, "id -u; id -g"); got != "1000\n1000" {
		t.Errorf("uid/gid = %q, want 1000/1000", got)
	}
	status := itMustExec(t, inst.ID, "grep -E '^(Cap(Inh|Prm|Eff|Bnd|Amb)|NoNewPrivs|Seccomp):' /proc/self/status")
	t.Logf("/proc/self/status:\n%s", status)
	for _, line := range strings.Split(status, "\n") {
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		switch {
		case strings.HasPrefix(k, "Cap") && strings.Trim(v, "0") != "":
			t.Errorf("%s = %s, want all zero", k, v)
		case k == "NoNewPrivs" && v != "1":
			t.Errorf("NoNewPrivs = %s, want 1", v)
		case k == "Seccomp" && v != "2" && !itGVisor():
			t.Errorf("Seccomp = %s, want 2 (filter)", v)
		case k == "Seccomp" && v != "0" && itGVisor():
			// Documented difference: runsc does not load the OCI profile
			// (gvisorSpec). If this ever changes, the docs must too.
			t.Errorf("Seccomp = %s under gVisor, where the OCI profile is documented as not applied", v)
		}
	}

	// Filesystem: read-only root, capped tmpfs, usable work dir.
	if out, err := itExec(t, inst.ID, "touch /root-write-test"); err == nil {
		t.Errorf("writing to / succeeded: %s", out)
	}
	if out, err := itExec(t, inst.ID, "touch /etc/x"); err == nil {
		t.Errorf("writing to /etc succeeded: %s", out)
	}
	mounts := itMustExec(t, inst.ID, "grep -E ' (/|/workspace|/tmp|/var/tmp|/run|/dev/shm|/run/sbx) ' /proc/mounts")
	t.Logf("/proc/mounts:\n%s", mounts)
	for _, want := range []struct{ dest, opt string }{
		{" /workspace tmpfs ", "size=65536k"},
		{" /tmp tmpfs ", "size=32768k"},
		{" /var/tmp tmpfs ", "size=1024k"},
		{" /run tmpfs ", "size=1024k"},
		{" /dev/shm ", "size=16384k"},
	} {
		ok := false
		for _, line := range strings.Split(mounts, "\n") {
			if !strings.Contains(line, want.dest) {
				continue
			}
			ok = strings.Contains(line, want.opt) && strings.Contains(line, "nosuid") && strings.Contains(line, "nodev")
			if itGVisor() {
				// The sentry's /proc/mounts has no nodev (its tmpfs
				// cannot hold host devices), and /dev/shm is a gofer
				// mount whose cap is the host tmpfs behind it; that
				// cap is checked by filling it, below.
				ok = strings.Contains(line, "nosuid") && (strings.Contains(line, want.opt) || want.dest == " /dev/shm ")
			}
		}
		if !ok {
			t.Errorf("mount %q: want option %s with nosuid,nodev", want.dest, want.opt)
		}
	}
	rootLine := ""
	for _, line := range strings.Split(mounts, "\n") {
		if f := strings.Fields(line); len(f) > 3 && f[1] == "/" {
			rootLine = line
			if !strings.HasPrefix(f[3], "ro,") && !strings.Contains(f[3], ",ro,") {
				t.Errorf("rootfs is not read-only: %s", line)
			}
		}
	}
	if rootLine == "" {
		t.Error("no rootfs line in /proc/mounts")
	}
	itMustExec(t, inst.ID, "cd /workspace && echo hi > f && [ \"$(stat -c %u /workspace)\" = 1000 ] && printf '#!/bin/sh\\necho ran\\n' > x.sh && chmod +x x.sh && ./x.sh")
	itMustExec(t, inst.ID, "printf '#!/bin/sh\\necho ran\\n' > /tmp/x.sh && chmod +x /tmp/x.sh && /tmp/x.sh")
	itMustExec(t, inst.ID, "echo x > /dev/shm/x && echo x > /run/sbx/x && [ \"$HOME\" = /workspace ] && [ \"$PWD\" = /workspace ]")
	if out, err := itExec(t, inst.ID, "dd if=/dev/zero of=/tmp/fill bs=1M count=40 2>&1"); err == nil {
		t.Errorf("writing 40 MiB into a 32 MiB /tmp succeeded: %s", out)
	}
	itMustExec(t, inst.ID, "rm -f /tmp/fill")
	if out, err := itExec(t, inst.ID, "dd if=/dev/zero of=/dev/shm/fill bs=1M count=20 2>&1"); err == nil {
		t.Errorf("writing 20 MiB into a 16 MiB /dev/shm succeeded: %s", out)
	}
	itMustExec(t, inst.ID, "rm -f /dev/shm/fill")

	// Network: loopback only. (gVisor's sysfs has no /sys/class/net, so
	// the interfaces are read from /proc/net/dev on both tiers.)
	if got := itMustExec(t, inst.ID, "tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' ' | sort | tr '\\n' ' '"); got != "lo" {
		t.Errorf("network interfaces = %q, want only lo", got)
	}

	// Seccomp: nested user namespaces are refused. Under gVisor they are
	// the sentry's own and allowed; see TestIntegrationPythonUnderSeccomp.
	if out, err := itExec(t, inst.ID, "unshare -U true"); (err == nil) != itGVisor() {
		t.Errorf("unshare -U: err=%v output=%s", err, out)
	} else {
		t.Logf("unshare -U: err=%v %s", err, out)
	}
	if got := itMustExec(t, inst.ID, "echo a b c | tr ' ' '\\n' | sort -r | head -1"); got != "c" {
		t.Errorf("pipeline output %q", got)
	}
	if got := itMustExec(t, inst.ID, "ulimit -n"); got != "1024" {
		t.Errorf("nofile = %q, want 1024", got)
	}

	// Stats, list, pause.
	st, err := b.Stats(ctx, inst.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	t.Logf("Stats: %+v", st)
	if st.MemoryBytes <= 0 || st.PidsCurrent < 1 || st.CPUUsageUsec <= 0 {
		t.Errorf("Stats look empty: %+v", st)
	}
	list, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range list {
		if i.ID == inst.ID {
			found = i.SandboxID == inst.SandboxID && i.HostID == itHostID && i.Running && i.Generation == 7
		}
	}
	if !found {
		t.Errorf("List does not contain %s correctly: %+v", inst.ID, list)
	}
	if err := b.Pause(ctx, inst.ID); err != nil {
		t.Errorf("Pause: %v", err)
	} else {
		// crun freezes the cgroup; runsc pauses inside the sentry.
		if got := itCgroupFile(t, b, inst.ID, "cgroup.freeze"); got != "1" && !itGVisor() {
			t.Errorf("cgroup.freeze after Pause = %q", got)
		}
		if out, err := itPodman(ctx, "inspect", "-f", "{{.State.Status}}", inst.ID).Output(); err != nil || strings.TrimSpace(string(out)) != "paused" {
			t.Errorf("state after Pause = %q (%v), want paused", strings.TrimSpace(string(out)), err)
		}
		if i, err := b.Inspect(ctx, inst.ID); err != nil || !i.Running {
			t.Errorf("Inspect while paused: %+v, %v", i, err)
		}
		if err := b.Unpause(ctx, inst.ID); err != nil {
			t.Errorf("Unpause: %v", err)
		}
	}

	// Destroy, twice, and nothing left behind.
	if err := b.Destroy(ctx, inst.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := b.Destroy(ctx, inst.ID); err != nil {
		t.Fatalf("second Destroy: %v", err)
	}
	if _, err := b.Inspect(ctx, inst.ID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Inspect after Destroy: %v, want ErrNotFound", err)
	}
	if _, err := b.Stats(ctx, inst.ID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Stats after Destroy: %v, want ErrNotFound", err)
	}
	if err := b.Pause(ctx, inst.ID); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Pause after Destroy: %v, want ErrNotFound", err)
	}
	if out, _ := itPodman(context.Background(), "ps", "-a", "--format", "{{.ID}} {{.Names}}").CombinedOutput(); strings.Contains(string(out), inst.SandboxID) {
		t.Errorf("container still listed after Destroy:\n%s", out)
	}
}

func TestIntegrationPythonUnderSeccomp(t *testing.T) {
	b := itBackend(t, nil)
	inst := itCreate(t, b, itPython, itResources())

	if got := itMustExec(t, inst.ID, "python3 -c 'print(1)'"); got != "1" {
		t.Errorf("python print = %q", got)
	}
	if got := itMustExec(t, inst.ID, "seq 1 100 | grep 7 | wc -l"); got != "19" {
		t.Errorf("pipeline = %q", got)
	}
	// Threads, subprocesses, POSIX semaphores on /dev/shm, AF_UNIX and
	// AF_INET sockets, and a venv in the work directory all still work.
	script := `
import socket, subprocess, threading, multiprocessing, tempfile, os, errno
t = threading.Thread(target=lambda: None); t.start(); t.join()
assert subprocess.run(["sh", "-c", "echo ok"], capture_output=True).stdout == b"ok\n"
with multiprocessing.Pool(2) as p:
    assert p.map(abs, [-1, -2]) == [1, 2]
a, b = socket.socketpair(); a.send(b"x"); assert b.recv(1) == b"x"
socket.socket(socket.AF_INET, socket.SOCK_STREAM).close()
socket.socket(socket.AF_INET6, socket.SOCK_DGRAM).close()
for fam in (socket.AF_NETLINK, socket.AF_PACKET, 38):  # 38 = AF_ALG
    try:
        socket.socket(fam, socket.SOCK_RAW, 0)
    except OSError as e:
        print("family", fam, "refused:", errno.errorcode.get(e.errno, e.errno))
    else:
        print("family", fam, "allowed")
with tempfile.NamedTemporaryFile() as f:
    f.write(b"x")
print("python-ok")
`
	out := itMustExec(t, inst.ID, "python3 - <<'EOF'\n"+script+"\nEOF")
	t.Logf("python checks:\n%s", out)
	if !strings.HasSuffix(out, "python-ok") {
		t.Errorf("python checks did not complete:\n%s", out)
	}
	// Host-kernel attack surface. Under crun the seccomp profile refuses
	// all three families (as if they did not exist: ENOSYS). Under gVisor
	// the calls never reach the host:
	// AF_ALG does not exist in the sentry, packet sockets need a
	// capability the sandbox does not have, and AF_NETLINK is the
	// sentry's own (route) implementation, which is allowed.
	wantFamilies := []string{"family 16 refused: ENOSYS", "family 17 refused: ENOSYS", "family 38 refused: ENOSYS"}
	if itGVisor() {
		wantFamilies = []string{"family 16 allowed", "family 17 refused: EPERM", "family 38 refused: EAFNOSUPPORT"}
	}
	for _, want := range wantFamilies {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	out = itMustExec(t, inst.ID, "python3 -m venv /workspace/venv && /workspace/venv/bin/python -c 'import sys; print(sys.prefix)'")
	if out != "/workspace/venv" {
		t.Errorf("venv prefix = %q", out)
	}
	// Namespaces. Under crun the profile refuses all of them. Under
	// gVisor a user namespace is an object of the sentry, not of the host
	// kernel, and is allowed; mount and network namespaces still need a
	// capability the sandbox user does not have.
	for _, c := range []string{"unshare -U true", "unshare -m true", "unshare -n true"} {
		allowed := itGVisor() && c == "unshare -U true"
		if out, err := itExec(t, inst.ID, c); (err == nil) != allowed {
			t.Errorf("%q: err=%v, want allowed=%v: %s", c, err, allowed, out)
		} else {
			t.Logf("%s: err=%v %s", c, err, out)
		}
	}
	// clone3 must look unimplemented, not forbidden, so libcs fall back.
	out = itMustExec(t, inst.ID, `python3 -c '
import ctypes, errno
libc = ctypes.CDLL(None, use_errno=True)
for name, nr in (("clone3", 435), ("io_uring_setup", 425), ("bpf", 321), ("userfaultfd", 323), ("keyctl", 250), ("mount", 165), ("ptrace", 101)):
    ctypes.set_errno(0)
    r = libc.syscall(nr, 0, 0, 0, 0, 0)
    print(name, r, errno.errorcode.get(ctypes.get_errno()))
'`)
	t.Logf("raw syscalls (x86-64 numbers):\n%s", out)
	if goarch := itMustExec(t, inst.ID, "uname -m"); goarch == "x86_64" {
		want := []string{"clone3 -1 ENOSYS", "io_uring_setup -1 ENOSYS", "bpf -1 EPERM", "userfaultfd -1 EPERM", "keyctl -1 EPERM", "mount -1 EPERM", "ptrace -1 EPERM"}
		if itGVisor() {
			// What the sentry answers (release-20261005.0): io_uring,
			// userfaultfd and keyctl do not exist, bpf and mount need
			// capabilities, clone3 exists (and rejects the null
			// arguments), and ptrace is the sentry's own, between the
			// sandbox's processes only.
			want = []string{"clone3 -1 EINVAL", "io_uring_setup -1 ENOSYS", "bpf -1 EPERM", "userfaultfd -1 ENOSYS", "keyctl -1 ENOSYS", "mount -1 EPERM", "ptrace 0 None"}
		}
		for _, want := range want {
			if !strings.Contains(out, want) {
				t.Errorf("want %q in:\n%s", want, out)
			}
		}
	}
}

func TestIntegrationLimitsAreEnforced(t *testing.T) {
	b := itBackend(t, nil)
	res := itResources()
	res.MemoryBytes = 128 << 20
	if itGVisor() {
		// The sentry and the gofer live inside the sandbox's memory
		// limit, and a process costs more there: 63 idle Python children
		// do not fit in 128 MiB under gVisor (they are OOM-killed, which
		// is the limit working, but not what this part tests).
		res.MemoryBytes = 384 << 20
	}
	res.WorkdirBytes = 32 << 20
	res.TmpBytes = 16 << 20
	itWatchOOMFile(t) // the memory bomb below makes conmon drop an "oom" file
	inst := itCreate(t, b, itPython, res)
	ctx := context.Background()

	// Fork bomb: pids.max holds and the hit is counted.
	out, _ := itExec(t, inst.ID, `python3 -c '
import os, time
kids = []
try:
    for _ in range(500):
        pid = os.fork()
        if pid == 0:
            time.sleep(20); os._exit(0)
        kids.append(pid)
except OSError as e:
    print("fork failed after", len(kids), "children:", e)
import signal
for k in kids: os.kill(k, signal.SIGKILL)
for k in kids: os.waitpid(k, 0)
'`)
	t.Logf("fork bomb: %s", out)
	st, err := b.Stats(ctx, inst.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	t.Logf("Stats after fork bomb: %+v", st)
	if !strings.Contains(out, "fork failed after") {
		t.Errorf("the fork bomb was not stopped: %s", out)
	}
	// Under gVisor the limit is the sentry's RLIMIT_NPROC: fork fails the
	// same way, but the host cgroup never sees the hit.
	if (st.PidsLimitHits == 0) != itGVisor() {
		t.Errorf("PidsLimitHits = %d after a fork bomb on %s; fork output: %s", st.PidsLimitHits, itRuntime(), out)
	}
	if i, err := b.Inspect(ctx, inst.ID); err != nil || !i.Running {
		t.Fatalf("the sandbox did not survive the fork bomb: %+v, %v", i, err)
	}

	// Memory bomb: the cgroup OOM killer fires and memory.oom.group takes
	// the whole sandbox down, not just the offending process.
	out, _ = itExec(t, inst.ID, `python3 -c '
b = []
while True:
    b.append(bytearray(8 << 20))
'`)
	t.Logf("memory bomb: %s", out)
	time.Sleep(time.Second)
	i, err := b.Inspect(ctx, inst.ID)
	t.Logf("Inspect after memory bomb: running=%v err=%v", i.Running, err)
	if err == nil && i.Running {
		t.Error("sandbox still running after OOM with memory.oom.group=1")
	}
	if err == nil && (!i.OOMKilled || i.ExitCode != sigkillExitCode) {
		t.Errorf("Inspect after memory bomb: OOMKilled=%v ExitCode=%d, want true and %d", i.OOMKilled, i.ExitCode, sigkillExitCode)
	}
	// Known gap, asserted so that a Podman that starts reporting the
	// kill shows up: on rootless 4.9.3 the scope and its counters are
	// gone and State.OOMKilled is false, so Stats can only say that the
	// instance is no longer running.
	st, err = b.Stats(ctx, inst.ID)
	t.Logf("Stats after memory bomb: %+v err=%v", st, err)
	if err == nil && st.OOMKills == 0 {
		t.Error("Stats succeeded on a dead sandbox without reporting the OOM kill")
	}
	if err != nil && !errors.Is(err, ErrNotRunning) {
		t.Errorf("Stats after OOM: %v, want ErrNotRunning or OOMKills > 0", err)
	}
}

// TestIntegrationUsernsModes checks that every fixed mapping is accepted
// by the service and yields the documented host identity: the owner of
// the container's init process, as seen from the host.
func TestIntegrationUsernsModes(t *testing.T) {
	hostUID := os.Getuid()
	for _, tc := range []struct {
		mode        UsernsMode
		wantHostUID func(uid int) bool
		runDirOK    bool
	}{
		{UsernsKeepID, func(uid int) bool { return uid == hostUID }, true},
		{UsernsNoMap, func(uid int) bool { return uid != hostUID && uid >= 100000 }, false},
		{UsernsHost, func(uid int) bool { return uid != hostUID && uid >= 100000 }, false},
		{UsernsAuto, func(uid int) bool { return uid != hostUID && uid >= 100000 }, false},
	} {
		if itGVisor() {
			// The host process of a gVisor sandbox is the sentry, which
			// runs as the container's root whatever the sandbox user is:
			// a subordinate UID, except with "host", where container
			// root is the pool user. The sandbox user's files in the run
			// directory are created by the sentry on its behalf.
			sub := func(uid int) bool { return uid != hostUID && uid >= 100000 }
			tc.wantHostUID = sub
			if tc.mode == UsernsHost {
				tc.wantHostUID = func(uid int) bool { return uid == hostUID }
			}
		}
		t.Run(string(tc.mode), func(t *testing.T) {
			b := itBackend(t, func(c *Config) { c.UsernsMode = tc.mode })
			inst := itCreate(t, b, itAlpine, itResources())
			if got := itMustExec(t, inst.ID, "id -u"); got != "1000" {
				t.Errorf("container uid = %q, want 1000", got)
			}
			if got := itMustExec(t, inst.ID, "id -un; id -gn"); got != "sandbox\nsandbox" {
				t.Errorf("mode %s: container user and group are %q, want sandbox", tc.mode, got)
			}
			itMustExec(t, inst.ID, "echo x > /workspace/f && echo x > /tmp/f")
			out, err := itPodman(context.Background(), "inspect", "-f", "{{.State.Pid}}", inst.ID).Output()
			if err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat("/proc/" + strings.TrimSpace(string(out)))
			if err != nil {
				t.Fatal(err)
			}
			uid := int(fi.Sys().(*syscall.Stat_t).Uid)
			t.Logf("mode %s: container uid 1000 is host uid %d; uid_map: %s", tc.mode, uid,
				strings.Join(strings.Fields(itMustExec(t, inst.ID, "cat /proc/self/uid_map")), " "))
			if !tc.wantHostUID(uid) {
				t.Errorf("mode %s: host uid of the sandbox process is %d (pool user is %d)", tc.mode, uid, hostUID)
			}
			// The run directory is created 0700 by the pool user, as the
			// host agent would; only keep-id can write to it unaided.
			_, err = itExec(t, inst.ID, "echo x > /run/sbx/probe")
			if (err == nil) != tc.runDirOK {
				t.Errorf("mode %s: write to run dir succeeded=%v, documented=%v", tc.mode, err == nil, tc.runDirOK)
			}
		})
	}
}

// TestIntegrationNoHostIdentity checks that a sandbox cannot learn who
// runs it: the host account's login and full name, the host's name and
// the machine's model must not appear in the sandbox's user database,
// hostname, hosts file, environment, or the parts of /proc and /sys a
// sandbox can read. One place is known to remain under crun and is
// reported, not failed: /proc/self/mountinfo shows the host paths of the
// image store (overlayfs options) and of bind-mount sources.
func TestIntegrationNoHostIdentity(t *testing.T) {
	b := itBackend(t, nil)
	if os.Getenv("SBX_PODMAN_SOCKET") != "" {
		t.Log("external Podman service: /etc/hosts is that service's business (base_hosts_file), only the user database is checked strictly")
	}
	needles := map[string]string{}
	if u, err := user.Current(); err == nil {
		needles["login name"] = u.Username
		for _, part := range strings.Fields(strings.Split(u.Name, ",")[0]) {
			if len(part) >= 4 {
				needles["full name ("+part+")"] = part
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		needles["hostname"] = h
	}
	if raw, err := os.ReadFile("/sys/devices/virtual/dmi/id/product_name"); err == nil && len(strings.TrimSpace(string(raw))) >= 4 {
		needles["DMI product name"] = strings.TrimSpace(string(raw))
	}
	if len(needles) == 0 {
		t.Skip("cannot determine the host identity to look for")
	}
	for _, image := range []string{itAlpine, itPython} {
		inst := itCreate(t, b, image, itResources())
		if got := itMustExec(t, inst.ID, "id"); !strings.HasPrefix(got, "uid=1000(sandbox) gid=1000(sandbox)") {
			t.Errorf("%s: id = %q", image, got)
		}
		if got := itMustExec(t, inst.ID, "whoami; hostname; cat /proc/sys/kernel/hostname; echo $HOSTNAME; grep :1000: /etc/passwd; grep :1000: /etc/group"); got !=
			"sandbox\nsandbox\nsandbox\nsandbox\nsandbox:x:1000:1000:sandbox:/workspace:/bin/sh\nsandbox:x:1000:" {
			t.Errorf("%s: identity inside the sandbox:\n%s", image, got)
		}
		t.Logf("%s /etc/hosts: %s", image, strings.Join(strings.Split(itMustExec(t, inst.ID, "cat /etc/hosts"), "\n"), " | "))
		t.Logf("%s resolv.conf: %q", image, itMustExec(t, inst.ID, "cat /etc/resolv.conf 2>&1 || true"))
		// Everything a sandbox can read that is not its own files: the
		// whole of /etc, its environment and PID 1's, and the readable
		// text files of /proc and /sys that are about the machine.
		const sources = `{ env; id; hostname; cat /proc/1/environ /proc/self/environ | tr '\0' '\n';
			cat /proc/sys/kernel/hostname /proc/sys/kernel/domainname /proc/self/cgroup /proc/self/uid_map /proc/self/status /proc/cmdline /proc/1/cmdline;
			find /etc /run -xdev -type f -size -1024k 2>/dev/null | while read f; do echo "== $f"; cat "$f" 2>/dev/null; done;
			find /sys/devices/virtual/dmi /sys/class/dmi -type f 2>/dev/null | while read f; do echo "== $f"; cat "$f" 2>/dev/null; done;
		} 2>/dev/null | tr -d '\0'`
		dump := strings.ToLower(itMustExec(t, inst.ID, sources))
		mounts := strings.ToLower(itMustExec(t, inst.ID, "cat /proc/self/mountinfo"))
		for what, needle := range needles {
			n := strings.ToLower(needle)
			if strings.Contains(dump, n) {
				for _, line := range strings.Split(dump, "\n") {
					if strings.Contains(line, n) {
						t.Errorf("%s: the host's %s is visible inside the sandbox: %q", image, what, line)
					}
				}
			}
			if strings.Contains(mounts, n) {
				t.Logf("%s: known remainder: /proc/self/mountinfo shows the host's %s (image store and bind-mount paths)", image, what)
			}
		}
		if os.Getenv("SBX_PODMAN_SOCKET") == "" {
			if hosts := itMustExec(t, inst.ID, "cat /etc/hosts"); strings.Contains(hosts, "host.containers.internal") {
				t.Errorf("%s: /etc/hosts names the host's address:\n%s", image, hosts)
			}
		}
		if out, err := itExec(t, inst.ID, "ls /sys/devices/virtual/dmi/id/ 2>/dev/null | head -3"); err == nil && out != "" {
			t.Errorf("%s: the DMI tables are readable: %s", image, out)
		}
	}
}

// thirtyStdlibModules is the import list of the benchmark that found the
// recompilation problem (bench/sbxbench.py, python_import_30_stdlib_modules).
const thirtyStdlibModules = "json,re,ssl,sqlite3,asyncio,http.client,decimal,datetime,subprocess,argparse,logging,unittest," +
	"email.parser,xml.etree.ElementTree,csv,hashlib,urllib.request,zipfile,tempfile,pathlib," +
	"multiprocessing,ctypes,uuid,random,statistics,gzip,shutil,socket,threading,dataclasses"

// TestIntegrationPythonBytecodeCache measures what PYTHONPYCACHEPREFIX in
// the default environment is worth on the read-only root: a fresh
// interpreter importing 30 standard-library modules, with the sandbox's
// default environment and with the variable emptied (which Python treats
// as unset, i.e. the behaviour before the default existed).
func TestIntegrationPythonBytecodeCache(t *testing.T) {
	b := itBackend(t, nil)
	inst := itCreate(t, b, itPython, itResources())

	// The import time is taken inside the interpreter, so the cost of
	// `podman exec` is not in it; start-up is the wall time of a child
	// `python3 -c pass`, measured by a parent interpreter.
	const probe = `python3 -c 'import time; t=time.perf_counter(); import ` + thirtyStdlibModules + `; print(round((time.perf_counter()-t)*1000, 1))'`
	measure := func(env string, n int) []float64 {
		t.Helper()
		var out []float64
		for i := 0; i < n; i++ {
			v, err := strconv.ParseFloat(itMustExec(t, inst.ID, env+probe), 64)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	median := func(v []float64) float64 {
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		return s[len(s)/2]
	}
	startup := func(env string) float64 {
		t.Helper()
		out := itMustExec(t, inst.ID, env+`python3 -c 'import subprocess, sys, time
t = time.perf_counter()
for _ in range(20):
    subprocess.run([sys.executable, "-c", "pass"], check=True)
print(round((time.perf_counter() - t) * 1000 / 20, 1))'`)
		ms, err := strconv.ParseFloat(out, 64)
		if err != nil {
			t.Fatalf("start-up probe: %q", out)
		}
		return ms
	}

	if got := itMustExec(t, inst.ID, `python3 -c 'import sys; print(sys.pycache_prefix)'`); got != PycachePrefix {
		t.Fatalf("sys.pycache_prefix = %q, want %q", got, PycachePrefix)
	}
	if got := itMustExec(t, inst.ID, `PYTHONPYCACHEPREFIX= python3 -c 'import sys; print(sys.pycache_prefix)'`); got != "None" {
		t.Fatalf("an empty PYTHONPYCACHEPREFIX gives sys.pycache_prefix = %q, want None", got)
	}

	const noCache = "PYTHONPYCACHEPREFIX= "
	without := measure(noCache, 5)
	first := measure("", 1) // compiles and fills the cache
	with := measure("", 5)
	startWithout, startWith := startup(noCache), startup("")
	t.Logf("MEASURED 30 stdlib imports, read-only root, ms: without PYTHONPYCACHEPREFIX %v (median %.1f); "+
		"with it, first process %v, later processes %v (median %.1f)", without, median(without), first, with, median(with))
	t.Logf("MEASURED python3 -c pass, mean of 20, ms: without %.1f, with %.1f", startWithout, startWith)
	if n := itMustExec(t, inst.ID, "find /tmp/pycache -name '*.pyc' | wc -l"); n == "0" {
		t.Error("no bytecode was written under /tmp/pycache")
	} else {
		t.Logf("bytecode files under %s: %s (%s)", PycachePrefix, n, itMustExec(t, inst.ID, "du -sh /tmp/pycache | cut -f1"))
	}
	// The benchmark saw 1153 ms against 173 ms. Machines and load differ,
	// so only a clear win is asserted.
	if median(with) > median(without)/2 {
		t.Errorf("cached imports took %.1f ms, uncached %.1f ms: expected at least a 2x improvement", median(with), median(without))
	}
}

// TestIntegrationOOMEvidence checks what a managed service's working
// directory buys: an OOM kill and a SIGKILL from the host both end the
// sandbox's init process with exit status 137, and conmon's "oom" file
// tells them apart.
func TestIntegrationOOMEvidence(t *testing.T) {
	b := itBackend(t, nil)
	if b.oom == nil {
		t.Skip("needs a managed Podman service (unset SBX_PODMAN_SOCKET): an external service's working directory is not ours to read")
	}
	ctx := context.Background()
	res := itResources()
	waitExit := func(id string) runtime.Instance {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			i, err := b.Inspect(ctx, id)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !i.Running {
				return i
			}
			if time.Now().After(deadline) {
				t.Fatalf("sandbox %s is still running", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// 1. SIGKILL from the host, with no OOM kill anywhere near it.
	victim := itCreate(t, b, itAlpine, res)
	out, err := itPodman(ctx, "inspect", "-f", "{{.State.Pid}}", victim.ID).Output()
	if err != nil {
		t.Fatal(err)
	}
	// Through `podman unshare`: under gVisor the process is the sentry,
	// owned by a subordinate UID.
	if out, err := itPodman(ctx, "unshare", "kill", "-9", strings.TrimSpace(string(out))).CombinedOutput(); err != nil {
		t.Fatalf("kill -9: %v: %s", err, out)
	}
	if i := waitExit(victim.ID); i.ExitCode != sigkillExitCode || i.OOMKilled {
		t.Errorf("after a host kill -9: ExitCode=%d OOMKilled=%v, want %d and false", i.ExitCode, i.OOMKilled, sigkillExitCode)
	}
	list, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range list {
		if i.ID == victim.ID && (i.Running || i.OOMKilled) {
			t.Errorf("List after a host kill -9: %+v", i)
		}
	}
	// Out of the attribution window before the OOM kills below.
	time.Sleep(oomBefore + oomAfter + time.Second)

	// 2. Several sandboxes OOM-killed at the same moment: every one of
	// them is attributed, although they share one file name.
	const n = 4
	var insts []runtime.Instance
	for i := 0; i < n; i++ {
		insts = append(insts, itCreate(t, b, itPython, res))
	}
	var wg sync.WaitGroup
	for _, inst := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = itExec(t, inst.ID, `python3 -c '
b = []
while True:
    b.append(bytearray(8 << 20))
'`)
		}()
	}
	wg.Wait()
	for _, inst := range insts {
		i := waitExit(inst.ID)
		if i.ExitCode != sigkillExitCode || !i.OOMKilled {
			t.Errorf("after a memory bomb: ExitCode=%d OOMKilled=%v, want %d and true", i.ExitCode, i.OOMKilled, sigkillExitCode)
		}
	}
	for _, dir := range b.cfg.OOMEvidenceDirs {
		if _, err := os.Stat(filepath.Join(dir, oomFileName)); err == nil {
			time.Sleep(2 * oomPollInterval)
			if _, err := os.Stat(filepath.Join(dir, oomFileName)); err == nil {
				t.Errorf("the oom file in %s was not consumed", dir)
			}
		}
	}
}
