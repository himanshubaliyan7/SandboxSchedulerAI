package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/api"
	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/promtext"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// TestDeleteAnswersOnceFenced: DELETE does not wait for the container to
// be destroyed. It answers when the sandbox is fenced (recycling, token
// dead, egress detached) and the teardown finishes in the background;
// ?wait=true keeps the old, synchronous behaviour.
func TestDeleteAnswersOnceFenced(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, map[string]any{"template_id": tmplID, "network": map[string]any{"mode": "allowlist"}})
	held := h.create(keyA, nil)
	if _, ok := h.egress.Attached(sb.ID); !ok {
		t.Fatal("no egress attachment before the delete")
	}
	inst, ok := h.be.BySandbox(sb.ID)
	if !ok {
		t.Fatal("no instance before the delete")
	}

	// The backend takes "forever" to remove a container.
	release := h.be.HoldDestroys()
	defer release()

	start := time.Now()
	h.want(h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 204, "")
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("DELETE took %s although the destroy is still blocked", took)
	}
	// Fenced: recycling, reason set, token refused, egress detached...
	var view api.SandboxView
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &view)
	if view.State != "recycling" || view.Reason != "released" {
		t.Errorf("right after DELETE: state %q reason %q, want recycling/released", view.State, view.Reason)
	}
	r := h.want(h.do("GET", "/v1/sandboxes/"+sb.ID+"/commands", nil, withToken(sb.AccessToken)), 409, api.CodeSandboxNotRunning)
	if !strings.Contains(string(r.Body), "recycling") {
		t.Errorf("data plane after DELETE: %s", r.Body)
	}
	h.want(h.do("POST", "/v1/sandboxes/"+sb.ID+"/commands", map[string]any{"cmd": "true"}, withKey(keyA)), 409, api.CodeSandboxNotRunning)
	if _, ok := h.egress.Attached(sb.ID); ok || h.egress.Detaches(sb.ID) == 0 {
		t.Error("egress is still attached after DELETE returned")
	}
	// ...while the instance is still there, because its removal is blocked.
	if _, err := h.be.Inspect(context.Background(), inst.ID); err != nil {
		t.Errorf("the instance is already gone although Destroy is blocked: %v", err)
	}
	// It no longer counts as one of the tenant's live sandboxes, and a
	// second DELETE is still a 204.
	var list struct {
		Sandboxes []api.SandboxView `json:"sandboxes"`
	}
	h.want(h.do("GET", "/v1/sandboxes", nil, withKey(keyA)), 200, "").json(t, &list)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != held.ID {
		t.Errorf("live listing after DELETE: %+v", list.Sandboxes)
	}
	h.want(h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 204, "")

	// ?wait=true does wait for the teardown.
	done := make(chan response, 1)
	go func() { done <- h.do("DELETE", "/v1/sandboxes/"+held.ID+"?wait=true", nil, withKey(keyA)) }()
	select {
	case r := <-done:
		t.Fatalf("DELETE ?wait=true answered %d while the destroy was blocked", r.Status)
	case <-time.After(150 * time.Millisecond):
	}

	release()
	h.want(<-done, 204, "")
	h.want(h.do("GET", "/v1/sandboxes/"+held.ID, nil, withKey(keyA)), 200, "").json(t, &view)
	if view.State != "destroyed" {
		t.Errorf("after DELETE ?wait=true: state %q, want destroyed", view.State)
	}
	// The background teardown of the first one completes on its own.
	eventually(t, "the asynchronously deleted sandbox to be destroyed", func() bool {
		h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &view)
		return view.State == "destroyed"
	})
	if view.Reason != "released" || !h.be.WasDestroyed(inst.ID) {
		t.Errorf("after the background teardown: %+v, instance destroyed=%v", view, h.be.WasDestroyed(inst.ID))
	}
	if _, err := os.Stat(h.runDir(sb.ID)); !os.IsNotExist(err) {
		t.Errorf("run dir after the background teardown: %v", err)
	}
}

// TestAsyncDeleteIsFinishedBySweeperWhenTeardownFails: the background
// teardown is not the only chance. If it fails, the row stays recycling
// and the sweeper retries at the deadline.
func TestAsyncDeleteIsFinishedBySweeperWhenTeardownFails(t *testing.T) {
	h := newHarness(t, options{pool: func(c *pool.Config) { c.DestroyTimeout = 300 * time.Millisecond }})
	sb := h.create(keyA, nil)
	h.be.SetDestroyError(context.DeadlineExceeded)
	h.want(h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 204, "")
	var view api.SandboxView
	// Still recycling well after the background attempt has failed.
	time.Sleep(100 * time.Millisecond)
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &view)
	if view.State != "recycling" {
		t.Fatalf("with a failing destroy: state %q, want recycling", view.State)
	}
	h.be.SetDestroyError(nil)
	eventually(t, "the sweeper to finish the teardown", func() bool {
		h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &view)
		return view.State == "destroyed"
	})
	if _, ok := h.be.BySandbox(sb.ID); ok {
		t.Error("instance still exists after the sweeper's retry")
	}
}

// TestPausedIsNotAPublicState: the internal PAUSED state never reaches a
// client; such a sandbox reads as running and is found by the filters a
// client can use.
func TestPausedIsNotAPublicState(t *testing.T) {
	h := newHarness(t, options{})
	sb := h.create(keyA, nil)
	acc, err := h.st.GetSandboxAccess(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Transition(context.Background(), store.TransitionParams{
		ID: sb.ID, Generation: acc.Sandbox.Generation, From: core.StateRunning, To: core.StatePaused}); err != nil {
		t.Fatalf("force PAUSED: %v", err)
	}
	r := h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "")
	var view api.SandboxView
	r.json(t, &view)
	if view.State != "running" || strings.Contains(string(r.Body), "paused") {
		t.Errorf("a PAUSED row is shown as %q: %s", view.State, r.Body)
	}
	for _, q := range []string{"", "?state=running", "?state=all", "?state=paused", "?state=assigned,running"} {
		r := h.want(h.do("GET", "/v1/sandboxes"+q, nil, withKey(keyA)), 200, "")
		var list struct {
			Sandboxes []api.SandboxView `json:"sandboxes"`
		}
		r.json(t, &list)
		if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != sb.ID || list.Sandboxes[0].State != "running" {
			t.Errorf("GET /v1/sandboxes%s: %s", q, r.Body)
		}
	}
	// In the metrics it is counted with the running ones, in the one
	// "running" series of its template: a PAUSED count written as a
	// second series with the same labels is a duplicate, and a strict
	// scraper rejects the whole scrape for it.
	eventually(t, "the paused sandbox to be counted as running", func() bool {
		body := string(h.want(h.do("GET", "/metrics", nil), 200, "").Body)
		samples, err := promtext.Validate(body)
		if err != nil {
			t.Fatalf("/metrics is not valid exposition text: %v\n%s", err, body)
		}
		if strings.Contains(body, "paused") {
			t.Fatalf("/metrics mentions the paused state:\n%s", body)
		}
		for _, sm := range samples {
			if sm.Name == "sbx_pool_sandboxes" && sm.Labels["template"] == tmplID && sm.Labels["state"] == "running" {
				return sm.Value == 1
			}
		}
		return false
	})
	// The data plane refuses it without calling it paused or running.
	r = h.want(h.do("GET", "/v1/sandboxes/"+sb.ID+"/commands", nil, withToken(sb.AccessToken)), 409, api.CodeSandboxNotRunning)
	if strings.Contains(string(r.Body), "paused") || strings.Contains(string(r.Body), "is running") {
		t.Errorf("409 for a PAUSED row: %s", r.Body)
	}
	// It can still be deleted.
	h.want(h.do("DELETE", "/v1/sandboxes/"+sb.ID+"?wait=true", nil, withKey(keyA)), 204, "")
	h.want(h.do("GET", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 200, "").json(t, &view)
	if view.State != "destroyed" {
		t.Errorf("after delete: %q", view.State)
	}
}

// TestMetricsTokenAndSeparateHandler covers the three ways /metrics can
// be served: open on the main handler (default), behind a bearer token,
// and only on MetricsHandler.
func TestMetricsTokenAndSeparateHandler(t *testing.T) {
	t.Run("default is open", func(t *testing.T) {
		h := newHarness(t, options{noStart: true})
		r := h.want(h.do("GET", "/metrics", nil), 200, "")
		if !strings.Contains(string(r.Body), "sbx_http_requests_total") {
			t.Errorf("metrics body: %.200s", r.Body)
		}
	})
	t.Run("token", func(t *testing.T) {
		h := newHarness(t, options{noStart: true, api: func(c *api.Config) { c.MetricsToken = "s3cret-token" }})
		r := h.want(h.do("GET", "/metrics", nil), 401, api.CodeUnauthorized)
		if r.Header.Get("WWW-Authenticate") == "" || strings.Contains(string(r.Body), "sbx_") {
			t.Errorf("unauthenticated /metrics: header %q body %.200s", r.Header.Get("WWW-Authenticate"), r.Body)
		}
		for _, bad := range []string{"Bearer wrong", "Bearer s3cret-token2", "s3cret-token", "Basic s3cret-token", "Bearer "} {
			h.want(h.do("GET", "/metrics", nil, withHeader("Authorization", bad)), 401, api.CodeUnauthorized)
		}
		// A tenant API key is not a metrics credential.
		h.want(h.do("GET", "/metrics", nil, withKey(keyA)), 401, api.CodeUnauthorized)
		r = h.want(h.do("GET", "/metrics", nil, withHeader("Authorization", "Bearer s3cret-token")), 200, "")
		if !strings.Contains(string(r.Body), "sbx_http_requests_total") {
			t.Errorf("metrics body: %.200s", r.Body)
		}
		// Health checks stay open.
		h.want(h.do("GET", "/healthz", nil), 200, "")
	})
	t.Run("separate handler", func(t *testing.T) {
		h := newHarness(t, options{noStart: true, api: func(c *api.Config) { c.NoMetricsRoute = true; c.MetricsToken = "tok" }})
		h.want(h.do("GET", "/metrics", nil), 404, api.CodeNotFound)
		h.want(h.do("GET", "/metrics", nil, withHeader("Authorization", "Bearer tok")), 404, api.CodeNotFound)
		h.want(h.do("GET", "/healthz", nil), 200, "")

		ms := httptest.NewServer(h.api.MetricsHandler())
		defer ms.Close()
		get := func(path, auth string) (int, string) {
			req, _ := http.NewRequest("GET", ms.URL+path, nil)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := ms.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b := new(strings.Builder)
			buf := make([]byte, 64<<10)
			for {
				n, err := resp.Body.Read(buf)
				b.Write(buf[:n])
				if err != nil {
					break
				}
			}
			return resp.StatusCode, b.String()
		}
		if code, _ := get("/metrics", ""); code != 401 {
			t.Errorf("metrics listener without the token: %d", code)
		}
		if code, body := get("/metrics", "Bearer tok"); code != 200 || !strings.Contains(body, "sbx_pool_") {
			t.Errorf("metrics listener with the token: %d %.200s", code, body)
		}
		// The metrics listener serves nothing else: no API, no health.
		for _, p := range []string{"/v1/sandboxes", "/healthz", "/"} {
			if code, _ := get(p, "Bearer tok"); code != 404 {
				t.Errorf("metrics listener served %s: %d", p, code)
			}
		}
	})
}
