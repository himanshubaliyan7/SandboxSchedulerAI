package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentclient"
)

// The suite runs the agent in one of two ways:
//
//   - in-process (default): New + Serve in the test binary, ReapAll off,
//     which lets tests look at internal state;
//   - subprocess: the test binary re-executes itself as `sbx-agent serve`
//     (race detector included), which exercises the real command line and
//     the PID-1-style reaper. SBX_AGENT_TEST_MODE=subprocess runs every
//     test this way; the reaper tests always do.
const helperEnv = "SBX_AGENT_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(Main(os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}

type opts struct {
	ring         int
	keepalive    time.Duration
	drainGrace   time.Duration
	egressListen string
	subprocess   bool
}

type harness struct {
	t         *testing.T
	c         *agentclient.Client
	dir       string
	sock      string
	workspace string
	a         *Agent // nil in subprocess mode
	pid       int    // agent pid in subprocess mode
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func startAgent(t *testing.T, o opts) *harness {
	t.Helper()
	// Short path: Unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &harness{t: t, dir: dir, sock: filepath.Join(dir, "agent.sock"), workspace: filepath.Join(dir, "workspace")}
	if err := os.Mkdir(h.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if o.keepalive == 0 {
		o.keepalive = 5 * time.Second
	}
	if o.egressListen == "" {
		o.egressListen = "127.0.0.1:0"
	}
	if os.Getenv("SBX_AGENT_TEST_MODE") == "subprocess" {
		o.subprocess = true
	}

	if o.subprocess {
		args := []string{"serve", "--socket", h.sock, "--workspace", h.workspace,
			"--keepalive", o.keepalive.String(), "--egress-listen", o.egressListen}
		if o.ring > 0 {
			args = append(args, "--ring-bytes", strconv.Itoa(o.ring))
		}
		if o.drainGrace > 0 {
			args = append(args, "--drain-grace", o.drainGrace.String())
		}
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		var stderr lockedBuffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		h.pid = cmd.Process.Pid
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("agent subprocess: %v\n%s", err, stderr.String())
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Errorf("agent subprocess did not stop on SIGTERM\n%s", stderr.String())
			}
			if strings.Contains(stderr.String(), "DATA RACE") {
				t.Errorf("agent subprocess reported a data race:\n%s", stderr.String())
			}
			if _, err := os.Stat(h.sock); err == nil {
				t.Errorf("agent left its socket behind")
			}
		})
	} else {
		h.a = New(Config{
			SocketPath:        h.sock,
			Workspace:         h.workspace,
			RingBytes:         o.ring,
			KeepaliveInterval: o.keepalive,
			DrainGrace:        o.drainGrace,
			EgressListenAddr:  o.egressListen,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.a.Serve(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Errorf("Serve did not return")
			}
		})
	}

	h.c = agentclient.New(h.sock)
	t.Cleanup(h.c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.c.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (h *harness) start(req agentapi.StartRequest) agentapi.ProcessInfo {
	h.t.Helper()
	pi, err := h.c.Start(testCtx(h.t), req)
	if err != nil {
		h.t.Fatalf("Start(%s %v): %v", req.Cmd, req.Args, err)
	}
	return pi
}

func sh(script string) agentapi.StartRequest {
	return agentapi.StartRequest{Cmd: "sh", Args: []string{"-c", script}}
}

// result is everything one stream delivered.
type result struct {
	stdout, stderr, pty []byte
	all                 []byte // every channel, in offset order
	events              []agentapi.Event
	truncated           []agentapi.Event
	keepalives          int
	exit                *agentapi.ExitStatus
	exitOffset          int64
}

// collect reads a stream to its exit event and checks that offsets are
// contiguous and that no chunk exceeds MaxFrameBytes.
func (h *harness) collect(id string, from int64) result {
	h.t.Helper()
	var r result
	cursor := from
	for ev, err := range h.c.Stream(testCtx(h.t), id, from) {
		if err != nil {
			h.t.Fatalf("stream %s: %v (after %d events)", id, err, len(r.events))
		}
		r.events = append(r.events, ev)
		switch ev.Type {
		case agentapi.EventTruncated:
			r.truncated = append(r.truncated, ev)
			if ev.Offset-ev.Dropped != cursor {
				h.t.Fatalf("truncated event offset %d dropped %d does not continue from %d", ev.Offset, ev.Dropped, cursor)
			}
			cursor = ev.Offset
		case agentapi.EventKeepalive:
			r.keepalives++
		case agentapi.EventExit:
			r.exit = ev.Exit
			r.exitOffset = ev.Offset
			if ev.Offset != cursor {
				h.t.Fatalf("exit offset %d, want %d", ev.Offset, cursor)
			}
		case agentapi.EventStdout, agentapi.EventStderr, agentapi.EventPTY:
			if ev.Offset != cursor {
				h.t.Fatalf("%s event at offset %d, want %d", ev.Type, ev.Offset, cursor)
			}
			if len(ev.Data) == 0 || len(ev.Data) > agentapi.MaxFrameBytes {
				h.t.Fatalf("%s event with %d bytes", ev.Type, len(ev.Data))
			}
			cursor += int64(len(ev.Data))
			r.all = append(r.all, ev.Data...)
			switch ev.Type {
			case agentapi.EventStdout:
				r.stdout = append(r.stdout, ev.Data...)
			case agentapi.EventStderr:
				r.stderr = append(r.stderr, ev.Data...)
			default:
				r.pty = append(r.pty, ev.Data...)
			}
		default:
			h.t.Fatalf("unknown event type %q", ev.Type)
		}
	}
	if r.exit == nil {
		h.t.Fatalf("stream %s ended without an exit event", id)
	}
	return r
}

func (h *harness) waitExit(id string) agentapi.ExitStatus {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		pi, err := h.c.Get(testCtx(h.t), id)
		if err != nil {
			h.t.Fatal(err)
		}
		if pi.Exit != nil {
			return *pi.Exit
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("process %s did not exit", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantExit(t *testing.T, got *agentapi.ExitStatus, code int, signal string, reason agentapi.ExitReason) {
	t.Helper()
	if got == nil {
		t.Fatalf("no exit status")
	}
	if got.ExitCode != code || got.Signal != signal || got.Reason != reason {
		t.Fatalf("exit = %+v, want code %d signal %q reason %q", *got, code, signal, reason)
	}
}

// procStat returns the state and parent pid of a process from /proc.
func procStat(pid int) (state string, ppid int, comm string, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, "", err
	}
	s := string(b)
	open, cl := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || cl < 0 {
		return "", 0, "", fmt.Errorf("bad stat line")
	}
	f := strings.Fields(s[cl+1:])
	if len(f) < 2 {
		return "", 0, "", fmt.Errorf("bad stat line")
	}
	ppid, _ = strconv.Atoi(f[1])
	return f[0], ppid, s[open+1 : cl], nil
}

type child struct {
	pid   int
	state string
	comm  string
}

// childrenOf lists the direct children of pid.
func childrenOf(pid int) []child {
	var out []child
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, ppid, comm, err := procStat(n)
		if err == nil && ppid == pid {
			out = append(out, child{n, state, comm})
		}
	}
	return out
}
