// Package e2e is the end-to-end test of the whole scheduler on a real
// host: the sbxd binary as a subprocess, rootless Podman, the real
// sbx-agent binary under the hardened seccomp profile, the real egress
// proxy and a real Postgres.
//
// It is gated: set SBX_E2E=1 and SBX_E2E_DATABASE_URL to a Postgres URL
// whose role may create databases (the test creates and drops its own).
// It needs the Podman user socket and the images alpine:3.20 and
// python:3.12-alpine, already pulled. For the egress checks it needs
// Docker (a container on the default bridge is the private upstream) or
// SBX_E2E_UPSTREAM=<ip:port> of an HTTP server on a private address that
// is not one of this host's own; without either those checks are skipped.
//
// sbxd owns every container labelled sbx.managed of its Podman user, so
// do not run this next to another sbxd or next to the Podman backend's
// integration tests (run it on its own, not as part of `go test ./...`
// with other packages in parallel):
//
//	SBX_E2E=1 SBX_E2E_DATABASE_URL=postgres://... go test -count=1 -v ./test/e2e/
//
// SBX_E2E_RACE=1 builds sbxd with the race detector (a detected race makes
// sbxd exit non-zero, which fails the shutdown checks); the latencies the
// test logs are then not representative.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

const (
	imgAlpine = "docker.io/library/alpine:3.20"
	imgPython = "docker.io/library/python:3.12-alpine"

	// WebSocket channels (agentapi.Channel*), spelled out so that this
	// test checks the wire format an SDK author would code against.
	chStdin, chStdout, chStatus, chResize, chOffset = 0, 1, 3, 4, 9
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// templates is the JSON given to `sbxd admin template apply --file`.
const templates = `[
 {"id":"py",    "image":"` + imgPython + `", "cpus":1, "memory":"256m", "pids_limit":64, "workdir_size":"64m", "tmp_size":"32m",
  "network":"allowlist", "idle_timeout":"10m", "max_lifetime":"1h", "warm_ttl":"30m", "warm_target":3, "max_total":16, "refill_rate":3},
 {"id":"none",  "image":"` + imgAlpine + `", "cpus":0.5, "memory":"128m", "pids_limit":64, "workdir_size":"32m", "tmp_size":"16m",
  "network":"none", "idle_timeout":"10m", "max_lifetime":"1h", "warm_target":1, "max_total":4},
 {"id":"idle",  "image":"` + imgAlpine + `", "cpus":0.5, "memory":"128m", "pids_limit":64, "workdir_size":"32m", "tmp_size":"16m",
  "network":"none", "idle_timeout":"3s", "max_lifetime":"1h", "warm_target":2, "max_total":4},
 {"id":"life",  "image":"` + imgAlpine + `", "cpus":0.5, "memory":"128m", "pids_limit":64, "workdir_size":"32m", "tmp_size":"16m",
  "network":"none", "idle_timeout":"0s", "max_lifetime":"4s", "warm_target":1, "max_total":4},
 {"id":"bench", "image":"` + imgAlpine + `", "cpus":0.5, "memory":"64m", "pids_limit":32, "workdir_size":"16m", "tmp_size":"8m",
  "network":"none", "idle_timeout":"10m", "max_lifetime":"1h", "warm_target":0, "max_total":40, "refill_rate":4}
]`

// startUpstream returns "ip:port" of an HTTP server on a private address
// that is not one of the host's own (so the egress proxy can be allowed
// to reach it), and a stop function.
func startUpstream(t *testing.T) (addr string, stop func()) {
	t.Helper()
	if a := os.Getenv("SBX_E2E_UPSTREAM"); a != "" {
		return a, func() {}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return "", func() {}
	}
	image := os.Getenv("SBX_E2E_UPSTREAM_IMAGE")
	if image == "" {
		image = "postgres:17-alpine" // any image with a busybox nc; this one is already here for the database
	}
	name := "sbx-e2e-upstream-" + randHex(3)
	script := `while true; do printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 12\r\nConnection: close\r\n\r\nupstream-ok\n' | nc -l -p 8080 >/dev/null; done`
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--entrypoint", "sh", image, "-c", script).CombinedOutput(); err != nil {
		t.Logf("no upstream container (docker run: %v: %s)", err, out)
		return "", func() {}
	}
	stop = func() { _ = exec.Command("docker", "rm", "-f", name).Run() }
	ip := strings.TrimSpace(run(t, "docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name))
	if net.ParseIP(ip) == nil {
		stop()
		t.Fatalf("upstream container has no address: %q", ip)
	}
	addr = ip + ":8080"
	waitFor(t, "the upstream to answer", 15*time.Second, func() bool {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return addr, stop
}

func TestEndToEnd(t *testing.T) {
	if os.Getenv("SBX_E2E") != "1" {
		t.Skip("set SBX_E2E=1 (and SBX_E2E_DATABASE_URL) to run the end-to-end test")
	}
	adminURL := os.Getenv("SBX_E2E_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("SBX_E2E_DATABASE_URL is not set")
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if _, err := os.Stat(filepath.Join(xdg, "podman", "podman.sock")); xdg == "" || err != nil {
		t.Fatalf("no Podman socket under XDG_RUNTIME_DIR=%q: %v", xdg, err)
	}
	for _, img := range []string{imgAlpine, imgPython} {
		if err := exec.Command("podman", "image", "exists", img).Run(); err != nil {
			t.Fatalf("image %s is not pulled for this user (podman pull %s)", img, img)
		}
	}

	// A private database, dropped at the end.
	run1 := randHex(4)
	dbName := "sbx_e2e_" + run1
	ctx, cancel := ctxTimeout(30 * time.Second)
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to SBX_E2E_DATABASE_URL: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create database: %v", err)
	}
	conn.Close(ctx)
	cancel()
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	t.Cleanup(func() {
		ctx, cancel := ctxTimeout(30 * time.Second)
		defer cancel()
		if conn, err := pgx.Connect(ctx, adminURL); err == nil {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
			conn.Close(ctx)
		}
	})

	upstream, stopUpstream := startUpstream(t)
	t.Cleanup(stopUpstream)
	upstreamIP, _, _ := net.SplitHostPort(upstream)
	internet := false
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://example.com/"); err == nil {
		resp.Body.Close()
		internet = resp.StatusCode == 200
	}
	t.Logf("egress upstream: private=%q, public internet (example.com)=%v", upstream, internet)

	e := &env{t: t, bin: buildBinaries(t), dbURL: u.String(), hostID: "e2e-" + run1,
		runDir: filepath.Join(xdg, "sbxe2e-"+run1), listen: freePort(t), logPath: filepath.Join(t.TempDir(), "sbxd.log")}
	e.base = "http://" + e.listen
	if upstream != "" {
		e.extra = append(e.extra, "--egress-permit-private-cidrs", upstreamIP+"/32")
	}
	t.Cleanup(e.cleanup)
	t.Cleanup(func() {
		if log, _ := os.ReadFile(e.logPath); bytes.Contains(log, []byte("DATA RACE")) {
			i := bytes.Index(log, []byte("WARNING: DATA RACE"))
			t.Errorf("the race detector fired in sbxd:\n%s", log[max(i, 0):min(len(log), max(i, 0)+6000)])
		}
		if t.Failed() {
			t.Log(e.logTail(60))
		}
	})

	// Bootstrap with the admin CLI only: no SQL.
	e.admin("tenant", "create", "--id", "acme", "--max-concurrent", "40", "--rate", "2000", "--burst", "4000")
	e.admin("tenant", "create", "--id", "rival", "--max-concurrent", "5", "--rate", "2000", "--burst", "4000")
	keyOf := func(tenant string) string {
		var k struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(e.admin("key", "create", "--tenant", tenant, "--json")), &k); err != nil || !strings.HasPrefix(k.Key, "sbx_") {
			t.Fatalf("key create: %v %q", err, k.Key)
		}
		return k.Key
	}
	tplFile := filepath.Join(t.TempDir(), "templates.json")
	if err := os.WriteFile(tplFile, []byte(templates), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := e.admin("template", "apply", "--file", tplFile); strings.Count(out, "created") != 5 {
		t.Fatalf("template apply: %s", out)
	}
	if out := e.admin("template", "list"); !strings.Contains(out, "py") || !strings.Contains(out, "allowlist") {
		t.Fatalf("template list: %s", out)
	}
	hc := &http.Client{Timeout: 2 * time.Minute}
	acme := &client{t: t, base: e.base, key: keyOf("acme"), hc: hc}
	rival := &client{t: t, base: e.base, key: keyOf("rival"), hc: hc}
	anon := &client{t: t, base: e.base, hc: hc}

	// `sbxd preflight` passes on this host (warnings are fine).
	if out, err := exec.Command(filepath.Join(e.bin, "sbxd"), "preflight").CombinedOutput(); err != nil {
		t.Fatalf("sbxd preflight: %v\n%s", err, out)
	}
	// A missing agent binary is a clear start-up failure, not a pool of
	// sandboxes that never become ready.
	bad := exec.Command(filepath.Join(e.bin, "sbxd"), "serve", "--listen", "127.0.0.1:0", "--run-dir", e.runDir, "--agent-binary", "/nonexistent/sbx-agent")
	bad.Env = append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL)
	if out, err := bad.CombinedOutput(); err == nil || !strings.Contains(string(out), "agent binary") {
		t.Fatalf("serve with a missing agent binary: err=%v output=%s", err, out)
	}

	startedAt := time.Now()
	e.start()
	// Every sandbox below is created by Backend.Create with the keep-id
	// user namespace and the embedded seccomp profile; a warm pool that
	// fills is the proof that the host reaches each agent's 0600 socket.
	if log := e.logTail(400); !strings.Contains(log, "sbx-seccomp-") {
		t.Fatalf("the hardened seccomp profile is not in use:\n%s", log)
	}

	t.Run("WarmPoolFills", func(t *testing.T) {
		took := waitFor(t, "the warm pools to fill", 90*time.Second, func() bool {
			return acme.warm("py") == 3 && acme.warm("none") == 1 && acme.warm("idle") == 2 && acme.warm("life") == 1
		})
		t.Logf("MEASURED warm pools (7 sandboxes over 4 templates) full %s after sbxd start (%s after the poll began)",
			time.Since(startedAt).Round(time.Millisecond), took.Round(time.Millisecond))
		cold := e.coldCreateTimes()
		if len(cold) < 7 {
			t.Fatalf("expected at least 7 creates in the log, found %d", len(cold))
		}
		t.Logf("MEASURED cold create to WARM (container create+start+agent ready), n=%d: p50=%s p95=%s max=%s",
			len(cold), percentile(cold, 0.5).Round(time.Millisecond), percentile(cold, 0.95).Round(time.Millisecond), percentile(cold, 1).Round(time.Millisecond))
		have := e.containers(t)
		for _, id := range e.runDirs(t) {
			if have[id] != "running" {
				t.Errorf("sandbox %s has a run dir but its container is %q", id, have[id])
			}
		}
		if n := len(e.runDirs(t)); n != 7 {
			t.Errorf("%d run dirs, want 7", n)
		}
	})

	t.Run("ClaimIsFastAndPoolRefills", func(t *testing.T) {
		start := time.Now()
		sb := acme.create(map[string]any{"template_id": "py", "metadata": map[string]string{"case": "claim"}})
		took := time.Since(start)
		if took > 2*time.Second {
			t.Errorf("claim from a warm pool took %s", took)
		}
		t.Logf("MEASURED first warm claim (py): %s", took.Round(time.Microsecond))
		if acme.metric("sbx_pool_claims_waited_total") != 0 {
			t.Error("the claim had to wait although the pool was warm")
		}
		refill := waitFor(t, "the py pool to refill to 3", 60*time.Second, func() bool { return acme.warm("py") == 3 })
		t.Logf("MEASURED pool refilled to target %s after the claim", (took + refill).Round(time.Millisecond))
		acme.must(204, "DELETE", "/v1/sandboxes/"+sb.ID, nil, "")
	})

	sb := acme.create(map[string]any{"template_id": "py", "env": map[string]string{"SESSION_VAR": "from-create"}})

	t.Run("PythonOverStream", func(t *testing.T) {
		events := acme.stream(sb, map[string]any{"cmd": "python3", "args": []string{"-c", "print(6*7)"}}, nil)
		if events[0].Type != "start" || events[0].Command == nil || events[0].Command.Pid == "" {
			t.Fatalf("first event: %+v", events[0])
		}
		if out := collect(events, "stdout"); out != "42\n" {
			t.Errorf("stdout %q, want 42", out)
		}
		if end := lastEvent(events); end.Type != "exit" || end.Exit == nil || end.Exit.ExitCode != 0 || end.Exit.Reason != "exited" {
			t.Errorf("exit event: %+v", end)
		}
		// The sandbox is what the hardening says it is.
		res := acme.exec(sb, shell(`id -u; echo $SESSION_VAR; grep -E '^(Seccomp|NoNewPrivs|CapEff):' /proc/self/status; touch /probe 2>&1; echo rc=$?; cat /proc/1/comm`))
		out := string(res.Stdout)
		for _, want := range []string{"1000\n", "from-create\n", "Seccomp:\t2", "NoNewPrivs:\t1", "CapEff:\t0000000000000000", "rc=1", "sbx-agent"} {
			if !strings.Contains(out, want) {
				t.Errorf("sandbox probe lacks %q:\n%s", want, out)
			}
		}
		// A script file, written and run in the work directory, using
		// only the standard library.
		script := "import json, hashlib, sqlite3, subprocess, tempfile, os\n" +
			"d = tempfile.mkdtemp()\n" +
			"db = sqlite3.connect(os.path.join(d, 'x.db')); db.execute('create table t(v)'); db.execute('insert into t values (41)')\n" +
			"v = db.execute('select v+1 from t').fetchone()[0]\n" +
			"sub = subprocess.run(['sh', '-c', 'echo sub'], capture_output=True, text=True).stdout.strip()\n" +
			"print(json.dumps({'v': v, 'sub': sub, 'sha': hashlib.sha256(b'x').hexdigest()[:8]}))\n"
		acme.must(204, "PUT", "/v1/sandboxes/"+sb.ID+"/files?path=job.py", script, sb.AccessToken)
		res = acme.exec(sb, map[string]any{"cmd": "python3", "args": []string{"job.py"}})
		if got := strings.TrimSpace(string(res.Stdout)); got != `{"v": 42, "sub": "sub", "sha": "2d711642"}` || res.Exit.ExitCode != 0 {
			t.Errorf("script: %q stderr %q exit %+v", got, res.Stderr, res.Exit)
		}
	})

	t.Run("StderrSeparationAndExitCode", func(t *testing.T) {
		events := acme.stream(sb, map[string]any{"cmd": "python3", "args": []string{"-c",
			"import sys; print('to-out'); sys.stdout.flush(); print('to-err', file=sys.stderr); sys.exit(3)"}}, nil)
		if out, errOut := collect(events, "stdout"), collect(events, "stderr"); out != "to-out\n" || errOut != "to-err\n" {
			t.Errorf("stdout %q stderr %q", out, errOut)
		}
		if end := lastEvent(events); end.Exit == nil || end.Exit.ExitCode != 3 || end.Exit.Reason != "exited" {
			t.Errorf("exit: %+v", end)
		}
		res := acme.exec(sb, shell("kill -TERM $$"))
		if res.Exit.Reason != "killed" || res.Exit.Signal != "SIGTERM" {
			t.Errorf("signalled command: %+v", res.Exit)
		}
	})

	t.Run("PTYOverWebSocket", func(t *testing.T) {
		ctx, cancel := ctxTimeout(60 * time.Second)
		defer cancel()
		var cmd command
		acme.must(201, "POST", "/v1/sandboxes/"+sb.ID+"/commands", map[string]any{"cmd": "sh",
			"pty": map[string]int{"cols": 100, "rows": 30}, "env": map[string]string{"PS1": "$ "}}, sb.AccessToken).decode(t, &cmd)
		wsURL := "ws" + strings.TrimPrefix(e.base, "http") + "/v1/sandboxes/" + sb.ID + "/commands/" + cmd.Pid + "/ws"
		// The browser way: no headers, the token as a subprotocol.
		c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"sbx.v1", "sbx.token." + sb.AccessToken}})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		var received int64
		recv := func(ch byte) []byte {
			t.Helper()
			for {
				_, data, err := c.Read(ctx)
				if err != nil {
					t.Fatalf("ws read (waiting for channel %d): %v", ch, err)
				}
				if data[0] == chStdout {
					received += int64(len(data) - 1)
				}
				if data[0] == ch {
					return data[1:]
				}
			}
		}
		send := func(ch byte, payload []byte) {
			t.Helper()
			if err := c.Write(ctx, websocket.MessageBinary, append([]byte{ch}, payload...)); err != nil {
				t.Fatalf("ws write: %v", err)
			}
		}
		until := func(want string) string {
			t.Helper()
			var out strings.Builder
			for !strings.Contains(out.String(), want) {
				out.Write(recv(chStdout))
			}
			return out.String()
		}
		var off struct {
			Offset int64 `json:"offset"`
		}
		if err := json.Unmarshal(recv(chOffset), &off); err != nil || off.Offset != 0 {
			t.Fatalf("offset frame: %+v %v", off, err)
		}
		send(chStdin, []byte("stty size\n"))
		until("30 100")
		send(chResize, []byte(`{"cols":132,"rows":43}`))
		send(chStdin, []byte("stty size\n"))
		until("43 132")
		// A flood, interrupted with Ctrl+C; the shell must stay responsive.
		send(chStdin, []byte("yes\n"))
		until("y\r\ny\r\ny\r\ny")
		start := time.Now()
		send(chStdin, []byte{0x03})
		send(chStdin, []byte("echo alive-$((6*7))\n"))
		until("alive-42")
		t.Logf("MEASURED Ctrl+C of `yes` to the next prompt answering: %s (%d bytes of flood read)", time.Since(start).Round(time.Millisecond), received)
		send(chStdin, []byte("exit 7\n"))
		var status struct {
			Exit *exitStatus `json:"exit"`
		}
		if err := json.Unmarshal(recv(chStatus), &status); err != nil || status.Exit == nil || status.Exit.ExitCode != 7 {
			t.Fatalf("status frame: %+v %v", status.Exit, err)
		}
		if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Errorf("close after exit: %v", err)
		}
	})

	t.Run("FileRoundTrip", func(t *testing.T) {
		payload := make([]byte, 6<<20)
		_, _ = rand.Read(payload)
		sum := sha256.Sum256(payload)
		acme.must(204, "PUT", "/v1/sandboxes/"+sb.ID+"/files?path=in/data.bin", payload, sb.AccessToken)
		res := acme.exec(sb, shell("sha256sum in/data.bin | cut -d' ' -f1; cp in/data.bin out.bin"))
		if got := strings.TrimSpace(string(res.Stdout)); got != hex.EncodeToString(sum[:]) {
			t.Errorf("sha256 inside the sandbox: %q, want %x", got, sum)
		}
		r := acme.must(200, "GET", "/v1/sandboxes/"+sb.ID+"/files?path=out.bin", nil, sb.AccessToken)
		if !bytes.Equal(r.Body, payload) {
			t.Errorf("download differs: %d bytes", len(r.Body))
		}
		var list struct {
			Files []struct {
				Name  string `json:"name"`
				Size  int64  `json:"size"`
				IsDir bool   `json:"is_dir"`
			} `json:"files"`
		}
		acme.must(200, "GET", "/v1/sandboxes/"+sb.ID+"/files/list?path=.", nil, sb.AccessToken).decode(t, &list)
		names := map[string]int64{}
		for _, f := range list.Files {
			names[f.Name] = f.Size
		}
		if names["out.bin"] != int64(len(payload)) || names["job.py"] == 0 {
			t.Errorf("listing: %+v", list.Files)
		}
		// The work directory is a size-capped tmpfs: overfilling it is a
		// clean error (507, not an OOM kill), and the sandbox lives on.
		big := bytes.Repeat([]byte("x"), 70<<20) // the cap is 64 MiB and 12 MiB are used
		if r := acme.do("PUT", "/v1/sandboxes/"+sb.ID+"/files?path=too-big.bin", big, sb.AccessToken); r.Status != 507 || r.apiError().Code != "no_space" {
			t.Errorf("overfilling the workdir: %d %s", r.Status, r.Body)
		}
		acme.must(204, "DELETE", "/v1/sandboxes/"+sb.ID+"/files?path=in", nil, sb.AccessToken)
		acme.must(204, "DELETE", "/v1/sandboxes/"+sb.ID+"/files?path=out.bin", nil, sb.AccessToken)
		if res := acme.exec(sb, shell("echo still-here")); string(res.Stdout) != "still-here\n" {
			t.Errorf("after the disk-full error: %+v", res)
		}
	})

	t.Run("CommandTimeout", func(t *testing.T) {
		start := time.Now()
		res := acme.exec(sb, map[string]any{"cmd": "sleep", "args": []string{"60"}, "timeout_ms": 500})
		if res.Exit.Reason != "timeout" || time.Since(start) > 5*time.Second {
			t.Errorf("timeout: %+v after %s", res.Exit, time.Since(start))
		}
	})

	t.Run("ForkBombIsContained", func(t *testing.T) {
		victim := acme.create(map[string]any{"template_id": "py"})
		start := time.Now()
		res := acme.exec(victim, map[string]any{"cmd": "sh", "args": []string{"-c", "bomb() { bomb | bomb & }; bomb; sleep 30"}, "timeout_ms": 3000})
		if res.Exit.Reason != "timeout" {
			t.Errorf("fork bomb exit: %+v", res.Exit)
		}
		var st struct {
			PidsCurrent   int64 `json:"pids_current"`
			PidsLimitHits int64 `json:"pids_limit_hits"`
		}
		acme.must(200, "GET", "/v1/sandboxes/"+victim.ID+"/stats", nil, "").decode(t, &st)
		if st.PidsLimitHits == 0 || st.PidsCurrent > 64 {
			t.Errorf("stats after the fork bomb: %+v (want pids_limit_hits > 0 and at most 64 pids)", st)
		}
		t.Logf("fork bomb contained in %s: reason=%s pids_limit_hits=%d pids_current=%d", time.Since(start).Round(time.Millisecond), res.Exit.Reason, st.PidsLimitHits, st.PidsCurrent)
		// The per-command timeout killed the whole process group, so the
		// same sandbox is usable again; and the server kept serving.
		waitFor(t, "the bombed sandbox to run a command again", 20*time.Second, func() bool {
			r := acme.do("POST", "/v1/sandboxes/"+victim.ID+"/commands?wait=true", shell("echo recovered"), victim.AccessToken)
			var w waitResult
			_ = json.Unmarshal(r.Body, &w)
			return r.Status == 200 && string(w.Stdout) == "recovered\n"
		})
		if res := acme.exec(sb, shell("echo neighbour-ok")); string(res.Stdout) != "neighbour-ok\n" {
			t.Errorf("neighbour sandbox after the fork bomb: %+v", res)
		}
		acme.must(204, "DELETE", "/v1/sandboxes/"+victim.ID, nil, "")
	})

	t.Run("MemoryBombIsContainedAndReported", func(t *testing.T) {
		victim := acme.create(map[string]any{"template_id": "py"})
		start := time.Now()
		events := acme.stream(victim, map[string]any{"cmd": "python3", "args": []string{"-c",
			"print('allocating', flush=True)\nb = []\nwhile True:\n    b.append(bytearray(8 << 20))\n"}}, nil)
		took := time.Since(start)
		end := lastEvent(events)
		if end.Type != "error" || end.Error == nil || end.Error.Code != "sandbox_terminated" || end.Error.Details["reason"] != "oom_killed" {
			t.Fatalf("the stream of a memory bomb ended with %+v (error %+v)", end, end.Error)
		}
		if took > 15*time.Second {
			t.Errorf("the memory bomb took %s to die", took)
		}
		t.Logf("MEASURED memory bomb (256 MiB limit): killed and reported as oom_killed on the stream after %s", took.Round(time.Millisecond))
		gone, _ := acme.waitState(victim.ID, "destroyed", 30*time.Second)
		if gone.Reason != "oom_killed" {
			t.Errorf("sandbox reason %q, want oom_killed", gone.Reason)
		}
		var ev struct {
			Events []struct {
				Type    string         `json:"type"`
				ToState string         `json:"to_state"`
				Reason  string         `json:"reason"`
				Data    map[string]any `json:"data"`
			} `json:"events"`
		}
		acme.must(200, "GET", "/v1/sandboxes/"+victim.ID+"/events", nil, "").decode(t, &ev)
		var sawFailed, sawRecord bool
		for _, e := range ev.Events {
			if e.ToState == "failed" && e.Reason == "oom_killed" && e.Data["exit_code"] == float64(137) {
				sawFailed = true
			}
			if e.Type == "command_finished" && e.Data["reason"] == "sandbox_terminated" && e.Data["sandbox_reason"] == "oom_killed" {
				sawRecord = true
			}
		}
		if !sawFailed || !sawRecord {
			t.Errorf("events lack the failure (%v) or the execution record (%v): %+v", sawFailed, sawRecord, ev.Events)
		}
		r := acme.do("POST", "/v1/sandboxes/"+victim.ID+"/commands", shell("id"), victim.AccessToken)
		if r.Status != 409 || r.apiError().Details["reason"] != "oom_killed" {
			t.Errorf("command on the dead sandbox: %d %s", r.Status, r.Body)
		}
		if have := e.containers(t); have[victim.ID] != "" {
			t.Errorf("the dead sandbox's container still exists (%s)", have[victim.ID])
		}
		// The server and the neighbours are unaffected.
		if res := acme.exec(sb, shell("echo neighbour-ok")); string(res.Stdout) != "neighbour-ok\n" {
			t.Errorf("neighbour sandbox after the memory bomb: %+v", res)
		}
	})

	// probe is run inside a sandbox: it fetches a URL through whatever
	// proxy the environment configures and prints one verdict line.
	const probe = `
import sys, urllib.request, urllib.error
try:
    r = urllib.request.urlopen(sys.argv[1], timeout=8)
    print('OK', r.status, r.read(40).decode('latin1').strip())
except urllib.error.HTTPError as e:
    print('HTTP', e.code, e.headers.get('X-Egress-Reason', ''))
except Exception as e:
    print('ERR', type(e).__name__, e)
`
	fetch := func(sb sandbox, target string) string {
		t.Helper()
		res := acme.exec(sb, map[string]any{"cmd": "python3", "args": []string{"-c", probe, target}, "timeout_ms": 20000})
		return strings.TrimSpace(string(res.Stdout) + string(res.Stderr))
	}
	// direct tries a TCP connection that does not go through the proxy.
	const direct = `
import socket, sys
s = socket.socket(); s.settimeout(3)
try:
    s.connect((sys.argv[1], int(sys.argv[2]))); print('CONNECTED')
except Exception as e:
    print('ERR', type(e).__name__, e)
`

	t.Run("NetworkNoneReachesNothing", func(t *testing.T) {
		// One sandbox from the none template, and one from the allowlist
		// template that asked for the stricter mode.
		for _, body := range []map[string]any{
			{"template_id": "none"},
			{"template_id": "py", "network": map[string]any{"mode": "none"}},
		} {
			sbn := acme.create(body)
			res := acme.exec(sbn, shell(`ls /sys/class/net | tr '\n' ' '; echo; env | grep -i _proxy | wc -l; ls /run/sbx`))
			lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
			if len(lines) < 3 || strings.TrimSpace(lines[0]) != "lo" || strings.TrimSpace(lines[1]) != "0" || strings.Contains(string(res.Stdout), "egress.sock") {
				t.Errorf("%v: interfaces/proxy env/run dir: %q", body, res.Stdout)
			}
			targets := []string{"1.1.1.1:80", "169.254.169.254:80", "127.0.0.1:3128"}
			if upstream != "" {
				targets = append(targets, upstream)
			}
			for _, target := range targets {
				host, port, _ := net.SplitHostPort(target)
				cmd := map[string]any{"cmd": "nc", "args": []string{"-w", "2", "-z", host, port}, "timeout_ms": 8000}
				if body["template_id"] == "py" {
					cmd = map[string]any{"cmd": "python3", "args": []string{"-c", direct, host, port}, "timeout_ms": 8000}
				}
				res := acme.exec(sbn, cmd)
				if out := string(res.Stdout); strings.Contains(out, "CONNECTED") || (body["template_id"] == "none" && res.Exit.ExitCode == 0) {
					t.Errorf("%v: a network-none sandbox reached %s: exit %+v %q", body, target, res.Exit, out)
				}
			}
			// There is nothing to update either.
			r := acme.do("PUT", "/v1/sandboxes/"+sbn.ID+"/network", map[string]any{"mode": "allowlist"}, "")
			if body["template_id"] == "none" && (r.Status != 400 || r.apiError().Code != "network_not_allowed") {
				t.Errorf("none template: network update: %d %s", r.Status, r.Body)
			}
			if body["template_id"] == "py" && (r.Status != 409 || r.apiError().Code != "network_not_attached") {
				t.Errorf("mode none on an allowlist template: network update: %d %s", r.Status, r.Body)
			}
			acme.must(204, "DELETE", "/v1/sandboxes/"+sbn.ID, nil, "")
		}
	})

	t.Run("AllowlistEgressAndLivePolicyUpdate", func(t *testing.T) {
		if upstream == "" && !internet {
			t.Skip("no private upstream (Docker or SBX_E2E_UPSTREAM) and no internet: nothing to allow")
		}
		policy := map[string]any{"mode": "allowlist"}
		if upstream != "" {
			// Also "allow" the metadata address and loopback: the built-in
			// deny set must win over the sandbox's own policy.
			policy["allow_cidrs"] = []string{upstreamIP + "/32", "169.254.169.254/32", "127.0.0.0/8"}
		}
		if internet {
			policy["allow_domains"] = []string{"example.com"}
		}
		net1 := acme.create(map[string]any{"template_id": "py", "network": policy})
		path := "/v1/sandboxes/" + net1.ID + "/network"

		if upstream != "" {
			if got := fetch(net1, "http://"+upstream+"/"); got != "OK 200 upstream-ok" {
				t.Errorf("allowed private upstream through the proxy: %q", got)
			}
			// The proxy is the only way: a direct connection has no route.
			host, port, _ := net.SplitHostPort(upstream)
			if res := acme.exec(net1, map[string]any{"cmd": "python3", "args": []string{"-c", direct, host, port}}); strings.Contains(string(res.Stdout), "CONNECTED") {
				t.Errorf("direct connection to the upstream bypassed the proxy: %q", res.Stdout)
			}
		}
		if internet {
			if got := fetch(net1, "http://example.com/"); !strings.HasPrefix(got, "OK 200") {
				t.Errorf("allowlisted public host over HTTP: %q", got)
			}
			if got := fetch(net1, "https://example.com/"); !strings.HasPrefix(got, "OK 200") {
				t.Errorf("allowlisted public host over HTTPS (CONNECT): %q", got)
			}
		}
		for target, reason := range map[string]string{
			"http://example.org/":                               "host_not_allowed",
			"http://1.1.1.1/":                                   "ip_not_allowed",
			"http://169.254.169.254/latest/":                    "denied_address", // allowed by the policy, denied by the built-in set
			"http://127.0.0.1:" + portOf(e.listen) + "/healthz": "",               // the API itself, on host loopback
		} {
			got := fetch(net1, target)
			if strings.HasPrefix(target, "http://127.0.0.1") {
				// NO_PROXY covers loopback, so this goes direct, to the
				// sandbox's own empty loopback, and must not reach the host.
				if strings.HasPrefix(got, "OK") {
					t.Errorf("the sandbox reached the host's loopback API: %q", got)
				}
				continue
			}
			if got != "HTTP 403 "+reason {
				t.Errorf("%s: %q, want HTTP 403 %s", target, got, reason)
			}
		}
		// Host loopback through the proxy is refused as well.
		res := acme.exec(net1, shell(`http_proxy=http://127.0.0.1:3128 no_proxy= wget -q -O- -T 5 http://127.0.0.1:`+portOf(e.listen)+`/healthz 2>&1; echo rc=$?`))
		if out := string(res.Stdout); !strings.Contains(out, "403") || strings.Contains(out, `"status"`) {
			t.Errorf("host loopback through the proxy: %q", out)
		}

		// Live update: open one more destination, then lock down, then reopen.
		newPolicy := map[string]any{"mode": "allowlist", "allow_cidrs": []string{"1.1.1.1/32"}}
		if upstream != "" {
			newPolicy["allow_cidrs"] = []string{"1.1.1.1/32", upstreamIP + "/32"}
		}
		acme.must(200, "PUT", path, newPolicy, "")
		if internet {
			if got := fetch(net1, "http://1.1.1.1/"); strings.HasPrefix(got, "HTTP 403") {
				t.Errorf("after allowing 1.1.1.1: %q", got)
			}
			if got := fetch(net1, "http://example.com/"); got != "HTTP 403 host_not_allowed" {
				t.Errorf("example.com after it was removed from the policy: %q", got)
			}
		}
		acme.must(200, "PUT", path, map[string]any{"mode": "none"}, "")
		target := "http://1.1.1.1/"
		if upstream != "" {
			target = "http://" + upstream + "/"
		}
		if got := fetch(net1, target); got != "HTTP 403 mode_none" {
			t.Errorf("after the lock-down: %q, want HTTP 403 mode_none", got)
		}
		if upstream != "" {
			acme.must(200, "PUT", path, map[string]any{"mode": "allowlist", "allow_cidrs": []string{upstreamIP + "/32"}}, "")
			if got := fetch(net1, "http://"+upstream+"/"); got != "OK 200 upstream-ok" {
				t.Errorf("after re-opening: %q", got)
			}
		}
		if r := acme.do("PUT", path, map[string]any{"mode": "full"}, ""); r.Status != 400 {
			t.Errorf("escalating past the template's mode: %d %s", r.Status, r.Body)
		}
		if allowed, denied := acme.metric(`sbx_egress_requests_total{verdict="allowed"}`), acme.metric(`sbx_egress_requests_total{verdict="denied"}`); allowed < 1 || denied < 4 {
			t.Errorf("egress metrics: allowed=%v denied=%v", allowed, denied)
		}
		// Deleting the sandbox removes its proxy socket with its run dir.
		acme.must(204, "DELETE", "/v1/sandboxes/"+net1.ID, nil, "")
		if _, err := os.Stat(filepath.Join(e.runDir, net1.ID)); !os.IsNotExist(err) {
			t.Errorf("run dir of the deleted sandbox: %v", err)
		}
	})

	t.Run("SecondTenantCannotTouchIt", func(t *testing.T) {
		for _, c := range []struct{ method, path string }{
			{"GET", "/v1/sandboxes/" + sb.ID},
			{"DELETE", "/v1/sandboxes/" + sb.ID},
			{"GET", "/v1/sandboxes/" + sb.ID + "/events"},
			{"POST", "/v1/sandboxes/" + sb.ID + "/commands"},
			{"GET", "/v1/sandboxes/" + sb.ID + "/files?path=job.py"},
			{"POST", "/v1/sandboxes/" + sb.ID + "/timeout"},
		} {
			var body any
			if c.method == "POST" {
				body = shell("id")
			}
			if r := rival.do(c.method, c.path, body, ""); r.Status != 404 {
				t.Errorf("rival %s %s: %d %s, want 404", c.method, c.path, r.Status, r.Body)
			}
		}
		theirs := rival.create(map[string]any{"template_id": "none"})
		if r := rival.do("GET", "/v1/sandboxes/"+sb.ID+"/commands", nil, theirs.AccessToken); r.Status != 401 {
			t.Errorf("another sandbox's token: %d %s, want 401", r.Status, r.Body)
		}
		if r := anon.do("GET", "/v1/sandboxes/"+sb.ID, nil, ""); r.Status != 401 {
			t.Errorf("no credentials: %d", r.Status)
		}
		var list struct {
			Sandboxes []sandbox `json:"sandboxes"`
		}
		rival.must(200, "GET", "/v1/sandboxes", nil, "").decode(t, &list)
		if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != theirs.ID {
			t.Errorf("rival's listing: %+v", list.Sandboxes)
		}
		rival.must(204, "DELETE", "/v1/sandboxes/"+theirs.ID, nil, "")
		if got := acme.get(sb.ID); got.State != "running" {
			t.Errorf("acme's sandbox after rival's attempts: %+v", got)
		}
	})

	t.Run("DeleteDestroysContainerAndRunDir", func(t *testing.T) {
		victim := acme.create(map[string]any{"template_id": "py", "network": map[string]any{"mode": "allowlist"}})
		acme.must(201, "POST", "/v1/sandboxes/"+victim.ID+"/commands", map[string]any{"cmd": "sleep", "args": []string{"600"}}, victim.AccessToken)
		if have := e.containers(t); have[victim.ID] != "running" {
			t.Fatalf("container of %s before delete: %q", victim.ID, have[victim.ID])
		}
		for _, sock := range []string{"agent.sock", "egress.sock"} {
			if _, err := os.Stat(filepath.Join(e.runDir, victim.ID, sock)); err != nil {
				t.Errorf("before delete: %v", err)
			}
		}
		start := time.Now()
		acme.must(204, "DELETE", "/v1/sandboxes/"+victim.ID, nil, "")
		t.Logf("MEASURED delete (SIGKILL, remove, detach egress, remove run dir): %s", time.Since(start).Round(time.Millisecond))
		// Delete is synchronous: nothing is left by the time it returns.
		if out := run(t, "podman", "ps", "-a", "--format", "{{.Names}}"); strings.Contains(out, victim.ID) {
			t.Errorf("podman ps -a still shows %s:\n%s", victim.ID, out)
		}
		if _, err := os.Stat(filepath.Join(e.runDir, victim.ID)); !os.IsNotExist(err) {
			t.Errorf("run dir after delete: %v", err)
		}
		if got := acme.get(victim.ID); got.State != "destroyed" || got.Reason != "released" {
			t.Errorf("after delete: %+v", got)
		}
	})

	t.Run("IdleTimeout", func(t *testing.T) {
		idle := acme.create(map[string]any{"template_id": "idle"})
		busy := acme.create(map[string]any{"template_id": "idle"})
		start := time.Now()
		// Keep one of the two active for more than two idle periods.
		for time.Since(start) < 7*time.Second {
			if res := acme.exec(busy, shell("true")); res.Exit.ExitCode != 0 {
				t.Fatalf("busy sandbox: %+v", res.Exit)
			}
			time.Sleep(400 * time.Millisecond)
		}
		if got := acme.get(busy.ID); got.State != "running" {
			t.Errorf("the active sandbox was terminated: %+v", got)
		}
		gone, _ := acme.waitState(idle.ID, "destroyed", 20*time.Second)
		if gone.Reason != "idle_timeout" {
			t.Errorf("idle sandbox reason %q, want idle_timeout", gone.Reason)
		}
		stop := time.Now()
		gone, took := acme.waitState(busy.ID, "destroyed", 20*time.Second)
		if gone.Reason != "idle_timeout" || time.Since(stop) < 2*time.Second {
			t.Errorf("once-active sandbox: reason %q after %s of idleness (idle timeout 3s)", gone.Reason, took)
		}
		t.Logf("MEASURED idle timeout (3s): terminated %s after the last activity", time.Since(stop).Round(100*time.Millisecond))
	})

	t.Run("MaxLifetime", func(t *testing.T) {
		start := time.Now()
		short := acme.create(map[string]any{"template_id": "life"})
		// Activity does not extend the lifetime.
		for time.Since(start) < 3*time.Second {
			acme.exec(short, shell("true"))
			time.Sleep(300 * time.Millisecond)
		}
		// Neither can the timeout endpoint, past the template's limit.
		var v sandbox
		acme.must(200, "POST", "/v1/sandboxes/"+short.ID+"/timeout", map[string]any{"timeout_ms": 3_600_000}, "").decode(t, &v)
		if v.ExpiresAt == nil || v.StartedAt == nil || v.ExpiresAt.Sub(*v.StartedAt) != 4*time.Second {
			t.Errorf("timeout past the template limit: started %v expires %v", v.StartedAt, v.ExpiresAt)
		}
		gone, _ := acme.waitState(short.ID, "destroyed", 20*time.Second)
		if lived := time.Since(start); gone.Reason != "max_lifetime" || lived < 4*time.Second || lived > 12*time.Second {
			t.Errorf("reason %q after %s, want max_lifetime after about 4s", gone.Reason, lived)
		}
		// A create-time override shortens it; the timeout endpoint extends within the limit.
		py := acme.create(map[string]any{"template_id": "py", "timeout_ms": 2000})
		if d := py.ExpiresAt.Sub(*py.StartedAt); d != 2*time.Second {
			t.Errorf("timeout_ms override: lifetime %s", d)
		}
		acme.must(200, "POST", "/v1/sandboxes/"+py.ID+"/timeout", map[string]any{"timeout_ms": 600_000}, "")
		time.Sleep(4 * time.Second)
		if got := acme.get(py.ID); got.State != "running" {
			t.Errorf("extended sandbox was terminated: %+v", got)
		}
		acme.must(204, "DELETE", "/v1/sandboxes/"+py.ID, nil, "")
		if r := acme.do("POST", "/v1/sandboxes", map[string]any{"template_id": "life", "timeout_ms": 60_000}, ""); r.Status != 400 || r.apiError().Code != "invalid_timeout" {
			t.Errorf("timeout_ms above the template's max lifetime: %d %s", r.Status, r.Body)
		}
	})

	t.Run("Latency", func(t *testing.T) {
		// Fill a pool of 34 small sandboxes, then claim 30 back to back so
		// that every claim is served warm.
		e.admin("template", "apply", "--id", "bench", "--image", imgAlpine, "--cpus", "0.5", "--memory", "64m", "--pids", "32",
			"--workdir-size", "16m", "--tmp-size", "8m", "--network", "none", "--warm-target", "34", "--max-total", "40", "--refill-rate", "4")
		fill := waitFor(t, "the bench pool to fill to 34", 3*time.Minute, func() bool { return acme.warm("bench") >= 34 })
		t.Logf("MEASURED bench pool: 34 sandboxes warm in %s at refill rate 4 (%s per sandbox)", fill.Round(time.Millisecond), (fill / 34).Round(time.Millisecond))
		waitedBefore := acme.metric("sbx_pool_claims_waited_total")
		e.admin("template", "apply", "--id", "bench", "--image", imgAlpine, "--cpus", "0.5", "--memory", "64m", "--pids", "32",
			"--workdir-size", "16m", "--tmp-size", "8m", "--network", "none", "--warm-target", "0", "--max-total", "40", "--refill-rate", "4")
		time.Sleep(1500 * time.Millisecond) // let a refill pass read the new target, so claims do not compete with creates

		const n = 30
		var claims, firstByte, execOnly []time.Duration
		var claimed []sandbox
		for i := 0; i < n; i++ {
			start := time.Now()
			s := acme.create(map[string]any{"template_id": "bench"})
			claims = append(claims, time.Since(start))
			var first time.Time
			cmdStart := time.Now()
			events := acme.stream(s, map[string]any{"cmd": "echo", "args": []string{"hi"}}, &first)
			if collect(events, "stdout") != "hi\n" || first.IsZero() {
				t.Fatalf("claim %d: events %+v", i, events)
			}
			firstByte = append(firstByte, first.Sub(start))
			execOnly = append(execOnly, first.Sub(cmdStart))
			claimed = append(claimed, s)
		}
		if waited := acme.metric("sbx_pool_claims_waited_total") - waitedBefore; waited != 0 {
			t.Errorf("%v of the %d claims had to wait for a create; the latencies below are not all warm", waited, n)
		}
		rnd := func(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }
		t.Logf("MEASURED warm claim latency (POST /v1/sandboxes, n=%d, sequential, localhost): p50=%s p95=%s min=%s max=%s",
			n, rnd(percentile(claims, 0.5)), rnd(percentile(claims, 0.95)), rnd(percentile(claims, 0)), rnd(percentile(claims, 1)))
		t.Logf("MEASURED claim to first output byte (create + `echo hi` over ?wait=stream, n=%d): p50=%s p95=%s max=%s",
			n, rnd(percentile(firstByte, 0.5)), rnd(percentile(firstByte, 0.95)), rnd(percentile(firstByte, 1)))
		t.Logf("MEASURED command start to first output byte alone (n=%d): p50=%s p95=%s", n, rnd(percentile(execOnly, 0.5)), rnd(percentile(execOnly, 0.95)))
		for _, s := range claimed {
			acme.must(204, "DELETE", "/v1/sandboxes/"+s.ID, nil, "")
		}
		// The same with the egress proxy in the path: an allowlist claim
		// also creates the proxy socket and starts the agent's bridge.
		var proxied []time.Duration
		for i := 0; i < 10; i++ {
			waitFor(t, "a warm py sandbox", 60*time.Second, func() bool { return acme.warm("py") >= 1 })
			start := time.Now()
			s := acme.create(map[string]any{"template_id": "py", "network": map[string]any{"mode": "allowlist", "allow_domains": []string{"example.com"}}})
			proxied = append(proxied, time.Since(start))
			acme.must(204, "DELETE", "/v1/sandboxes/"+s.ID, nil, "")
		}
		t.Logf("MEASURED warm claim latency with egress attach (py, allowlist, n=%d): p50=%s p95=%s max=%s",
			len(proxied), rnd(percentile(proxied, 0.5)), rnd(percentile(proxied, 0.95)), rnd(percentile(proxied, 1)))
		// Quota: the tenant may hold 40; rival holds 5.
		var held []sandbox
		for i := 0; i < 4; i++ { // 4 warm bench sandboxes are left
			held = append(held, rival.create(map[string]any{"template_id": "bench"}))
		}
		held = append(held, rival.create(map[string]any{"template_id": "none"}))
		if r := rival.do("POST", "/v1/sandboxes", map[string]any{"template_id": "py"}, ""); r.Status != 429 || r.apiError().Code != "quota_exceeded" || r.Header.Get("Retry-After") == "" {
			t.Errorf("sixth sandbox of a tenant limited to 5: %d %s", r.Status, r.Body)
		}
		for _, s := range held {
			rival.must(204, "DELETE", "/v1/sandboxes/"+s.ID, nil, "")
		}
	})

	t.Run("KillAndRestartLeavesNoOrphans", func(t *testing.T) {
		waitFor(t, "the pools to be full before the kill", 60*time.Second, func() bool {
			return acme.warm("py") == 3 && acme.warm("none") == 1 && acme.warm("idle") == 2 && acme.warm("life") == 1
		})
		// State at the moment of the crash: the long-lived session sb with
		// a running command, a second session, and the warm pools.
		acme.must(201, "POST", "/v1/sandboxes/"+sb.ID+"/commands", map[string]any{"cmd": "sleep", "args": []string{"600"}}, sb.AccessToken)
		other := acme.create(map[string]any{"template_id": "py", "network": map[string]any{"mode": "allowlist"}})
		waitFor(t, "the py pool to refill", 60*time.Second, func() bool { return acme.warm("py") == 3 })
		before := e.containers(t)
		dirsBefore := e.runDirs(t)
		warmBefore := map[string]bool{}
		for _, id := range dirsBefore {
			if id != sb.ID && id != other.ID {
				warmBefore[id] = true
			}
		}
		if len(warmBefore) != 7 || before[sb.ID] != "running" || before[other.ID] != "running" {
			t.Fatalf("before the kill: %d warm run dirs, containers %v", len(warmBefore), before)
		}

		if err := e.stop(syscall.SIGKILL); err == nil {
			t.Fatal("sbxd exited cleanly from SIGKILL?")
		}
		// Nothing cleaned up: the containers outlive the server.
		after := e.containers(t)
		for _, id := range dirsBefore {
			if after[id] != "running" {
				t.Fatalf("container of %s is %q right after the server was killed", id, after[id])
			}
		}
		// While the server is down, lose one warm sandbox and plant an
		// orphan: a managed container no row accounts for.
		var lost string
		for id := range warmBefore {
			lost = id
			break
		}
		run(t, "podman", "rm", "-f", "-t", "0", "sbx-"+lost)
		orphan := "sb-" + randHex(8)
		run(t, "podman", "run", "-d", "--name", "sbx-"+orphan, "--network", "none", "--label", "sbx.managed=true",
			"--label", "sbx.sandbox_id="+orphan, "--label", "sbx.generation=0", imgAlpine, "sleep", "600")
		if err := os.Mkdir(filepath.Join(e.runDir, orphan), 0o700); err != nil {
			t.Fatal(err)
		}

		restart := time.Now()
		e.start()
		t.Logf("MEASURED restart after SIGKILL: ready (start-up reconcile done) in %s", time.Since(restart).Round(time.Millisecond))
		// Sessions of the dead process are ended with a reason...
		for _, id := range []string{sb.ID, other.ID} {
			got, _ := acme.waitState(id, "destroyed", 30*time.Second)
			if got.Reason != "host_drain" {
				t.Errorf("session %s after the restart: reason %q, want host_drain", id, got.Reason)
			}
		}
		// ...warm sandboxes are re-adopted, the lost one is replaced...
		waitFor(t, "the pools to be full again", 90*time.Second, func() bool {
			return acme.warm("py") == 3 && acme.warm("none") == 1 && acme.warm("idle") == 2 && acme.warm("life") == 1
		})
		adopted := acme.metric("sbx_pool_adopted_total")
		t.Logf("after restart: %v warm sandboxes re-adopted, %v orphans destroyed, %v instances lost",
			adopted, acme.metric("sbx_pool_orphans_destroyed_total"), acme.metric("sbx_pool_instances_lost_total"))
		if adopted != 6 {
			t.Errorf("re-adopted %v warm sandboxes, want 6 (7 warm, 1 removed while down)", adopted)
		}
		// ...and there are no orphans: every container is a warm sandbox
		// with a run dir, and every run dir has a running container.
		var have map[string]string
		var dirs []string
		waitFor(t, "containers and run dirs to converge", 30*time.Second, func() bool {
			have, dirs = e.containers(t), e.runDirs(t)
			if len(dirs) != 7 || have[orphan] != "" || have[sb.ID] != "" || have[other.ID] != "" || have[lost] != "" {
				return false
			}
			for _, id := range dirs {
				if have[id] != "running" {
					return false
				}
			}
			return true
		})
		kept := 0
		for _, id := range dirs {
			if warmBefore[id] {
				kept++
			}
			if id == orphan || id == lost || id == sb.ID || id == other.ID {
				t.Errorf("run dir of %s survived the restart", id)
			}
		}
		if kept != 6 {
			t.Errorf("%d of the 7 run dirs are sandboxes from before the kill, want 6", kept)
		}
		// A re-adopted sandbox serves a claim, including egress attach.
		again := acme.create(map[string]any{"template_id": "py", "network": map[string]any{"mode": "allowlist"}})
		if !warmBefore[again.ID] {
			t.Errorf("the claim after the restart got %s, which is not a re-adopted sandbox", again.ID)
		}
		if res := acme.exec(again, map[string]any{"cmd": "python3", "args": []string{"-c", "print(6*7)"}}); string(res.Stdout) != "42\n" {
			t.Errorf("command in a re-adopted sandbox: %+v", res)
		}
		sb = again // the next subtest's session
	})

	t.Run("GracefulShutdownDrains", func(t *testing.T) {
		waitFor(t, "the py pool to refill", 60*time.Second, func() bool { return acme.warm("py") == 3 })
		// A stream is open when SIGTERM arrives.
		resp, err := acme.request("POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=stream", shell("echo up; sleep 600"), sb.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		var ev streamEvent
		for ev.Type != "stdout" {
			if err := dec.Decode(&ev); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		if err := e.stop(syscall.SIGTERM); err != nil {
			t.Fatalf("sbxd after SIGTERM: %v\n%s", err, e.logTail(20))
		}
		t.Logf("MEASURED graceful shutdown with 1 session and 7 warm sandboxes: %s", time.Since(start).Round(time.Millisecond))
		for ev.Type != "error" {
			if err := dec.Decode(&ev); err != nil {
				t.Fatalf("the open stream ended without an error event: %v", err)
			}
		}
		if ev.Error == nil || ev.Error.Code != "shutting_down" {
			t.Errorf("stream on shutdown: %+v", ev.Error)
		}
		// Policy "drain": the session's container is gone, warm ones stay.
		have, dirs := e.containers(t), e.runDirs(t)
		if have[sb.ID] != "" {
			t.Errorf("the session's container survived a draining shutdown (%s)", have[sb.ID])
		}
		if len(dirs) != 7 {
			t.Errorf("%d run dirs after the shutdown, want the 7 warm ones", len(dirs))
		}
		for _, id := range dirs {
			if have[id] != "running" {
				t.Errorf("warm sandbox %s is %q after the shutdown", id, have[id])
			}
		}
		// The next start adopts them; deleting the templates then empties
		// the host, which is also this test's cleanup.
		e.start()
		if got := acme.get(sb.ID); got.State != "destroyed" || got.Reason != "host_drain" {
			t.Errorf("the drained session: %+v", got)
		}
		waitFor(t, "the warm sandboxes to be re-adopted", 30*time.Second, func() bool { return acme.metric("sbx_pool_adopted_total") == 7 })
		for _, id := range []string{"py", "none", "idle", "life", "bench"} {
			e.admin("template", "delete", "--id", id)
		}
		waitFor(t, "every sandbox to be recycled after its template was deleted", 60*time.Second, func() bool {
			have := e.containers(t)
			for _, id := range dirs {
				if have[id] != "" {
					return false
				}
			}
			return len(e.runDirs(t)) == 0
		})
		if err := e.stop(syscall.SIGTERM); err != nil {
			t.Fatalf("final stop: %v", err)
		}
		if out := run(t, "podman", "ps", "-a", "--filter", "label=sbx.managed=true", "--format", "{{.Names}}"); strings.TrimSpace(out) != "" {
			t.Logf("managed containers still on the host (not from this run unless listed above): %s", strings.TrimSpace(out))
		}
	})
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}
