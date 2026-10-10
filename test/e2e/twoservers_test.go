package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// newDatabase creates a private database next to adminURL's and returns
// its URL; the database is dropped when the test ends.
func newDatabase(t *testing.T, adminURL, name string) string {
	t.Helper()
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to SBX_E2E_DATABASE_URL: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := ctxTimeout(30 * time.Second)
		defer cancel()
		if conn, err := pgx.Connect(ctx, adminURL); err == nil {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			conn.Close(ctx)
		}
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// containerIDs returns the Podman container ids of an environment's
// sandboxes that are running, by sandbox id.
func (e *env) containerIDs(t *testing.T) map[string]string {
	t.Helper()
	out := runPodman(t, "ps", "--no-trunc", "--filter", "label=sbx.host_id="+e.hostID, "--filter", "status=running",
		"--format", `{{index .Labels "sbx.sandbox_id"}} {{.ID}}`)
	ids := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if sb, id, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			ids[sb] = id
		}
	}
	return ids
}

// TestTwoServersShareOnePodman is the proof that ownership is scoped by
// host id: two real sbxd processes, each with its own database, run dir
// and host id, run at the same time as the same user against the same
// Podman socket. Before the sbx.host_id label existed each of them
// destroyed the other's containers as orphans within one reconcile
// interval. Now each fills and keeps its own pool, reaps its own orphan,
// survives the other's crash and restart, and never touches a container
// of the other.
//
// The second server also runs with --metrics-listen and --metrics-token,
// so the test reads its metrics the way an operator would.
func TestTwoServersShareOnePodman(t *testing.T) {
	if os.Getenv("SBX_E2E") != "1" {
		t.Skip("set SBX_E2E=1 (and SBX_E2E_DATABASE_URL) to run the end-to-end tests")
	}
	adminURL := os.Getenv("SBX_E2E_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("SBX_E2E_DATABASE_URL is not set")
	}
	xdg := e2eSetup(t)
	if err := podmanCLI("image", "exists", imgAlpine).Run(); err != nil {
		t.Fatalf("image %s is not pulled for this user", imgAlpine)
	}
	bin := buildBinaries(t)
	hc := &http.Client{Timeout: 2 * time.Minute}
	const metricsToken = "e2e-metrics-token"
	const warm = 3

	type side struct {
		*env
		c *client
	}
	mk := func(name string, separateMetrics bool) *side {
		id := randHex(4)
		e := &env{t: t, bin: bin, dbURL: newDatabase(t, adminURL, "sbx_e2e_two_"+name+"_"+id), hostID: "e2e-two-" + name + "-" + id,
			runDir: filepath.Join(xdg, "sbxe2e-"+name+id), listen: freePort(t), logPath: filepath.Join(t.TempDir(), "sbxd-"+name+".log")}
		e.base = "http://" + e.listen
		c := &client{t: t, base: e.base, hc: hc}
		if separateMetrics {
			addr := freePort(t)
			e.extra = append(e.extra, "--metrics-listen", addr, "--metrics-token", metricsToken)
			c.metricsBase, c.metricsToken = "http://"+addr, metricsToken
		}
		t.Cleanup(e.cleanup)
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(e.logTail(60))
			}
		})
		e.admin("tenant", "create", "--id", "acme", "--max-concurrent", "10", "--rate", "2000", "--burst", "4000")
		var k struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(e.admin("key", "create", "--tenant", "acme", "--json")), &k); err != nil {
			t.Fatal(err)
		}
		c.key = k.Key
		e.admin("template", "apply", "--id", "t", "--image", imgAlpine, "--cpus", "0.5", "--memory", "64m", "--pids", "32",
			"--workdir-size", "16m", "--tmp-size", "8m", "--network", "none", "--warm-target", "3", "--max-total", "6", "--refill-rate", "3")
		return &side{e, c}
	}
	a, b := mk("a", false), mk("b", true)

	full := func(s *side) func() bool {
		return func() bool { return s.c.warm("t") == warm && len(s.containerIDs(t)) == warm }
	}
	// Same Podman user, same socket, at the same time. B starts while A's
	// pool is already full, so B's start-up reconcile lists A's containers.
	a.start()
	waitFor(t, "server A's pool to fill", 90*time.Second, full(a))
	aIDs := a.containerIDs(t)
	b.start()
	waitFor(t, "server B's pool to fill", 90*time.Second, full(b))
	bIDs := b.containerIDs(t)
	t.Logf("server A (host id %s): %d containers; server B (host id %s): %d containers", a.hostID, len(aIDs), b.hostID, len(bIDs))

	same := func(what string, s *side, want map[string]string) {
		t.Helper()
		got := s.containerIDs(t)
		for sb, id := range want {
			if got[sb] != id {
				t.Errorf("%s: container of sandbox %s (server %s) was %s and is now %q", what, sb, s.hostID, id[:12], got[sb])
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: server %s has %d running containers, want %d", what, s.hostID, len(got), len(want))
		}
	}

	// Item 7 on the real binary: B's metrics are only on the metrics
	// listener and only with the token; A's are where they always were.
	if r := b.c.do("GET", "/metrics", nil, ""); r.Status != 404 {
		t.Errorf("server B serves /metrics on its API listener: %d", r.Status)
	}
	if resp, err := hc.Get(b.c.metricsBase + "/metrics"); err != nil {
		t.Error(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("server B's metrics listener without the token: %d", resp.StatusCode)
		}
	}
	if resp, err := hc.Get(b.c.metricsBase + "/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("server B's metrics listener serves /healthz: %d", resp.StatusCode)
		}
	}
	if r := a.c.do("GET", "/metrics", nil, ""); r.Status != 200 {
		t.Errorf("server A's /metrics: %d", r.Status)
	}

	// Each server gets an orphan of its own to reap. Both reconcile every
	// 2 s; give them several passes over each other's containers.
	orphans := map[*side]string{}
	for _, s := range []*side{a, b} {
		id := "sb-" + randHex(8)
		orphans[s] = id
		runPodman(t, "run", "-d", "--name", "sbx-"+id, "--network", "none", "--label", "sbx.managed=true",
			"--label", "sbx.host_id="+s.hostID, "--label", "sbx.sandbox_id="+id, "--label", "sbx.generation=0", imgAlpine, "sleep", "600")
	}
	waitFor(t, "each server to reap its own orphan", 30*time.Second, func() bool {
		return a.c.metric("sbx_pool_orphans_destroyed_total") == 1 && b.c.metric("sbx_pool_orphans_destroyed_total") == 1
	})
	time.Sleep(7 * time.Second)
	for s, id := range orphans {
		if err := podmanCLI("container", "exists", "sbx-"+id).Run(); err == nil {
			t.Errorf("server %s did not remove its orphan %s", s.hostID, id)
		}
	}
	same("after several reconcile passes", a, aIDs)
	same("after several reconcile passes", b, bIDs)
	for _, s := range []*side{a, b} {
		if n := s.c.metric("sbx_pool_orphans_destroyed_total"); n != 1 {
			t.Errorf("server %s destroyed %v orphans, want only its own 1", s.hostID, n)
		}
		if n := s.c.metric("sbx_pool_instances_lost_total"); n != 0 {
			t.Errorf("server %s lost %v instances: somebody removed its containers", s.hostID, n)
		}
	}

	// Both serve claims; the claimed sandboxes run commands.
	for _, s := range []*side{a, b} {
		sb := s.c.create(map[string]any{"template_id": "t"})
		if res := s.c.exec(sb, shell("echo claimed-ok")); string(res.Stdout) != "claimed-ok\n" {
			t.Errorf("server %s: command in a claimed sandbox: %+v %q", s.hostID, res.Exit, res.Stdout)
		}
		s.c.must(204, "DELETE", "/v1/sandboxes/"+sb.ID+"?wait=true", nil, "")
		waitFor(t, "the pool of "+s.hostID+" to refill", 60*time.Second, full(s))
	}
	aIDs, bIDs = a.containerIDs(t), b.containerIDs(t)

	// A second server on A's run dir is refused instead of fighting over it.
	dup := exec.Command(filepath.Join(bin, "sbxd"), "serve", "--listen", "127.0.0.1:0", "--run-dir", a.runDir, "--skip-preflight")
	dup.Env = append(os.Environ(), "SBX_DATABASE_URL="+a.dbURL)
	if out, err := dup.CombinedOutput(); err == nil || !strings.Contains(string(out), "in use by another sbxd") {
		t.Errorf("a second server on the same run dir: err=%v output=%s", err, out)
	}

	// A crashes and comes back with a new agent epoch: its start-up
	// reconcile re-adopts its own warm sandboxes and leaves B's alone. B
	// keeps reconciling while A is down and does not touch A's either.
	if err := a.stop(syscall.SIGKILL); err == nil {
		t.Fatal("sbxd exited cleanly from SIGKILL?")
	}
	time.Sleep(5 * time.Second) // more than two of B's reconcile passes
	same("while server A is down", a, aIDs)
	same("while server A is down", b, bIDs)
	a.start()
	waitFor(t, "server A to re-adopt its warm sandboxes", 30*time.Second, func() bool { return a.c.metric("sbx_pool_adopted_total") == warm })
	time.Sleep(5 * time.Second)
	same("after server A restarted", a, aIDs)
	same("after server A restarted", b, bIDs)
	if n := a.c.metric("sbx_pool_orphans_destroyed_total"); n != 0 {
		t.Errorf("restarted server A destroyed %v orphans, want 0", n)
	}
	if n := b.c.metric("sbx_pool_instances_lost_total"); n != 0 {
		t.Errorf("server B lost %v instances across server A's restart", n)
	}
	// The debug log names the other server's containers once each.
	if log := b.logTail(1 << 30); !strings.Contains(log, "instance belongs to another host id; leaving it alone") ||
		!strings.Contains(log, "instance_host="+a.hostID) {
		t.Error("server B's debug log does not mention server A's containers")
	}

	// Clean shutdown: deleting the template drains each pool.
	for _, s := range []*side{a, b} {
		s.admin("template", "delete", "--id", "t")
	}
	for _, s := range []*side{a, b} {
		waitFor(t, "the pool of "+s.hostID+" to drain", 60*time.Second, func() bool { return len(s.containers(t)) == 0 && len(s.runDirs(t)) == 0 })
		if err := s.stop(syscall.SIGTERM); err != nil {
			t.Errorf("stop %s: %v", s.hostID, err)
		}
	}
}
