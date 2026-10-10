package webhook_test

// A live end-to-end test of webhooks: the real sbxd binary, rootless
// Podman, Postgres, and receivers in this process.
//
// It is gated like test/e2e: set SBX_E2E=1 and SBX_E2E_DATABASE_URL (a
// Postgres URL whose user may create databases), with the Podman API
// socket running and the alpine image pulled:
//
//	SBX_E2E=1 SBX_E2E_DATABASE_URL=postgres://... go test -count=1 -v -run TestLiveWebhooks ./internal/webhook/
//
// With SBX_E2E_RACE=1 the server is built with the race detector.
// The test has its own database, host id, run dir and port, and removes
// all of them.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/himanshubaliyan7/warmbox/internal/webhook"
)

const liveImage = "docker.io/library/alpine:3.20"

type liveReceiver struct {
	srv *httptest.Server

	mu      sync.Mutex
	script  []int
	headers []http.Header
	bodies  [][]byte
}

func newLiveReceiver(t *testing.T, script ...int) *liveReceiver {
	r := &liveReceiver{script: script}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.headers = append(r.headers, req.Header.Clone())
		r.bodies = append(r.bodies, body)
		status := http.StatusOK
		if len(r.script) > 0 {
			status, r.script = r.script[0], r.script[1:]
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *liveReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *liveReceiver) get(i int) (http.Header, []byte, webhook.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var env webhook.Envelope
	_ = json.Unmarshal(r.bodies[i], &env)
	return r.headers[i], r.bodies[i], env
}

type liveEnv struct {
	t       *testing.T
	bin     string
	dbURL   string
	base    string
	hostID  string
	runDir  string
	logPath string
	key     string
	cmd     *exec.Cmd
	done    chan error
}

func liveHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *liveEnv) admin(args ...string) string {
	e.t.Helper()
	cmd := exec.Command(filepath.Join(e.bin, "sbxd"), append([]string{"admin"}, args...)...)
	cmd.Env = append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("sbxd admin %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (e *liveEnv) logTail(n int) string {
	b, _ := os.ReadFile(e.logPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "--- sbxd log tail ---\n" + strings.Join(lines, "\n")
}

// api calls the server with the tenant's key and decodes a JSON answer.
func (e *liveEnv) api(method, path string, body, out any, wantStatus int) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.base+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("X-API-Key", e.key)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v\n%s", method, path, err, e.logTail(30))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("%s %s: %d %s (want %d)\n%s", method, path, resp.StatusCode, b, wantStatus, e.logTail(30))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
}

func liveWait(t *testing.T, e *liveEnv, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s\n%s", what, e.logTail(60))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveWebhooks(t *testing.T) {
	if os.Getenv("SBX_E2E") != "1" {
		t.Skip("set SBX_E2E=1 (and SBX_E2E_DATABASE_URL) to run the live webhook test")
	}
	adminURL := os.Getenv("SBX_E2E_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("SBX_E2E_DATABASE_URL is not set")
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if _, err := os.Stat(filepath.Join(xdg, "podman", "podman.sock")); xdg == "" || err != nil {
		t.Fatalf("no Podman socket under XDG_RUNTIME_DIR=%q: %v", xdg, err)
	}
	if err := exec.Command("podman", "image", "exists", liveImage).Run(); err != nil {
		t.Fatalf("image %s is not pulled for this user (podman pull %s)", liveImage, liveImage)
	}

	// A private database, dropped at the end.
	run := liveHex(4)
	dbName := "sbx_wh_" + run
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to SBX_E2E_DATABASE_URL: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}
	conn.Close(ctx)
	cancel()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if conn, err := pgx.Connect(ctx, adminURL); err == nil {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			conn.Close(ctx)
		}
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName

	// Build the server and the agent.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, b := range []struct{ pkg, cgo string }{{"./cmd/sbxd", ""}, {"./cmd/sbx-agent", "0"}} {
		args := []string{"build", "-o", bin + "/"}
		if b.cgo == "" && os.Getenv("SBX_E2E_RACE") == "1" {
			args = append(args, "-race")
		}
		cmd := exec.Command("go", append(args, b.pkg)...)
		cmd.Dir = root
		cmd.Env = os.Environ()
		if b.cgo != "" {
			cmd.Env = append(cmd.Env, "CGO_ENABLED="+b.cgo)
		}
		if msg, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", b.pkg, err, msg)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	ln.Close()

	e := &liveEnv{t: t, bin: bin, dbURL: u.String(), base: "http://" + listen, hostID: "whlive-" + run,
		runDir: filepath.Join(xdg, "sbxwh-"+run), logPath: filepath.Join(t.TempDir(), "sbxd.log")}
	t.Cleanup(func() {
		if e.cmd != nil {
			_ = e.cmd.Process.Kill()
			<-e.done
		}
		// Whatever still carries this run's host id is this run's.
		if out, err := exec.Command("podman", "ps", "-a", "-q", "--filter", "label=sbx.host_id="+e.hostID).Output(); err == nil {
			if ids := strings.Fields(string(out)); len(ids) > 0 {
				_ = exec.Command("podman", append([]string{"rm", "-f", "-t", "0"}, ids...)...).Run()
			}
		}
		_ = os.RemoveAll(e.runDir)
		if log, _ := os.ReadFile(e.logPath); bytes.Contains(log, []byte("DATA RACE")) {
			i := bytes.Index(log, []byte("WARNING: DATA RACE"))
			t.Errorf("the race detector fired in sbxd:\n%s", log[i:min(len(log), i+6000)])
		}
		if t.Failed() {
			t.Log(e.logTail(80))
		}
	})

	e.admin("tenant", "create", "--id", "acme", "--max-concurrent", "5", "--rate", "1000", "--burst", "2000")
	e.admin("tenant", "create", "--id", "rival", "--max-concurrent", "5", "--rate", "1000", "--burst", "2000")
	var k struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(e.admin("key", "create", "--tenant", "acme", "--json")), &k); err != nil || k.Key == "" {
		t.Fatalf("key create: %v", err)
	}
	e.key = k.Key
	e.admin("template", "apply", "--id", "wh", "--image", liveImage, "--cpus", "0.5", "--memory", "128m", "--pids", "64",
		"--workdir-size", "32m", "--tmp-size", "16m", "--network", "none", "--warm-target", "1", "--max-total", "3")

	// Start sbxd. Loopback and http are opened because the receivers live
	// in this process; the retry delays are shortened to keep the test quick.
	logf, err := os.OpenFile(e.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	e.cmd = exec.Command(filepath.Join(bin, "sbxd"), "serve", "--listen", listen, "--run-dir", e.runDir, "--host-id", e.hostID,
		"--log-level", "debug", "--claim-wait", "30s", "--shutdown-policy", "destroy",
		"--webhook-allow-http", "--webhook-allow-loopback", "--webhook-poll-interval", "100ms",
		"--webhook-retry-base", "300ms", "--webhook-retry-max", "1s", "--webhook-timeout", "5s")
	e.cmd.Env = append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL)
	e.cmd.Stdout, e.cmd.Stderr = logf, logf
	if err := e.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logf.Close()
	e.done = make(chan error, 1)
	go func(c *exec.Cmd) { e.done <- c.Wait() }(e.cmd)
	liveWait(t, e, "sbxd to be ready", 90*time.Second, func() bool {
		select {
		case err := <-e.done:
			e.done <- err
			t.Fatalf("sbxd exited during start-up: %v\n%s", err, e.logTail(40))
		default:
		}
		resp, err := http.Get(e.base + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})

	// Receivers and webhooks.
	all := newLiveReceiver(t)
	flaky := newLiveReceiver(t, 500, 500) // 500 twice, then 200
	type hook struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
		URL    string `json:"url"`
	}
	var hAll, hFlaky hook
	e.api("POST", "/v1/webhooks", map[string]any{"url": all.srv.URL + "/all", "description": "everything"}, &hAll, 201)
	e.api("POST", "/v1/webhooks", map[string]any{"url": flaky.srv.URL + "/flaky", "event_types": []string{"sandbox.terminated"}}, &hFlaky, 201)
	if !strings.HasPrefix(hAll.Secret, "whsec_") || hAll.Secret == hFlaky.Secret {
		t.Fatalf("secrets: %q %q", hAll.Secret, hFlaky.Secret)
	}
	// The SSRF rules hold on the live server too, even with the two
	// development switches on.
	for _, bad := range []string{"http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/", "http://user:pw@127.0.0.1/", "ftp://127.0.0.1/"} {
		e.api("POST", "/v1/webhooks", map[string]any{"url": bad}, nil, 400)
	}
	// The operator sees them with the CLI; a webhook of another tenant
	// made there never receives acme's events.
	rival := newLiveReceiver(t)
	e.admin("webhook", "create", "--tenant", "rival", "--url", rival.srv.URL+"/rival", "--allow-http", "--allow-loopback")
	if out := e.admin("webhook", "list"); !strings.Contains(out, hAll.ID) || !strings.Contains(out, "rival") || strings.Contains(out, "whsec_") {
		t.Fatalf("admin webhook list:\n%s", out)
	}

	// The test route.
	var tr struct {
		EventID   string `json:"event_id"`
		Delivered bool   `json:"delivered"`
		Status    int    `json:"status"`
	}
	e.api("POST", "/v1/webhooks/"+hAll.ID+"/test", nil, &tr, 200)
	if !tr.Delivered || tr.Status != 200 || all.count() != 1 {
		t.Fatalf("test event: %+v, %d requests", tr, all.count())
	}
	if h, body, env := all.get(0); env.Type != "webhook.test" || env.ID != tr.EventID || webhook.Verify(hAll.Secret, h, body, time.Now(), 0) != nil {
		t.Fatalf("test event at the receiver: %s", body)
	}

	// A sandbox's life: create, run a command, kill.
	var sb struct {
		ID          string `json:"id"`
		AccessToken string `json:"access_token"`
	}
	e.api("POST", "/v1/sandboxes", map[string]any{"template_id": "wh", "metadata": map[string]string{"job": "live"},
		"env": map[string]string{"LIVE_SECRET": "must-not-leave"}}, &sb, 201)
	var res struct {
		Exit struct {
			ExitCode int `json:"exit_code"`
		} `json:"exit"`
	}
	e.api("POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=true", map[string]any{"cmd": "sh", "args": []string{"-c", "echo from-the-sandbox; exit 7"}}, &res, 200)
	if res.Exit.ExitCode != 7 {
		t.Fatalf("command result: %+v", res)
	}
	e.api("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil, 204)

	want := []string{"webhook.test", "sandbox.created", "sandbox.running", "command.started", "command.finished", "sandbox.terminated", "sandbox.destroyed"}
	liveWait(t, e, "the lifecycle events at the receiver", 60*time.Second, func() bool { return all.count() >= len(want) })
	time.Sleep(500 * time.Millisecond) // anything that should not be there would arrive now
	if all.count() != len(want) {
		t.Fatalf("%d requests at the receiver, want %d", all.count(), len(want))
	}
	seen := map[string]bool{}
	for i, typ := range want {
		h, body, env := all.get(i)
		if env.Type != typ {
			var got []string
			for j := 0; j < all.count(); j++ {
				_, _, e := all.get(j)
				got = append(got, e.Type)
			}
			t.Fatalf("events arrived as %v, want %v", got, want)
		}
		if err := webhook.Verify(hAll.Secret, h, body, time.Now(), 0); err != nil {
			t.Errorf("%s does not verify: %v", typ, err)
		}
		if err := webhook.Verify(hFlaky.Secret, h, body, time.Now(), 0); err == nil {
			t.Errorf("%s verifies with another webhook's secret", typ)
		}
		if h.Get("Webhook-Id") != env.ID || seen[env.ID] || env.Version != 1 {
			t.Errorf("%s: id %q, header %q", typ, env.ID, h.Get("Webhook-Id"))
		}
		seen[env.ID] = true
		if i > 0 && (env.SandboxID != sb.ID || env.TemplateID != "wh" || env.CreatedAt.IsZero()) {
			t.Errorf("%s: %s", typ, body)
		}
		for _, leak := range []string{sb.AccessToken, "must-not-leave", "LIVE_SECRET", e.hostID, "access_token"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s contains %q: %s", typ, leak, body)
			}
		}
	}
	if _, _, env := all.get(1); env.Data["state"] != "assigned" {
		t.Errorf("sandbox.created: %v", env.Data)
	}
	if _, _, env := all.get(4); env.Data["exit_code"] != float64(7) || env.Data["cmd"] != "sh" {
		t.Errorf("command.finished: %v", env.Data)
	}
	if _, body, env := all.get(5); env.Data["reason"] != "released" || env.Data["state"] != "recycling" ||
		fmt.Sprint(env.Data["metadata"]) != "map[job:live]" {
		t.Errorf("sandbox.terminated: %s", body)
	}

	// The endpoint that answers 500 twice gets the event until it takes
	// it: three requests, one id, and then no more.
	liveWait(t, e, "the third request at the flaky receiver", 30*time.Second, func() bool { return flaky.count() >= 3 })
	time.Sleep(1500 * time.Millisecond) // longer than the largest retry delay
	if flaky.count() != 3 {
		t.Fatalf("the flaky receiver got %d requests, want exactly 3", flaky.count())
	}
	_, _, first := flaky.get(0)
	for i := 0; i < 3; i++ {
		h, body, env := flaky.get(i)
		if env.Type != "sandbox.terminated" || env.ID != first.ID || h.Get("Warmbox-Delivery-Attempt") != fmt.Sprint(i+1) {
			t.Errorf("flaky request %d: attempt %q: %s", i, h.Get("Warmbox-Delivery-Attempt"), body)
		}
		if err := webhook.Verify(hFlaky.Secret, h, body, time.Now(), 0); err != nil {
			t.Errorf("flaky request %d does not verify: %v", i, err)
		}
	}
	var log struct {
		Deliveries []struct {
			Status         string `json:"status"`
			Attempt        int    `json:"attempt"`
			ResponseStatus int    `json:"response_status"`
			EventID        string `json:"event_id"`
		} `json:"deliveries"`
	}
	e.api("GET", "/v1/webhooks/"+hFlaky.ID+"/deliveries", nil, &log, 200)
	if len(log.Deliveries) != 3 || log.Deliveries[0].Status != "succeeded" || log.Deliveries[0].Attempt != 3 ||
		log.Deliveries[1].Status != "failed" || log.Deliveries[2].ResponseStatus != 500 || log.Deliveries[0].EventID != first.ID {
		t.Errorf("flaky delivery log: %+v", log.Deliveries)
	}
	var state struct {
		Enabled             bool    `json:"enabled"`
		ConsecutiveFailures int     `json:"consecutive_failures"`
		FailingSince        *string `json:"failing_since"`
		LastSuccessAt       *string `json:"last_success_at"`
	}
	e.api("GET", "/v1/webhooks/"+hFlaky.ID, nil, &state, 200)
	if !state.Enabled || state.ConsecutiveFailures != 0 || state.FailingSince != nil || state.LastSuccessAt == nil {
		t.Errorf("flaky webhook after recovery: %+v", state)
	}
	if rival.count() != 0 {
		t.Errorf("the other tenant's webhook received %d requests", rival.count())
	}

	// Metrics.
	resp, err := http.Get(e.base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, wantLine := range []string{`sbx_webhook_deliveries_total{outcome="succeeded"} 7`, `sbx_webhook_deliveries_total{outcome="failed"} 2`,
		`sbx_webhook_delivery_failures_total{code="http_status"} 2`, `sbx_webhook_test_events_total{outcome="succeeded"} 1`,
		"sbx_webhook_backlog_events ", "sbx_webhook_lag_seconds "} {
		if !strings.Contains(string(metrics), wantLine) {
			t.Errorf("/metrics lacks %q", wantLine)
		}
	}

	// A graceful stop ends cleanly with the worker running.
	_ = e.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-e.done:
		e.cmd = nil
		if err != nil {
			t.Errorf("sbxd exited with %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("sbxd did not stop within 60s of SIGTERM")
	}
}
