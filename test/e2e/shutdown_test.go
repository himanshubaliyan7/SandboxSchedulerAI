package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// activeRows returns the host's sandbox rows that are not terminal, by id.
func activeRows(t *testing.T, dbURL, hostID string) map[string]core.Sandbox {
	t.Helper()
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.ListActiveByHost(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]core.Sandbox{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestShutdownPolicies stops the real server with SIGTERM under each
// shutdown policy, at the worst moment (tenant sandboxes exist and the
// refill triggered by their claims is still creating containers), and
// checks that what is left on the host is exactly what the policy
// promises: no container without a row, no row without a container, and
// no container that is not a whole, running sandbox.
func TestShutdownPolicies(t *testing.T) {
	if os.Getenv("SBX_E2E") != "1" {
		t.Skip("set SBX_E2E=1 (and SBX_E2E_DATABASE_URL) to run the end-to-end tests")
	}
	adminURL := os.Getenv("SBX_E2E_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("SBX_E2E_DATABASE_URL is not set")
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if _, err := os.Stat(filepath.Join(xdg, "podman", "podman.sock")); xdg == "" || err != nil {
		t.Fatalf("no Podman socket under XDG_RUNTIME_DIR=%q: %v", xdg, err)
	}
	if err := exec.Command("podman", "image", "exists", imgAlpine).Run(); err != nil {
		t.Fatalf("image %s is not pulled for this user", imgAlpine)
	}
	bin := buildBinaries(t)
	const warm, claimed = 3, 2

	for _, policy := range []string{"drain", "leave", "destroy"} {
		t.Run(policy, func(t *testing.T) {
			id := randHex(4)
			e := &env{t: t, bin: bin, dbURL: newDatabase(t, adminURL, "sbx_e2e_stop_"+policy+"_"+id), hostID: "e2e-stop-" + policy + "-" + id,
				runDir: filepath.Join(xdg, "sbxe2e-s"+id), listen: freePort(t), logPath: filepath.Join(t.TempDir(), "sbxd.log"),
				extra: []string{"--shutdown-policy", policy}}
			e.base = "http://" + e.listen
			c := &client{t: t, base: e.base, hc: &http.Client{Timeout: 2 * time.Minute}}
			t.Cleanup(e.cleanup)
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(e.logTail(80))
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
				"--workdir-size", "16m", "--tmp-size", "8m", "--network", "none", "--warm-target", "3", "--max-total", "8", "--refill-rate", "3")
			e.start()
			waitFor(t, "a full pool", 60*time.Second, func() bool { return c.warm("t") == warm && len(e.containers(t)) == warm })

			// The offline destroy refuses to run next to the server.
			cmd := exec.Command(filepath.Join(bin, "sbxd"), "admin", "host", "destroy", "--run-dir", e.runDir, "--host-id", e.hostID)
			cmd.Env = append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL)
			if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "in use") {
				t.Errorf("admin host destroy next to a running server: err %v, output %q", err, out)
			}
			if got := len(e.containers(t)); got != warm {
				t.Fatalf("the refused destroy removed containers: %d left", got)
			}

			// Claims wake the refill; SIGTERM lands while it is creating.
			var tenantIDs []string
			for i := 0; i < claimed; i++ {
				tenantIDs = append(tenantIDs, c.create(map[string]any{"template_id": "t"}).ID)
			}
			if err := e.stop(syscall.SIGTERM); err != nil {
				t.Fatalf("stop: %v", err)
			}
			logText, _ := os.ReadFile(e.logPath)
			for _, bad := range []string{"sandbox create failed", "could not start create", "DATA RACE"} {
				if strings.Contains(string(logText), bad) {
					t.Errorf("the log of a graceful stop contains %q", bad)
				}
			}

			rows := activeRows(t, e.dbURL, e.hostID)
			containers := e.containers(t)
			t.Logf("policy %s: %d container(s) left %v, %d live row(s), %d run dir(s)", policy, len(containers), containers, len(rows), len(e.runDirs(t)))

			// Whatever the policy: containers, live rows and run dirs agree,
			// and every container is a whole running sandbox whose row is
			// in a resting state.
			if got, want := keysOf(containers), keysOf(rows); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("containers %v do not match the live rows %v", got, want)
			}
			if got, want := e.runDirs(t), keysOf(rows); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("run dirs %v do not match the live rows %v", got, want)
			}
			for id, state := range containers {
				if state != "running" {
					t.Errorf("container of %s is %q, not running", id, state)
				}
			}
			byState := map[core.State]int{}
			for _, r := range rows {
				byState[r.State]++
				if r.InstanceID == "" {
					t.Errorf("row %s (%s) has no instance id: a half-made sandbox", r.ID, r.State)
				}
			}
			isTenant := map[string]bool{}
			for _, id := range tenantIDs {
				isTenant[id] = true
			}
			switch policy {
			case "drain":
				// The warm pool, complete (the creates in flight were
				// awaited), and nothing else.
				if byState[core.StateWarm] != warm || len(rows) != warm {
					t.Errorf("drain left %v, want exactly %d warm", byState, warm)
				}
				for id := range rows {
					if isTenant[id] {
						t.Errorf("tenant sandbox %s survived a drain", id)
					}
				}
			case "leave":
				if byState[core.StateWarm] != warm || byState[core.StateRunning] != claimed || len(rows) != warm+claimed {
					t.Errorf("leave left %v, want %d warm and %d running", byState, warm, claimed)
				}
				for _, id := range tenantIDs {
					if _, ok := rows[id]; !ok {
						t.Errorf("tenant sandbox %s did not survive a leave", id)
					}
				}
			case "destroy":
				if len(rows) != 0 || len(containers) != 0 {
					t.Errorf("destroy left %d row(s) and %d container(s)", len(rows), len(containers))
				}
			}

			switch policy {
			case "drain":
				// The next start adopts the warm sandboxes instead of
				// recreating them.
				before := e.containerIDs(t)
				e.start()
				waitFor(t, "the pool after the restart", 60*time.Second, func() bool { return c.warm("t") == warm })
				after := e.containerIDs(t)
				for id, cid := range before {
					if after[id] != cid {
						t.Errorf("warm sandbox %s was not re-adopted (container %q, now %q)", id, cid, after[id])
					}
				}
				if got := c.metric("sbx_pool_adopted_total"); got != warm {
					t.Errorf("sbx_pool_adopted_total = %v, want %d", got, warm)
				}
				if err := e.stop(syscall.SIGTERM); err != nil {
					t.Fatalf("second stop: %v", err)
				}
			case "leave":
				// The offline destroy removes what the stopped server left.
				var rep struct {
					HostID     string `json:"host_id"`
					Containers int    `json:"containers"`
					RunDirs    int    `json:"run_dirs"`
					Rows       int    `json:"rows"`
				}
				out := e.admin("host", "destroy", "--run-dir", e.runDir, "--host-id", e.hostID, "--json")
				if err := json.Unmarshal([]byte(out), &rep); err != nil {
					t.Fatalf("report %q: %v", out, err)
				}
				if rep.HostID != e.hostID || rep.Containers != warm+claimed || rep.Rows != warm+claimed || rep.RunDirs != warm+claimed {
					t.Errorf("report: %+v", rep)
				}
				if n, m, d := len(e.containers(t)), len(activeRows(t, e.dbURL, e.hostID)), len(e.runDirs(t)); n != 0 || m != 0 || d != 0 {
					t.Errorf("after admin host destroy: %d container(s), %d live row(s), %d run dir(s)", n, m, d)
				}
				// Idempotent.
				e.admin("host", "destroy", "--run-dir", e.runDir, "--host-id", e.hostID)
			}
		})
	}
}
