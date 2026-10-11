package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/api"
	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook"
	"github.com/himanshubaliyan7/warmbox/internal/webhook/webhooktest"
)

// staticResolver resolves a fixed set of names.
type staticResolver map[string]string

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return []netip.Addr{netip.MustParseAddr(a)}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// receiver is a webhook endpoint on the test host. It records what it
// gets and answers from a script (200 when the script has run out).
type receiver struct {
	srv *httptest.Server

	mu      sync.Mutex
	script  []int
	headers []http.Header
	bodies  [][]byte
}

func newReceiver(t *testing.T, script ...int) *receiver {
	t.Helper()
	r := &receiver{script: script}
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

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

type received struct {
	Header http.Header
	Body   []byte
	Env    webhook.Envelope
}

func (r *receiver) all(t *testing.T) []received {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]received, len(r.bodies))
	for i := range r.bodies {
		out[i] = received{Header: r.headers[i], Body: r.bodies[i]}
		if err := json.Unmarshal(r.bodies[i], &out[i].Env); err != nil {
			t.Fatalf("request %d: body %q: %v", i, r.bodies[i], err)
		}
	}
	return out
}

type webhookHarness struct {
	*harness
	hooks *webhooktest.MemStore
	svc   *webhook.Service
	send  *webhook.HTTPSender
	m     *webhook.Metrics
}

// newWebhookHarness is the API harness with the real webhook service and
// the real HTTP sender over an in-memory webhook store. dev opens http
// and loopback, as --webhook-allow-http and --webhook-allow-loopback do,
// so that a receiver on the test host can be reached.
func newWebhookHarness(t *testing.T, dev bool, o options, maxPerTenant int) *webhookHarness {
	t.Helper()
	dest, err := webhook.NewDestinations(webhook.DestinationConfig{AllowHTTP: dev, AllowLoopback: dev,
		Resolver: staticResolver{"hooks.example.com": "93.184.216.34", "internal.example.com": "10.1.2.3"}})
	if err != nil {
		t.Fatal(err)
	}
	wh := &webhookHarness{hooks: webhooktest.NewMemStore(time.Now), m: webhook.NewMetrics()}
	wh.send = webhook.NewHTTPSender(dest, 5*time.Second)
	wh.svc, err = webhook.NewService(webhook.ServiceConfig{Store: wh.hooks, Destinations: dest, Sender: wh.send,
		Metrics: wh.m, MaxPerTenant: maxPerTenant})
	if err != nil {
		t.Fatal(err)
	}
	prev := o.api
	o.api = func(c *api.Config) {
		c.Webhooks = wh.svc
		if prev != nil {
			prev(c)
		}
	}
	wh.harness = newHarness(t, o)
	return wh
}

func (h *webhookHarness) createHook(key string, body any) api.WebhookView {
	h.t.Helper()
	r := h.want(h.do("POST", "/v1/webhooks", body, withKey(key)), http.StatusCreated, "")
	var v api.WebhookView
	r.json(h.t, &v)
	return v
}

func TestWebhookRoutesAuthentication(t *testing.T) {
	h := newWebhookHarness(t, false, options{}, 0)
	v := h.createHook(keyA, map[string]any{"url": "https://hooks.example.com/in"})
	sb := h.create(keyA, nil)
	routes := []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/webhooks", map[string]any{"url": "https://hooks.example.com/x"}},
		{"GET", "/v1/webhooks", nil},
		{"GET", "/v1/webhooks/" + v.ID, nil},
		{"PATCH", "/v1/webhooks/" + v.ID, map[string]any{"description": "x"}},
		{"POST", "/v1/webhooks/" + v.ID + "/rotate-secret", nil},
		{"POST", "/v1/webhooks/" + v.ID + "/test", nil},
		{"GET", "/v1/webhooks/" + v.ID + "/deliveries", nil},
		{"DELETE", "/v1/webhooks/" + v.ID, nil},
	}
	for _, r := range routes {
		// No credential, a wrong key, and a sandbox access token: the
		// token is a data-plane credential and opens nothing here, in any
		// of the places it may be sent.
		h.want(h.do(r.method, r.path, r.body), 401, api.CodeUnauthorized)
		h.want(h.do(r.method, r.path, r.body, withKey("sbx_zzzzzzzz_nope")), 401, api.CodeUnauthorized)
		h.want(h.do(r.method, r.path, r.body, withToken(sb.AccessToken)), 401, api.CodeUnauthorized)
		h.want(h.do(r.method, r.path, r.body, withHeader("Authorization", "Bearer "+sb.AccessToken)), 401, api.CodeUnauthorized)
		h.want(h.do(r.method, r.path+"?token="+sb.AccessToken, r.body), 401, api.CodeUnauthorized)
	}
	// None of that changed anything.
	got := h.want(h.do("GET", "/v1/webhooks/"+v.ID, nil, withKey(keyA)), 200, "")
	var after api.WebhookView
	got.json(t, &after)
	if after.Description != "" || !after.UpdatedAt.Equal(v.UpdatedAt) {
		t.Errorf("an unauthenticated request changed the webhook: %+v", after)
	}
	// A bearer API key works like X-API-Key.
	h.want(h.do("GET", "/v1/webhooks", nil, withHeader("Authorization", "Bearer "+keyA)), 200, "")
	h.want(h.do("PUT", "/v1/webhooks/"+v.ID, map[string]any{}, withKey(keyA)), 405, api.CodeMethodNotAllowed)
}

func TestWebhookTenantIsolation(t *testing.T) {
	h := newWebhookHarness(t, false, options{}, 0)
	a := h.createHook(keyA, map[string]any{"url": "https://hooks.example.com/a", "description": "A's"})
	b := h.createHook(keyB, map[string]any{"url": "https://hooks.example.com/b"})

	for _, r := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/webhooks/" + a.ID, nil},
		{"PATCH", "/v1/webhooks/" + a.ID, map[string]any{"url": "https://hooks.example.com/stolen"}},
		{"PATCH", "/v1/webhooks/" + a.ID, map[string]any{"enabled": false}},
		{"POST", "/v1/webhooks/" + a.ID + "/rotate-secret", nil},
		{"POST", "/v1/webhooks/" + a.ID + "/test", nil},
		{"GET", "/v1/webhooks/" + a.ID + "/deliveries", nil},
		{"DELETE", "/v1/webhooks/" + a.ID, nil},
	} {
		// Exactly what an id that never existed answers.
		got := h.want(h.do(r.method, r.path, r.body, withKey(keyB)), 404, api.CodeNotFound)
		never := h.do(r.method, strings.Replace(r.path, a.ID, "wh_000000000000000000000000", 1), r.body, withKey(keyB))
		if never.Status != 404 || strings.Replace(got.errBody(t).Message, a.ID, "X", 1) != strings.Replace(never.errBody(t).Message, "wh_000000000000000000000000", "X", 1) {
			t.Errorf("%s %s: a foreign webhook (%s) is distinguishable from a missing one (%s)", r.method, r.path, got.Body, never.Body)
		}
	}
	var list struct {
		Webhooks []api.WebhookView `json:"webhooks"`
	}
	h.want(h.do("GET", "/v1/webhooks", nil, withKey(keyB)), 200, "").json(t, &list)
	if len(list.Webhooks) != 1 || list.Webhooks[0].ID != b.ID {
		t.Errorf("tenant B lists %+v", list.Webhooks)
	}
	// A's webhook is untouched, secret included.
	row := h.hooks.Hook(a.ID)
	if row.URL != "https://hooks.example.com/a" || !row.Enabled || row.Secret != a.Secret {
		t.Errorf("tenant B changed tenant A's webhook: %+v", row)
	}
}

func TestWebhookSecretIsShownOnce(t *testing.T) {
	h := newWebhookHarness(t, false, options{}, 0)
	created := h.do("POST", "/v1/webhooks", map[string]any{"url": "https://hooks.example.com/in"}, withKey(keyA))
	h.want(created, 201, "")
	var v api.WebhookView
	created.json(t, &v)
	if !strings.HasPrefix(v.Secret, webhook.SecretPrefix) || len(v.Secret) < 40 || !strings.HasPrefix(v.ID, webhook.IDPrefix) {
		t.Fatalf("created: %+v", v)
	}
	if created.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("the response with the secret is cacheable: %v", created.Header)
	}
	for _, r := range []response{
		h.do("GET", "/v1/webhooks/"+v.ID, nil, withKey(keyA)),
		h.do("GET", "/v1/webhooks", nil, withKey(keyA)),
		h.do("PATCH", "/v1/webhooks/"+v.ID, map[string]any{"description": "d"}, withKey(keyA)),
		h.do("GET", "/v1/webhooks/"+v.ID+"/deliveries", nil, withKey(keyA)),
	} {
		if r.Status != 200 || strings.Contains(string(r.Body), "whsec_") || strings.Contains(string(r.Body), `"secret"`) {
			t.Errorf("a response after creation carries the secret: %d %s", r.Status, r.Body)
		}
	}

	// Rotation is the only other way to see a secret: a new one.
	rot := h.want(h.do("POST", "/v1/webhooks/"+v.ID+"/rotate-secret", nil, withKey(keyA)), 200, "")
	var rv api.WebhookView
	rot.json(t, &rv)
	if rv.Secret == v.Secret || !strings.HasPrefix(rv.Secret, webhook.SecretPrefix) || rv.PreviousSecretExpiresAt == nil ||
		time.Until(*rv.PreviousSecretExpiresAt) < 23*time.Hour {
		t.Fatalf("rotated: %+v", rv)
	}
	if strings.Contains(string(rot.Body), v.Secret) {
		t.Error("the rotation response contains the previous secret")
	}
	rot = h.want(h.do("POST", "/v1/webhooks/"+v.ID+"/rotate-secret", map[string]any{"keep_previous_seconds": 0}, withKey(keyA)), 200, "")
	var rv2 api.WebhookView
	rot.json(t, &rv2)
	if rv2.Secret == rv.Secret || rv2.PreviousSecretExpiresAt != nil {
		t.Fatalf("rotated without grace: %+v", rv2)
	}
	for _, bad := range []any{map[string]any{"keep_previous_seconds": -1}, map[string]any{"keep_previous_seconds": 604801},
		map[string]any{"secret": "whsec_" + "bWluZQ=="}, `{"keep_previous_seconds": "1d"}`} {
		h.want(h.do("POST", "/v1/webhooks/"+v.ID+"/rotate-secret", bad, withKey(keyA)), 400, api.CodeBadRequest)
	}
	if h.hooks.Hook(v.ID).Secret != rv2.Secret {
		t.Error("a refused rotation changed the secret")
	}
}

func TestWebhookValidation(t *testing.T) {
	h := newWebhookHarness(t, false, options{}, 3)
	for name, c := range map[string]struct {
		body any
		code string
	}{
		"no body":              {nil, api.CodeBadRequest},
		"no url":               {map[string]any{}, api.CodeInvalidWebhookURL},
		"http":                 {map[string]any{"url": "http://hooks.example.com/in"}, api.CodeInvalidWebhookURL},
		"userinfo":             {map[string]any{"url": "https://u:p@hooks.example.com/in"}, api.CodeInvalidWebhookURL},
		"fragment":             {map[string]any{"url": "https://hooks.example.com/in#x"}, api.CodeInvalidWebhookURL},
		"not a url":            {map[string]any{"url": "hooks.example.com"}, api.CodeInvalidWebhookURL},
		"decimal host":         {map[string]any{"url": "https://2130706433/"}, api.CodeInvalidWebhookURL},
		"loopback":             {map[string]any{"url": "https://127.0.0.1/in"}, api.CodeWebhookURLNotAllowed},
		"loopback v6":          {map[string]any{"url": "https://[::1]/in"}, api.CodeWebhookURLNotAllowed},
		"mapped loopback":      {map[string]any{"url": "https://[::ffff:127.0.0.1]/in"}, api.CodeWebhookURLNotAllowed},
		"private":              {map[string]any{"url": "https://192.168.1.1/in"}, api.CodeWebhookURLNotAllowed},
		"metadata":             {map[string]any{"url": "https://169.254.169.254/latest/meta-data/"}, api.CodeWebhookURLNotAllowed},
		"name to private":      {map[string]any{"url": "https://internal.example.com/in"}, api.CodeWebhookURLNotAllowed},
		"name that is missing": {map[string]any{"url": "https://missing.example.com/in"}, api.CodeWebhookURLNotAllowed},
		"unknown event type":   {map[string]any{"url": "https://hooks.example.com/in", "event_types": []string{"sandbox.paused"}}, api.CodeBadRequest},
		"internal event name":  {map[string]any{"url": "https://hooks.example.com/in", "event_types": []string{"transition"}}, api.CodeBadRequest},
		"long description":     {map[string]any{"url": "https://hooks.example.com/in", "description": strings.Repeat("x", 513)}, api.CodeBadRequest},
		"unknown member":       {map[string]any{"url": "https://hooks.example.com/in", "secret": "whsec_" + "bWluZQ=="}, api.CodeBadRequest},
		"wrong type":           {`{"url": 5}`, api.CodeBadRequest},
	} {
		r := h.do("POST", "/v1/webhooks", c.body, withKey(keyA))
		if r.Status != 400 || r.errCode() != c.code {
			t.Errorf("%s: %d %s, want 400 %s", name, r.Status, r.Body, c.code)
		}
	}
	// A name that does not exist and a name that points inside read the same.
	inside := h.do("POST", "/v1/webhooks", map[string]any{"url": "https://internal.example.com/in"}, withKey(keyA)).errBody(t).Message
	missing := h.do("POST", "/v1/webhooks", map[string]any{"url": "https://missing.example.com/in"}, withKey(keyA)).errBody(t).Message
	if strings.Replace(inside, "internal", "X", 1) != strings.Replace(missing, "missing", "X", 1) || strings.Contains(inside, "10.1.2.3") {
		t.Errorf("the refusals tell internal names apart: %q vs %q", inside, missing)
	}
	if list, _ := h.svc.List(context.Background(), tenantA); len(list) != 0 {
		t.Fatalf("%d webhooks were stored by refused requests", len(list))
	}

	v := h.createHook(keyA, map[string]any{"url": "HTTPS://Hooks.Example.com:443/in?x=1", "event_types": []string{"sandbox.terminated", "sandbox.created"}, "enabled": false})
	if v.URL != "https://hooks.example.com/in?x=1" || strings.Join(v.EventTypes, ",") != "sandbox.created,sandbox.terminated" ||
		v.Enabled || v.DisabledReason != "user" || v.ConsecutiveFailures != 0 || v.DroppedEvents != 0 {
		t.Fatalf("created: %+v", v)
	}
	all := h.createHook(keyA, map[string]any{"url": "https://hooks.example.com/all"})
	if len(all.EventTypes) != 1 || all.EventTypes[0] != "*" || !all.Enabled {
		t.Fatalf("default subscription: %+v", all)
	}

	// PATCH: partial, validated, and an empty change is an error.
	p := "/v1/webhooks/" + v.ID
	for name, c := range map[string]struct {
		body any
		code string
	}{
		"empty":            {map[string]any{}, api.CodeBadRequest},
		"private url":      {map[string]any{"url": "https://10.0.0.1/"}, api.CodeWebhookURLNotAllowed},
		"http url":         {map[string]any{"url": "http://hooks.example.com/"}, api.CodeInvalidWebhookURL},
		"unknown type":     {map[string]any{"event_types": []string{"nope"}}, api.CodeBadRequest},
		"secret":           {map[string]any{"secret": "whsec_" + "bWluZQ=="}, api.CodeBadRequest},
		"id":               {map[string]any{"id": "wh_other"}, api.CodeBadRequest},
		"enabled not bool": {`{"enabled":"yes"}`, api.CodeBadRequest},
	} {
		r := h.do("PATCH", p, c.body, withKey(keyA))
		if r.Status != 400 || r.errCode() != c.code {
			t.Errorf("PATCH %s: %d %s, want 400 %s", name, r.Status, r.Body, c.code)
		}
	}
	var u api.WebhookView
	h.want(h.do("PATCH", p, map[string]any{"enabled": true, "description": "now on", "event_types": []string{}}, withKey(keyA)), 200, "").json(t, &u)
	if !u.Enabled || u.DisabledReason != "" || u.Description != "now on" || u.URL != v.URL || len(u.EventTypes) != 1 || u.EventTypes[0] != "*" {
		t.Fatalf("patched: %+v", u)
	}

	// The per-tenant limit, and that it is per tenant.
	h.createHook(keyA, map[string]any{"url": "https://hooks.example.com/3"})
	h.want(h.do("POST", "/v1/webhooks", map[string]any{"url": "https://hooks.example.com/4"}, withKey(keyA)), 409, api.CodeWebhookLimitExceeded)
	h.createHook(keyB, map[string]any{"url": "https://hooks.example.com/b"})
	h.want(h.do("DELETE", p, nil, withKey(keyA)), 204, "")
	h.want(h.do("DELETE", p, nil, withKey(keyA)), 404, api.CodeNotFound)
	h.createHook(keyA, map[string]any{"url": "https://hooks.example.com/4"})

	h.want(h.do("GET", "/v1/webhooks/"+all.ID+"/deliveries?limit=0", nil, withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("GET", "/v1/webhooks/"+all.ID+"/deliveries?limit=501", nil, withKey(keyA)), 400, api.CodeBadRequest)
	h.want(h.do("GET", "/v1/webhooks/"+all.ID+"/deliveries?limit=x", nil, withKey(keyA)), 400, api.CodeBadRequest)
}

func TestWebhookRoutesAreRateLimitedAndCanBeOff(t *testing.T) {
	h := newWebhookHarness(t, false, options{tenantA: func(tn *store.Tenant) { tn.RatePerSec, tn.Burst = 0.001, 2 }}, 0)
	h.want(h.do("GET", "/v1/webhooks", nil, withKey(keyA)), 200, "")
	h.want(h.do("GET", "/v1/webhooks", nil, withKey(keyA)), 200, "")
	r := h.want(h.do("GET", "/v1/webhooks", nil, withKey(keyA)), 429, api.CodeRateLimited)
	if r.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	h.want(h.do("POST", "/v1/webhooks", map[string]any{"url": "https://hooks.example.com/x"}, withKey(keyA)), 429, api.CodeRateLimited)
	h.want(h.do("GET", "/v1/webhooks", nil, withKey(keyB)), 200, "") // another tenant's bucket

	// A server without webhooks keeps the routes and says so.
	off := newHarness(t, options{noStart: true})
	for _, r := range [][2]string{{"GET", "/v1/webhooks"}, {"POST", "/v1/webhooks"}, {"GET", "/v1/webhooks/wh_x"}, {"PATCH", "/v1/webhooks/wh_x"},
		{"DELETE", "/v1/webhooks/wh_x"}, {"POST", "/v1/webhooks/wh_x/rotate-secret"}, {"POST", "/v1/webhooks/wh_x/test"}, {"GET", "/v1/webhooks/wh_x/deliveries"}} {
		off.want(off.do(r[0], r[1], map[string]any{"url": "https://hooks.example.com/x"}, withKey(keyA)), 501, api.CodeNotImplemented)
		off.want(off.do(r[0], r[1], nil), 401, api.CodeUnauthorized)
	}
}

// The test route sends a real, signed request and reports the answer.
func TestWebhookTestRoute(t *testing.T) {
	h := newWebhookHarness(t, true, options{}, 0)
	rcv := newReceiver(t, 200, 503)
	v := h.createHook(keyA, map[string]any{"url": rcv.srv.URL + "/hook", "enabled": false})

	var res api.WebhookTestView
	h.want(h.do("POST", "/v1/webhooks/"+v.ID+"/test", nil, withKey(keyA)), 200, "").json(t, &res)
	if !res.Delivered || res.Status != 200 || res.ErrorCode != "" || !strings.HasPrefix(res.EventID, "evt_test_") {
		t.Fatalf("result: %+v", res)
	}
	got := rcv.all(t)
	if len(got) != 1 {
		t.Fatalf("%d requests at the receiver", len(got))
	}
	if err := webhook.Verify(v.Secret, got[0].Header, got[0].Body, time.Now(), 0); err != nil {
		t.Fatalf("the test event does not verify with the secret from the create response: %v", err)
	}
	if err := webhook.Verify("whsec_"+strings.Repeat("A", 43)+"=", got[0].Header, got[0].Body, time.Now(), 0); !errors.Is(err, webhook.ErrNoMatch) {
		t.Fatalf("the test event verifies with another secret: %v", err)
	}
	if e := got[0].Env; e.Type != webhook.TypeTest || e.ID != res.EventID || e.Version != 1 || e.Data["webhook_id"] != v.ID ||
		got[0].Header.Get(webhook.HeaderID) != res.EventID || got[0].Header.Get(webhook.HeaderEventType) != webhook.TypeTest {
		t.Fatalf("test event: %s", got[0].Body)
	}

	// A failing endpoint is a result, not an error of the call.
	h.want(h.do("POST", "/v1/webhooks/"+v.ID+"/test", nil, withKey(keyA)), 200, "").json(t, &res)
	if res.Delivered || res.Status != 503 || res.ErrorCode != webhook.CodeHTTPStatus || res.Error == "" {
		t.Fatalf("failing endpoint: %+v", res)
	}
	// An endpoint that is not there.
	rcv.srv.Close()
	res = api.WebhookTestView{}
	h.want(h.do("POST", "/v1/webhooks/"+v.ID+"/test", nil, withKey(keyA)), 200, "").json(t, &res)
	if res.Delivered || res.Status != 0 || res.ErrorCode != webhook.CodeConnectionFailed {
		t.Fatalf("dead endpoint: %+v", res)
	}

	var log struct {
		Deliveries []api.WebhookDeliveryView `json:"deliveries"`
	}
	h.want(h.do("GET", "/v1/webhooks/"+v.ID+"/deliveries", nil, withKey(keyA)), 200, "").json(t, &log)
	if len(log.Deliveries) != 3 {
		t.Fatalf("delivery log: %+v", log.Deliveries)
	}
	newest, oldest := log.Deliveries[0], log.Deliveries[2]
	if newest.Status != "failed" || newest.ErrorCode != webhook.CodeConnectionFailed || newest.ResponseStatus != 0 ||
		log.Deliveries[1].ResponseStatus != 503 || oldest.Status != "succeeded" || oldest.ResponseStatus != 200 ||
		oldest.EventType != webhook.TypeTest || !strings.HasPrefix(oldest.EventID, "evt_test_") || oldest.Attempt != 1 || oldest.CreatedAt.IsZero() {
		t.Fatalf("delivery log: %+v", log.Deliveries)
	}
	h.want(h.do("GET", "/v1/webhooks/"+v.ID+"/deliveries?limit=1", nil, withKey(keyA)), 200, "").json(t, &log)
	if len(log.Deliveries) != 1 || log.Deliveries[0].ID != newest.ID {
		t.Fatalf("limit=1: %+v", log.Deliveries)
	}
	// Tests leave the delivery state alone.
	if row := h.hooks.Hook(v.ID); row.Attempts != 0 || row.FailingSince != nil || row.Enabled {
		t.Errorf("after tests: %+v", row)
	}

	// On a server with the default policy the same receiver is refused,
	// at creation, because it listens on loopback.
	strict := newWebhookHarness(t, false, options{noStart: true}, 0)
	strict.want(strict.do("POST", "/v1/webhooks", map[string]any{"url": "https://127.0.0.1:8443/hook"}, withKey(keyA)), 400, api.CodeWebhookURLNotAllowed)
}

// The recorder's event names are what the webhook mapping expects.
func TestWebhookMappingKnowsTheRecorderEvents(t *testing.T) {
	for internal, public := range map[string]string{
		api.EventCommandStarted: webhook.TypeCommandStarted, api.EventCommandFinished: webhook.TypeCommandFinished,
	} {
		env, ok := webhook.Translate(store.WebhookEvent{Event: store.Event{ID: 1, Type: internal}})
		if !ok || env.Type != public {
			t.Errorf("event log type %q maps to %q, %v; want %q", internal, env.Type, ok, public)
		}
	}
}

// startWorker runs a delivery worker over the harness's event log: the
// rows the real pool controller and the real execution recorder write.
func (h *webhookHarness) startWorker(mut func(*webhook.Config)) *webhook.Worker {
	h.t.Helper()
	h.hooks.SetSource(func() []store.WebhookEvent {
		templates := map[string]string{}
		for _, sb := range h.st.Sandboxes() {
			templates[sb.ID] = sb.TemplateID
		}
		var out []store.WebhookEvent
		for _, e := range h.st.Events("") {
			out = append(out, store.WebhookEvent{Event: e, Txid: e.ID, TemplateID: templates[e.SandboxID]})
		}
		return out
	})
	cfg := webhook.Config{Store: h.hooks, Sender: h.send, Metrics: h.m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 5 * time.Millisecond, RetryBase: 20 * time.Millisecond, RetryMax: 50 * time.Millisecond}
	if mut != nil {
		mut(&cfg)
	}
	w, err := webhook.NewWorker(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	w.Start()
	h.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		w.Stop(ctx)
	})
	return w
}

// End to end inside the harness: the real API, pool controller and
// execution recorder produce the events; the real worker and HTTP sender
// deliver them to a receiver on the test host.
func TestWebhookDeliveryOfARealSandboxLife(t *testing.T) {
	h := newWebhookHarness(t, true, options{}, 0)
	rcv := newReceiver(t)
	flaky := newReceiver(t, 500, 500) // fails the first two requests it gets
	filtered := newReceiver(t)
	v := h.createHook(keyA, map[string]any{"url": rcv.srv.URL + "/all"})
	fv := h.createHook(keyA, map[string]any{"url": flaky.srv.URL + "/flaky", "event_types": []string{"sandbox.terminated"}})
	h.createHook(keyA, map[string]any{"url": filtered.srv.URL + "/cmd", "event_types": []string{"command.finished"}})
	other := newReceiver(t)
	h.createHook(keyB, map[string]any{"url": other.srv.URL + "/b"})
	h.startWorker(nil)

	sb := h.create(keyA, map[string]any{"template_id": tmplID, "metadata": map[string]string{"job": "42"},
		"env": map[string]string{"SECRET_ENV": "do-not-leak"}})
	r := h.want(h.do("POST", "/v1/sandboxes/"+sb.ID+"/commands?wait=true", sh("echo hi; exit 3"), withKey(keyA)), 200, "")
	if !strings.Contains(string(r.Body), `"exit_code":3`) {
		t.Fatalf("command: %s", r.Body)
	}
	h.want(h.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, withKey(keyA)), 204, "")

	want := []string{"sandbox.created", "sandbox.running", "command.started", "command.finished", "sandbox.terminated", "sandbox.destroyed"}
	eventually(t, "all six events at the receiver", func() bool { return rcv.count() >= len(want) })
	time.Sleep(50 * time.Millisecond)
	got := rcv.all(t)
	var types []string
	ids := map[string]bool{}
	for i, g := range got {
		types = append(types, g.Env.Type)
		if err := webhook.Verify(v.Secret, g.Header, g.Body, time.Now(), 0); err != nil {
			t.Errorf("event %d (%s) does not verify: %v", i, g.Env.Type, err)
		}
		if g.Env.SandboxID != sb.ID || g.Env.TemplateID != tmplID || g.Env.Version != 1 || g.Env.CreatedAt.IsZero() ||
			g.Header.Get(webhook.HeaderID) != g.Env.ID || ids[g.Env.ID] {
			t.Errorf("event %d: %s", i, g.Body)
		}
		ids[g.Env.ID] = true
		for _, leak := range []string{sb.AccessToken, "do-not-leak", "SECRET_ENV", "access_token", hostName, "generation", "instance"} {
			if strings.Contains(string(g.Body), leak) {
				t.Errorf("event %d contains %q: %s", i, leak, g.Body)
			}
		}
	}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("events at the receiver:\n got %v\nwant %v", types, want)
	}
	if d := got[3].Env.Data; d["exit_code"] != float64(3) || d["cmd"] != "sh" || d["reason"] != "exited" {
		t.Errorf("command.finished data: %v", d)
	}
	if d := got[4].Env.Data; d["reason"] != "released" || d["state"] != "recycling" {
		t.Errorf("sandbox.terminated data: %v", d)
	}
	if d := got[0].Env.Data; d["state"] != "assigned" {
		t.Errorf("sandbox.created data: %v", d)
	}

	// The endpoint that failed twice got the event three times, with the
	// same id and a rising attempt number, and accepted it exactly once.
	eventually(t, "the flaky receiver's third request", func() bool { return flaky.count() >= 3 })
	time.Sleep(100 * time.Millisecond)
	fg := flaky.all(t)
	if len(fg) != 3 {
		t.Fatalf("the flaky receiver got %d requests, want 3 (500, 500, 200)", len(fg))
	}
	for i, g := range fg {
		if g.Env.Type != "sandbox.terminated" || g.Env.ID != fg[0].Env.ID || g.Header.Get(webhook.HeaderAttempt) != string(rune('1'+i)) {
			t.Errorf("flaky request %d: attempt %q, %s", i, g.Header.Get(webhook.HeaderAttempt), g.Body)
		}
		if err := webhook.Verify(fv.Secret, g.Header, g.Body, time.Now(), 0); err != nil {
			t.Errorf("flaky request %d does not verify: %v", i, err)
		}
	}
	var log struct {
		Deliveries []api.WebhookDeliveryView `json:"deliveries"`
	}
	h.want(h.do("GET", "/v1/webhooks/"+fv.ID+"/deliveries", nil, withKey(keyA)), 200, "").json(t, &log)
	var statuses []string
	for _, d := range log.Deliveries {
		statuses = append(statuses, d.Status)
	}
	if strings.Join(statuses, " ") != "succeeded failed failed" || log.Deliveries[0].Attempt != 3 || log.Deliveries[2].ResponseStatus != 500 {
		t.Errorf("flaky delivery log: %+v", log.Deliveries)
	}
	var view api.WebhookView
	h.want(h.do("GET", "/v1/webhooks/"+fv.ID, nil, withKey(keyA)), 200, "").json(t, &view)
	if view.ConsecutiveFailures != 0 || view.FailingSince != nil || view.LastSuccessAt == nil || view.LastFailureAt == nil || !view.Enabled {
		t.Errorf("flaky webhook after recovery: %+v", view)
	}

	if f := filtered.all(t); len(f) != 1 || f[0].Env.Type != "command.finished" {
		t.Errorf("the filtered webhook got %d events", len(f))
	}
	if other.count() != 0 {
		t.Errorf("tenant B's webhook received %d of tenant A's events", other.count())
	}
}
