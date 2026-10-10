package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentclient"
)

func wantAgentError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var ae *agentclient.Error
	if !asError(err, &ae) || ae.Status != status || ae.Code != code {
		t.Fatalf("got %v, want %d %s", err, status, code)
	}
}

func TestFileStatAndConflicts(t *testing.T) {
	h := startAgent(t, opts{})
	ctx := testCtx(t)
	if err := h.c.WriteFile(ctx, "d/f.txt", 0o640, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}

	fi, err := h.c.StatFile(ctx, "d/f.txt")
	if err != nil || fi.Name != "f.txt" || fi.Path != filepath.Join(h.workspace, "d/f.txt") || fi.Size != 5 || fi.IsDir || fi.Mode != 0o640 {
		t.Fatalf("stat file: %+v %v", fi, err)
	}
	if fi, err = h.c.StatFile(ctx, "d"); err != nil || !fi.IsDir || fi.Name != "d" {
		t.Fatalf("stat dir: %+v %v", fi, err)
	}
	_, err = h.c.StatFile(ctx, "d/nope")
	wantAgentError(t, err, 404, "not_found")
	_, err = h.c.StatFile(ctx, "")
	wantAgentError(t, err, 400, "bad_request")

	// A link is followed; a dangling one is described as itself.
	if err := os.Symlink("d", filepath.Join(h.workspace, "to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gone", filepath.Join(h.workspace, "dangling")); err != nil {
		t.Fatal(err)
	}
	if fi, err = h.c.StatFile(ctx, "to-dir"); err != nil || !fi.IsDir {
		t.Fatalf("stat link to a directory: %+v %v", fi, err)
	}
	if fi, err = h.c.StatFile(ctx, "dangling"); err != nil || fi.IsDir || fi.Name != "dangling" {
		t.Fatalf("stat dangling link: %+v %v", fi, err)
	}

	// The documented 409 "conflict" cases.
	_, err = h.c.StatFile(ctx, "d/f.txt/below") // ENOTDIR
	wantAgentError(t, err, 409, "conflict")
	_, err = h.c.ListFiles(ctx, "d/f.txt") // ENOTDIR
	wantAgentError(t, err, 409, "conflict")
	_, err = h.c.Mkdir(ctx, "d/f.txt") // EEXIST: a file is in the way
	wantAgentError(t, err, 409, "conflict")
	_, err = h.c.Mkdir(ctx, "d/f.txt/sub") // ENOTDIR
	wantAgentError(t, err, 409, "conflict")
	err = h.c.WriteFile(ctx, "d/f.txt/x", 0, strings.NewReader("x")) // parent is a file
	wantAgentError(t, err, 409, "conflict")
	err = h.c.WriteFile(ctx, "d", 0, strings.NewReader("x"))
	wantAgentError(t, err, 409, "is_directory")
}

// The stream route answers with its status line and headers at once,
// also for a process that prints nothing.
func TestStreamHeadersAreImmediate(t *testing.T) {
	h := startAgent(t, opts{keepalive: time.Minute})
	p := h.start(sh("sleep 30"))
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
		},
		ResponseHeaderTimeout: 2 * time.Second,
	}}
	start := time.Now()
	resp, err := hc.Get("http://agent/v1/processes/" + p.ID + "/stream")
	if err != nil {
		t.Fatalf("no response headers from a silent process: %v", err)
	}
	defer resp.Body.Close()
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("headers took %v", took)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Errorf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	if err := h.c.Signal(testCtx(t), p.ID, "SIGKILL"); err != nil {
		t.Fatal(err)
	}
}
