package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
	"github.com/himanshubaliyan7/warmbox/internal/api"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

func TestAuthentication(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	other := h.create(keyA, nil)
	cmds := "/v1/sandboxes/" + sb.ID + "/commands"

	// Control plane: API key only, in either header.
	h.want(h.do("GET", "/v1/sandboxes", nil), 401, api.CodeUnauthorized)
	h.want(h.do("GET", "/v1/sandboxes", nil, withKey("sbx_deadbeef_0000")), 401, api.CodeUnauthorized)
	h.want(h.do("GET", "/v1/sandboxes", nil, withKey("garbage")), 401, api.CodeUnauthorized)
	r := h.want(h.do("GET", "/v1/sandboxes", nil, withHeader("Authorization", "Bearer "+keyA)), 200, "")
	if r.Header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id on a response")
	}
	r = h.do("GET", "/v1/templates", nil)
	if r.Status != 401 || r.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("unauthenticated: %d, WWW-Authenticate %q", r.Status, r.Header.Get("WWW-Authenticate"))
	}
	// The sandbox token is not a control-plane credential.
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withToken(sb.AccessToken)), 401, api.CodeUnauthorized)
	h.want(h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, withToken(sb.AccessToken)), 401, api.CodeUnauthorized)
	h.want(h.do("GET", "/v1/sandboxes", nil, withHeader("Authorization", "Bearer "+sb.AccessToken)), 401, api.CodeUnauthorized)

	// Data plane: the API key, or the token of that one sandbox, in a
	// header, as a bearer token or in the query string.
	h.want(h.do("GET", cmds, nil), 401, api.CodeUnauthorized)
	h.want(h.do("GET", cmds, nil, withKey(keyA)), 200, "")
	h.want(h.do("GET", cmds, nil, withToken(sb.AccessToken)), 200, "")
	h.want(h.do("GET", cmds, nil, withHeader("Authorization", "Bearer "+sb.AccessToken)), 200, "")
	h.want(h.do("GET", cmds+"?token="+sb.AccessToken, nil), 200, "")
	h.want(h.do("GET", cmds, nil, withToken(other.AccessToken)), 401, api.CodeUnauthorized)
	h.want(h.do("GET", cmds, nil, withToken("sbt_"+strings.Repeat("0", 64))), 401, api.CodeUnauthorized)
	// A token holder cannot tell an unknown sandbox from a wrong token.
	h.want(h.do("GET", "/v1/sandboxes/sb-0000000000000000/commands", nil, withToken(sb.AccessToken)), 401, api.CodeUnauthorized)

	// The token is 256 random bits, returned once and stored hashed.
	if len(sb.AccessToken) != len(api.AccessTokenPrefix)+64 || sb.AccessToken == other.AccessToken {
		t.Errorf("access tokens: %q %q", sb.AccessToken, other.AccessToken)
	}
	if hash := h.st.AccessTokenHash(sb.ID); len(hash) != 32 || bytes.Contains(hash, []byte(sb.AccessToken)) {
		t.Errorf("stored token hash: %x", hash)
	}
	var got api.SandboxView
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &got)
	if got.AccessToken != "" {
		t.Error("the access token was returned again by GET")
	}

	// Health endpoints need no credentials.
	h.want(h.do("GET", "/healthz", nil), 200, "")
	h.want(h.do("GET", "/readyz", nil), 200, "")
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t, options{})
	a := h.create(keyA, map[string]any{"template_id": tmplID, "metadata": map[string]string{"owner": "a"}})
	h.waitWarm(1)
	var warm string
	for _, sb := range h.st.Sandboxes() {
		if sb.State == core.StateWarm {
			warm = sb.ID
		}
	}
	base := "/v1/sandboxes/" + a.ID
	// Every route of A's sandbox is a 404 for B: not a 403, so B learns
	// nothing about what exists.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", base, nil},
		{"DELETE", base, nil},
		{"POST", base + "/timeout", map[string]any{"timeout_ms": 1000}},
		{"POST", base + "/timeout", `not json`}, // ownership is checked before the body
		{"PUT", base + "/network", map[string]any{"mode": "allowlist"}},
		{"PUT", base + "/network", map[string]any{"mode": "bogus"}},
		{"GET", base + "/events", nil},
		{"GET", base + "/stats", nil},
		{"POST", base + "/commands", sh("id")},
		{"GET", base + "/commands", nil},
		{"GET", base + "/commands/p1", nil},
		{"GET", base + "/commands/p1/stream", nil},
		{"POST", base + "/commands/p1/stdin", "x"},
		{"POST", base + "/commands/p1/close-stdin", nil},
		{"POST", base + "/commands/p1/signal", map[string]any{"signal": "SIGKILL"}},
		{"POST", base + "/commands/p1/resize", map[string]any{"cols": 80, "rows": 24}},
		{"GET", base + "/commands/p1/ws", nil},
		{"GET", base + "/files?path=x", nil},
		{"PUT", base + "/files?path=x", "data"},
		{"DELETE", base + "/files?path=x", nil},
		{"GET", base + "/files/list", nil},
		{"POST", base + "/files/mkdir?path=d", nil},
	} {
		r := h.do(c.method, c.path, c.body, withKey(keyB))
		if r.Status != 404 || r.errCode() != api.CodeNotFound {
			t.Errorf("tenant B %s %s: %d %s, want 404 not_found", c.method, c.path, r.Status, r.Body)
		}
	}
	// A warm sandbox belongs to nobody and is invisible to everybody.
	h.want(h.do("GET", "/v1/sandboxes/"+warm, nil, withKey(keyA)), 404, api.CodeNotFound)
	h.want(h.do("POST", "/v1/sandboxes/"+warm+"/commands", sh("id"), withKey(keyA)), 404, api.CodeNotFound)

	// Listings are per tenant.
	var list struct {
		Sandboxes []api.SandboxView `json:"sandboxes"`
	}
	h.want(h.do("GET", "/v1/sandboxes", nil, withKey(keyB)), 200, "").json(t, &list)
	if len(list.Sandboxes) != 0 {
		t.Errorf("tenant B sees %+v", list.Sandboxes)
	}
	h.want(h.do("GET", "/v1/sandboxes", nil, withKey(keyA)), 200, "").json(t, &list)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != a.ID {
		t.Errorf("tenant A sees %+v", list.Sandboxes)
	}
	// And A's sandbox is still there after all of B's attempts.
	h.want(h.do("GET", base, nil, withKey(keyA)), 200, "")
	if h.count(core.StateRunning) != 1 {
		t.Error("tenant B affected tenant A's sandbox")
	}
}

func TestSandboxLifecycleAndListing(t *testing.T) {
	h := newHarness(t, options{})
	a := h.create(keyA, map[string]any{"template_id": tmplID, "metadata": map[string]string{"job": "one", "team": "x"},
		"env": map[string]string{"GREETING": "hello"}, "timeout_ms": 600_000,
		"network": map[string]any{"mode": "allowlist", "allow_domains": []string{"pypi.org"}}})
	b := h.create(keyA, map[string]any{"template_id": tmplID, "metadata": map[string]string{"job": "two", "team": "x"}})

	if a.Network == nil || a.Network.Mode != core.NetworkAllowlist || len(a.Network.AllowDomains) != 1 || a.Metadata["job"] != "one" {
		t.Errorf("create view: %+v", a)
	}
	if a.ExpiresAt == nil || a.StartedAt == nil || a.ExpiresAt.Sub(*a.StartedAt) != 10*time.Minute {
		t.Errorf("timeout override: started %v expires %v", a.StartedAt, a.ExpiresAt)
	}
	if b.ExpiresAt == nil || b.ExpiresAt.Sub(*b.StartedAt) != time.Hour {
		t.Errorf("default lifetime: %v", b.ExpiresAt)
	}
	// The public view carries no internal fields.
	raw := h.do("GET", "/v1/sandboxes/"+a.ID, nil, withKey(keyA))
	for _, forbidden := range []string{"generation", "instance", "host_id", "access_token"} {
		if bytes.Contains(raw.Body, []byte(forbidden)) {
			t.Errorf("public view leaks %q: %s", forbidden, raw.Body)
		}
	}
	// The session env reached the agent.
	var res api.WaitResult
	h.want(h.do("POST", "/v1/sandboxes/"+a.ID+"/commands?wait=true", sh("echo $GREETING"), withKey(keyA)), 200, "").json(t, &res)
	if string(res.Stdout) != "hello\n" {
		t.Errorf("env: stdout %q", res.Stdout)
	}

	var list struct {
		Sandboxes []api.SandboxView `json:"sandboxes"`
	}
	ids := func(path string) []string {
		t.Helper()
		h.want(h.do("GET", path, nil, withKey(keyA)), 200, "").json(t, &list)
		var out []string
		for _, s := range list.Sandboxes {
			out = append(out, s.ID)
		}
		return out
	}
	if got := ids("/v1/sandboxes"); len(got) != 2 {
		t.Errorf("list: %v", got)
	}
	if got := ids("/v1/sandboxes?metadata=job%3Dtwo"); len(got) != 1 || got[0] != b.ID {
		t.Errorf("metadata filter: %v", got)
	}
	if got := ids("/v1/sandboxes?metadata=team%3Dx&metadata=job%3Done"); len(got) != 1 || got[0] != a.ID {
		t.Errorf("two metadata filters: %v", got)
	}
	if got := ids("/v1/sandboxes?template_id=nope"); len(got) != 0 {
		t.Errorf("template filter: %v", got)
	}
	if got := ids("/v1/sandboxes?limit=1"); len(got) != 1 {
		t.Errorf("limit: %v", got)
	}
	h.want(h.do("GET", "/v1/sandboxes?state=bogus", nil, withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("GET", "/v1/sandboxes?limit=0", nil, withKey(keyA)), 400, api.CodeBadRequest)

	// Delete destroys, is idempotent, and the sandbox stays readable.
	// With ?wait=true the teardown is over when the answer arrives.
	h.want(h.do("DELETE", "/v1/sandboxes/"+a.ID+"?wait=maybe", nil, withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("DELETE", "/v1/sandboxes/"+a.ID+"?wait=true", nil, withKey(keyA)), 204, "")
	h.want(h.do("DELETE", "/v1/sandboxes/"+a.ID, nil, withKey(keyA)), 204, "")
	var gone api.SandboxView
	h.want(h.do("GET", "/v1/sandboxes/"+a.ID, nil, withKey(keyA)), 200, "").json(t, &gone)
	if gone.State != "destroyed" || gone.Reason != "released" {
		t.Errorf("after delete: %+v", gone)
	}
	if _, ok := h.be.BySandbox(a.ID); ok {
		t.Error("instance still exists after delete")
	}
	if got := ids("/v1/sandboxes"); len(got) != 1 || got[0] != b.ID {
		t.Errorf("default listing after delete: %v", got)
	}
	if got := ids("/v1/sandboxes?state=destroyed"); len(got) != 1 || got[0] != a.ID {
		t.Errorf("state filter: %v", got)
	}
	if got := ids("/v1/sandboxes?state=all"); len(got) != 2 {
		t.Errorf("state=all: %v", got)
	}
	// Its data plane says why it is gone; the token still identifies it.
	r := h.want(h.do("POST", "/v1/sandboxes/"+a.ID+"/commands", sh("id"), withToken(a.AccessToken)), 409, api.CodeSandboxNotRunning)
	if d := r.errBody(t).Details; d["state"] != "destroyed" || d["reason"] != "released" {
		t.Errorf("details: %v", d)
	}
	h.want(h.do("POST", "/v1/sandboxes/"+a.ID+"/timeout", map[string]any{"timeout_ms": 1000}, withKey(keyA)), 409, api.CodeSandboxNotRunning)

	// Templates are readable by tenants, without pool sizing.
	tr := h.want(h.do("GET", "/v1/templates", nil, withKey(keyB)), 200, "")
	var tl struct {
		Templates []api.TemplateView `json:"templates"`
	}
	tr.json(t, &tl)
	if len(tl.Templates) != 1 || tl.Templates[0].ID != tmplID || tl.Templates[0].MaxLifetimeMs != 3_600_000 || tl.Templates[0].Network != "allowlist" {
		t.Errorf("templates: %s", tr.Body)
	}
	if bytes.Contains(tr.Body, []byte("warm_target")) {
		t.Errorf("template view leaks pool sizing: %s", tr.Body)
	}
}

func TestErrorMapping(t *testing.T) {
	h := newHarness(t, options{
		tenantA: func(tn *store.Tenant) { tn.MaxConcurrent = 1 },
		api:     func(c *api.Config) { c.MaxJSONBytes = 2048; c.MaxUploadBytes = 1024 },
	})
	create := func(body any) response { return h.do("POST", "/v1/sandboxes", body, withKey(keyA)) }

	h.want(create(`{"template_id":`), 400, api.CodeBadRequest)
	h.want(create(``), 400, api.CodeBadRequest)
	h.want(create(`{"template_id":"py","bogus":1}`), 400, api.CodeBadRequest)
	h.want(create(`{}`), 400, api.CodeBadRequest)
	h.want(create(`{"template_id":"nope"}`), 404, api.CodeTemplateNotFound)
	r := h.want(create(`{"template_id":"py","timeout_ms":7200000}`), 400, api.CodeInvalidTimeout)
	if d := r.errBody(t).Details; d["max_timeout_ms"] != float64(3_600_000) {
		t.Errorf("invalid_timeout details: %v", d)
	}
	h.want(create(`{"template_id":"py","timeout_ms":-1}`), 400, api.CodeInvalidTimeout)
	h.want(create(`{"template_id":"py","network":{"mode":"full"}}`), 400, api.CodeNetworkNotAllowed)
	h.want(create(`{"template_id":"py","network":{"mode":"sideways"}}`), 400, api.CodeBadRequest)
	h.want(create(`{"template_id":"py","network":{"mode":"allowlist","allow_cidrs":["not-a-cidr"]}}`), 400, api.CodeBadRequest)
	h.want(create(`{"template_id":"py","env":{"A=B":"x"}}`), 400, api.CodeBadRequest)
	h.want(create(`{"template_id":"py","metadata":{"k":"`+strings.Repeat("x", 4000)+`"}}`), 413, api.CodePayloadTooLarge)
	if n := h.count(core.StateWarm); n != 2 {
		t.Fatalf("rejected creates consumed sandboxes: %d warm", n)
	}

	// Quota: 429 with Retry-After, distinct from the rate limit.
	sb := h.create(keyA, nil)
	r = h.want(create(`{"template_id":"py"}`), 429, api.CodeQuotaExceeded)
	if r.Header.Get("Retry-After") == "" {
		t.Error("quota_exceeded without Retry-After")
	}

	// Unknown routes and methods use the same envelope.
	r = h.want(h.do("GET", "/v1/nope", nil, withKey(keyA)), 404, api.CodeNotFound)
	if r.errBody(t).RequestID == "" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("404 envelope: %s", r.Body)
	}
	r = h.want(h.do("PATCH", "/v1/sandboxes", nil, withKey(keyA)), 405, api.CodeMethodNotAllowed)
	if !strings.Contains(r.Header.Get("Allow"), "POST") {
		t.Errorf("405 Allow header: %q", r.Header.Get("Allow"))
	}
	// A caller-supplied request id is echoed in the header and the envelope.
	r = h.do("GET", "/v1/sandboxes/sb-nope", nil, withKey(keyA), withHeader("X-Request-Id", "req-123"))
	if r.Status != 404 || r.Header.Get("X-Request-Id") != "req-123" || r.errBody(t).RequestID != "req-123" {
		t.Errorf("request id: %d %v %s", r.Status, r.Header, r.Body)
	}
	r = h.do("GET", "/healthz", nil, withHeader("X-Request-Id", "bad id\twith spaces"))
	if id := r.Header.Get("X-Request-Id"); id == "" || strings.ContainsAny(id, " \t") {
		t.Errorf("unsafe request id echoed: %q", id)
	}

	// Agent errors keep their status and code; not_found is made specific.
	base := "/v1/sandboxes/" + sb.ID
	h.want(h.do("GET", base+"/commands/p-nope", nil, withKey(keyA)), 404, api.CodeProcessNotFound)
	h.want(h.do("GET", base+"/commands/p-nope/stream", nil, withKey(keyA)), 404, api.CodeProcessNotFound)
	h.want(h.do("POST", base+"/commands/p-nope/signal", map[string]any{"signal": "SIGTERM"}, withKey(keyA)), 404, api.CodeProcessNotFound)
	h.want(h.do("GET", base+"/files?path=missing.txt", nil, withKey(keyA)), 404, api.CodeFileNotFound)
	h.want(h.do("GET", base+"/files", nil, withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("POST", base+"/commands", map[string]any{"cmd": "definitely-not-a-command"}, withKey(keyA)), 400, "command_not_found")
	h.want(h.do("POST", base+"/commands", map[string]any{"args": []string{"x"}}, withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("POST", base+"/commands?wait=maybe", sh("id"), withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("GET", base+"/commands/p-nope/stream?from=-1", nil, withKey(keyA)), 400, api.CodeBadRequest)
	// Upload limit, with and without a declared length.
	h.want(h.do("PUT", base+"/files?path=big.bin", bytes.Repeat([]byte("x"), 2000), withKey(keyA)), 413, api.CodePayloadTooLarge)
	h.want(h.do("PUT", base+"/files?path=big.bin", io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("x"), 2000))), withKey(keyA)), 413, api.CodePayloadTooLarge)
	h.want(h.do("GET", base+"/files?path=big.bin", nil, withKey(keyA)), 404, api.CodeFileNotFound)
}

func TestPoolEmptyIs503WithRetryAfter(t *testing.T) {
	h := newHarness(t, options{noStart: true, template: func(tm *core.Template) { tm.WarmTarget = 0 },
		pool: func(c *pool.Config) { c.ClaimWait = 50 * time.Millisecond }})
	h.be.SetCreateHang(true) // nothing ever becomes warm
	r := h.want(h.do("POST", "/v1/sandboxes", map[string]any{"template_id": tmplID}, withKey(keyA)), 503, api.CodePoolEmpty)
	if r.Header.Get("Retry-After") == "" {
		t.Error("pool_empty without Retry-After")
	}
}

func TestRateLimitBucketsAreSeparatePerTenantAndClass(t *testing.T) {
	h := newHarness(t, options{tenantA: func(tn *store.Tenant) { tn.RatePerSec, tn.Burst = 0.5, 3 }})

	// The default bucket: 3 requests, then 429 with Retry-After.
	for i := 0; i < 3; i++ {
		h.want(h.do("GET", "/v1/sandboxes", nil, withKey(keyA)), 200, "")
	}
	r := h.want(h.do("GET", "/v1/sandboxes", nil, withKey(keyA)), 429, api.CodeRateLimited)
	secs, err := strconv.Atoi(r.Header.Get("Retry-After"))
	if err != nil || secs < 1 || secs > 2 {
		t.Errorf("Retry-After = %q, want 1 or 2 seconds at 0.5 tokens/s", r.Header.Get("Retry-After"))
	}
	if d := r.errBody(t).Details; d["class"] != "default" {
		t.Errorf("details: %v", d)
	}
	// The creation bucket is untouched by that: 3 creates pass.
	var sb api.SandboxView
	for i := 0; i < 3; i++ {
		h.waitWarm(1)
		sb = h.create(keyA, nil)
	}
	r = h.want(h.do("POST", "/v1/sandboxes", map[string]any{"template_id": tmplID}, withKey(keyA)), 429, api.CodeRateLimited)
	if d := r.errBody(t).Details; d["class"] != "create" || r.Header.Get("Retry-After") == "" {
		t.Errorf("create bucket: %v %v", d, r.Header)
	}
	// Requests made with the sandbox token count against the same tenant bucket.
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID+"/commands", nil, withToken(sb.AccessToken)), 429, api.CodeRateLimited)
	// Another tenant has its own buckets (and no limit at all here).
	for i := 0; i < 10; i++ {
		h.want(h.do("GET", "/v1/sandboxes", nil, withKey(keyB)), 200, "")
	}
	// The limiter is visible in the metrics.
	m := string(h.do("GET", "/metrics", nil).Body)
	for _, want := range []string{`sbx_http_rate_limited_total{class="default"} 2`, `sbx_http_rate_limited_total{class="create"} 1`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestCommandsStreamWaitAndRecord(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	tok := withToken(sb.AccessToken)

	// wait=stream: start, output with separate stdout and stderr, exit.
	resp := h.raw("POST", base+"?wait=stream", sh("echo out; echo err >&2; exit 3"), tok)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readStream(t, resp.Body)
	resp.Body.Close()
	if len(events) < 4 || events[0].Type != api.EventStart || events[0].Command == nil || events[0].Command.Pid == "" {
		t.Fatalf("events: %+v", events)
	}
	if output(events, "stdout") != "out\n" || output(events, "stderr") != "err\n" {
		t.Errorf("stdout %q stderr %q", output(events, "stdout"), output(events, "stderr"))
	}
	if e := last(events); e.Type != "exit" || e.Exit == nil || e.Exit.ExitCode != 3 || e.Exit.Reason != "exited" || e.Offset != 8 {
		t.Errorf("last event: %+v", e)
	}
	pid := events[0].Command.Pid

	// wait=true: one JSON document.
	var res api.WaitResult
	h.want(h.do("POST", base+"?wait=true", sh("printf abc; printf xyz >&2"), tok), 200, "").json(t, &res)
	if string(res.Stdout) != "abc" || string(res.Stderr) != "xyz" || res.Exit == nil || res.Exit.ExitCode != 0 || res.Truncated || res.NextOffset != 6 {
		t.Errorf("wait result: %+v", res)
	}
	// Per-command timeout: killed, reason timeout.
	h.want(h.do("POST", base+"?wait=true", map[string]any{"cmd": "sleep", "args": []string{"30"}, "timeout_ms": 150}, tok), 200, "").json(t, &res)
	if res.Exit == nil || res.Exit.Reason != "timeout" || res.Exit.Signal != "SIGKILL" {
		t.Errorf("timeout: %+v", res.Exit)
	}

	// Default: returns at once; the command is then inspectable.
	var cmd api.CommandView
	r := h.want(h.do("POST", base, map[string]any{"cmd": "sleep", "args": []string{"30"}, "tag": "napper"}, tok), 201, "")
	r.json(t, &cmd)
	if cmd.Pid == "" || cmd.Exit != nil || cmd.Tag != "napper" || r.Header.Get("Location") != base+"/"+cmd.Pid {
		t.Errorf("start: %+v, Location %q", cmd, r.Header.Get("Location"))
	}
	h.want(h.do("POST", base, map[string]any{"cmd": "sleep", "args": []string{"30"}, "tag": "napper"}, tok), 409, "tag_in_use")
	var got api.CommandView
	h.want(h.do("GET", base+"/"+cmd.Pid, nil, tok), 200, "").json(t, &got)
	if got.Pid != cmd.Pid || got.Exit != nil {
		t.Errorf("get: %+v", got)
	}
	var list struct {
		Commands []api.CommandView `json:"commands"`
	}
	h.want(h.do("GET", base, nil, tok), 200, "").json(t, &list)
	if len(list.Commands) != 4 {
		t.Errorf("list: %d commands", len(list.Commands))
	}
	// Signal it and watch it end on the stream.
	h.want(h.do("POST", base+"/"+cmd.Pid+"/signal", map[string]any{"signal": "SIGTERM"}, tok), 204, "")
	resp = h.raw("GET", base+"/"+cmd.Pid+"/stream", nil, tok)
	events = readStream(t, resp.Body)
	resp.Body.Close()
	if e := last(events); e.Type != "exit" || e.Exit == nil || e.Exit.Reason != "killed" || e.Exit.Signal != "SIGTERM" {
		t.Errorf("after SIGTERM: %+v", events)
	}
	h.want(h.do("POST", base+"/"+cmd.Pid+"/signal", map[string]any{"signal": "SIGNOPE"}, tok), 400, "")
	h.want(h.do("POST", base+"/"+cmd.Pid+"/resize", map[string]any{"cols": 80, "rows": 24}, tok), 409, "not_a_pty")

	// stdin and close-stdin.
	h.want(h.do("POST", base, map[string]any{"cmd": "cat", "stdin": true}, tok), 201, "").json(t, &cmd)
	h.want(h.do("POST", base+"/"+cmd.Pid+"/stdin", "hello ", tok), 204, "")
	h.want(h.do("POST", base+"/"+cmd.Pid+"/stdin", "stdin\n", tok), 204, "")
	h.want(h.do("POST", base+"/"+cmd.Pid+"/close-stdin", nil, tok), 204, "")
	resp = h.raw("GET", base+"/"+cmd.Pid+"/stream", nil, tok)
	events = readStream(t, resp.Body)
	resp.Body.Close()
	if output(events, "stdout") != "hello stdin\n" || last(events).Exit == nil || last(events).Exit.ExitCode != 0 {
		t.Errorf("cat: %+v", events)
	}

	// The execution record: one started and one finished event per
	// command, with exit code, reason, duration and byte counts.
	var finished map[string]any
	eventually(t, "the execution record of the first command", func() bool {
		var ev struct {
			Events []api.EventView `json:"events"`
		}
		h.want(h.do("GET", "/v1/sandboxes/"+sb.ID+"/events?type="+api.EventCommandFinished, nil, withKey(keyA)), 200, "").json(t, &ev)
		for _, e := range ev.Events {
			if e.Data["pid"] == pid {
				finished = e.Data
			}
		}
		return finished != nil
	})
	if finished["exit_code"] != float64(3) || finished["reason"] != "exited" || finished["stdout_bytes"] != float64(4) ||
		finished["stderr_bytes"] != float64(4) || finished["cmd"] != "sh" || finished["duration_ms"] == nil {
		t.Errorf("execution record: %v", finished)
	}
	var ev struct {
		Events []api.EventView `json:"events"`
	}
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID+"/events", nil, withKey(keyA)), 200, "").json(t, &ev)
	started := 0
	for _, e := range ev.Events {
		if e.Type == api.EventCommandStarted {
			started++
		}
		if e.Type == "created" {
			t.Error("the pre-assignment history is visible to the tenant")
		}
		if bytes.Contains(mustJSON(e), []byte("GREETING")) {
			t.Error("an execution record contains the environment")
		}
	}
	if started != 5 || ev.Events[0].Type != "claimed" || ev.Events[0].ToState != "assigned" {
		t.Errorf("events: %d started, first %+v", started, ev.Events[0])
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestStreamReconnectWithFrom(t *testing.T) {
	h := newHarness(t, options{keepalive: 40 * time.Millisecond})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	tok := withToken(sb.AccessToken)

	var cmd api.CommandView
	h.want(h.do("POST", base, map[string]any{"cmd": "sh", "args": []string{"-c", "echo one; read x; echo two-$x"}, "stdin": true}, tok), 201, "").json(t, &cmd)

	// First connection: read "one", see a keepalive while the command
	// waits, then drop the connection mid-stream.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+base+"/"+cmd.Pid+"/stream", nil)
	req.Header.Set("X-Access-Token", sb.AccessToken)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(resp.Body)
	var next int64
	sawKeepalive := false
	for !sawKeepalive || next == 0 {
		var ev api.StreamEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("first connection: %v", err)
		}
		switch ev.Type {
		case "stdout":
			if string(ev.Data) != "one\n" || ev.Offset != 0 {
				t.Fatalf("first output: %+v", ev)
			}
			next = ev.Offset + int64(len(ev.Data))
		case "keepalive":
			sawKeepalive = true
		default:
			t.Fatalf("unexpected event %+v", ev)
		}
	}
	cancel()
	resp.Body.Close()

	// The command did not notice. Let it finish while nobody is attached.
	h.want(h.do("POST", base+"/"+cmd.Pid+"/stdin", "go\n", tok), 204, "")
	eventually(t, "the command to exit", func() bool {
		var c api.CommandView
		h.want(h.do("GET", base+"/"+cmd.Pid, nil, tok), 200, "").json(t, &c)
		return c.Exit != nil
	})

	// Reconnect from the offset reached: only what was missed, then exit.
	resp = h.raw("GET", fmt.Sprintf("%s/%s/stream?from=%d", base, cmd.Pid, next), nil, tok)
	events := readStream(t, resp.Body)
	resp.Body.Close()
	if len(events) != 2 || string(events[0].Data) != "two-go\n" || events[0].Offset != next || events[1].Type != "exit" || events[1].Offset != 11 {
		t.Errorf("replay from %d: %+v", next, events)
	}
	// From zero: the full replay. From the end: just the exit.
	resp = h.raw("GET", base+"/"+cmd.Pid+"/stream?from=0", nil, tok)
	events = readStream(t, resp.Body)
	resp.Body.Close()
	if output(events, "stdout") != "one\ntwo-go\n" || last(events).Type != "exit" {
		t.Errorf("full replay: %+v", events)
	}
	resp = h.raw("GET", base+"/"+cmd.Pid+"/stream?from=11", nil, tok)
	events = readStream(t, resp.Body)
	resp.Body.Close()
	if len(events) != 1 || events[0].Type != "exit" || events[0].Exit.ExitCode != 0 {
		t.Errorf("replay from the end: %+v", events)
	}
}

func TestFiles(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/files"
	tok := withToken(sb.AccessToken)

	payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	// mkdir says whether it made the directory: 201, or 204 if it was there.
	h.want(h.do("POST", base+"/mkdir?path=data/in", nil, tok), 201, "")
	h.want(h.do("POST", base+"/mkdir?path=data/in", nil, tok), 204, "")
	h.want(h.do("POST", base+"/mkdir?path=data", nil, tok), 204, "")
	h.want(h.do("PUT", base+"?path=data/in/blob.bin&mode=0600", payload, tok), 204, "")
	r := h.want(h.do("GET", base+"?path=data/in/blob.bin", nil, tok), 200, "")
	if !bytes.Equal(r.Body, payload) || r.Header.Get("Content-Length") != strconv.Itoa(len(payload)) ||
		r.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("download: %d bytes, headers %v", len(r.Body), r.Header)
	}
	var list struct {
		Files []api.FileView `json:"files"`
	}
	h.want(h.do("GET", base+"/list?path=data/in", nil, tok), 200, "").json(t, &list)
	if len(list.Files) != 1 || list.Files[0].Name != "blob.bin" || list.Files[0].Size != int64(len(payload)) ||
		list.Files[0].Mode != "0600" || list.Files[0].IsDir {
		t.Errorf("list: %+v", list.Files)
	}
	// A file written through the API is visible to a command, and back.
	var res api.WaitResult
	h.want(h.do("POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=true", sh("wc -c < data/in/blob.bin; echo made-here > note.txt"), tok), 200, "").json(t, &res)
	if strings.TrimSpace(string(res.Stdout)) != strconv.Itoa(len(payload)) {
		t.Errorf("wc: %q %q", res.Stdout, res.Stderr)
	}
	if r := h.want(h.do("GET", base+"?path=note.txt", nil, tok), 200, ""); string(r.Body) != "made-here\n" {
		t.Errorf("note: %q", r.Body)
	}
	h.want(h.do("PUT", base+"?path=x&mode=9999", "x", tok), 400, api.CodeBadRequest)
	h.want(h.do("DELETE", base+"?path=data", nil, tok), 204, "")
	h.want(h.do("GET", base+"?path=data/in/blob.bin", nil, tok), 404, api.CodeFileNotFound)
	h.want(h.do("GET", base+"/list", nil, tok), 200, "").json(t, &list)
	if len(list.Files) != 1 || list.Files[0].Name != "note.txt" {
		t.Errorf("root listing: %+v", list.Files)
	}
}

// wsFrameLimit is the largest frame the gateway relays to a client: one
// channel byte plus agentapi.MaxFrameBytes of output. It is one byte over
// the default read limit of this WebSocket library, so clients raise it.
const wsFrameLimit = agentapi.MaxFrameBytes + 1

// wsRecv reads frames until one on channel ch arrives.
func wsRecv(t *testing.T, ctx context.Context, c *websocket.Conn, ch byte) []byte {
	t.Helper()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("ws read (waiting for channel %d): %v", ch, err)
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			t.Fatalf("ws frame: type %v, %d bytes", typ, len(data))
		}
		if data[0] == ch {
			return data[1:]
		}
	}
}

// wsUntil accumulates output frames until the output contains want.
func wsUntil(t *testing.T, ctx context.Context, c *websocket.Conn, want string) string {
	t.Helper()
	var out strings.Builder
	for !strings.Contains(out.String(), want) {
		out.Write(wsRecv(t, ctx, c, agentapi.ChannelStdout))
	}
	return out.String()
}

func wsSend(t *testing.T, ctx context.Context, c *websocket.Conn, ch byte, payload []byte) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageBinary, append([]byte{ch}, payload...)); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func TestWebSocketProxyRoundTrip(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	wsURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + base
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var cmd api.CommandView
	h.want(h.do("POST", base, map[string]any{"cmd": "sh", "pty": map[string]int{"cols": 80, "rows": 24},
		"env": map[string]string{"PS1": "$ "}}, withToken(sb.AccessToken)), 201, "").json(t, &cmd)
	if !cmd.PTY {
		t.Fatalf("not a PTY: %+v", cmd)
	}

	// The handshake is authenticated like every other data-plane call.
	for name, opts := range map[string]*websocket.DialOptions{
		"no credentials": nil,
		"wrong token":    {HTTPHeader: http.Header{"X-Access-Token": {"sbt_" + strings.Repeat("0", 64)}}},
	} {
		_, resp, err := websocket.Dial(ctx, wsURL+"/"+cmd.Pid+"/ws", opts)
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("%s: err %v, response %v", name, err, resp)
		}
	}
	_, resp, err := websocket.Dial(ctx, wsURL+"/p-nope/ws?token="+sb.AccessToken, nil)
	if err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("unknown process: err %v, response %v", err, resp)
	}

	// A browser cannot set headers: the token travels as a subprotocol.
	c, _, err := websocket.Dial(ctx, wsURL+"/"+cmd.Pid+"/ws", &websocket.DialOptions{
		Subprotocols: []string{api.WSSubprotocol, "sbx.token." + sb.AccessToken}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(wsFrameLimit)
	if c.Subprotocol() != api.WSSubprotocol {
		t.Errorf("negotiated subprotocol %q", c.Subprotocol())
	}
	// First frame: the agent's offset frame, relayed unchanged.
	var off agentapi.OffsetFrame
	if err := json.Unmarshal(wsRecv(t, ctx, c, agentapi.ChannelOffset), &off); err != nil || off.Offset != 0 {
		t.Fatalf("offset frame: %+v %v", off, err)
	}
	// stdin -> PTY -> stdout.
	wsSend(t, ctx, c, agentapi.ChannelStdin, []byte("stty size\n"))
	if out := wsUntil(t, ctx, c, "24 80"); !strings.Contains(out, "24 80") {
		t.Fatalf("stty size: %q", out)
	}
	// Resize travels on its own channel.
	wsSend(t, ctx, c, agentapi.ChannelResize, mustJSON(agentapi.ResizeRequest{Cols: 132, Rows: 43}))
	wsSend(t, ctx, c, agentapi.ChannelStdin, []byte("stty size\n"))
	wsUntil(t, ctx, c, "43 132")
	// Application-level ping is echoed by the agent through the gateway.
	wsSend(t, ctx, c, agentapi.ChannelPing, []byte("marco"))
	if got := wsRecv(t, ctx, c, agentapi.ChannelPong); string(got) != "marco" {
		t.Errorf("pong: %q", got)
	}
	// Ctrl+C interrupts a flood and the shell stays usable.
	wsSend(t, ctx, c, agentapi.ChannelStdin, []byte("yes\n"))
	wsUntil(t, ctx, c, "y\r\ny\r\ny")
	wsSend(t, ctx, c, agentapi.ChannelStdin, []byte{0x03})
	wsSend(t, ctx, c, agentapi.ChannelStdin, []byte("echo back-$((40+2))\n"))
	wsUntil(t, ctx, c, "back-42")

	// Detach and reattach from the current offset: the session survives.
	var info api.CommandView
	h.want(h.do("GET", base+"/"+cmd.Pid, nil, withToken(sb.AccessToken)), 200, "").json(t, &info)
	c.CloseNow()
	c2, _, err := websocket.Dial(ctx, fmt.Sprintf("%s/%s/ws?from=%d&token=%s", wsURL, cmd.Pid, info.NextOffset, sb.AccessToken), nil)
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	defer c2.CloseNow()
	c2.SetReadLimit(wsFrameLimit)
	if err := json.Unmarshal(wsRecv(t, ctx, c2, agentapi.ChannelOffset), &off); err != nil || off.Offset < info.NextOffset {
		t.Fatalf("offset after reattach: %+v (want >= %d) %v", off, info.NextOffset, err)
	}
	wsSend(t, ctx, c2, agentapi.ChannelStdin, []byte("exit 7\n"))
	var status agentapi.StatusFrame
	if err := json.Unmarshal(wsRecv(t, ctx, c2, agentapi.ChannelStatus), &status); err != nil {
		t.Fatal(err)
	}
	if status.Exit == nil || status.Exit.ExitCode != 7 || status.Exit.Reason != agentapi.ExitExited {
		t.Errorf("status frame: %+v", status)
	}
	// After the status frame the gateway closes normally.
	if _, _, err := c2.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("close after exit: %v", err)
	}
	m := string(h.do("GET", "/metrics", nil).Body)
	if !strings.Contains(m, `sbx_http_active_streams{kind="ws"}`) {
		t.Error("metrics lack the ws stream gauge")
	}
}

// A slow WebSocket consumer must pause the producer through the
// protocol's ACKs instead of growing memory in the gateway: the agent
// stops reading from the process once FlowHighWatermark bytes are
// unacknowledged.
func TestWebSocketBackpressureIsEndToEnd(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID + "/commands"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var cmd api.CommandView
	h.want(h.do("POST", base, map[string]any{"cmd": "sh", "args": []string{"-c", "yes 0123456789abcdef"},
		"pty": map[string]int{"cols": 80, "rows": 24}}, withToken(sb.AccessToken)), 201, "").json(t, &cmd)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+base+"/"+cmd.Pid+"/ws?token="+sb.AccessToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(wsFrameLimit)
	wsRecv(t, ctx, c, agentapi.ChannelOffset)
	// Turn flow control on by acknowledging nothing, then read slowly.
	wsSend(t, ctx, c, agentapi.ChannelAck, mustJSON(agentapi.AckFrame{Offset: 0}))
	var received int64
	for i := 0; i < 3; i++ {
		received += int64(len(wsRecv(t, ctx, c, agentapi.ChannelStdout)))
	}
	time.Sleep(300 * time.Millisecond) // the process is free to run meanwhile
	var info api.CommandView
	h.want(h.do("GET", base+"/"+cmd.Pid, nil, withToken(sb.AccessToken)), 200, "").json(t, &info)
	// With nothing acknowledged, the process may get at most the high
	// watermark ahead (plus what was in flight in kernel and PTY buffers).
	if limit := int64(agentapi.FlowHighWatermark + 256<<10); info.NextOffset > limit {
		t.Errorf("process produced %d bytes with nothing acknowledged; want <= %d", info.NextOffset, limit)
	}
	// Acknowledging lets it run again.
	before := info.NextOffset
	for i := 0; i < 400; i++ {
		// Acknowledge every frame: the agent resumes only once fewer than
		// FlowLowWatermark bytes are unacknowledged, so a client that
		// acknowledged in larger steps could wait for output forever.
		received += int64(len(wsRecv(t, ctx, c, agentapi.ChannelStdout)))
		wsSend(t, ctx, c, agentapi.ChannelAck, mustJSON(agentapi.AckFrame{Offset: received}))
	}
	h.want(h.do("GET", base+"/"+cmd.Pid, nil, withToken(sb.AccessToken)), 200, "").json(t, &info)
	if info.NextOffset <= before {
		t.Errorf("process did not resume after acknowledgements: %d -> %d", before, info.NextOffset)
	}
	wsSend(t, ctx, c, agentapi.ChannelSignal, mustJSON(agentapi.SignalRequest{Signal: "SIGKILL"}))
	var status agentapi.StatusFrame
	if err := json.Unmarshal(wsRecv(t, ctx, c, agentapi.ChannelStatus), &status); err != nil || status.Exit == nil || status.Exit.Reason != agentapi.ExitKilled {
		t.Errorf("status after SIGKILL: %+v %v", status, err)
	}
}

func TestTimeoutEndpointStaysWithinTemplateLimits(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, map[string]any{"template_id": tmplID, "timeout_ms": 60_000})
	path := "/v1/sandboxes/" + sb.ID + "/timeout"
	var v api.SandboxView

	h.want(h.do("POST", path, map[string]any{"timeout_ms": 0}, withKey(keyA)), 400, api.CodeInvalidTimeout)
	h.want(h.do("POST", path, `{}`, withKey(keyA)), 400, api.CodeInvalidTimeout)

	// Extend: counted from now.
	h.want(h.do("POST", path, map[string]any{"timeout_ms": 1_800_000}, withKey(keyA)), 200, "").json(t, &v)
	if d := time.Until(*v.ExpiresAt); d < 29*time.Minute || d > 30*time.Minute {
		t.Errorf("extended: expires in %s", d)
	}
	// Past the template's max lifetime: capped at assignment + max.
	h.want(h.do("POST", path, map[string]any{"timeout_ms": 86_400_000}, withKey(keyA)), 200, "").json(t, &v)
	if got := v.ExpiresAt.Sub(*v.StartedAt); got != time.Hour {
		t.Errorf("capped: %s after start, want 1h", got)
	}
	// Shorten, and the sweeper ends the sandbox with the right reason.
	h.want(h.do("POST", path, map[string]any{"timeout_ms": 50}, withKey(keyA)), 200, "")
	eventually(t, "max lifetime to end the sandbox", func() bool {
		h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &v)
		return v.State == "destroyed"
	})
	if v.Reason != string(core.ReasonMaxLifetime) {
		t.Errorf("reason %q, want max_lifetime", v.Reason)
	}
}

func TestNetworkPolicyUpdate(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, map[string]any{"template_id": tmplID, "network": map[string]any{"mode": "allowlist", "allow_domains": []string{"pypi.org"}}})
	none := h.create(keyA, map[string]any{"template_id": tmplID, "network": map[string]any{"mode": "none"}})
	path := "/v1/sandboxes/" + sb.ID + "/network"

	var v api.SandboxView
	h.want(h.do("PUT", path, map[string]any{"mode": "allowlist", "allow_domains": []string{"example.com", "*.example.org"}, "deny_cidrs": []string{"203.0.113.0/24"}},
		withKey(keyA)), 200, "").json(t, &v)
	if v.Network == nil || len(v.Network.AllowDomains) != 2 || len(v.Network.DenyCIDRs) != 1 {
		t.Errorf("view after update: %+v", v.Network)
	}
	if att, ok := h.egress.Attached(sb.ID); !ok || len(att.Policy.AllowDomains) != 2 {
		t.Errorf("proxy policy: %+v %v", att, ok)
	}
	// Lock down: mode none on a proxied sandbox refuses everything.
	h.want(h.do("PUT", path, map[string]any{"mode": "none"}, withKey(keyA)), 200, "").json(t, &v)
	if att, _ := h.egress.Attached(sb.ID); att.Policy.Mode != core.NetworkNone || v.Network.Mode != core.NetworkNone {
		t.Errorf("lock down: proxy %+v view %+v", att.Policy, v.Network)
	}
	h.want(h.do("PUT", path, map[string]any{"mode": "full"}, withKey(keyA)), 400, api.CodeNetworkNotAllowed)
	// ... and the lock-down is reversible.
	h.want(h.do("PUT", path, map[string]any{"mode": "allowlist", "allow_domains": []string{"pypi.org"}}, withKey(keyA)), 200, "").json(t, &v)
	if att, _ := h.egress.Attached(sb.ID); att.Policy.Mode != core.NetworkAllowlist || len(v.Network.AllowDomains) != 1 {
		t.Errorf("re-open: proxy %+v view %+v", att.Policy, v.Network)
	}
	h.want(h.do("PUT", path, map[string]any{"mode": "allowlist", "allow_domains": []string{"bad domain!"}}, withKey(keyA)), 400, api.CodeBadRequest)
	// The sandbox token cannot change the policy that confines the sandbox.
	h.want(h.do("PUT", path, map[string]any{"mode": "allowlist"}, withToken(sb.AccessToken)), 401, api.CodeUnauthorized)
	// A sandbox created without a proxy cannot get one later.
	h.want(h.do("PUT", "/v1/sandboxes/"+none.ID+"/network", map[string]any{"mode": "allowlist"}, withKey(keyA)), 409, api.CodeNetworkNotAttached)
	if _, ok := h.egress.Attached(none.ID); ok {
		t.Error("a mode-none sandbox has a proxy attachment")
	}
}

func TestLostSandboxIsReportedWithItsReason(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	base := "/v1/sandboxes/" + sb.ID
	tok := withToken(sb.AccessToken)

	// A command is streaming when the sandbox is OOM-killed.
	resp := h.raw("POST", base+"/commands?wait=stream", sh("echo started; sleep 30"), tok)
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	var ev api.StreamEvent
	for ev.Type != "stdout" {
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
	}
	inst, _ := h.be.BySandbox(sb.ID)
	h.be.KillInstance(inst.ID, 137, true)
	h.agents.kill(h.runDir(sb.ID))

	// The stream ends with an error event that names the cause.
	for ev.Type != api.EventError {
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("stream ended without an error event: %v", err)
		}
	}
	resp.Body.Close()
	if ev.Error == nil || ev.Error.Code != api.CodeSandboxTerminated || ev.Error.Details["reason"] != "oom_killed" {
		t.Errorf("error event: %+v", ev.Error)
	}
	// The sandbox is destroyed with that reason, without waiting for the
	// reconciler (which is effectively off in this harness).
	var v api.SandboxView
	eventually(t, "the sandbox to be destroyed", func() bool {
		h.want(h.do("GET", base, nil, withKey(keyA)), 200, "").json(t, &v)
		return v.State == "destroyed"
	})
	if v.Reason != "oom_killed" {
		t.Errorf("reason %q, want oom_killed", v.Reason)
	}
	r := h.want(h.do("POST", base+"/commands", sh("id"), tok), 409, api.CodeSandboxNotRunning)
	if d := r.errBody(t).Details; d["reason"] != "oom_killed" {
		t.Errorf("details: %v", d)
	}
	// The interrupted command has an execution record too.
	eventually(t, "the execution record of the interrupted command", func() bool {
		for _, e := range h.st.Events(sb.ID) {
			if e.Type == api.EventCommandFinished && e.Data["reason"] == "sandbox_terminated" {
				return true
			}
		}
		return false
	})
	// The server keeps serving: the pool refilled and a new claim works.
	h.waitWarm(1)
	next := h.create(keyA, nil)
	var res api.WaitResult
	h.want(h.do("POST", "/v1/sandboxes/"+next.ID+"/commands?wait=true", sh("echo alive"), withToken(next.AccessToken)), 200, "").json(t, &res)
	if string(res.Stdout) != "alive\n" {
		t.Errorf("after the loss: %q", res.Stdout)
	}

	// A sandbox whose agent is gone but whose instance is fine is a 502,
	// not a termination.
	h.agents.kill(h.runDir(next.ID))
	r = h.want(h.do("GET", "/v1/sandboxes/"+next.ID+"/commands", nil, withToken(next.AccessToken)), 502, api.CodeAgentUnavailable)
	if r.Header.Get("Retry-After") == "" {
		t.Error("agent_unavailable without Retry-After")
	}
}

func TestActivityKeepsIdleTimerAtBay(t *testing.T) {
	h := newHarness(t, options{template: func(tm *core.Template) { tm.Timeouts.Idle = 400 * time.Millisecond }})
	busy := h.create(keyA, nil)
	idle := h.create(keyA, nil)
	// Data-plane calls on one sandbox for three idle periods.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		h.want(h.do("GET", "/v1/sandboxes/"+busy.ID+"/commands", nil, withToken(busy.AccessToken)), 200, "")
		time.Sleep(50 * time.Millisecond)
	}
	var v api.SandboxView
	h.want(h.do("GET", "/v1/sandboxes/"+busy.ID, nil, withKey(keyA)), 200, "").json(t, &v)
	if v.State != "running" {
		t.Errorf("the active sandbox is %s (%s)", v.State, v.Reason)
	}
	h.want(h.do("GET", "/v1/sandboxes/"+idle.ID, nil, withKey(keyA)), 200, "").json(t, &v)
	if v.State != "destroyed" || v.Reason != "idle_timeout" {
		t.Errorf("the idle sandbox is %s (%s), want destroyed (idle_timeout)", v.State, v.Reason)
	}
	// Control-plane reads are not activity: the busy one now idles out.
	eventually(t, "the once-busy sandbox to idle out", func() bool {
		h.want(h.do("GET", "/v1/sandboxes/"+busy.ID, nil, withKey(keyA)), 200, "").json(t, &v)
		return v.State == "destroyed"
	})
	if v.Reason != "idle_timeout" {
		t.Errorf("reason %q", v.Reason)
	}
}

func TestStatsAndMetrics(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	var st api.StatsView
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID+"/stats", nil, withKey(keyA)), 200, "").json(t, &st)

	h.do("GET", "/v1/nope", nil)
	body := string(h.do("GET", "/metrics", nil).Body)
	for _, want := range []string{
		`sbx_http_requests_total{method="POST",route="/v1/sandboxes",code="201"} 1`,
		`sbx_http_requests_total{method="unmatched",route="",code="404"} 1`,
		`sbx_http_request_duration_seconds_bucket{method="POST",route="/v1/sandboxes",le="+Inf"} 1`,
		`sbx_http_request_duration_seconds_count{method="POST",route="/v1/sandboxes"} 1`,
		`sbx_http_active_streams{kind="ndjson"} 0`,
		`sbx_pool_claims_total{result="ok"} 1`,
		`sbx_pool_claim_wait_seconds_count 1`,
		`sbx_pool_claim_wait_seconds_bucket{le="+Inf"} 1`,
		`sbx_pool_warm_target{template="py"} 2`,
		`sbx_pool_sandboxes{template="py",state="warm"}`,
		`# TYPE sbx_pool_claim_wait_seconds histogram`,
		`sbx_api_sessions 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	// Every sample line is "name{labels} value" with a numeric value.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("malformed metrics line %q", line)
		}
		if _, err := strconv.ParseFloat(line[i+1:], 64); err != nil {
			t.Fatalf("malformed metrics value in %q", line)
		}
	}
}

func TestCloseEndsStreams(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	resp := h.raw("POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=stream", sh("echo up; sleep 30"), withToken(sb.AccessToken))
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var ev api.StreamEvent
	for ev.Type != "stdout" {
		if err := dec.Decode(&ev); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() { h.api.Close(); close(done) }()
	for ev.Type != api.EventError {
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("stream ended without an error event on shutdown")
			}
			t.Fatal(err)
		}
	}
	if ev.Error.Code != api.CodeShuttingDown {
		t.Errorf("error event: %+v", ev.Error)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	h.want(h.do("GET", "/readyz", nil), 503, api.CodeShuttingDown)
}
