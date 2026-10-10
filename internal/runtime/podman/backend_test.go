package podman

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

type recorded struct {
	Method, Path, Query, Body string
}

// fakePodman is a Libpod API stand-in served on a real Unix socket.
type fakePodman struct {
	t       *testing.T
	version string // Libpod-API-Version header; "" omits it
	handler func(w http.ResponseWriter, r *http.Request, body string) bool

	mu   sync.Mutex
	reqs []recorded
}

func (f *fakePodman) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func startFake(t *testing.T, f *fakePodman, mutate func(*Config)) *Backend {
	t.Helper()
	f.t = t
	// Unix socket paths are limited to about 108 bytes, so not t.TempDir().
	dir, err := os.MkdirTemp("", "sbxpm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(raw)})
		f.mu.Unlock()
		if f.version != "" {
			w.Header().Set("Libpod-API-Version", f.version)
		}
		if r.URL.Path == "/_ping" {
			io.WriteString(w, "OK")
			return
		}
		if f.handler == nil || !f.handler(w, r, string(raw)) {
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	cfg := Config{SocketPath: sock, SeccompProfilePath: "/etc/sbx/seccomp.json", CgroupRoot: t.TempDir(), RequestTimeout: 5 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func libpodError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"cause": "root cause", "message": msg, "response": status})
}

const inspectRunning = `{"Id":"cid1","Created":"2026-10-09T10:00:00.5Z","OCIRuntime":"crun",
 "State":{"Status":"running","Running":true,"Paused":false,"OOMKilled":false,
          "CgroupPath":"/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-cid1.scope"},
 "Config":{"Labels":{"sbx.managed":"true","sbx.sandbox_id":"sb-123","sbx.generation":"4","sbx.agent_epoch":"epoch-9","sbx.template_id":"py312"}}}`

func TestCreateSendsHardenedSpecAndStarts(t *testing.T) {
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		switch r.Method + " " + r.URL.Path {
		case "POST /v4.9.3/libpod/containers/create":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"cid1","Warnings":[]}`)
		case "POST /v4.9.3/libpod/containers/cid1/start":
			w.WriteHeader(http.StatusNoContent)
		case "GET /v4.9.3/libpod/containers/cid1/json":
			io.WriteString(w, inspectRunning)
		default:
			return false
		}
		return true
	}
	b := startFake(t, f, nil)
	inst, err := b.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.Instance{ID: "cid1", SandboxID: "sb-123", Generation: 4, AgentEpoch: "epoch-9", TemplateID: "py312",
		Running: true, CreatedAt: time.Date(2026, 10, 9, 10, 0, 0, 500_000_000, time.UTC)}
	if inst != want {
		t.Errorf("instance %+v\nwant %+v", inst, want)
	}
	reqs := f.requests()
	var seq []string
	for _, r := range reqs {
		seq = append(seq, r.Method+" "+r.Path)
	}
	wantSeq := "GET /_ping,POST /v4.9.3/libpod/containers/create,POST /v4.9.3/libpod/containers/cid1/start,GET /v4.9.3/libpod/containers/cid1/json"
	if got := strings.Join(seq, ","); got != wantSeq {
		t.Fatalf("request sequence:\n got %s\nwant %s", got, wantSeq)
	}
	// The body on the wire is exactly the hardened spec.
	if got, want := canonicalJSON(t, []byte(reqs[1].Body)), canonicalJSON(t, []byte(wantCreateJSON)); got != want {
		t.Errorf("create body differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestAPIVersionDiscovery(t *testing.T) {
	for header, wantPrefix := range map[string]string{
		"6.0.1":          "/v6.0.1/libpod/",
		"5.4.0-dev":      "/v5.4.0-dev/libpod/",
		"":               "/v4.0.0/libpod/", // no header: minimum supported version
		"../../etc/pass": "/v4.0.0/libpod/", // never placed in a path
	} {
		f := &fakePodman{version: header}
		f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
			io.WriteString(w, "[]")
			return true
		}
		b := startFake(t, f, nil)
		for i := 0; i < 2; i++ {
			if _, err := b.List(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		reqs := f.requests()
		if len(reqs) != 3 || reqs[0].Path != "/_ping" {
			t.Fatalf("header %q: want one ping then two lists, got %+v", header, reqs)
		}
		if !strings.HasPrefix(reqs[1].Path, wantPrefix) {
			t.Errorf("header %q: path %q, want prefix %q", header, reqs[1].Path, wantPrefix)
		}
	}
}

func TestCreateFailures(t *testing.T) {
	// A rejected create surfaces Libpod's message and makes no further calls.
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		libpodError(w, http.StatusInternalServerError, "the requested cgroup controller `cpu` is not available")
		return true
	}
	b := startFake(t, f, nil)
	_, err := b.Create(context.Background(), testSpec())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || !strings.Contains(err.Error(), "controller `cpu` is not available") || ae.Cause != "root cause" {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != 2 {
		t.Errorf("%d requests after a failed create, want 2", n)
	}

	// A missing image is a 404 the caller can recognise.
	f = &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		libpodError(w, http.StatusNotFound, "no such image")
		return true
	}
	if _, err = startFake(t, f, nil).Create(context.Background(), testSpec()); !IsStatus(err, http.StatusNotFound) {
		t.Errorf("missing image: %v", err)
	}

	// A start failure removes the half-made container.
	for name, startStatus := range map[string]int{"start fails": 500, "exits at once": 204} {
		f = &fakePodman{version: "4.9.3"}
		f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
			switch {
			case strings.HasSuffix(r.URL.Path, "/create"):
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"Id":"cid1"}`)
			case strings.HasSuffix(r.URL.Path, "/start"):
				if startStatus != 204 {
					libpodError(w, startStatus, "crun: exec failed")
				} else {
					w.WriteHeader(204)
				}
			case r.Method == http.MethodGet:
				io.WriteString(w, `{"Id":"cid1","State":{"Status":"exited","Running":false},"Config":{"Labels":{}}}`)
			case r.Method == http.MethodDelete:
				io.WriteString(w, `[{"Id":"cid1"}]`)
			default:
				return false
			}
			return true
		}
		b = startFake(t, f, nil)
		if _, err := b.Create(context.Background(), testSpec()); err == nil {
			t.Errorf("%s: Create succeeded", name)
		}
		reqs := f.requests()
		last := reqs[len(reqs)-1]
		if last.Method != http.MethodDelete || last.Path != "/v4.9.3/libpod/containers/cid1" {
			t.Errorf("%s: last request %+v, want DELETE of cid1", name, last)
		}
	}

	// An invalid spec never reaches the service.
	f = &fakePodman{version: "4.9.3"}
	b = startFake(t, f, nil)
	spec := testSpec()
	spec.Template.Resources.MemoryBytes = 0
	if _, err := b.Create(context.Background(), spec); err == nil || len(f.requests()) != 0 {
		t.Errorf("invalid spec: err=%v requests=%d", err, len(f.requests()))
	}
}

func TestDestroy(t *testing.T) {
	status := http.StatusOK
	f := &fakePodman{version: "6.0.0"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if status != http.StatusOK {
			libpodError(w, status, "no container with name or ID \"cid1\" found: no such container")
			return true
		}
		io.WriteString(w, `[{"Id":"cid1"}]`)
		return true
	}
	b := startFake(t, f, nil)
	if err := b.Destroy(context.Background(), "cid1"); err != nil {
		t.Fatal(err)
	}
	r := f.requests()[1]
	if r.Method != http.MethodDelete || r.Path != "/v6.0.0/libpod/containers/cid1" || r.Query != "force=true&timeout=0&volumes=true" {
		t.Errorf("destroy request: %+v", r)
	}
	// 404: already gone, which is success.
	status = http.StatusNotFound
	if err := b.Destroy(context.Background(), "cid1"); err != nil {
		t.Errorf("destroy of a missing container: %v", err)
	}
	// Anything else is an error carrying the body.
	status = http.StatusInternalServerError
	if err := b.Destroy(context.Background(), "cid1"); !IsStatus(err, 500) || !strings.Contains(err.Error(), "no such container") {
		t.Errorf("destroy 500: %v", err)
	}
	if err := b.Destroy(context.Background(), ""); err == nil {
		t.Error("empty id accepted")
	}
}

func TestListFiltersOnManagedLabel(t *testing.T) {
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		io.WriteString(w, `[
 {"Id":"a","State":"running","Created":"2026-10-09T10:00:00Z","Labels":{"sbx.managed":"true","sbx.sandbox_id":"s1","sbx.generation":"2","sbx.agent_epoch":"e","sbx.template_id":"t"}},
 {"Id":"b","State":"exited","Created":"2026-10-09T10:00:01Z","Labels":{"sbx.managed":"true","sbx.sandbox_id":"s2","sbx.generation":"junk"}},
 {"Id":"c","State":"paused","Created":"2026-10-09T10:00:02Z","Labels":{"sbx.managed":"true","sbx.sandbox_id":"s3"}},
 {"Id":"d","State":"running","Created":"2026-10-09T10:00:03Z","Labels":{"other":"x"}},
 {"Id":"e","State":"running","Created":"2026-10-09T10:00:03Z","Labels":null}
]`)
		return true
	}
	b := startFake(t, f, nil)
	got, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := f.requests()[1]
	if r.Path != "/v4.9.3/libpod/containers/json" || r.Query != `all=true&filters=%7B%22label%22%3A%5B%22sbx.managed%3Dtrue%22%5D%7D` {
		t.Errorf("list request: %+v", r)
	}
	if len(got) != 3 {
		t.Fatalf("got %d instances, want 3 (unlabelled ones dropped): %+v", len(got), got)
	}
	if got[0].ID != "a" || got[0].SandboxID != "s1" || got[0].Generation != 2 || !got[0].Running || got[0].TemplateID != "t" || got[0].CreatedAt.IsZero() {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].ID != "b" || got[1].Running || got[1].Generation != 0 {
		t.Errorf("second: %+v", got[1])
	}
	if got[2].ID != "c" || !got[2].Running {
		t.Errorf("paused instance should count as running: %+v", got[2])
	}
}

func TestInspectPauseUnpauseAndNotFound(t *testing.T) {
	missing := false
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		if missing {
			libpodError(w, http.StatusNotFound, "no such container")
			return true
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, inspectRunning)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
		return true
	}
	b := startFake(t, f, nil)
	ctx := context.Background()
	inst, err := b.Inspect(ctx, "cid1")
	if err != nil || inst.SandboxID != "sb-123" || !inst.Running {
		t.Fatalf("inspect: %+v %v", inst, err)
	}
	if err := b.Pause(ctx, "cid1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Unpause(ctx, "cid1"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if reqs[2].Method != "POST" || reqs[2].Path != "/v4.9.3/libpod/containers/cid1/pause" ||
		reqs[3].Method != "POST" || reqs[3].Path != "/v4.9.3/libpod/containers/cid1/unpause" {
		t.Errorf("pause/unpause requests: %+v", reqs[2:])
	}
	missing = true
	if _, err := b.Inspect(ctx, "cid1"); !errors.Is(err, runtime.ErrNotFound) || !IsStatus(err, 404) {
		t.Errorf("inspect 404: %v", err)
	}
	if err := b.Pause(ctx, "cid1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("pause 404: %v", err)
	}
	if err := b.Unpause(ctx, "cid1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("unpause 404: %v", err)
	}
	if _, err := b.Stats(ctx, "cid1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("stats 404: %v", err)
	}
}

func TestStatsReadsCgroupFromInspectPath(t *testing.T) {
	state := `{"Status":"running","Running":true,"CgroupPath":"/user.slice/libpod-cid1.scope"}`
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		io.WriteString(w, `{"Id":"cid1","State":`+state+`,"Config":{"Labels":{}}}`)
		return true
	}
	b := startFake(t, f, nil)
	ctx := context.Background()
	scope := filepath.Join(b.Config().CgroupRoot, "user.slice", "libpod-cid1.scope")
	// The scope has systemd defaults; the leaf has the real counters.
	writeFiles(t, scope, map[string]string{"memory.current": "1\n", "pids.events": "max 0\n"})
	writeFiles(t, filepath.Join(scope, "container"), cgroupFixture)
	st, err := b.Stats(ctx, "cid1")
	if err != nil {
		t.Fatal(err)
	}
	if st.MemoryBytes != 104857600 || st.OOMKills != 1 || st.PidsLimitHits != 4 || st.MemoryHighHits != 12 || st.NrThrottled != 7 {
		t.Errorf("stats %+v", st)
	}

	// Exited, cgroup gone, Podman says nothing: ErrNotRunning.
	state = `{"Status":"exited","Running":false,"CgroupPath":""}`
	if _, err := b.Stats(ctx, "cid1"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("exited: %v, want ErrNotRunning", err)
	}
	state = `{"Status":"exited","Running":false,"CgroupPath":"/user.slice/libpod-gone.scope"}`
	if _, err := b.Stats(ctx, "cid1"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("cgroup removed: %v, want ErrNotRunning", err)
	}
	// Exited and Podman recorded the OOM kill: reported, not an error.
	state = `{"Status":"exited","Running":false,"OOMKilled":true,"CgroupPath":""}`
	if st, err := b.Stats(ctx, "cid1"); err != nil || st.OOMKills != 1 {
		t.Errorf("OOMKilled: %+v %v", st, err)
	}
}

func TestErrorHandling(t *testing.T) {
	// Non-JSON error bodies are kept verbatim.
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "plain text failure\n")
		return true
	}
	b := startFake(t, f, nil)
	_, err := b.List(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 502 || ae.Message != "plain text failure" || ae.Method != "GET" {
		t.Errorf("err = %#v", err)
	}
	// A 2xx with a broken body is an error, not an empty list.
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		io.WriteString(w, "{not json")
		return true
	}
	if _, err := b.List(context.Background()); err == nil {
		t.Error("broken JSON accepted")
	}
	// Request timeout.
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		return true
	}
	slow := startFake(t, f, func(c *Config) { c.RequestTimeout = 100 * time.Millisecond })
	if _, err := slow.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := slow.List(context.Background()); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("timeout: err=%v after %v", err, time.Since(start))
	}
	// No service at all.
	dead, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "absent.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.Ping(context.Background()); err == nil {
		t.Error("ping of a missing socket succeeded")
	}
	if _, err := dead.List(context.Background()); err == nil {
		t.Error("list on a missing socket succeeded")
	}
}

// A Podman service runs every container with its default runtime and
// ignores the one in the request. Create must notice.
func TestCreateRefusesAnotherRuntimeThanAsked(t *testing.T) {
	removed := false
	f := &fakePodman{version: "4.9.3"}
	f.handler = func(w http.ResponseWriter, r *http.Request, _ string) bool {
		switch r.Method + " " + r.URL.Path {
		case "POST /v4.9.3/libpod/containers/create":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"cid1"}`)
		case "POST /v4.9.3/libpod/containers/cid1/start":
			w.WriteHeader(http.StatusNoContent)
		case "GET /v4.9.3/libpod/containers/cid1/json":
			io.WriteString(w, inspectRunning) // OCIRuntime: crun
		case "DELETE /v4.9.3/libpod/containers/cid1":
			removed = true
			w.WriteHeader(http.StatusNoContent)
		default:
			return false
		}
		return true
	}
	// The same fake stands in for both services.
	var sock string
	b := startFake(t, f, func(c *Config) {
		sock = c.SocketPath
		c.Runtimes = []string{"crun", "runsc"}
		c.RuntimeSockets = map[string]string{"runsc": c.SocketPath}
	})
	spec := testSpec()
	spec.Template.Runtime = "runsc"
	_, err := b.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), `OCI runtime "crun" although "runsc" was asked`) || !strings.Contains(err.Error(), sock) {
		t.Fatalf("Create with the wrong runtime: %v", err)
	}
	if !removed {
		t.Error("the container that ran with the wrong runtime was not removed")
	}
}

// A second runtime needs a service of its own; creates go to it,
// everything else to the default service.
func TestCreateGoesToTheRuntimesService(t *testing.T) {
	respond := func(rt string) func(http.ResponseWriter, *http.Request, string) bool {
		return func(w http.ResponseWriter, r *http.Request, _ string) bool {
			switch r.Method + " " + r.URL.Path {
			case "POST /v4.9.3/libpod/containers/create":
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"Id":"cid1"}`)
			case "POST /v4.9.3/libpod/containers/cid1/start":
				w.WriteHeader(http.StatusNoContent)
			case "GET /v4.9.3/libpod/containers/cid1/json":
				io.WriteString(w, strings.Replace(inspectRunning, `"OCIRuntime":"crun"`, `"OCIRuntime":"`+rt+`"`, 1))
			default:
				return false
			}
			return true
		}
	}
	gv := &fakePodman{version: "4.9.3", handler: respond("runsc")}
	gb := startFake(t, gv, nil)
	def := &fakePodman{version: "4.9.3", handler: respond("runsc")}
	b := startFake(t, def, func(c *Config) {
		c.Runtimes = []string{"crun", "runsc"}
		c.RuntimeSockets = map[string]string{"runsc": gb.Config().SocketPath}
	})
	spec := testSpec()
	spec.Template.Runtime = "runsc"
	if _, err := b.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	paths := func(f *fakePodman) string {
		var out []string
		for _, r := range f.requests() {
			out = append(out, r.Method+" "+strings.TrimPrefix(r.Path, "/v4.9.3/libpod"))
		}
		return strings.Join(out, ",")
	}
	if got := paths(gv); got != "GET /_ping,POST /containers/create,POST /containers/cid1/start" {
		t.Errorf("runsc service saw: %s", got)
	}
	if got := paths(def); got != "GET /_ping,GET /containers/cid1/json" {
		t.Errorf("default service saw: %s", got)
	}
	if _, err := New(Config{SocketPath: "/s", Runtimes: []string{"crun", "runsc"}}); err == nil || !strings.Contains(err.Error(), "service of its own") {
		t.Errorf("a second runtime without a service was accepted: %v", err)
	}
}
