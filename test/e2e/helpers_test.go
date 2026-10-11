package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/promtext"
)

// env is one end-to-end environment: real binaries, a private database,
// a private run dir and one sbxd subprocess that can be killed and
// restarted.
type env struct {
	t       *testing.T
	bin     string // directory holding sbxd and sbx-agent
	dbURL   string
	runDir  string
	hostID  string
	listen  string
	base    string
	logPath string
	extra   []string // extra serve flags

	cmd  *exec.Cmd
	done chan error
}

// The suite runs once per isolation tier and Podman mode:
//
//	SBX_E2E_RUNTIME      crun (default) or runsc: the runtime of every
//	                     template the tests create
//	SBX_E2E_RUNSC        path of the gVisor runsc binary (default: PATH)
//	SBX_E2E_PODMAN_MODE  managed (default: sbxd's own Podman service) or
//	                     external (the user's socket, crun only)
//
// Every check runs on both tiers. Where the tiers are documented to
// differ, the test asserts the documented behaviour of the tier it runs
// on (look for onGVisor); nothing is skipped because of the tier.

// e2eRuntime is the OCI runtime of the templates under test.
func e2eRuntime() string {
	if r := os.Getenv("SBX_E2E_RUNTIME"); r != "" {
		return r
	}
	return "crun"
}

// onGVisor reports whether the suite runs on the gVisor tier.
func onGVisor() bool { return e2eRuntime() == "runsc" }

// findRunsc returns the gVisor binary for the tests, or "".
func findRunsc() string {
	if p := os.Getenv("SBX_E2E_RUNSC"); p != "" {
		return p
	}
	if p, err := exec.LookPath("runsc"); err == nil {
		return p
	}
	return ""
}

// e2eSetup checks the host and puts the suite's Podman settings into the
// environment, which every sbxd the tests start (serve, admin, preflight)
// inherits. It returns XDG_RUNTIME_DIR.
func e2eSetup(t *testing.T) string {
	t.Helper()
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		t.Fatal("XDG_RUNTIME_DIR is not set")
	}
	for _, k := range []string{"SBX_PODMAN_MODE", "SBX_PODMAN_SOCKET", "SBX_RUNTIMES", "SBX_RUNSC_FLAGS", "SBX_RUN_DIR", "SBX_HOST_ID"} {
		t.Setenv(k, "")
	}
	switch mode := os.Getenv("SBX_E2E_PODMAN_MODE"); mode {
	case "", "managed":
	case "external":
		socket := filepath.Join(xdg, "podman", "podman.sock")
		if _, err := os.Stat(socket); err != nil {
			t.Fatalf("SBX_E2E_PODMAN_MODE=external needs the user's Podman socket: %v", err)
		}
		if onGVisor() {
			t.Fatal("the gVisor tier needs managed mode: an external Podman service has one runtime")
		}
		t.Setenv("SBX_PODMAN_SOCKET", socket)
	default:
		t.Fatalf("SBX_E2E_PODMAN_MODE=%q: want managed or external", mode)
	}
	switch e2eRuntime() {
	case "crun":
	case "runsc":
		path := findRunsc()
		if path == "" {
			t.Fatal("SBX_E2E_RUNTIME=runsc but there is no runsc: set SBX_E2E_RUNSC or put runsc in PATH")
		}
		t.Setenv("SBX_RUNTIMES", "crun,runsc="+path)
	default:
		t.Fatalf("SBX_E2E_RUNTIME=%q: want crun or runsc", e2eRuntime())
	}
	return xdg
}

// cliConf is the containers.conf of the last server started. The podman
// CLI needs it to act on containers of the gVisor tier, whose runtime is
// not in the user's own configuration.
var cliConf string

// podmanCLI builds a podman command that sees the sandboxes of the
// servers under test with the runtime table they were created with.
func podmanCLI(args ...string) *exec.Cmd {
	cmd := exec.Command("podman", args...)
	if cliConf != "" {
		if _, err := os.Stat(cliConf); err == nil {
			cmd.Env = append(os.Environ(), "CONTAINERS_CONF_OVERRIDE="+cliConf)
		}
	}
	return cmd
}

func runPodman(t *testing.T, args ...string) string {
	t.Helper()
	// Standard output only: the podman CLI writes notices to standard
	// error that are not about this suite (with a gVisor sandbox of some
	// other server on the host, "OCI Runtime runsc is in use by a
	// container, but is not available" on every command), and callers
	// parse what this returns line by line.
	cmd := podmanCLI(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("podman %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
	}
	return string(out)
}

// withRuntime puts the suite's runtime into a JSON template list.
func withRuntime(templatesJSON string) string {
	return strings.ReplaceAll(templatesJSON, `{"id":`, `{"runtime":"`+e2eRuntime()+`", "id":`)
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func buildBinaries(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	out := t.TempDir()
	for _, b := range []struct {
		pkg string
		cgo string
	}{{"./cmd/sbxd", ""}, {"./cmd/sbx-agent", "0"}} {
		args := []string{"build", "-o", out + "/"}
		if b.cgo == "" && os.Getenv("SBX_E2E_RACE") == "1" {
			// The server under the race detector; the agent stays static.
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
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// admin runs `sbxd admin ...` against the environment's database.
func (e *env) admin(args ...string) string {
	e.t.Helper()
	if len(args) > 2 && args[0] == "template" && args[1] == "apply" && !slices.Contains(args, "--file") && !slices.Contains(args, "--runtime") {
		args = append(slices.Clone(args), "--runtime", e2eRuntime())
	}
	cmd := exec.Command(filepath.Join(e.bin, "sbxd"), append([]string{"admin"}, args...)...)
	cmd.Env = append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("sbxd admin %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// start launches sbxd and waits until it is ready.
func (e *env) start() {
	e.t.Helper()
	logf, err := os.OpenFile(e.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	args := append([]string{"serve", "--listen", e.listen, "--run-dir", e.runDir, "--host-id", e.hostID,
		"--log-level", "debug", "--touch-interval", "300ms", "--reconcile-interval", "2s", "--claim-wait", "20s"}, e.extra...)
	e.cmd = exec.Command(filepath.Join(e.bin, "sbxd"), args...)
	cliConf = filepath.Join(e.runDir, "podman", "containers.conf")
	// The agent binary is found next to sbxd; everything else comes from
	// flags, so the environment only carries the database URL.
	e.cmd.Env = append(os.Environ(), "SBX_DATABASE_URL="+e.dbURL)
	e.cmd.Stdout, e.cmd.Stderr = logf, logf
	if err := e.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	logf.Close()
	e.done = make(chan error, 1)
	go func(c *exec.Cmd, done chan error) { done <- c.Wait() }(e.cmd, e.done)

	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case err := <-e.done:
			e.done <- err
			e.t.Fatalf("sbxd exited during start-up: %v\n%s", err, e.logTail(40))
		default:
		}
		resp, err := http.Get(e.base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("sbxd not ready after 60s\n%s", e.logTail(40))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop signals sbxd and waits for it to exit; it returns the wait error.
func (e *env) stop(sig syscall.Signal) error {
	if e.cmd == nil || e.cmd.Process == nil {
		return nil
	}
	_ = e.cmd.Process.Signal(sig)
	select {
	case err := <-e.done:
		e.cmd = nil
		return err
	case <-time.After(60 * time.Second):
		_ = e.cmd.Process.Kill()
		e.cmd = nil
		return fmt.Errorf("sbxd did not exit within 60s of %v", sig)
	}
}

func (e *env) logTail(n int) string {
	b, _ := os.ReadFile(e.logPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "--- sbxd log tail ---\n" + strings.Join(lines, "\n")
}

// containers lists the Podman containers that belong to this
// environment's server, by sandbox id: the ones labelled with its host
// id. Other schedulers of the same Podman user (a developer's server,
// another test run) have their own host id and are not in the result.
func (e *env) containers(t *testing.T) map[string]string {
	t.Helper()
	out := runPodman(t, "ps", "-a", "--filter", "label=sbx.managed=true", "--filter", "label=sbx.host_id="+e.hostID,
		"--format", "{{.Names}} {{.State}}")
	found := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		found[strings.TrimPrefix(name, "sbx-")] = state
	}
	return found
}

// runDirs lists the sandbox ids that have a run dir.
func (e *env) runDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(e.runDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var ids []string
	for _, en := range entries {
		if en.IsDir() && strings.HasPrefix(en.Name(), "sb-") {
			ids = append(ids, en.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// cleanup removes whatever the environment left on the host.
func (e *env) cleanup() {
	if e.cmd != nil {
		_ = e.cmd.Process.Kill()
		<-e.done
	}
	// Whatever still carries this run's host id is this run's.
	if out, err := podmanCLI("ps", "-a", "-q", "--filter", "label=sbx.host_id="+e.hostID).Output(); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = podmanCLI(append([]string{"rm", "-f", "-t", "0"}, ids...)...).Run()
		}
	}
	_ = os.RemoveAll(e.runDir)
}

// watchOOMFile cleans up after conmon. When a container is OOM-killed,
// conmon (2.1.10) creates an empty file named "oom" in its working
// directory. Podman 4.9.3 sets neither a working directory nor
// --persist-dir for conmon, so that is the working directory of whatever
// started the container: for a sandbox the Podman API service (the user's
// home, when systemd socket-activates it), for a `podman run` of this
// test the test's own directory. Nothing in the create request moves it
// and sbxd cannot move the service, so the test removes the file
// afterwards, and only when it was not there before, is empty and is a
// regular file. (An operator can give podman.service a WorkingDirectory
// in a systemd drop-in to send it elsewhere.)
func watchOOMFile(t *testing.T) {
	t.Helper()
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, home)
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, "oom")
		if _, err := os.Lstat(path); err == nil {
			continue // not this test's to remove
		}
		t.Cleanup(func() {
			if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
				if os.Remove(path) == nil {
					t.Logf("removed %s, left by conmon for an OOM-killed container", path)
				}
			}
		})
	}
}

// --- HTTP client ---------------------------------------------------------------

type client struct {
	t    *testing.T
	base string
	key  string
	hc   *http.Client
	// metricsBase and metricsToken say where GET /metrics lives when it
	// is not on the API listener (--metrics-listen, --metrics-token).
	metricsBase  string
	metricsToken string
}

type result struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r result) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func (r result) apiError() apiError {
	var env struct {
		Error apiError `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &env)
	return env.Error
}

func (c *client) request(method, path string, body any, token string) (*http.Response, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-Access-Token", token)
	} else if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}
	return c.hc.Do(req)
}

// do sends a request authenticated with the API key, or with a sandbox
// access token when token is not empty.
func (c *client) do(method, path string, body any, token string) result {
	c.t.Helper()
	resp, err := c.request(method, path, body, token)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return result{resp.StatusCode, resp.Header, b}
}

func (c *client) must(status int, method, path string, body any, token string) result {
	c.t.Helper()
	r := c.do(method, path, body, token)
	if r.Status != status {
		c.t.Fatalf("%s %s: got %d %s, want %d", method, path, r.Status, r.Body, status)
	}
	return r
}

type sandbox struct {
	ID          string            `json:"id"`
	TemplateID  string            `json:"template_id"`
	State       string            `json:"state"`
	Reason      string            `json:"reason"`
	StartedAt   *time.Time        `json:"started_at"`
	ExpiresAt   *time.Time        `json:"expires_at"`
	Metadata    map[string]string `json:"metadata"`
	AccessToken string            `json:"access_token"`
}

func (c *client) create(body map[string]any) sandbox {
	c.t.Helper()
	var sb sandbox
	c.must(201, "POST", "/v1/sandboxes", body, "").decode(c.t, &sb)
	if sb.State != "running" || sb.AccessToken == "" {
		c.t.Fatalf("create: %+v", sb)
	}
	return sb
}

func (c *client) get(id string) sandbox {
	c.t.Helper()
	var sb sandbox
	c.must(200, "GET", "/v1/sandboxes/"+id, nil, "").decode(c.t, &sb)
	return sb
}

type exitStatus struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal"`
	Reason   string `json:"reason"`
}

type command struct {
	Pid        string      `json:"pid"`
	PTY        bool        `json:"pty"`
	Exit       *exitStatus `json:"exit"`
	NextOffset int64       `json:"next_offset"`
}

type waitResult struct {
	command
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	Truncated bool   `json:"truncated"`
}

// exec runs a command to completion with ?wait=true.
func (c *client) exec(sb sandbox, body map[string]any) waitResult {
	c.t.Helper()
	var res waitResult
	c.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=true", body, sb.AccessToken).decode(c.t, &res)
	if res.Exit == nil {
		c.t.Fatalf("exec: no exit status: %+v", res)
	}
	return res
}

func shell(script string) map[string]any {
	return map[string]any{"cmd": "sh", "args": []string{"-c", script}}
}

type streamEvent struct {
	Type    string      `json:"type"`
	Offset  int64       `json:"offset"`
	Data    []byte      `json:"data"`
	Dropped int64       `json:"dropped"`
	Exit    *exitStatus `json:"exit"`
	Command *command    `json:"command"`
	Error   *apiError   `json:"error"`
}

// stream runs a command with ?wait=stream and returns every event. If
// first is not nil it receives the time of the first output event.
func (c *client) stream(sb sandbox, body map[string]any, first *time.Time) []streamEvent {
	c.t.Helper()
	resp, err := c.request("POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=stream", body, sb.AccessToken)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("stream: %d %s", resp.StatusCode, b)
	}
	var out []streamEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var ev streamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			c.t.Fatalf("stream line %q: %v", sc.Text(), err)
		}
		if first != nil && first.IsZero() && (ev.Type == "stdout" || ev.Type == "stderr" || ev.Type == "pty") {
			*first = time.Now()
		}
		out = append(out, ev)
	}
	return out
}

func collect(events []streamEvent, typ string) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == typ {
			b.Write(e.Data)
		}
	}
	return b.String()
}

func lastEvent(events []streamEvent) streamEvent {
	if len(events) == 0 {
		return streamEvent{}
	}
	return events[len(events)-1]
}

// metric returns the value of the first sample whose line starts with
// prefix (a metric name with its full label set), or -1.
func (c *client) metric(prefix string) float64 {
	c.t.Helper()
	var body []byte
	if c.metricsBase == "" {
		body = c.do("GET", "/metrics", nil, "").Body
	} else {
		req, err := http.NewRequest("GET", c.metricsBase+"/metrics", nil)
		if err != nil {
			c.t.Fatal(err)
		}
		if c.metricsToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.metricsToken)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			c.t.Fatalf("GET %s/metrics: %v", c.metricsBase, err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			c.t.Fatalf("GET %s/metrics: %d %s", c.metricsBase, resp.StatusCode, body)
		}
	}
	// Every scrape of the real server is checked as a strict scraper
	// would read it: all families (HTTP, pool, egress, webhooks), no
	// duplicate series, no malformed line.
	if _, err := promtext.Validate(string(body)); err != nil {
		c.t.Errorf("/metrics is not valid Prometheus exposition text: %v\n%s", err, body)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			v, err := strconv.ParseFloat(strings.TrimPrefix(line, prefix+" "), 64)
			if err == nil {
				return v
			}
		}
	}
	return -1
}

func (c *client) warm(template string) int {
	return int(c.metric(fmt.Sprintf(`sbx_pool_sandboxes{template=%q,state="warm"}`, template)))
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %s waiting for: %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// waitState polls a sandbox until it reaches state.
func (c *client) waitState(id, state string, timeout time.Duration) (sandbox, time.Duration) {
	c.t.Helper()
	var sb sandbox
	took := waitFor(c.t, "sandbox "+id+" to be "+state, timeout, func() bool {
		sb = c.get(id)
		return sb.State == state
	})
	return sb, took
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s))*p+0.9999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

var warmTookRE = regexp.MustCompile(`msg="sandbox warm".* took=([0-9a-zµ.]+)`)

// coldCreateTimes reads, from the server's debug log, how long each
// create took from the start of the create to WARM (container create and
// start plus agent readiness).
func (e *env) coldCreateTimes() []time.Duration {
	b, _ := os.ReadFile(e.logPath)
	var out []time.Duration
	for _, m := range warmTookRE.FindAllStringSubmatch(string(b), -1) {
		if d, err := time.ParseDuration(m[1]); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
