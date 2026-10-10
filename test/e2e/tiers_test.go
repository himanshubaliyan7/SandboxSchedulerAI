package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTwoTiersInOneServer runs both isolation tiers side by side in one
// sbxd: a crun template and a runsc (gVisor) template, each with its own
// warm pool. It checks that each sandbox is on the tier its template
// names (the kernel it sees, the runtime Podman used, what the API says),
// that a template for a tier the server has not enabled is refused at
// apply time and at start, and that a restart re-adopts the warm
// sandboxes of both tiers.
//
// It needs gVisor: SBX_E2E_RUNSC or runsc in PATH. SBX_E2E_RUNTIME does
// not apply to it.
func TestTwoTiersInOneServer(t *testing.T) {
	if os.Getenv("SBX_E2E") != "1" {
		t.Skip("set SBX_E2E=1 (and SBX_E2E_DATABASE_URL) to run the end-to-end tests")
	}
	adminURL := os.Getenv("SBX_E2E_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("SBX_E2E_DATABASE_URL is not set")
	}
	xdg := e2eSetup(t)
	// This test enables the tiers itself, whatever tier the suite is on.
	t.Setenv("SBX_RUNTIMES", "")
	runsc := findRunsc()
	if runsc == "" {
		t.Skip("no gVisor on this host: set SBX_E2E_RUNSC or put runsc in PATH")
	}
	if os.Getenv("SBX_PODMAN_SOCKET") != "" {
		t.Skip("SBX_E2E_PODMAN_MODE=external: two tiers need managed mode (one Podman service per runtime)")
	}
	if err := exec.Command("podman", "image", "exists", imgPython).Run(); err != nil {
		t.Fatalf("image %s is not pulled for this user", imgPython)
	}
	bin := buildBinaries(t)
	id := randHex(4)
	e := &env{t: t, bin: bin, dbURL: newDatabase(t, adminURL, "sbx_e2e_tiers_"+id), hostID: "e2e-tiers-" + id,
		runDir: filepath.Join(xdg, "sbxe2e-t"+id), listen: freePort(t), logPath: filepath.Join(t.TempDir(), "sbxd.log")}
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
	apply := func(id, runtime string) []string {
		return []string{"admin", "template", "apply", "--id", id, "--runtime", runtime, "--image", imgPython, "--cpus", "1", "--memory", "256m",
			"--pids", "64", "--workdir-size", "64m", "--tmp-size", "32m", "--network", "allowlist", "--warm-target", "2", "--max-total", "6"}
	}
	sbxd := func(env []string, args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(bin, "sbxd"), args...)
		cmd.Env = append(append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// A template for a tier that is not enabled is refused when it is
	// applied, with the same setting the server takes.
	if out, err := sbxd(nil, apply("gv", "runsc")...); err == nil || !strings.Contains(out, "has not enabled") || !strings.Contains(out, "gv (runtime runsc)") {
		t.Fatalf("applying a runsc template without --runtimes: err=%v output=%s", err, out)
	}
	both := []string{"SBX_RUNTIMES=crun,runsc=" + runsc}
	for _, tpl := range [][2]string{{"host", "crun"}, {"gv", "runsc"}} {
		if out, err := sbxd(both, apply(tpl[0], tpl[1])...); err != nil {
			t.Fatalf("template apply %v: %v\n%s", tpl, err, out)
		}
	}
	// And a server that has not enabled it does not start.
	out, err := sbxd(nil, "serve", "--listen", "127.0.0.1:0", "--run-dir", e.runDir, "--host-id", e.hostID)
	if err == nil || !strings.Contains(out, "gv (runtime runsc)") || !strings.Contains(out, "--runtimes") {
		t.Fatalf("serve without the gVisor tier and a runsc template: err=%v output=%s", err, out)
	}
	// Nor does one that is asked for two tiers on somebody else's service.
	out, err = sbxd(both, "serve", "--listen", "127.0.0.1:0", "--run-dir", e.runDir, "--host-id", e.hostID, "--podman-socket", filepath.Join(xdg, "podman", "podman.sock"))
	if err == nil || !strings.Contains(out, "needs --podman-mode managed") {
		t.Fatalf("serve with two tiers in external mode: err=%v output=%s", err, out)
	}
	// Preflight says which tiers are usable.
	out, err = sbxd(both, "preflight")
	if err != nil || !strings.Contains(out, "enabled and usable (user-space kernel") || !strings.Contains(out, "enabled (sandboxes share the host kernel)") {
		t.Fatalf("preflight with both tiers: %v\n%s", err, out)
	}

	t.Setenv("SBX_RUNTIMES", both[0][len("SBX_RUNTIMES="):])
	e.start()
	waitFor(t, "both warm pools", 90*time.Second, func() bool { return c.warm("host") == 2 && c.warm("gv") == 2 })

	var tl struct {
		Templates []struct {
			ID           string `json:"id"`
			Runtime      string `json:"runtime"`
			SharedKernel *bool  `json:"shared_kernel"`
		} `json:"templates"`
	}
	c.must(200, "GET", "/v1/templates", nil, "").decode(t, &tl)
	seen := map[string]string{}
	for _, tpl := range tl.Templates {
		if tpl.SharedKernel == nil {
			t.Fatalf("template %s has no shared_kernel", tpl.ID)
		}
		seen[tpl.ID] = tpl.Runtime + " shared_kernel=" + map[bool]string{true: "true", false: "false"}[*tpl.SharedKernel]
	}
	if seen["host"] != "crun shared_kernel=true" || seen["gv"] != "runsc shared_kernel=false" {
		t.Errorf("GET /v1/templates: %v", seen)
	}

	hostKernel := strings.TrimSpace(run(t, "uname", "-r"))
	check := func(template, runtime string) sandbox {
		t.Helper()
		sb := c.create(map[string]any{"template_id": template})
		if got := strings.TrimSpace(runPodman(t, "inspect", "-f", "{{.OCIRuntime}}", "sbx-"+sb.ID)); got != runtime {
			t.Errorf("template %s: Podman ran the sandbox with %q, want %q", template, got, runtime)
		}
		res := c.exec(sb, shell("uname -r; python3 -c 'print(6*7)'; echo $$ > /run/sbx/probe && echo run-dir-ok"))
		out := string(res.Stdout)
		t.Logf("template %s (%s): %q", template, runtime, out)
		if !strings.Contains(out, "42\n") || !strings.Contains(out, "run-dir-ok") {
			t.Errorf("template %s: %q %q", template, out, res.Stderr)
		}
		kernel, _, _ := strings.Cut(out, "\n")
		if (runtime == "runsc") != strings.Contains(kernel, "gvisor") || (runtime == "crun") != (kernel == hostKernel) {
			t.Errorf("template %s on %s sees kernel %q (host kernel %q)", template, runtime, kernel, hostKernel)
		}
		return sb
	}
	hostSB := check("host", "crun")
	gvSB := check("gv", "runsc")

	// One service per tier, and each started its tier's sandboxes: the
	// stats of both come from their own cgroups.
	for _, sb := range []sandbox{hostSB, gvSB} {
		var st struct {
			MemoryBytes int64 `json:"memory_bytes"`
		}
		c.must(200, "GET", "/v1/sandboxes/"+sb.ID+"/stats", nil, "").decode(t, &st)
		if st.MemoryBytes <= 0 {
			t.Errorf("stats of %s: %+v", sb.ID, st)
		}
	}
	for _, name := range []string{"podman.sock", "podman-runsc.sock"} {
		if fi, err := os.Stat(filepath.Join(e.runDir, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want a 0600 socket", name, fi, err)
		}
	}

	// Kill the server outright: the services go with it, the sandboxes do
	// not, and the next start re-adopts the warm ones of both tiers.
	warmBefore := e.containers(t)
	if err := e.stop(syscall.SIGKILL); err == nil {
		t.Fatal("SIGKILL: sbxd exited cleanly?")
	}
	waitFor(t, "the Podman services to end with their sbxd", 10*time.Second, func() bool {
		out, _ := exec.Command("pgrep", "-f", "podman system service --time=0 unix://"+e.runDir+"/").Output()
		return strings.TrimSpace(string(out)) == ""
	})
	for id, state := range e.containers(t) {
		if state != "running" || warmBefore[id] != "running" {
			t.Errorf("sandbox %s is %q after the server was killed", id, state)
		}
	}
	e.start()
	waitFor(t, "both pools to be warm again", 60*time.Second, func() bool { return c.warm("host") == 2 && c.warm("gv") == 2 })
	if log := e.logTail(400); strings.Count(log, "re-adopted warm sandbox") < 4 {
		t.Errorf("expected the 4 warm sandboxes (2 per tier) to be re-adopted:\n%s", log)
	}
	check("gv", "runsc")
	check("host", "crun")
}
