package agentclient_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// TestPodmanSmoke runs the static agent as PID 1 of a hardened rootless
// Podman container and drives it from the host. It needs Podman and an
// image, so it only runs when SBX_PODMAN_SMOKE=1:
//
//	SBX_PODMAN_SMOKE=1 go test -run TestPodmanSmoke -v ./internal/agentclient/
//
// SBX_PODMAN_IMAGE overrides the image (default python:3.12-alpine).
//
// User namespace: in rootless Podman, container uid 1000 is by default a
// subordinate uid on the host, which can neither create the socket in a
// 0700 run directory owned by the host user nor be reached through a
// 0600 socket. Mapping the host user onto the container user with
// --userns=keep-id:uid=1000,gid=1000 makes both sides the same uid, so
// the run directory can stay 0700 and the socket 0600.
func TestPodmanSmoke(t *testing.T) {
	if os.Getenv("SBX_PODMAN_SMOKE") != "1" {
		t.Skip("set SBX_PODMAN_SMOKE=1 to run")
	}
	image := os.Getenv("SBX_PODMAN_IMAGE")
	if image == "" {
		image = "docker.io/library/python:3.12-alpine"
	}
	work := t.TempDir()
	bin := filepath.Join(work, "sbx-agent")
	build := exec.Command("go", "build", "-o", bin, "github.com/himanshubaliyan7/warmbox/cmd/sbx-agent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the agent: %v\n%s", err, out)
	}
	runDir := filepath.Join(work, "run")
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// The host side of the egress socket: a line echo.
	eln, err := net.Listen("unix", filepath.Join(runDir, runtime.EgressSocket))
	if err != nil {
		// t.TempDir can be too deep for bind(2); the bridge check is skipped then.
		t.Logf("egress socket: %v (bridge check skipped)", err)
	} else {
		defer eln.Close()
		go func() {
			for {
				c, err := eln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					line, _ := bufio.NewReader(c).ReadString('\n')
					fmt.Fprintf(c, "host proxy saw: %s", line)
				}()
			}
		}()
	}

	name := fmt.Sprintf("sbx-smoke-%d", os.Getpid())
	args := []string{"run", "--rm", "-d", "--name", name,
		"--read-only", "--cap-drop=all", "--security-opt", "no-new-privileges",
		"--network", "none", "--user", "1000:1000",
		"--userns=keep-id:uid=1000,gid=1000",
		"--tmpfs", "/workspace:rw,exec,nosuid,nodev,size=64m", "--tmpfs", "/tmp",
		"-v", runDir + ":" + runtime.RunDir,
		"-v", bin + ":" + runtime.AgentBinary + ":ro",
		image,
		runtime.AgentBinary, "serve", "--socket", runtime.RunDir + "/" + runtime.AgentSocket}
	if out, err := exec.Command("podman", args...).CombinedOutput(); err != nil {
		t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	defer func() {
		if t.Failed() {
			out, _ := exec.Command("podman", "logs", name).CombinedOutput()
			t.Logf("agent log:\n%s", out)
		}
		_ = exec.Command("podman", "rm", "-f", "-t", "0", name).Run()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	begin := time.Now()
	if err := (agentclient.Sandboxes{}).WaitReady(ctx, runDir); err != nil {
		t.Fatal(err)
	}
	t.Logf("agent ready %v after podman run returned", time.Since(begin).Round(time.Millisecond))
	c := agentclient.ForRunDir(runDir)
	defer c.Close()

	if fi, err := os.Stat(c.SocketPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v, %v", fi, err)
	}
	h, err := c.Health(ctx)
	if err != nil || !h.Ready {
		t.Fatalf("health = %+v, %v", h, err)
	}

	run := func(req agentapi.StartRequest) (stdout, stderr string, exit agentapi.ExitStatus) {
		t.Helper()
		pi, err := c.Start(ctx, req)
		if err != nil {
			t.Fatalf("start %s %v: %v", req.Cmd, req.Args, err)
		}
		for ev, err := range c.Stream(ctx, pi.ID, 0) {
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			switch ev.Type {
			case agentapi.EventStdout:
				stdout += string(ev.Data)
			case agentapi.EventStderr:
				stderr += string(ev.Data)
			case agentapi.EventExit:
				exit = *ev.Exit
			}
		}
		return
	}

	// A command, in the workspace, as the unprivileged user, agent as PID 1.
	out, _, st := run(agentapi.StartRequest{Cmd: "python3", Args: []string{"-c", "print(1)"}})
	if out != "1\n" || st.ExitCode != 0 {
		t.Fatalf("python3: %q %+v", out, st)
	}
	out, _, _ = run(agentapi.StartRequest{Cmd: "sh", Args: []string{"-c", "pwd; id -u; cat /proc/1/comm; echo $PPID"}})
	if out != "/workspace\n1000\nsbx-agent\n1\n" {
		t.Fatalf("identity = %q", out)
	}
	_, errOut, st := run(agentapi.StartRequest{Cmd: "sh", Args: []string{"-c", "echo oops >&2; exit 3"}})
	if errOut != "oops\n" || st.ExitCode != 3 || st.Reason != agentapi.ExitExited {
		t.Fatalf("stderr %q exit %+v", errOut, st)
	}

	// Orphans are reaped by the agent as the real PID 1.
	_, _, st = run(agentapi.StartRequest{Cmd: "sh", Args: []string{"-c", "(sleep 0.3 >/dev/null 2>&1 &) ; exit 7"}})
	if st.ExitCode != 7 {
		t.Fatalf("exit = %+v", st)
	}
	out, _, _ = run(agentapi.StartRequest{Cmd: "sh", Args: []string{"-c",
		"sleep 1; z=0; for f in /proc/[0-9]*/stat; do read -r l < $f 2>/dev/null || continue; case \"$l\" in *') Z '*) z=$((z+1));; esac; done; echo zombies=$z"}})
	if out != "zombies=0\n" {
		t.Fatalf("after an orphan exited: %q", out)
	}

	// Timeout kills the tree.
	_, _, st = run(agentapi.StartRequest{Cmd: "sh", Args: []string{"-c", "sleep 100 & sleep 100"}, TimeoutMs: 300})
	if st.Reason != agentapi.ExitTimeout {
		t.Fatalf("timeout exit = %+v", st)
	}

	// A PTY shell session over the WebSocket.
	pi, err := c.Start(ctx, agentapi.StartRequest{Cmd: "sh", PTY: &agentapi.PTYSize{Cols: 90, Rows: 25}})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.AutoAck = true
	var acc []byte
	expect := func(want string) {
		t.Helper()
		for !bytes.Contains(acc, []byte(want)) {
			f, err := ws.Recv(ctx)
			if err != nil || f.Status != nil {
				t.Fatalf("waiting for %q: %v %+v (have %q)", want, err, f.Status, acc)
			}
			acc = append(acc, f.Data...)
		}
	}
	send := func(s string) {
		t.Helper()
		if err := ws.Write(ctx, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	send("stty size; echo term=$TERM\n")
	expect("25 90")
	expect("term=xterm-256color")
	if err := ws.Resize(ctx, 132, 43); err != nil {
		t.Fatal(err)
	}
	send("stty size\n")
	expect("43 132")
	acc = nil
	send("yes\n")
	expect("y\r\ny\r\ny\r\n")
	send("\x03")
	send("echo interrupted-$((40+2))\n")
	expect("interrupted-42")
	send("exit 5\n")
	for {
		f, err := ws.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Status != nil {
			if f.Status.Exit == nil || f.Status.Exit.ExitCode != 5 {
				t.Fatalf("shell status = %+v", f.Status)
			}
			break
		}
	}

	// Files: the tmpfs workspace is writable, the root filesystem is not.
	data := make([]byte, 3<<20)
	_, _ = rand.Read(data)
	if err := c.WriteFile(ctx, "dir/blob.bin", 0o600, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var back bytes.Buffer
	if _, err := c.ReadFile(ctx, "/workspace/dir/blob.bin", &back); err != nil || !bytes.Equal(back.Bytes(), data) {
		t.Fatalf("read back %d bytes, %v", back.Len(), err)
	}
	ls, err := c.ListFiles(ctx, "dir")
	if err != nil || len(ls) != 1 || ls[0].Size != int64(len(data)) || ls[0].Mode != 0o600 {
		t.Fatalf("list = %+v, %v", ls, err)
	}
	out, _, _ = run(agentapi.StartRequest{Cmd: "sh", Args: []string{"-c", "wc -c < dir/blob.bin"}})
	if strings.TrimSpace(out) != fmt.Sprint(len(data)) {
		t.Fatalf("wc = %q", out)
	}
	if err := c.WriteFile(ctx, "/etc/sbx-test", 0, strings.NewReader("x")); agentclient.StatusOf(err) != http.StatusForbidden {
		t.Fatalf("write to the read-only root: %v, want 403", err)
	}
	if err := c.DeleteFile(ctx, "dir"); err != nil {
		t.Fatal(err)
	}

	// Init, session env, and the egress bridge with no network at all.
	if err := (agentclient.Sandboxes{}).Init(ctx, runDir, agentapi.InitRequest{
		SandboxID: "smoke", Generation: 1, Env: map[string]string{"SESSION": "s1"}, EgressProxy: eln != nil,
	}); err != nil {
		t.Fatal(err)
	}
	if eln != nil {
		out, errOut, st = run(agentapi.StartRequest{Cmd: "python3", Args: []string{"-c", `
import os, socket
print(os.environ["SESSION"], os.environ["HTTPS_PROXY"])
s = socket.create_connection(("127.0.0.1", 3128), timeout=5)
s.sendall(b"hello from the sandbox\n")
print(s.makefile().readline().strip())
`}})
		if out != "s1 http://127.0.0.1:3128\nhost proxy saw: hello from the sandbox\n" {
			t.Fatalf("egress bridge: stdout %q stderr %q exit %+v", out, errOut, st)
		}
	}
}
