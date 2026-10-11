package podman

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseRuntimes(t *testing.T) {
	got, err := ParseRuntimes(" crun , runsc=/opt/gvisor/runsc ,")
	want := []OCIRuntime{{Name: "crun"}, {Name: "runsc", Path: "/opt/gvisor/runsc"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, %v", got, err)
	}
	if got, err := ParseRuntimes(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %+v, %v", got, err)
	}
	for _, bad := range []string{"crun,crun", "runsc=relative/path", "bad name", "x=/a b,=/p"} {
		if _, err := ParseRuntimes(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestContainersConf(t *testing.T) {
	conf, err := ContainersConf([]OCIRuntime{{Name: "crun", Path: "/usr/bin/crun"}, {Name: "runsc", Path: `/run/user/1000/s b"x/runsc`}}, "runsc")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[containers]\nkeyring = false\ndefault_sysctls = []\nlog_driver = \"none\"\nbase_hosts_file = \"none\"\nhost_containers_internal_ip = \"none\"\n",
		"[engine]\ncgroup_manager = \"systemd\"\nevents_logger = \"none\"\nruntime = \"runsc\"\n",
		"[engine.runtimes]\ncrun = [\"/usr/bin/crun\"]\nrunsc = [\"/run/user/1000/s b\\\"x/runsc\"]\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("containers.conf lacks %q:\n%s", want, conf)
		}
	}
	if _, err := ContainersConf([]OCIRuntime{{Name: "crun", Path: "/usr/bin/crun"}}, "runsc"); err == nil {
		t.Error("a default runtime that is not in the table was accepted")
	}
	if _, err := ContainersConf([]OCIRuntime{{Name: "crun", Path: "crun"}}, ""); err == nil {
		t.Error("a relative runtime path was accepted")
	}
}

func TestServiceEnv(t *testing.T) {
	got := serviceEnv([]string{"PATH=/bin", "CONTAINERS_CONF_OVERRIDE=/old", "CONTAINER_HOST=unix:///x", "NOTIFY_SOCKET=/n",
		"LISTEN_FDS=1", "LISTEN_PID=7", "CONTAINERS_CONF=/base", "XDG_RUNTIME_DIR=/run/user/1"}, "/new")
	want := []string{"PATH=/bin", "CONTAINERS_CONF=/base", "XDG_RUNTIME_DIR=/run/user/1", "CONTAINERS_CONF_OVERRIDE=/new"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestWriteRunscWrapper(t *testing.T) {
	dir := t.TempDir()
	// /bin/true stands in for both the shim and runsc: the wrapper's
	// self-check (`wrapper --version`) only needs it to run.
	path, err := WriteRunscWrapper(dir, []string{"/bin/true", "runsc-shim"}, "/opt/it's/runsc", []string{"--platform=kvm"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	want := "exec '/bin/true' 'runsc-shim' --runsc '/opt/it'\\''s/runsc' '--flag=--platform=kvm' -- \"$@\"\n"
	if !strings.HasPrefix(string(raw), "#!/bin/sh\n") || !strings.HasSuffix(string(raw), want) {
		t.Errorf("wrapper:\n%s", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", fi.Mode())
	}
	if _, err := WriteRunscWrapper(dir, []string{"/bin/true"}, "/r", []string{"platform=kvm"}); err == nil {
		t.Error("a flag without a dash was accepted")
	}
	if _, err := WriteRunscWrapper(dir, []string{"/bin/false"}, "/r", nil); err == nil || !strings.Contains(err.Error(), "--version failed") {
		t.Errorf("a wrapper that cannot run was accepted: %v", err)
	}
	if _, err := PrepareRuntimes(dir, []OCIRuntime{{Name: "no-such-runtime-xyz"}}, nil, nil); err == nil {
		t.Error("a runtime that is not installed was accepted")
	}
}

func TestOOMEvidence(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	e := newOOMEvidence([]string{dirA, dirB})
	defer e.close()
	now := time.Now()
	if e.killed(now.Add(-time.Minute), 0) {
		t.Fatal("evidence without any file")
	}
	// conmon's file, in the second service's directory.
	path := filepath.Join(dirB, oomFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !e.killed(time.Now(), 0) {
		t.Fatal("an oom file written just now is not evidence for a container that ended just now")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the file was not consumed, so a second OOM kill would leave no trace")
	}
	// The same event does not explain a death outside the window, in
	// either direction, but does with the slack of a coarse end time.
	if e.near(time.Now().Add(oomBefore+time.Second), 0) || e.near(time.Now().Add(-oomAfter-time.Second), 0) {
		t.Error("the attribution window is not bounded")
	}
	if !e.near(time.Now().Add(-oomAfter-500*time.Millisecond), time.Second) {
		t.Error("slack was not applied")
	}
	// Something that is not conmon's empty file is left alone.
	if err := os.WriteFile(filepath.Join(dirA, oomFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.poll()
	if _, err := os.Stat(filepath.Join(dirA, oomFileName)); err != nil {
		t.Error("a non-empty file named oom was removed")
	}
	// A death long ago is judged at once; one just now waits for conmon.
	start := time.Now()
	if e.killed(time.Now().Add(-time.Hour), 0) || time.Since(start) > 100*time.Millisecond {
		t.Errorf("an old death took %s to judge", time.Since(start))
	}
}

// TestIntegrationManagedService starts a real `podman system service`
// the way sbxd does and checks its life cycle: socket permissions, the
// configuration it runs with, restart after a crash, replacement of a
// service that outlived its supervisor, and a clean stop.
func TestIntegrationManagedService(t *testing.T) {
	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not found")
	}
	crun, err := exec.LookPath("crun")
	if err != nil {
		t.Skip("crun not found")
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		t.Skip("XDG_RUNTIME_DIR is not set")
	}
	dir, err := os.MkdirTemp(base, "sbx-svc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := ServiceConfig{Dir: filepath.Join(dir, "podman"), SocketPath: filepath.Join(dir, "podman.sock"),
		Runtimes: []OCIRuntime{{Name: "crun", Path: crun}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, err := StartService(ctx, cfg)
	if err != nil {
		t.Skipf("a private Podman service cannot be started here: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	fi, err := os.Stat(cfg.SocketPath)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v, want a 0600 socket", fi, err)
	}
	if fi, _ := os.Stat(cfg.Dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("service dir mode %v", fi.Mode())
	}
	// The service runs with the generated file and in its own directory.
	pid := svc.Pid()
	environ, _ := os.ReadFile(filepath.Join("/proc", itoa(pid), "environ"))
	if !strings.Contains(string(environ), "CONTAINERS_CONF_OVERRIDE="+svc.ConfPath()+"\x00") {
		t.Errorf("the service's environment does not carry %s", svc.ConfPath())
	}
	if cwd, _ := os.Readlink(filepath.Join("/proc", itoa(pid), "cwd")); cwd != svc.WorkDir() {
		t.Errorf("the service's working directory is %q, want %q", cwd, svc.WorkDir())
	}
	// It shares the user's store: what plain `podman images` lists, it lists.
	plain, _ := exec.Command(podmanPath, "images", "-q").Output()
	private, err := exec.Command(podmanPath, "--url", "unix://"+cfg.SocketPath, "images", "-q").Output()
	if err != nil || string(plain) != string(private) {
		t.Errorf("image lists differ (err %v):\nplain:   %q\nprivate: %q", err, plain, private)
	}

	// A crash is recovered.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for svc.Restarts() == 0 || svc.Pid() == 0 || svc.Pid() == pid {
		if time.Now().After(deadline) {
			t.Fatalf("the service was not restarted (restarts=%d pid=%d)", svc.Restarts(), svc.Pid())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		pctx, pcancel := context.WithTimeout(ctx, time.Second)
		_, err := PingSocket(pctx, cfg.SocketPath)
		pcancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted service does not answer: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("service restarted: pid %d -> %d", pid, svc.Pid())

	// Stop removes the socket and the pid file, keeps the configuration.
	if err := svc.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Stop(ctx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if _, err := os.Stat(cfg.SocketPath); err == nil {
		t.Error("the socket is still there after Stop")
	}
	if _, err := os.Stat(svc.ConfPath()); err != nil {
		t.Errorf("the configuration is gone after Stop (live containers still need it): %v", err)
	}
	if !processGone(svc.Pid()) && svc.Pid() != 0 {
		t.Error("the service is still running after Stop")
	}

	// A service that outlived its sbxd (started here by hand, with a pid
	// file as the supervisor would have left it) is replaced by the next.
	stale := exec.Command(podmanPath, "system", "service", "--time=0", "unix://"+cfg.SocketPath)
	stale.Env = serviceEnv(os.Environ(), svc.ConfPath())
	stale.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	go stale.Wait()
	if err := os.WriteFile(filepath.Join(cfg.Dir, ServicePidFile), []byte(itoa(stale.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for !isServiceProcess(stale.Process.Pid, cfg.SocketPath) {
		time.Sleep(10 * time.Millisecond)
	}
	svc2, err := StartService(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Stop(context.Background())
	if !processGone(stale.Process.Pid) {
		t.Errorf("the stale service (pid %d) survived the start of a new one", stale.Process.Pid)
	}
	if svc2.Pid() == stale.Process.Pid || svc2.Pid() == 0 {
		t.Errorf("new service pid %d", svc2.Pid())
	}
	// A pid file that names an unrelated process touches nothing.
	if isServiceProcess(os.Getpid(), cfg.SocketPath) {
		t.Error("this test process was taken for a Podman service")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
