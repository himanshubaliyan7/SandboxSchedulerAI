package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/agent"
	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/agentclient"
	"github.com/himanshubaliyan7/warmbox/internal/api"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/egress"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/pool/pooltest"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/fake"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// The handler tests run the real API and the real pool controller over
// an in-memory store, the fake backend, and one real in-process agent
// per sandbox (so commands, PTYs and files are real, on the test host).

const (
	tmplID   = "py"
	tenantA  = "tenant-a"
	tenantB  = "tenant-b"
	keyA     = "sbx_aaaaaaaa_" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	keyB     = "sbx_bbbbbbbb_" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	hostName = "test-host"
)

// fakeStore is api.Store over pooltest.MemStore.
type fakeStore struct {
	*pooltest.MemStore
	mu       sync.Mutex
	keys     map[string]string // plaintext -> tenant id
	tenants  map[string]store.Tenant
	metadata map[string]map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{MemStore: pooltest.NewMemStore(time.Now), keys: map[string]string{},
		tenants: map[string]store.Tenant{}, metadata: map[string]map[string]string{}}
}

func (f *fakeStore) putTenant(t store.Tenant, key string) {
	f.mu.Lock()
	f.tenants[t.ID] = t
	f.keys[key] = t.ID
	f.mu.Unlock()
	f.PutTenant(t.ID, t.MaxConcurrent)
}

func (f *fakeStore) AuthenticateAPIKey(ctx context.Context, plaintext string) (store.APIKey, store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.keys[plaintext]
	if !ok {
		return store.APIKey{}, store.Tenant{}, fmt.Errorf("%w: api key", store.ErrNotFound)
	}
	return store.APIKey{TenantID: id}, f.tenants[id], nil
}

func (f *fakeStore) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tenants[id]
	if !ok {
		return store.Tenant{}, fmt.Errorf("%w: tenant %s", store.ErrNotFound, id)
	}
	return t, nil
}

func (f *fakeStore) access(sb core.Sandbox) store.SandboxAccess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return store.SandboxAccess{Sandbox: sb, AccessTokenHash: f.AccessTokenHash(sb.ID),
		NetworkPolicy: f.NetworkPolicy(sb.ID), Metadata: f.metadata[sb.ID]}
}

func (f *fakeStore) GetSandboxAccess(ctx context.Context, id string) (store.SandboxAccess, error) {
	sb, err := f.GetSandbox(ctx, id)
	if err != nil {
		return store.SandboxAccess{}, err
	}
	return f.access(sb), nil
}

func (f *fakeStore) ListSandboxAccess(ctx context.Context, tenantID string, flt store.SandboxFilter) ([]store.SandboxAccess, error) {
	var out []store.SandboxAccess
	for _, sb := range f.Sandboxes() {
		if sb.TenantID != tenantID || (flt.TemplateID != "" && sb.TemplateID != flt.TemplateID) {
			continue
		}
		if len(flt.States) > 0 {
			ok := false
			for _, s := range flt.States {
				ok = ok || s == sb.State
			}
			if !ok {
				continue
			}
		}
		acc := f.access(sb)
		match := true
		for k, v := range flt.Metadata {
			match = match && acc.Metadata[k] == v
		}
		if match {
			out = append(out, acc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox.AssignedAt.After(*out[j].Sandbox.AssignedAt) })
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
	}
	return out, nil
}

func (f *fakeStore) SetMetadata(ctx context.Context, id string, generation int64, md map[string]string) error {
	sb, err := f.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	if sb.Generation != generation {
		return store.ErrStaleGeneration
	}
	f.mu.Lock()
	f.metadata[id] = md
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) SetLifetime(ctx context.Context, id string, generation int64, fromNow, maxTotal time.Duration) (core.Sandbox, error) {
	sb, err := f.GetSandbox(ctx, id)
	if err != nil {
		return core.Sandbox{}, err
	}
	if sb.Generation != generation {
		return core.Sandbox{}, store.ErrStaleGeneration
	}
	exp := time.Now().Add(fromNow)
	if maxTotal > 0 {
		if limit := sb.AssignedAt.Add(maxTotal); limit.Before(exp) {
			exp = limit
		}
	}
	f.Update(id, func(s *core.Sandbox) { s.ExpiresAt = &exp })
	return f.GetSandbox(ctx, id)
}

func (f *fakeStore) ListEvents(ctx context.Context, sandboxID string) ([]store.Event, error) {
	return f.Events(sandboxID), nil
}

// inprocAgents is a pool.AgentConn that starts a real in-process agent
// for a sandbox the first time the pool waits for it.
type inprocAgents struct {
	t         *testing.T
	keepalive time.Duration

	mu     sync.Mutex
	cancel map[string]context.CancelFunc // by host run dir
	done   map[string]chan struct{}
}

func (a *inprocAgents) WaitReady(ctx context.Context, hostRunDir string) error {
	a.mu.Lock()
	if _, ok := a.cancel[hostRunDir]; !ok {
		actx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		ag := agent.New(agent.Config{
			SocketPath: agentclient.SocketPath(hostRunDir), Workspace: a.t.TempDir(),
			KeepaliveInterval: a.keepalive, EgressListenAddr: "127.0.0.1:0", DrainGrace: 100 * time.Millisecond,
		})
		ln, err := ag.Listen()
		if err != nil {
			a.mu.Unlock()
			cancel()
			return err
		}
		go func() { defer close(done); _ = ag.ServeListener(actx, ln) }()
		a.cancel[hostRunDir], a.done[hostRunDir] = cancel, done
	}
	a.mu.Unlock()
	return agentclient.Sandboxes{}.WaitReady(ctx, hostRunDir)
}

func (a *inprocAgents) Init(ctx context.Context, hostRunDir string, req agentapi.InitRequest) error {
	return agentclient.Sandboxes{}.Init(ctx, hostRunDir, req)
}

// kill stops the agent of one sandbox, as if its container had died.
func (a *inprocAgents) kill(hostRunDir string) {
	a.mu.Lock()
	cancel, done := a.cancel[hostRunDir], a.done[hostRunDir]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (a *inprocAgents) stopAll() {
	a.mu.Lock()
	dirs := make([]string, 0, len(a.cancel))
	for d := range a.cancel {
		dirs = append(dirs, d)
	}
	a.mu.Unlock()
	for _, d := range dirs {
		a.kill(d)
	}
}

type harness struct {
	t      *testing.T
	st     *fakeStore
	be     *fake.Backend
	agents *inprocAgents
	egress *pooltest.FakeEgress
	pool   *pool.Controller
	api    *api.Server
	srv    *httptest.Server
}

type options struct {
	template  func(*core.Template)
	tenantA   func(*store.Tenant)
	api       func(*api.Config)
	pool      func(*pool.Config)
	keepalive time.Duration
	noStart   bool // do not wait for the pool to fill
}

func baseTemplate() core.Template {
	return core.Template{
		ID: tmplID, Version: 1, Image: "img", Runtime: "crun",
		Resources:  core.Resources{CPUs: 1, MemoryBytes: 256 << 20, PidsLimit: 128, WorkdirBytes: 64 << 20, TmpBytes: 32 << 20, NofileLimit: 1024},
		Network:    core.NetworkAllowlist,
		Timeouts:   core.Timeouts{Idle: 10 * time.Minute, MaxLifetime: time.Hour, WarmTTL: 30 * time.Minute},
		WarmTarget: 2, MaxTotal: 10, RefillRate: 2,
	}
}

func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	// A short path: the agents bind Unix sockets below it.
	dir, err := os.MkdirTemp("", "sbxapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	h := &harness{t: t, st: newFakeStore(), be: fake.New(), egress: &pooltest.FakeEgress{},
		agents: &inprocAgents{t: t, keepalive: o.keepalive, cancel: map[string]context.CancelFunc{}, done: map[string]chan struct{}{}}}
	tmpl := baseTemplate()
	if o.template != nil {
		o.template(&tmpl)
	}
	h.st.PutTemplate(tmpl)
	ta := store.Tenant{ID: tenantA, Name: "A", MaxConcurrent: 5}
	if o.tenantA != nil {
		o.tenantA(&ta)
	}
	h.st.putTenant(ta, keyA)
	h.st.putTenant(store.Tenant{ID: tenantB, Name: "B", MaxConcurrent: 5}, keyB)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pcfg := pool.Config{
		Store: h.st.MemStore, Backend: h.be, Agent: h.agents, Egress: h.egress, Logger: logger,
		HostID: hostName, RunDirBase: dir, AgentBinaryPath: "/opt/sbx-agent",
		RefillInterval: 20 * time.Millisecond, SweepInterval: 20 * time.Millisecond, ReconcileInterval: time.Hour,
		ClaimWait: 200 * time.Millisecond,
	}
	if o.pool != nil {
		o.pool(&pcfg)
	}
	if h.pool, err = pool.New(pcfg); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	acfg := api.Config{Store: h.st, Pool: h.pool, Stats: h.be, ValidatePolicy: egress.ValidatePolicy, Logger: logger,
		TouchInterval: time.Millisecond, AuthCacheTTL: -1, JanitorInterval: 50 * time.Millisecond}
	if o.api != nil {
		o.api(&acfg)
	}
	if h.api, err = api.New(acfg); err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(h.api.Handler())
	t.Cleanup(func() {
		h.srv.CloseClientConnections()
		h.api.Close()
		h.srv.Close()
		h.pool.Stop()
		h.agents.stopAll()
	})
	if !o.noStart {
		h.waitWarm(tmpl.WarmTarget)
	}
	return h
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) count(state core.State) int {
	n := 0
	for _, sb := range h.st.Sandboxes() {
		if sb.State == state {
			n++
		}
	}
	return n
}

func (h *harness) waitWarm(n int) {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("%d warm sandboxes", n), func() bool { return h.count(core.StateWarm) >= n })
}

// response is a decoded API response.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %q: %v", r.Body, err)
	}
}

// errCode returns the code of an error envelope ("" if the body is not one).
func (r response) errCode() string {
	var env struct {
		Error api.ErrorBody `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &env)
	return env.Error.Code
}

func (r response) errBody(t *testing.T) api.ErrorBody {
	t.Helper()
	var env struct {
		Error api.ErrorBody `json:"error"`
	}
	r.json(t, &env)
	return env.Error
}

type reqOpt func(*http.Request)

func withKey(key string) reqOpt { return func(r *http.Request) { r.Header.Set("X-API-Key", key) } }
func withToken(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("X-Access-Token", tok) }
}
func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// do sends a request; a string or []byte body is sent raw, anything else
// as JSON.
func (h *harness) do(method, path string, body any, opts ...reqOpt) response {
	h.t.Helper()
	resp := h.raw(method, path, body, opts...)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return response{resp.StatusCode, resp.Header, b}
}

func (h *harness) raw(method, path string, body any, opts ...reqOpt) *http.Response {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	case io.Reader:
		rd = b
	default:
		j, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (h *harness) want(r response, status int, code string) response {
	h.t.Helper()
	if r.Status != status || (code != "" && r.errCode() != code) {
		h.t.Fatalf("got %d %s, want %d %q", r.Status, r.Body, status, code)
	}
	return r
}

// create claims a sandbox for a tenant and returns its view.
func (h *harness) create(key string, body any) api.SandboxView {
	h.t.Helper()
	if body == nil {
		body = map[string]any{"template_id": tmplID}
	}
	r := h.want(h.do("POST", "/v1/sandboxes", body, withKey(key)), http.StatusCreated, "")
	var v api.SandboxView
	r.json(h.t, &v)
	if v.ID == "" || !strings.HasPrefix(v.AccessToken, api.AccessTokenPrefix) || v.State != "running" {
		h.t.Fatalf("create: unexpected view %+v", v)
	}
	return v
}

// runDir is the host run dir of a sandbox.
func (h *harness) runDir(id string) string { return h.pool.RunDir(id) }

// stream reads an NDJSON body to its end.
func readStream(t *testing.T, body io.Reader) []api.StreamEvent {
	t.Helper()
	var out []api.StreamEvent
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var ev api.StreamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad stream line %q: %v", sc.Text(), err)
		}
		out = append(out, ev)
	}
	return out
}

// output concatenates the data of the events of one type.
func output(events []api.StreamEvent, typ string) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == typ {
			b.Write(e.Data)
		}
	}
	return b.String()
}

func last(events []api.StreamEvent) api.StreamEvent {
	if len(events) == 0 {
		return api.StreamEvent{}
	}
	return events[len(events)-1]
}

func sh(script string) map[string]any {
	return map[string]any{"cmd": "sh", "args": []string{"-c", script}}
}
