package agent

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
)

func TestHealthAndSocket(t *testing.T) {
	h := startAgent(t, opts{})
	hl, err := h.c.Health(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !hl.Ready || hl.Initialized || hl.AgentVersion == "" {
		t.Fatalf("health = %+v", hl)
	}
	fi, err := os.Stat(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want socket 0600", fi.Mode())
	}
}

func TestPipeStdoutStderrAndExitCode(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(sh("for i in 1 2 3; do echo out$i; echo err$i >&2; done; exit 3"))
	if pi.ID == "" || pi.PTY || pi.Exit != nil && pi.Exit.ExitCode != 3 {
		t.Fatalf("info = %+v", pi)
	}
	r := h.collect(pi.ID, 0)
	if got, want := string(r.stdout), "out1\nout2\nout3\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := string(r.stderr), "err1\nerr2\nerr3\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if len(r.pty) != 0 {
		t.Errorf("pipe mode produced pty output %q", r.pty)
	}
	wantExit(t, r.exit, 3, "", agentapi.ExitExited)
	if r.exitOffset != int64(len(r.stdout)+len(r.stderr)) {
		t.Errorf("exit offset = %d", r.exitOffset)
	}

	got, err := h.c.Get(testCtx(t), pi.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, got.Exit, 3, "", agentapi.ExitExited)
	if got.NextOffset != r.exitOffset {
		t.Errorf("NextOffset = %d, want %d", got.NextOffset, r.exitOffset)
	}
}

func TestPipeStdinRoundTripAndCloseStdin(t *testing.T) {
	h := startAgent(t, opts{})
	req := agentapi.StartRequest{Cmd: "cat", Stdin: true}
	pi := h.start(req)
	ctx := testCtx(t)
	if err := h.c.WriteStdin(ctx, pi.ID, []byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if err := h.c.WriteStdinFrom(ctx, pi.ID, strings.NewReader("world\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.c.CloseStdin(ctx, pi.ID); err != nil {
		t.Fatal(err)
	}
	r := h.collect(pi.ID, 0)
	if string(r.stdout) != "hello world\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	wantExit(t, r.exit, 0, "", agentapi.ExitExited)
	if err := h.c.WriteStdin(ctx, pi.ID, []byte("x")); !agentclient.IsConflict(err) {
		t.Errorf("stdin after exit: err = %v, want 409", err)
	}
}

func TestPipeStdinDefaultsToDevNull(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(agentapi.StartRequest{Cmd: "cat"})
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 0, "", agentapi.ExitExited)
	if len(r.all) != 0 {
		t.Errorf("output = %q", r.all)
	}
	p2 := h.start(agentapi.StartRequest{Cmd: "sleep", Args: []string{"30"}})
	if err := h.c.WriteStdin(testCtx(t), p2.ID, []byte("x")); !agentclient.IsConflict(err) {
		t.Errorf("stdin without Stdin: err = %v, want 409", err)
	}
	if err := h.c.Resize(testCtx(t), p2.ID, 80, 24); !agentclient.IsConflict(err) {
		t.Errorf("resize in pipe mode: err = %v, want 409", err)
	}
}

func TestKilledBySignal(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(agentapi.StartRequest{Cmd: "sleep", Args: []string{"100"}})
	if err := h.c.Signal(testCtx(t), pi.ID, "SIGTERM"); err != nil {
		t.Fatal(err)
	}
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 128+15, "SIGTERM", agentapi.ExitKilled)
	if err := h.c.Signal(testCtx(t), pi.ID, "SIGTERM"); !agentclient.IsConflict(err) {
		t.Errorf("signal after exit: err = %v, want 409", err)
	}
	if err := h.c.Signal(testCtx(t), pi.ID, "SIGBOGUS"); agentclient.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("bad signal: err = %v, want 400", err)
	}
}

// The signal goes to the whole group: a background child holding the
// output pipe dies too, so the exit arrives long before the drain grace.
func TestSignalKillsProcessGroup(t *testing.T) {
	h := startAgent(t, opts{drainGrace: 20 * time.Second})
	pi := h.start(sh("sleep 100 & sleep 100"))
	time.Sleep(100 * time.Millisecond)
	begin := time.Now()
	if err := h.c.Signal(testCtx(t), pi.ID, "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 128+9, "SIGKILL", agentapi.ExitKilled)
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("exit took %v: the background child was not killed", d)
	}
}

func TestTimeoutKillsProcessTree(t *testing.T) {
	// With a long drain grace, a prompt exit event proves the background
	// sleep (which holds the output pipes) was killed as well.
	h := startAgent(t, opts{drainGrace: 20 * time.Second})
	req := sh("sleep 100 & sleep 100")
	req.TimeoutMs = 300
	begin := time.Now()
	pi := h.start(req)
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 128+9, "SIGKILL", agentapi.ExitTimeout)
	if d := time.Since(begin); d < 250*time.Millisecond || d > 5*time.Second {
		t.Errorf("timeout fired after %v, want about 300ms", d)
	}
}

func TestTimeoutNotReportedForNormalExit(t *testing.T) {
	h := startAgent(t, opts{})
	req := sh("exit 4")
	req.TimeoutMs = 5000
	r := h.collect(h.start(req).ID, 0)
	wantExit(t, r.exit, 4, "", agentapi.ExitExited)
}

// A descendant that outlives the process and keeps the pipe open delays
// the exit event only by the drain grace.
func TestExitReportedDespiteLingeringChild(t *testing.T) {
	h := startAgent(t, opts{drainGrace: 200 * time.Millisecond})
	begin := time.Now()
	pi := h.start(sh("sleep 30 & echo started; exit 5"))
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 5, "", agentapi.ExitExited)
	if string(r.stdout) != "started\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Errorf("exit took %v", d)
	}
}

func TestCwdEnvAndStartErrors(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)

	r := h.collect(h.start(agentapi.StartRequest{Cmd: "pwd"}).ID, 0)
	if got := strings.TrimSpace(string(r.stdout)); got != h.workspace {
		t.Errorf("default cwd = %q, want %q", got, h.workspace)
	}
	if err := os.Mkdir(h.workspace+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	r = h.collect(h.start(agentapi.StartRequest{Cmd: "pwd", Cwd: "sub"}).ID, 0)
	if got := strings.TrimSpace(string(r.stdout)); got != h.workspace+"/sub" {
		t.Errorf("relative cwd = %q", got)
	}
	req := sh("echo $FOO-$PWD")
	req.Env = map[string]string{"FOO": "bar"}
	req.Cwd = "/"
	r = h.collect(h.start(req).ID, 0)
	if string(r.stdout) != "bar-/\n" {
		t.Errorf("env/cwd output = %q", r.stdout)
	}

	_, err := h.c.Start(ctx, agentapi.StartRequest{Cmd: "definitely-not-a-command-xyz"})
	var ae *agentclient.Error
	if !asError(err, &ae) || ae.Status != http.StatusBadRequest || ae.Code != "command_not_found" {
		t.Errorf("missing command: err = %v", err)
	}
	if _, err := h.c.Start(ctx, agentapi.StartRequest{}); agentclient.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("empty cmd: err = %v", err)
	}
	if _, err := h.c.Start(ctx, agentapi.StartRequest{Cmd: "pwd", Cwd: "/no/such/dir"}); agentclient.StatusOf(err) != http.StatusBadRequest {
		t.Errorf("bad cwd: err = %v", err)
	}
	if _, err := h.c.Get(ctx, "nope"); !agentclient.IsNotFound(err) {
		t.Errorf("unknown id: err = %v", err)
	}
	for _, e := range h.c.Stream(ctx, "nope", 0) {
		if !agentclient.IsNotFound(e) {
			t.Errorf("stream of unknown id: err = %v", e)
		}
	}
}

func asError(err error, target **agentclient.Error) bool {
	e, ok := err.(*agentclient.Error)
	if ok {
		*target = e
	}
	return ok
}

func TestTagsAndList(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	a := h.start(agentapi.StartRequest{Cmd: "sleep", Args: []string{"100"}, Tag: "job"})
	if _, err := h.c.Start(ctx, agentapi.StartRequest{Cmd: "sleep", Args: []string{"100"}, Tag: "job"}); !agentclient.IsConflict(err) {
		t.Fatalf("duplicate live tag: err = %v, want 409", err)
	}
	byTag, err := h.c.Get(ctx, "job")
	if err != nil || byTag.ID != a.ID || byTag.Tag != "job" {
		t.Fatalf("get by tag = %+v, %v", byTag, err)
	}
	if err := h.c.Signal(ctx, a.ID, "SIGKILL"); err != nil {
		t.Fatal(err)
	}
	h.waitExit(a.ID)
	// The tag is free again once its process has exited.
	b := h.start(agentapi.StartRequest{Cmd: "true", Tag: "job"})
	h.waitExit(b.ID)

	ps, err := h.c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != a.ID || ps[1].ID != b.ID {
		t.Fatalf("list = %+v", ps)
	}
	if ps[0].Exit == nil || ps[0].Cmd != "sleep" || len(ps[0].Args) != 1 {
		t.Fatalf("list[0] = %+v", ps[0])
	}
}

func TestTwoConcurrentReaders(t *testing.T) {
	h := startAgent(t, opts{})
	var want bytes.Buffer
	for i := 1; i <= 20000; i++ {
		fmt.Fprintf(&want, "%d\n", i)
	}
	// Starts slowly so that both readers are attached while it runs.
	pi := h.start(sh("sleep 0.2; seq 1 20000"))
	res := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { res <- h.collect(pi.ID, 0) }()
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-res:
			if !bytes.Equal(r.stdout, want.Bytes()) {
				t.Errorf("reader %d: got %d bytes, want %d", i, len(r.stdout), want.Len())
			}
			wantExit(t, r.exit, 0, "", agentapi.ExitExited)
		case <-time.After(30 * time.Second):
			t.Fatal("reader timed out")
		}
	}
}

func TestKeepalive(t *testing.T) {
	h := startAgent(t, opts{keepalive: 50 * time.Millisecond})
	r := h.collect(h.start(agentapi.StartRequest{Cmd: "sleep", Args: []string{"0.5"}}).ID, 0)
	if r.keepalives < 3 {
		t.Errorf("got %d keepalives in 0.5s at 50ms, want at least 3", r.keepalives)
	}
	wantExit(t, r.exit, 0, "", agentapi.ExitExited)
}
