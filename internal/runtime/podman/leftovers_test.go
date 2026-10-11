package podman

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fakeConmonEnv = "SBX_TEST_FAKE_CONMON"

// fakeConmon is what a copy of the test binary named "conmon" does: like
// the real one after `crun create`, it has one child and waits.
func fakeConmon() {
	child := exec.Command("sleep", "120")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	fmt.Println(child.Process.Pid)
	_ = child.Wait()
	time.Sleep(120 * time.Second)
	os.Exit(0)
}

func writeProc(t *testing.T, root string, pid int, comm string, argv []string, ppid int) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"comm":    comm + "\n",
		"cmdline": strings.Join(argv, "\x00") + "\x00",
		"stat":    fmt.Sprintf("%d (%s) S %d 1 1 0 -1 4194304 0 0\n", pid, comm, ppid),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The scan picks out this user's conmons of sandbox containers and reads
// the container id and the sandbox id from the command line, as recorded
// from a real leftover.
func TestScanConmons(t *testing.T) {
	root := t.TempDir()
	cid := strings.Repeat("5e615b87", 8)
	other := strings.Repeat("0a1b2c3d", 8)
	real := []string{"/usr/bin/conmon", "--api-version", "1", "-c", cid, "-u", cid, "-r", "/usr/bin/crun", "-b", "/store/" + cid + "/userdata",
		"-p", "/run/pidfile", "-n", "sbx-sb-2901b400f8290e9a", "--exit-dir", "/run/exits", "--full-attach", "-s", "-l", "none",
		"--exit-command", "/usr/bin/podman", "--exit-command-arg", "-n", "--exit-command-arg", "sbx-not-the-name",
		"--exit-command-arg", "container", "--exit-command-arg", "cleanup", "--exit-command-arg", cid}
	writeProc(t, root, 770557, "conmon", real, 2446)
	writeProc(t, root, 770560, "crun", []string{"/usr/bin/crun", "create", cid}, 770557)
	writeProc(t, root, 800, "conmon", []string{"/usr/bin/conmon", "-c", other, "-n", "my-own-container"}, 1)        // not a sandbox
	writeProc(t, root, 801, "conmon", []string{"/usr/bin/conmon", "-c", "short", "-n", "sbx-sb-x"}, 1)              // not a container id
	writeProc(t, root, 802, "bash", []string{"bash", "-c", "conmon -c " + other + " -n sbx-sb-fake"}, 1)            // not conmon
	writeProc(t, root, 803, "conmon", []string{"/usr/bin/conmon", "-c", other, "-n", "sbx-"}, 1)                    // no sandbox id
	writeProc(t, root, 804, "conmon (x) y", []string{"/usr/bin/conmon", "-c", other, "-n", "sbx-sb-other"}, 770557) // an odd comm
	if err := os.WriteFile(filepath.Join(root, "uptime"), []byte("1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := scanConmons(root, os.Getuid(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ConmonPID != 770557 || got[0].ContainerID != cid || got[0].SandboxID != "sb-2901b400f8290e9a" || got[0].Age < 59*time.Minute {
		t.Fatalf("scanConmons = %+v", got)
	}
	if other, _ := scanConmons(root, os.Getuid()+1, time.Now()); len(other) != 0 {
		t.Errorf("another user's conmons were returned: %+v", other)
	}
	if kids := childrenOf(root, 770557); len(kids) != 2 || kids[0]+kids[1] != 770560+804 {
		t.Errorf("childrenOf = %v, want the crun child and the process with the odd name", kids)
	}
}

// A real leftover: a process called conmon, started for a sandbox
// container Podman does not have, with a child. It is listed, attributed
// by the caller, and killed child first; one that Podman still has, one
// that is too young and one that is not the caller's are left alone.
func TestLeftoversAreFoundAndReaped(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "conmon")
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	type proc struct {
		cmd       *exec.Cmd
		cid, name string
		child     int
	}
	start := func(n int, name string) *proc {
		t.Helper()
		p := &proc{cid: fmt.Sprintf("%064x", 0xabc000+n), name: name}
		p.cmd = exec.Command(bin, "--api-version", "1", "-c", p.cid, "-u", p.cid, "-n", name, "--exit-command", "/usr/bin/podman")
		p.cmd.Env = append(os.Environ(), fakeConmonEnv+"=1")
		out, err := p.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fscan(out, &p.child); err != nil {
			t.Fatalf("fake conmon did not report its child: %v", err)
		}
		go func() { _, _ = io.Copy(io.Discard, out); _ = p.cmd.Wait() }()
		t.Cleanup(func() { _ = syscall.Kill(p.child, syscall.SIGKILL); _ = p.cmd.Process.Kill() })
		return p
	}
	suffix := fmt.Sprint(os.Getpid())
	orphan := start(1, "sbx-sb-leftover"+suffix)
	known := start(2, "sbx-sb-known"+suffix)       // Podman still has this container
	foreign := start(3, "sbx-sb-foreign"+suffix)   // nobody claims this sandbox
	notOurs := start(4, "some-other-thing"+suffix) // not a sandbox at all

	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/exists") {
			return false
		}
		if strings.Contains(r.URL.Path, known.cid) {
			w.WriteHeader(http.StatusNoContent)
		} else {
			libpodError(w, http.StatusNotFound, "no such container")
		}
		return true
	}
	b := startFake(t, f, nil)
	mine := func(ls []Leftover) map[string]Leftover {
		out := map[string]Leftover{}
		for _, l := range ls {
			if strings.HasSuffix(l.SandboxID, suffix) {
				out[l.SandboxID] = l
			}
		}
		return out
	}
	alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil && !zombie(pid) }

	// Too young: nothing is a leftover yet.
	left, err := b.Leftovers(context.Background(), time.Hour)
	if err != nil || len(mine(left)) != 0 {
		t.Fatalf("with an hour's grace: %+v, %v", mine(left), err)
	}
	left, err = b.Leftovers(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := mine(left)
	if len(got) != 2 || got["sb-leftover"+suffix].ConmonPID != orphan.cmd.Process.Pid || got["sb-leftover"+suffix].ContainerID != orphan.cid ||
		got["sb-foreign"+suffix].ConmonPID != foreign.cmd.Process.Pid {
		t.Fatalf("leftovers = %+v, want the orphan and the foreign one, not the known container or the non-sandbox", got)
	}

	n, err := b.reapLeftovers(context.Background(), 0, func(sandboxID string) bool { return sandboxID == "sb-leftover"+suffix })
	if err != nil || n != 1 {
		t.Fatalf("reapLeftovers = %d, %v, want 1", n, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (alive(orphan.cmd.Process.Pid) || alive(orphan.child)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(orphan.cmd.Process.Pid) || alive(orphan.child) {
		t.Errorf("the leftover survived: conmon alive=%v, its child alive=%v", alive(orphan.cmd.Process.Pid), alive(orphan.child))
	}
	for what, p := range map[string]*proc{"the conmon of a container Podman has": known, "a leftover nobody claimed": foreign, "a conmon that is not a sandbox's": notOurs} {
		if !alive(p.cmd.Process.Pid) || !alive(p.child) {
			t.Errorf("%s was killed", what)
		}
	}
	// Nothing left to do on a second pass.
	if n, err := b.reapLeftovers(context.Background(), 0, func(id string) bool { return id == "sb-leftover"+suffix }); n != 0 || err != nil {
		t.Errorf("second pass: %d, %v", n, err)
	}
}

func zombie(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(raw), ')')
	return i < 0 || strings.HasPrefix(strings.TrimSpace(string(raw[i+1:])), "Z")
}
