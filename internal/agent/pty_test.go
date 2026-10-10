package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
)

func ptyReq(cmd string, args ...string) agentapi.StartRequest {
	return agentapi.StartRequest{Cmd: cmd, Args: args, PTY: &agentapi.PTYSize{Cols: 100, Rows: 30}}
}

func TestPTYOutputSizeAndTerm(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(ptyReq("sh", "-c", "stty size; echo $TERM; test -t 0 && test -t 1 && test -t 2 && echo tty"))
	if !pi.PTY {
		t.Fatalf("info = %+v", pi)
	}
	r := h.collect(pi.ID, 0)
	if len(r.stdout)+len(r.stderr) != 0 {
		t.Errorf("pty mode produced stdout/stderr events")
	}
	// The line discipline turns \n into \r\n.
	if got, want := string(r.pty), "30 100\r\nxterm-256color\r\ntty\r\n"; got != want {
		t.Errorf("pty output = %q, want %q", got, want)
	}
	wantExit(t, r.exit, 0, "", agentapi.ExitExited)
	if err := h.c.CloseStdin(testCtx(t), pi.ID); !agentclient.IsConflict(err) {
		t.Errorf("close-stdin on a pty: err = %v, want 409", err)
	}
}

// wsExpect reads frames until the accumulated output contains want.
func wsExpect(t *testing.T, ws *agentclient.WS, acc *[]byte, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for !bytes.Contains(*acc, []byte(want)) {
		f, err := ws.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (have %q)", want, err, *acc)
		}
		if f.Status != nil {
			t.Fatalf("waiting for %q: process ended: %+v (have %q)", want, f.Status, *acc)
		}
		*acc = append(*acc, f.Data...)
	}
}

// wsStatus reads to the status frame and checks the stream then ends.
func wsStatus(t *testing.T, ws *agentclient.WS, within time.Duration) (*agentapi.StatusFrame, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	var out []byte
	for {
		f, err := ws.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for the status frame: %v", err)
		}
		out = append(out, f.Data...)
		if f.Status != nil {
			if _, err := ws.Recv(ctx); !errors.Is(err, io.EOF) {
				t.Fatalf("after the status frame: err = %v, want io.EOF", err)
			}
			return f.Status, out
		}
	}
}

func TestPTYResize(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	pi := h.start(ptyReq("sh"))
	ws, err := h.c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var acc []byte

	if err := ws.Write(ctx, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	wsExpect(t, ws, &acc, "30 100")

	// Resize over HTTP.
	if err := h.c.Resize(ctx, pi.ID, 120, 40); err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	wsExpect(t, ws, &acc, "40 120")

	// Resize in band.
	if err := ws.Resize(ctx, 77, 22); err != nil {
		t.Fatal(err)
	}
	if err := ws.Ping(ctx, []byte("sync")); err != nil { // frames are handled in order
		t.Fatal(err)
	}
	if err := ws.Write(ctx, []byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	wsExpect(t, ws, &acc, "22 77")

	if err := ws.Write(ctx, []byte("exit 9\n")); err != nil {
		t.Fatal(err)
	}
	st, _ := wsStatus(t, ws, 15*time.Second)
	wantExit(t, st.Exit, 9, "", agentapi.ExitExited)
}

func TestPTYCtrlCInterrupts(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(ptyReq("sleep", "100"))
	time.Sleep(100 * time.Millisecond)
	begin := time.Now()
	if err := h.c.WriteStdin(testCtx(t), pi.ID, []byte{0x03}); err != nil {
		t.Fatal(err)
	}
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 128+2, "SIGINT", agentapi.ExitKilled)
	if d := time.Since(begin); d > 3*time.Second {
		t.Errorf("Ctrl+C took %v", d)
	}
	if !strings.Contains(string(r.pty), "^C") {
		t.Errorf("pty output = %q, want the ^C echo", r.pty)
	}
}

func TestPTYCtrlDEndsCat(t *testing.T) {
	h := startAgent(t, opts{})
	pi := h.start(ptyReq("cat"))
	ctx := testCtx(t)
	if err := h.c.WriteStdin(ctx, pi.ID, []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.c.WriteStdin(ctx, pi.ID, []byte{0x04}); err != nil {
		t.Fatal(err)
	}
	r := h.collect(pi.ID, 0)
	wantExit(t, r.exit, 0, "", agentapi.ExitExited)
	// Once echoed by the terminal, once written by cat.
	if got := string(r.pty); got != "hi\r\nhi\r\n" {
		t.Errorf("pty output = %q", got)
	}
}

func TestProcessSurvivesDisconnectAndReattach(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	pi := h.start(ptyReq("cat"))

	ws, err := h.c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var acc []byte
	if err := ws.Write(ctx, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	wsExpect(t, ws, &acc, "first\r\nfirst\r\n")
	resume := ws.Offset()
	if resume != int64(len(acc)) {
		t.Fatalf("Offset = %d, have %d bytes", resume, len(acc))
	}
	ws.Close()

	// Output produced while nobody is attached goes to the ring.
	time.Sleep(100 * time.Millisecond)
	if err := h.c.WriteStdin(ctx, pi.ID, []byte("detached\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	got, err := h.c.Get(ctx, pi.ID)
	if err != nil || got.Exit != nil {
		t.Fatalf("after disconnect: %+v, %v", got, err)
	}

	ws2, err := h.c.DialWS(ctx, pi.ID, resume)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close()
	if ws2.Start.Offset != resume || ws2.Start.Dropped != 0 {
		t.Fatalf("reattach start = %+v, want offset %d", ws2.Start, resume)
	}
	var acc2 []byte
	wsExpect(t, ws2, &acc2, "detached\r\ndetached\r\n")
	if err := ws2.Write(ctx, []byte("again\n")); err != nil {
		t.Fatal(err)
	}
	wsExpect(t, ws2, &acc2, "again\r\nagain\r\n")
	if string(acc2) != "detached\r\ndetached\r\nagain\r\nagain\r\n" {
		t.Fatalf("reattached output = %q", acc2)
	}
	if err := ws2.Write(ctx, []byte{0x04}); err != nil {
		t.Fatal(err)
	}
	st, _ := wsStatus(t, ws2, 15*time.Second)
	wantExit(t, st.Exit, 0, "", agentapi.ExitExited)
}

func TestWSPipeModeChannelsSignalAndPing(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)

	// Separate stdout and stderr channels, stdin, exit status.
	req := sh("read line; echo out:$line; echo err:$line >&2; exit 6")
	req.Stdin = true
	pi := h.start(req)
	ws, err := h.c.DialWS(ctx, pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if ws.Start.Offset != 0 || ws.Start.Dropped != 0 {
		t.Fatalf("start = %+v", ws.Start)
	}
	if err := ws.Ping(ctx, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	f, err := ws.Recv(ctx)
	if err != nil || f.Channel != agentapi.ChannelPong || string(f.Data) != "payload" {
		t.Fatalf("pong = %+v, %v", f, err)
	}
	if err := ws.Write(ctx, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr []byte
	var cursor int64
	for {
		f, err := ws.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Status != nil {
			wantExit(t, f.Status.Exit, 6, "", agentapi.ExitExited)
			break
		}
		if f.Offset != cursor {
			t.Fatalf("frame offset %d, want %d", f.Offset, cursor)
		}
		cursor += int64(len(f.Data))
		switch f.Channel {
		case agentapi.ChannelStdout:
			stdout = append(stdout, f.Data...)
		case agentapi.ChannelStderr:
			stderr = append(stderr, f.Data...)
		default:
			t.Fatalf("unexpected channel %d", f.Channel)
		}
	}
	if string(stdout) != "out:x\n" || string(stderr) != "err:x\n" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}
	if _, err := ws.Recv(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("after status: %v", err)
	}

	// Signal frame.
	p2 := h.start(agentapi.StartRequest{Cmd: "sleep", Args: []string{"100"}})
	ws2, err := h.c.DialWS(ctx, p2.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close()
	if err := ws2.Signal(ctx, "SIGQUIT"); err != nil {
		t.Fatal(err)
	}
	st, _ := wsStatus(t, ws2, 10*time.Second)
	wantExit(t, st.Exit, 128+3, "SIGQUIT", agentapi.ExitKilled)

	// Attaching to an exited process replays it and ends at once; a
	// truncated replay says so in the first OffsetFrame.
	ws3, err := h.c.DialWS(ctx, pi.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer ws3.Close()
	st, out := wsStatus(t, ws3, 10*time.Second)
	wantExit(t, st.Exit, 6, "", agentapi.ExitExited)
	if len(out) != int(cursor)-4 {
		t.Fatalf("replay from 4 returned %d bytes, want %d", len(out), cursor-4)
	}

	if _, err := h.c.DialWS(ctx, "nope", 0); !agentclient.IsNotFound(err) {
		t.Fatalf("ws of unknown process: err = %v, want 404", err)
	}
}

func TestWSTruncatedStart(t *testing.T) {
	h := startAgent(t, opts{ring: 100})
	pi := h.start(sh("i=0; while [ $i -lt 100 ]; do printf 0123456789; i=$((i+1)); done"))
	h.waitExit(pi.ID)
	ws, err := h.c.DialWS(testCtx(t), pi.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if ws.Start.Offset != 900 || ws.Start.Dropped != 900 {
		t.Fatalf("start = %+v, want offset 900 dropped 900", ws.Start)
	}
	st, out := wsStatus(t, ws, 10*time.Second)
	wantExit(t, st.Exit, 0, "", agentapi.ExitExited)
	if len(out) != 100 || ws.Offset() != 1000 {
		t.Fatalf("got %d bytes, offset %d", len(out), ws.Offset())
	}
}

// rssKB reads the resident set size of a process.
func rssKB(t *testing.T, pid int) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/" + itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			var kb int64
			for _, c := range strings.TrimSpace(v) {
				if c < '0' || c > '9' {
					break
				}
				kb = kb*10 + int64(c-'0')
			}
			return kb
		}
	}
	t.Fatal("no VmRSS")
	return 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
