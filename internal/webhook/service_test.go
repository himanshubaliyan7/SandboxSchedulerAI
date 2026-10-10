package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/pool/pooltest"
	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook/webhooktest"
)

type serviceEnv struct {
	svc    *Service
	st     *webhooktest.MemStore
	sender *fakeSender
	res    *fakeResolver
	clock  *pooltest.FakeClock
}

func newServiceEnv(t *testing.T, mut func(*ServiceConfig)) *serviceEnv {
	t.Helper()
	clock := pooltest.NewFakeClock()
	dest, res := testDest(t, DestinationConfig{})
	res.set("hooks.example.com", []string{publicAddr})
	e := &serviceEnv{st: webhooktest.NewMemStore(clock.Now), sender: newFakeSender(clock), res: res, clock: clock}
	cfg := ServiceConfig{Store: e.st, Destinations: dest, Sender: e.sender, Clock: clock, Metrics: NewMetrics(), MaxPerTenant: 3}
	if mut != nil {
		mut(&cfg)
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func TestServiceCreate(t *testing.T) {
	e := newServiceEnv(t, nil)
	ctx := context.Background()
	w, err := e.svc.Create(ctx, "acme", CreateParams{URL: "HTTPS://hooks.example.com:443/in", Description: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.ID, IDPrefix) || len(w.ID) != len(IDPrefix)+24 || w.URL != "https://hooks.example.com/in" ||
		len(w.EventTypes) != 1 || w.EventTypes[0] != AllEvents || !w.Enabled || w.Description != "ci" {
		t.Fatalf("created: %+v", w)
	}
	if _, err := secretKey(w.Secret); err != nil || len(w.Secret) < 40 {
		t.Fatalf("secret %q: %v", w.Secret, err)
	}
	w2, err := e.svc.Create(ctx, "acme", CreateParams{URL: "https://hooks.example.com/2", EventTypes: []string{TypeSandboxTerminated, TypeSandboxCreated}, Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Secret == w.Secret || w2.ID == w.ID || w2.Enabled || strings.Join(w2.EventTypes, ",") != "sandbox.created,sandbox.terminated" {
		t.Fatalf("second: %+v", w2)
	}

	for name, p := range map[string]CreateParams{
		"http":               {URL: "http://hooks.example.com/in"},
		"userinfo":           {URL: "https://u:p@hooks.example.com/in"},
		"private literal":    {URL: "https://10.0.0.1/in"},
		"metadata":           {URL: "https://169.254.169.254/latest"},
		"unresolvable":       {URL: "https://nowhere.example.com/in"},
		"unknown event type": {URL: "https://hooks.example.com/in", EventTypes: []string{"sandbox.exploded"}},
		"long description":   {URL: "https://hooks.example.com/in", Description: strings.Repeat("d", MaxDescriptionLength+1)},
		"control characters": {URL: "https://hooks.example.com/in", Description: "a\x00b"},
		"no url":             {},
	} {
		_, err := e.svc.Create(ctx, "acme", p)
		var inv *InvalidError
		if !errors.As(err, &inv) || inv.Message == "" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.Create(ctx, "acme", CreateParams{URL: "https://hooks.example.com/3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, "acme", CreateParams{URL: "https://hooks.example.com/4"}); !errors.Is(err, ErrLimit) {
		t.Errorf("fourth webhook with a limit of 3: %v", err)
	}
	if _, err := e.svc.Create(ctx, "other", CreateParams{URL: "https://hooks.example.com/4"}); err != nil {
		t.Errorf("another tenant: %v", err)
	}
	if e.svc.MaxPerTenant() != 3 {
		t.Errorf("MaxPerTenant = %d", e.svc.MaxPerTenant())
	}
	if list, _ := e.svc.List(ctx, "acme"); len(list) != 3 {
		t.Errorf("list: %d", len(list))
	}
}

func TestServiceUpdateRotateDelete(t *testing.T) {
	e := newServiceEnv(t, nil)
	ctx := context.Background()
	w, _ := e.svc.Create(ctx, "acme", CreateParams{URL: "https://hooks.example.com/in"})

	// Another tenant sees nothing, and cannot make the server resolve a
	// name by patching a webhook that is not theirs.
	e.res.set("probe.example.com", []string{publicAddr})
	probe := "https://probe.example.com/x"
	if _, err := e.svc.Update(ctx, "other", w.ID, UpdateParams{URL: &probe}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update by another tenant: %v", err)
	}
	if e.res.lookups("probe.example.com") != 0 {
		t.Error("the URL of a foreign webhook was resolved before the ownership check")
	}
	for name, err := range map[string]error{
		"get":        func() error { _, err := e.svc.Get(ctx, "other", w.ID); return err }(),
		"rotate":     func() error { _, err := e.svc.RotateSecret(ctx, "other", w.ID, 0); return err }(),
		"test":       func() error { _, err := e.svc.Test(ctx, "other", w.ID); return err }(),
		"deliveries": func() error { _, err := e.svc.Deliveries(ctx, "other", w.ID, 10); return err }(),
		"delete":     e.svc.Delete(ctx, "other", w.ID),
	} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s by another tenant: %v", name, err)
		}
	}

	off, desc, types := false, "new", []string{TypeCommandFinished}
	u, err := e.svc.Update(ctx, "acme", w.ID, UpdateParams{URL: &probe, Enabled: &off, Description: &desc, EventTypes: &types})
	if err != nil {
		t.Fatal(err)
	}
	if u.URL != probe || u.Enabled || u.DisabledReason != store.WebhookDisabledByUser || u.Description != "new" || u.EventTypes[0] != TypeCommandFinished {
		t.Fatalf("updated: %+v", u)
	}
	bad := "https://192.168.0.10/x"
	var inv *InvalidError
	if _, err := e.svc.Update(ctx, "acme", w.ID, UpdateParams{URL: &bad}); !errors.As(err, &inv) || inv.Code != CodeDestinationNotAllowed {
		t.Errorf("update to a private address: %v", err)
	}
	badTypes := []string{"nope"}
	if _, err := e.svc.Update(ctx, "acme", w.ID, UpdateParams{EventTypes: &badTypes}); !errors.As(err, &inv) {
		t.Errorf("update with an unknown type: %v", err)
	}
	if got, _ := e.svc.Get(ctx, "acme", w.ID); got.URL != probe {
		t.Errorf("a refused update changed the row: %+v", got)
	}

	r, err := e.svc.RotateSecret(ctx, "acme", w.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.Secret == w.Secret || r.PreviousSecret != w.Secret || r.PreviousSecretExpiresAt == nil {
		t.Fatalf("rotated: %+v", r)
	}
	if _, err := e.svc.RotateSecret(ctx, "acme", w.ID, MaxRotationGrace+time.Second); !errors.As(err, &inv) {
		t.Errorf("a grace period beyond the maximum: %v", err)
	}
	r2, _ := e.svc.RotateSecret(ctx, "acme", w.ID, -time.Second)
	if r2.PreviousSecret != "" || r2.Secret == r.Secret {
		t.Errorf("rotated without grace: %+v", r2)
	}
	if err := e.svc.Delete(ctx, "acme", w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, "acme", w.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
}

func TestServiceTestEvent(t *testing.T) {
	e := newServiceEnv(t, nil)
	ctx := context.Background()
	w, _ := e.svc.Create(ctx, "acme", CreateParams{URL: "https://hooks.example.com/in", EventTypes: []string{TypeCommandFinished}, Disabled: true})

	res, err := e.svc.Test(ctx, "acme", w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delivered || res.Status != 200 || res.ErrorCode != "" || !strings.HasPrefix(res.EventID, "evt_test_") {
		t.Fatalf("result: %+v", res)
	}
	s := e.sender.all()[0]
	if s.URL != w.URL || s.Type != TypeTest || s.ID != res.EventID || s.Attempt != 1 {
		t.Fatalf("sent: %+v", s)
	}
	if err := Verify(w.Secret, s.Header, s.Body, e.clock.Now(), 0); err != nil {
		t.Fatalf("the test event does not verify: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(s.Body, &env); err != nil || env.Type != TypeTest || env.Data["webhook_id"] != w.ID || env.SandboxID != "" {
		t.Fatalf("body %s: %v", s.Body, err)
	}

	e.sender.respond = func(sent) Result { return status(500) }
	res, err = e.svc.Test(ctx, "acme", w.ID)
	if err != nil || res.Delivered || res.Status != 500 || res.ErrorCode != CodeHTTPStatus || res.Error == "" {
		t.Fatalf("failing endpoint: %+v %v", res, err)
	}
	// A test is not a delivery: no cursor, no failure state, no retry.
	row := e.st.Hook(w.ID)
	if row.Attempts != 0 || row.FailingSince != nil || row.LastFailureAt != nil || row.Enabled {
		t.Errorf("a test changed the delivery state: %+v", row)
	}
	log, err := e.svc.Deliveries(ctx, "acme", w.ID, 10)
	if err != nil || len(log) != 2 || log[0].Status != store.DeliveryFailed || log[1].Status != store.DeliverySucceeded ||
		log[0].EventType != TypeTest || log[1].MessageID != s.ID || log[0].ResponseStatus != 500 {
		t.Fatalf("log: %+v %v", log, err)
	}

	noSender := newServiceEnv(t, func(c *ServiceConfig) { c.Sender = nil })
	w2, _ := noSender.svc.Create(ctx, "acme", CreateParams{URL: "https://hooks.example.com/in"})
	if _, err := noSender.svc.Test(ctx, "acme", w2.ID); !errors.Is(err, ErrNoSender) {
		t.Errorf("Test without a sender: %v", err)
	}
	if _, err := NewService(ServiceConfig{}); err == nil {
		t.Error("a service without a store was accepted")
	}
}
