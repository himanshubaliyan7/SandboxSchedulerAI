package webhook

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

func TestPublicTypeMapping(t *testing.T) {
	for _, c := range []struct {
		e    store.Event
		want string // "" = not published
	}{
		{evClaimed("sb"), TypeSandboxCreated},
		{evTransition("sb", core.StateAssigned, core.StateRunning, ""), TypeSandboxRunning},
		{evTransition("sb", core.StateRunning, core.StateRecycling, core.ReasonReleased), TypeSandboxTerminated},
		{evTransition("sb", core.StateRunning, core.StateRecycling, core.ReasonIdleTimeout), TypeSandboxTerminated},
		{evTransition("sb", core.StateAssigned, core.StateRecycling, core.ReasonError), TypeSandboxTerminated},
		{evTransition("sb", core.StateRunning, core.StateFailed, core.ReasonOOMKilled), TypeSandboxTerminated},
		{evTransition("sb", core.StateRunning, core.StateUnknown, ""), TypeSandboxTerminated},
		{evTransition("sb", core.StatePaused, core.StateRecycling, core.ReasonMaxLifetime), TypeSandboxTerminated},
		// Already reported as terminated: the rest of the teardown is one event.
		{evTransition("sb", core.StateFailed, core.StateRecycling, core.ReasonOOMKilled), ""},
		{evTransition("sb", core.StateUnknown, core.StateRecycling, ""), ""},
		{evTransition("sb", core.StateRecycling, core.StateDestroyed, core.ReasonReleased), TypeSandboxDestroyed},
		// No API reaches pause; it is not part of the contract.
		{evTransition("sb", core.StateRunning, core.StatePaused, ""), ""},
		{evTransition("sb", core.StatePaused, core.StateRunning, ""), ""},
		// The pool's own life of a sandbox.
		{store.Event{Type: store.EventCreated, ToState: core.StateCreating}, ""},
		{evTransition("sb", core.StateCreating, core.StateWarm, ""), ""},
		{evTransition("sb", core.StateWarm, core.StateRecycling, core.ReasonWarmTTL), ""},
		{store.Event{Type: "command_started"}, TypeCommandStarted},
		{store.Event{Type: "command_finished"}, TypeCommandFinished},
		{store.Event{Type: "orphan_destroyed", Reason: core.ReasonOrphan}, ""},
		{store.Event{Type: "something_new"}, ""},
	} {
		got, ok := publicType(c.e)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("publicType(%s %s->%s) = %q, %v; want %q", c.e.Type, c.e.FromState, c.e.ToState, got, ok, c.want)
		}
	}
	// A sandbox's ordinary life is exactly these public events, once each.
	life := []store.Event{
		{Type: store.EventCreated, ToState: core.StateCreating},
		evTransition("sb", core.StateCreating, core.StateWarm, ""),
		evClaimed("sb"),
		evTransition("sb", core.StateAssigned, core.StateRunning, ""),
		{Type: "command_started"}, {Type: "command_finished"},
		evTransition("sb", core.StateRunning, core.StateFailed, core.ReasonOOMKilled),
		evTransition("sb", core.StateFailed, core.StateRecycling, ""),
		evTransition("sb", core.StateRecycling, core.StateDestroyed, ""),
	}
	var got []string
	for _, e := range life {
		if typ, ok := publicType(e); ok {
			got = append(got, typ)
		}
	}
	want := []string{TypeSandboxCreated, TypeSandboxRunning, TypeCommandStarted, TypeCommandFinished, TypeSandboxTerminated, TypeSandboxDestroyed}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a sandbox's life maps to %v, want %v", got, want)
	}
	// Every type that can be produced can be subscribed to, and nothing else.
	all := EventTypes()
	sort.Strings(all)
	sort.Strings(want)
	if !reflect.DeepEqual(all, want) {
		t.Errorf("EventTypes() = %v, but a sandbox's life produces %v", all, want)
	}
}

func TestTranslateEnvelope(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 123456000, time.FixedZone("x", 3600))
	e := store.WebhookEvent{
		Event: store.Event{ID: 42, SandboxID: "sb-1", TenantID: "acme", Type: store.EventTransition,
			FromState: core.StateRunning, ToState: core.StateRecycling, Reason: core.ReasonIdleTimeout, At: at,
			// Free-form data of an internal transition is not published.
			Data: map[string]any{"cause": "admin_host_destroy", "instance_id": "abc", "error": "podman: boom"}},
		Txid: 7, TemplateID: "python", Metadata: map[string]string{"job": "9"},
	}
	env, ok := Translate(e)
	if !ok {
		t.Fatal("not published")
	}
	body, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"evt_42","type":"sandbox.terminated","version":1,"created_at":"2026-10-10T11:00:00.123456Z",` +
		`"sandbox_id":"sb-1","template_id":"python","data":{"metadata":{"job":"9"},"reason":"idle_timeout","state":"recycling"}}`
	if string(body) != want {
		t.Errorf("envelope:\n got %s\nwant %s", body, want)
	}

	// A command record: the listed fields and nothing else.
	cmd := store.WebhookEvent{Event: store.Event{ID: 43, SandboxID: "sb-1", Type: "command_finished", At: at, Data: map[string]any{
		"pid": 3, "cmd": "python", "args": []any{"-c", "print(1)"}, "pty": false, "exit_code": 0, "reason": "exited",
		"duration_ms": 12, "stdout_bytes": 2, "stderr_bytes": 0, "tag": "t1",
		// What a later version of the recorder might add by mistake.
		"env": map[string]any{"API_KEY": "hunter2"}, "access_token": "sbt_secret", "host_id": "node-1",
	}}}
	env, ok = Translate(cmd)
	if !ok || env.Type != TypeCommandFinished || env.TemplateID != "" {
		t.Fatalf("command envelope: %+v", env)
	}
	body, _ = env.Marshal()
	for _, leak := range []string{"hunter2", "API_KEY", "sbt_secret", "access_token", "host_id", "env"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the command envelope contains %q: %s", leak, body)
		}
	}
	var decoded struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pid", "cmd", "args", "pty", "exit_code", "reason", "duration_ms", "stdout_bytes", "stderr_bytes", "tag"} {
		if _, has := decoded.Data[k]; !has {
			t.Errorf("the command envelope lacks %q: %s", k, body)
		}
	}
	if strings.Contains(string(body), "template_id") {
		t.Errorf("an empty template id is in the body: %s", body)
	}

	if _, ok := Translate(store.WebhookEvent{Event: evTransition("sb", core.StateFailed, core.StateRecycling, "")}); ok {
		t.Error("an unpublished event was translated")
	}
	te := testEnvelope("wh_1", at)
	if te.Type != TypeTest || !strings.HasPrefix(te.ID, "evt_test_") || te.SandboxID != "" || te.Data["webhook_id"] != "wh_1" {
		t.Errorf("test envelope: %+v", te)
	}
	if testEnvelope("wh_1", at).ID == te.ID {
		t.Error("two test events share an id")
	}
}

func TestNormalizeEventTypes(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want []string
	}{
		{nil, []string{"*"}},
		{[]string{}, []string{"*"}},
		{[]string{"*"}, []string{"*"}},
		{[]string{"sandbox.created", "*"}, []string{"*"}},
		{[]string{"sandbox.terminated", "command.finished", "sandbox.terminated"}, []string{"command.finished", "sandbox.terminated"}},
		{[]string{" sandbox.running "}, []string{"sandbox.running"}},
	} {
		got, err := NormalizeEventTypes(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("NormalizeEventTypes(%v) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range [][]string{{"sandbox.paused"}, {"webhook.test"}, {"transition"}, {""}, {"sandbox.*"}, {"SANDBOX.CREATED"}} {
		if _, err := NormalizeEventTypes(bad); err == nil {
			t.Errorf("NormalizeEventTypes(%v) accepted", bad)
		}
	}
	if !Subscribed([]string{"*"}, TypeCommandStarted) || !Subscribed([]string{"a", TypeSandboxRunning}, TypeSandboxRunning) ||
		Subscribed([]string{TypeSandboxRunning}, TypeSandboxCreated) || Subscribed(nil, TypeSandboxCreated) {
		t.Error("Subscribed")
	}
}

func TestBackoffSchedule(t *testing.T) {
	base, max := 5*time.Second, 15*time.Minute
	// Without jitter (rnd = 0.5): 5s doubling to the cap.
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 320 * time.Second, 640 * time.Second, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := backoff(i+1, base, max, 0.5); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	// Jitter spreads a delay over [0.8, 1.2) of nominal and never past the cap.
	if lo, hi := backoff(3, base, max, 0), backoff(3, base, max, 0.999999); lo != 16*time.Second || hi <= 23*time.Second || hi >= 24*time.Second {
		t.Errorf("jitter range of the third delay: %s .. %s", lo, hi)
	}
	for n := 1; n < 80; n++ {
		for _, r := range []float64{0, 0.3, 0.999999} {
			if d := backoff(n, base, max, r); d <= 0 || d > max {
				t.Fatalf("backoff(%d, rnd %v) = %s", n, r, d)
			}
		}
	}
	if d := backoff(0, base, max, 0.5); d != base {
		t.Errorf("backoff(0) = %s", d)
	}
	if d := backoff(5, time.Second, time.Second, 0.5); d != time.Second {
		t.Errorf("base == max: %s", d)
	}
}
