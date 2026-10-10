package podman

// Integration tests against a real rootless Podman service. They are
// skipped unless the per-user API socket (or $SBX_PODMAN_SOCKET) answers
// and the test images are already present locally:
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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
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

// itBackend returns a Backend on the real socket, or skips.
func itBackend(t *testing.T, mutate func(*Config)) *Backend {
	t.Helper()
	socket := os.Getenv("SBX_PODMAN_SOCKET")
	if socket == "" {
		socket = DefaultSocketPath()
	}
	if socket == "" {
		t.Skip("no Podman socket: XDG_RUNTIME_DIR is not set")
	}
	if _, err := os.Stat(socket); err != nil {
		t.Skipf("no Podman socket at %s", socket)
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman CLI not found (needed for in-container checks)")
	}
	// The profile must outlive the test's containers but need not outlive
	// the test, so the test's temp dir is the right place.
	profile, err := WriteSeccompProfile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{SocketPath: socket, SeccompProfilePath: profile, Command: []string{"sleep", "600"}}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, err := b.Ping(ctx)
	if err != nil {
		t.Skipf("Podman socket %s does not answer: %v", socket, err)
	}
	t.Logf("Libpod API version %s", v)
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
		Template:   core.Template{ID: "tpl-it", Image: image, Resources: res, Network: core.NetworkAllowlist},
		HostRunDir: runDir,
	})
	if IsStatus(err, http.StatusNotFound) {
		t.Skipf("image %s is not present locally (podman pull it to run this test): %v", image, err)
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
	out, err := exec.CommandContext(ctx, "podman", "exec", id, "sh", "-c", script).CombinedOutput()
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

	// Limits, read from the host side of the cgroup.
	for file, want := range map[string]string{
		"memory.max":       "268435456",
		"memory.swap.max":  "0",
		"memory.high":      "max", // deliberately unset, see buildCreateSpec
		"memory.oom.group": "1",
		"cpu.max":          "50000 100000",
		"pids.max":         "64",
	} {
		if got := itCgroupFile(t, b, inst.ID, file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		} else {
			t.Logf("%s = %s", file, got)
		}
	}

	// Ownership label and default environment.
	if out, err := exec.Command("podman", "inspect", "-f", `{{index .Config.Labels "sbx.host_id"}}`, inst.ID).Output(); err != nil ||
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
	if out, err := exec.Command("podman", "inspect", "-f", "{{.OCIConfigPath}}", inst.ID).Output(); err == nil {
		// The bundle belongs to a subordinate uid under keep-id, hence unshare.
		if raw, err := exec.Command("podman", "unshare", "cat", strings.TrimSpace(string(out))).Output(); err == nil {
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
		case k == "Seccomp" && v != "2":
			t.Errorf("Seccomp = %s, want 2 (filter)", v)
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
			if strings.Contains(line, want.dest) {
				ok = strings.Contains(line, want.opt) && strings.Contains(line, "nosuid") && strings.Contains(line, "nodev")
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

	// Network: loopback only.
	if got := itMustExec(t, inst.ID, "ls /sys/class/net"); got != "lo" {
		t.Errorf("network interfaces = %q, want only lo", got)
	}

	// Seccomp: nested user namespaces are refused.
	if out, err := itExec(t, inst.ID, "unshare -U true"); err == nil {
		t.Errorf("unshare -U succeeded: %s", out)
	} else {
		t.Logf("unshare -U: %s", out)
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
		if got := itCgroupFile(t, b, inst.ID, "cgroup.freeze"); got != "1" {
			t.Errorf("cgroup.freeze after Pause = %q", got)
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
	if out, _ := exec.Command("podman", "ps", "-a", "--format", "{{.ID}} {{.Names}}").CombinedOutput(); strings.Contains(string(out), inst.SandboxID) {
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
        raise SystemExit("socket family %d was allowed" % fam)
with tempfile.NamedTemporaryFile() as f:
    f.write(b"x")
print("python-ok")
`
	out := itMustExec(t, inst.ID, "python3 - <<'EOF'\n"+script+"\nEOF")
	t.Logf("python checks:\n%s", out)
	if !strings.HasSuffix(out, "python-ok") {
		t.Errorf("python checks did not complete:\n%s", out)
	}
	out = itMustExec(t, inst.ID, "python3 -m venv /workspace/venv && /workspace/venv/bin/python -c 'import sys; print(sys.prefix)'")
	if out != "/workspace/venv" {
		t.Errorf("venv prefix = %q", out)
	}
	for _, c := range []string{"unshare -U true", "unshare -m true", "unshare -n true"} {
		if out, err := itExec(t, inst.ID, c); err == nil {
			t.Errorf("%q succeeded: %s", c, out)
		} else {
			t.Logf("%s: %s", c, out)
		}
	}
	// clone3 must look unimplemented, not forbidden, so libcs fall back.
	out = itMustExec(t, inst.ID, `python3 -c '
import ctypes, errno
libc = ctypes.CDLL(None, use_errno=True)
for name, nr in (("clone3", 435), ("io_uring_setup", 425), ("bpf", 321), ("userfaultfd", 323), ("keyctl", 250), ("mount", 165), ("ptrace", 101)):
    r = libc.syscall(nr, 0, 0, 0, 0, 0)
    print(name, r, errno.errorcode.get(ctypes.get_errno()))
'`)
	t.Logf("raw syscalls (x86-64 numbers):\n%s", out)
	if goarch := itMustExec(t, inst.ID, "uname -m"); goarch == "x86_64" {
		for _, want := range []string{"clone3 -1 ENOSYS", "io_uring_setup -1 ENOSYS", "bpf -1 EPERM", "userfaultfd -1 EPERM", "keyctl -1 EPERM", "mount -1 EPERM", "ptrace -1 EPERM"} {
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
	if st.PidsLimitHits == 0 {
		t.Errorf("PidsLimitHits = 0 after a fork bomb; fork output: %s", out)
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
		t.Run(string(tc.mode), func(t *testing.T) {
			b := itBackend(t, func(c *Config) { c.UsernsMode = tc.mode })
			inst := itCreate(t, b, itAlpine, itResources())
			if got := itMustExec(t, inst.ID, "id -u"); got != "1000" {
				t.Errorf("container uid = %q, want 1000", got)
			}
			itMustExec(t, inst.ID, "echo x > /workspace/f && echo x > /tmp/f")
			out, err := exec.Command("podman", "inspect", "-f", "{{.State.Pid}}", inst.ID).Output()
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
