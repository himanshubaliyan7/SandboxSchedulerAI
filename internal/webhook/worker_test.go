package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool/pooltest"
	"github.com/himanshubaliyan7/warmbox/internal/store"
	"github.com/himanshubaliyan7/warmbox/internal/webhook/webhooktest"
)

// sent is one request the fake sender saw.
type sent struct {
	URL     string
	ID      string
	Type    string
	Attempt int
	At      time.Time
	Header  http.Header
	Body    []byte
	ctx     context.Context
}

// fakeSender answers from a function and records what it was asked to
// send, how many requests were in flight per URL and in total.
type fakeSender struct {
	clock   Clock
	respond func(s sent) Result // nil: 200

	mu          sync.Mutex
	log         []sent
	inFlight    map[string]int
	maxInFlight map[string]int
	total, max  int
}

func newFakeSender(clock Clock) *fakeSender {
	return &fakeSender{clock: clock, inFlight: map[string]int{}, maxInFlight: map[string]int{}}
}

func ok() Result { return Result{Status: 200} }
func status(code int) Result {
	return Result{Status: code, Code: CodeHTTPStatus, Error: "the endpoint answered " + strconv.Itoa(code)}
}
func timedOut() Result {
	return Result{Code: CodeTimeout, Error: "the endpoint did not answer in time"}
}
func (f *fakeSender) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.log) }
func (f *fakeSender) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.log...)
}

func (f *fakeSender) ids() []string {
	var out []string
	for _, s := range f.all() {
		out = append(out, s.ID)
	}
	return out
}

func (f *fakeSender) Send(ctx context.Context, req Request) Result {
	attempt, _ := strconv.Atoi(req.Header.Get(HeaderAttempt))
	s := sent{URL: req.URL, ID: req.Header.Get(HeaderID), Type: req.Header.Get(HeaderEventType), Attempt: attempt,
		At: f.clock.Now(), Header: req.Header, Body: req.Body, ctx: ctx}
	f.mu.Lock()
	f.log = append(f.log, s)
	f.inFlight[req.URL]++
	f.maxInFlight[req.URL] = max(f.maxInFlight[req.URL], f.inFlight[req.URL])
	f.total++
	f.max = max(f.max, f.total)
	respond := f.respond
	f.mu.Unlock()
	res := ok()
	if respond != nil {
		res = respond(s)
	}
	f.mu.Lock()
	f.inFlight[req.URL]--
	f.total--
	f.mu.Unlock()
	return res
}

type workerEnv struct {
	t      *testing.T
	clock  *pooltest.FakeClock
	st     *webhooktest.MemStore
	sender *fakeSender
	w      *Worker
}

const tenant = "acme"

func newWorkerEnv(t *testing.T, mut func(*Config)) *workerEnv {
	t.Helper()
	clock := pooltest.NewFakeClock()
	e := &workerEnv{t: t, clock: clock, st: webhooktest.NewMemStore(clock.Now), sender: newFakeSender(clock)}
	cfg := Config{Store: e.st, Sender: e.sender, Clock: clock, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Rand: func() float64 { return 0.5 }} // no jitter: delays are exactly nominal
	if mut != nil {
		mut(&cfg)
	}
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.w = w
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		w.Stop(ctx)
	})
	return e
}

func (e *workerEnv) hook(id string, types ...string) store.Webhook {
	e.t.Helper()
	if len(types) == 0 {
		types = []string{AllEvents}
	}
	secret, _ := NewSecret()
	w, err := e.st.CreateWebhook(context.Background(), store.NewWebhook{ID: id, TenantID: tenant, URL: "https://" + id + ".example.com/hook",
		Secret: secret, EventTypes: types}, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *workerEnv) emit(ev store.Event) store.WebhookEvent {
	return e.st.Emit(tenant, ev, e.clock.Now())
}

func (e *workerEnv) commands(n int) []string {
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, EventID(e.emit(evCommand("sb-1", i)).ID))
	}
	return ids
}

// step runs one poll and waits for the deliveries it started.
func (e *workerEnv) step() {
	e.w.Poll(context.Background())
	e.w.Wait()
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s:\n got %v\nwant %v", what, got, want)
	}
}

func TestWorkerDeliversInOrderAndSigned(t *testing.T) {
	e := newWorkerEnv(t, nil)
	e.emit(evCommand("sb-0", 0)) // before the webhook existed: never sent
	wh := e.hook("wh1")
	life := []store.Event{
		evClaimed("sb-1"),
		evTransition("sb-1", core.StateAssigned, core.StateRunning, ""),
		{SandboxID: "sb-1", Type: internalCommandStarted, Data: map[string]any{"pid": 1, "cmd": "true"}},
		evCommand("sb-1", 1),
		evTransition("sb-1", core.StateRunning, core.StateFailed, core.ReasonOOMKilled),
		evTransition("sb-1", core.StateFailed, core.StateRecycling, ""), // not public
		evTransition("sb-1", core.StateRecycling, core.StateDestroyed, core.ReasonOOMKilled),
	}
	var last store.WebhookEvent
	for _, ev := range life {
		last = e.emit(ev)
	}
	e.st.Emit("someone-else", evClaimed("sb-9"), e.clock.Now()) // another tenant's event

	e.step()
	got := e.sender.all()
	var types []string
	for i, s := range got {
		types = append(types, s.Type)
		if s.URL != wh.URL || s.Attempt != 1 {
			t.Errorf("request %d: url %q attempt %d", i, s.URL, s.Attempt)
		}
		// What arrives verifies with the secret and nothing but the
		// headers and the raw body, as a receiver would do it.
		if err := Verify(wh.Secret, s.Header, s.Body, e.clock.Now(), 0); err != nil {
			t.Errorf("request %d does not verify: %v", i, err)
		}
		var env Envelope
		if err := json.Unmarshal(s.Body, &env); err != nil || env.ID != s.ID || env.Type != s.Type || env.Version != 1 ||
			env.SandboxID != "sb-1" || env.TemplateID != "py" {
			t.Errorf("request %d: body %s (%v)", i, s.Body, err)
		}
		if s.Header.Get(HeaderTimestamp) != strconv.FormatInt(e.clock.Now().Unix(), 10) {
			t.Errorf("request %d: timestamp %q", i, s.Header.Get(HeaderTimestamp))
		}
	}
	eq(t, "event types", types, []string{TypeSandboxCreated, TypeSandboxRunning, TypeCommandStarted, TypeCommandFinished,
		TypeSandboxTerminated, TypeSandboxDestroyed})
	if !bytes.Contains(got[4].Body, []byte(`"reason":"oom_killed"`)) || !bytes.Contains(got[4].Body, []byte(`"state":"failed"`)) {
		t.Errorf("terminated body: %s", got[4].Body)
	}

	row := e.st.Hook("wh1")
	if row.CursorEventID != last.ID || row.Attempts != 0 || row.FailingSince != nil || row.LastSuccessAt == nil || row.LeaseUntil != nil {
		t.Errorf("after delivery: %+v", row)
	}
	log := e.st.Log("wh1")
	if len(log) != 6 {
		t.Fatalf("delivery log has %d rows", len(log))
	}
	for i, d := range log {
		if d.Status != store.DeliverySucceeded || d.Attempt != 1 || d.ResponseStatus != 200 || d.MessageID != got[i].ID || d.EventType != types[i] {
			t.Errorf("log row %d: %+v", i, d)
		}
	}
	// Nothing left: the next poll claims nothing and sends nothing.
	e.step()
	if e.sender.n() != 6 {
		t.Errorf("%d requests after an idle poll", e.sender.n())
	}
	snap := e.w.Metrics().Snapshot()
	if snap.Succeeded != 6 || snap.Failed != 0 || snap.InFlight != 0 {
		t.Errorf("metrics: %+v", snap)
	}
}

func TestWorkerSubscriptionFilter(t *testing.T) {
	e := newWorkerEnv(t, nil)
	e.hook("wh1", TypeCommandFinished)
	e.hook("wh2", TypeSandboxCreated, TypeSandboxTerminated)
	e.emit(evClaimed("sb-1"))
	e.emit(evTransition("sb-1", core.StateAssigned, core.StateRunning, ""))
	cmd := e.emit(evCommand("sb-1", 1))
	last := e.emit(evTransition("sb-1", core.StateRunning, core.StateRecycling, core.ReasonReleased))
	e.step()
	byURL := map[string][]string{}
	for _, s := range e.sender.all() {
		byURL[s.URL] = append(byURL[s.URL], s.Type)
	}
	eq(t, "wh1", byURL["https://wh1.example.com/hook"], []string{TypeCommandFinished})
	eq(t, "wh2", byURL["https://wh2.example.com/hook"], []string{TypeSandboxCreated, TypeSandboxTerminated})
	// Events that were filtered out are behind the cursor all the same.
	for _, id := range []string{"wh1", "wh2"} {
		if row := e.st.Hook(id); row.CursorEventID != last.ID {
			t.Errorf("%s: cursor at %d, want %d (command was %d)", id, row.CursorEventID, last.ID, cmd.ID)
		}
	}
	e.step()
	if e.sender.n() != 3 {
		t.Errorf("%d requests", e.sender.n())
	}
}

// The retry schedule: 5s doubling to the cap, the same event every time,
// nothing behind it sent before it, and the count reset by a success.
func TestWorkerRetryBackoffSchedule(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Retention = -1; c.DisableAfter = -1 })
	e.hook("wh1")
	ids := e.commands(3)
	fail := true
	e.sender.respond = func(sent) Result {
		if fail {
			return status(503)
		}
		return ok()
	}
	e.step()
	if e.sender.n() != 1 {
		t.Fatalf("%d requests after the first poll", e.sender.n())
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		320 * time.Second, 640 * time.Second, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, d := range want {
		// Not a moment early ...
		e.clock.Advance(d - time.Millisecond)
		e.step()
		if n := e.sender.n(); n != i+1 {
			t.Fatalf("attempt %d was made %s after attempt %d; the delay is %s", n, d-time.Millisecond, i+1, d)
		}
		// ... and then at once.
		e.clock.Advance(time.Millisecond)
		e.step()
		if n := e.sender.n(); n != i+2 {
			t.Fatalf("no attempt %d after %s", i+2, d)
		}
	}
	for i, s := range e.sender.all() {
		if s.ID != ids[0] || s.Attempt != i+1 {
			t.Fatalf("request %d: event %s attempt %d; want %s attempt %d", i, s.ID, s.Attempt, ids[0], i+1)
		}
	}
	row := e.st.Hook("wh1")
	if row.Attempts != len(want)+1 || row.FailingSince == nil || row.LastError == "" || row.CursorEventID != e.st.Head()-3 {
		t.Fatalf("while failing: %+v", row)
	}

	// The endpoint recovers: the blocked event, then the two behind it.
	fail = false
	e.clock.Advance(15 * time.Minute)
	e.step()
	all := e.sender.ids()
	eq(t, "after recovery", all[len(all)-3:], ids)
	row = e.st.Hook("wh1")
	if row.Attempts != 0 || row.FailingSince != nil || row.LastError != "" || row.CursorEventID != e.st.Head() {
		t.Fatalf("after recovery: %+v", row)
	}
	// The count starts again: a new failure waits the base delay.
	fail = true
	e.commands(1)
	e.step()
	e.clock.Advance(5 * time.Second)
	before := e.sender.n()
	e.step()
	if e.sender.n() != before+1 || e.sender.all()[before].Attempt != 2 {
		t.Fatalf("the backoff did not start again at the base delay")
	}
	log := e.st.Log("wh1")
	if log[0].Status != store.DeliveryFailed || log[0].ResponseStatus != 503 || log[0].ErrorCode != CodeHTTPStatus || log[0].Attempt != 1 {
		t.Errorf("first log row: %+v", log[0])
	}
}

func TestWorkerJitterIsApplied(t *testing.T) {
	rnd := 0.0
	e := newWorkerEnv(t, func(c *Config) { c.Rand = func() float64 { return rnd } })
	e.hook("wh1")
	e.commands(1)
	e.sender.respond = func(sent) Result { return status(500) }
	e.step() // attempt 1, delay 5s * 0.8
	e.clock.Advance(4*time.Second - time.Millisecond)
	e.step()
	if e.sender.n() != 1 {
		t.Fatal("retried before 0.8 x 5s")
	}
	e.clock.Advance(time.Millisecond)
	rnd = 0.999999
	e.step() // attempt 2, delay just under 10s * 1.2
	if e.sender.n() != 2 {
		t.Fatal("no retry at 0.8 x 5s")
	}
	e.clock.Advance(11 * time.Second)
	e.step()
	if e.sender.n() != 2 {
		t.Fatal("retried before 1.2 x 10s")
	}
	e.clock.Advance(time.Second)
	e.step()
	if e.sender.n() != 3 {
		t.Fatal("no retry at 1.2 x 10s")
	}
}

// What the endpoint answers decides what happens to the event.
func TestWorkerFailureClasses(t *testing.T) {
	t.Run("5xx and timeouts are retried and never dropped", func(t *testing.T) {
		e := newWorkerEnv(t, func(c *Config) { c.MaxRejections = 3; c.Retention = -1; c.DisableAfter = -1 })
		e.hook("wh1")
		ids := e.commands(2)
		answers := []Result{status(500), timedOut(), status(502), {Code: CodeConnectionFailed, Error: "refused"}, status(503),
			{Code: CodeTLSError, Error: "tls"}, {Code: CodeRedirect, Status: 301, Error: "redirect"},
			{Code: CodeDestinationNotAllowed, Error: "no"}, status(408), status(425), status(429), status(599)}
		i := 0
		e.sender.respond = func(sent) Result { i++; return answers[i-1] }
		for range answers {
			e.step()
			e.clock.Advance(15 * time.Minute)
		}
		if e.sender.n() != len(answers) {
			t.Fatalf("%d attempts for %d failures", e.sender.n(), len(answers))
		}
		for _, s := range e.sender.all() {
			if s.ID != ids[0] {
				t.Fatalf("event %s was sent while %s was failing", s.ID, ids[0])
			}
		}
		row := e.st.Hook("wh1")
		if row.Attempts != len(answers) || row.DroppedEvents != 0 || !row.Enabled {
			t.Fatalf("after %d transient failures: %+v", len(answers), row)
		}
		for _, d := range e.st.Log("wh1") {
			if d.Status != store.DeliveryFailed {
				t.Errorf("log row: %+v", d)
			}
		}
		if snap := e.w.Metrics().Snapshot(); snap.Failures[CodeTimeout] != 1 || snap.Failures[CodeHTTPStatus] != 7 || snap.Failed != uint64(len(answers)) {
			t.Errorf("metrics: %+v", snap)
		}
	})

	t.Run("a 4xx rejection is retried a few times, then the event is dropped and the next one sent", func(t *testing.T) {
		e := newWorkerEnv(t, func(c *Config) { c.MaxRejections = 3 })
		e.hook("wh1")
		ids := e.commands(3)
		e.sender.respond = func(s sent) Result {
			if s.ID == ids[0] {
				return status(422)
			}
			return ok()
		}
		e.step()
		e.clock.Advance(5 * time.Second)
		e.step()
		if e.sender.n() != 2 || e.st.Hook("wh1").DroppedEvents != 0 {
			t.Fatalf("after two rejections: %d requests, %+v", e.sender.n(), e.st.Hook("wh1"))
		}
		e.clock.Advance(10 * time.Second)
		e.step() // third rejection: dropped, and the turn goes on
		eq(t, "requests", e.sender.ids(), []string{ids[0], ids[0], ids[0], ids[1], ids[2]})
		row := e.st.Hook("wh1")
		if row.DroppedEvents != 1 || row.Attempts != 0 || row.CursorEventID != e.st.Head() || !row.Enabled {
			t.Fatalf("after the drop: %+v", row)
		}
		log := e.st.Log("wh1")
		var statuses []string
		for _, d := range log {
			statuses = append(statuses, d.Status)
		}
		eq(t, "log", statuses, []string{"failed", "failed", "failed", "dropped", "succeeded", "succeeded"})
		if d := log[3]; d.ErrorCode != CodeRejected || d.DroppedEvents != 1 || d.MessageID != ids[0] || d.ResponseStatus != 422 {
			t.Errorf("drop row: %+v", d)
		}
		if snap := e.w.Metrics().Snapshot(); snap.Dropped[CodeRejected] != 1 {
			t.Errorf("metrics: %+v", snap)
		}
	})

	t.Run("410 Gone disables the webhook at once", func(t *testing.T) {
		e := newWorkerEnv(t, nil)
		e.hook("wh1")
		e.commands(2)
		e.sender.respond = func(sent) Result { return status(410) }
		e.step()
		row := e.st.Hook("wh1")
		if row.Enabled || row.DisabledReason != store.WebhookDisabledGone || row.DisabledAt == nil || row.LeaseUntil != nil {
			t.Fatalf("after 410: %+v", row)
		}
		e.clock.Advance(time.Hour)
		e.step()
		if e.sender.n() != 1 {
			t.Fatalf("%d requests to an endpoint that said it is gone", e.sender.n())
		}
		if snap := e.w.Metrics().Snapshot(); snap.Disabled[store.WebhookDisabledGone] != 1 {
			t.Errorf("metrics: %+v", snap)
		}
	})

	t.Run("Retry-After is honoured up to the cap", func(t *testing.T) {
		e := newWorkerEnv(t, nil)
		e.hook("wh1")
		e.commands(1)
		retryAfter := 2 * time.Minute
		e.sender.respond = func(sent) Result {
			r := status(429)
			r.RetryAfter = retryAfter
			return r
		}
		e.step()
		e.clock.Advance(2*time.Minute - time.Second)
		e.step()
		if e.sender.n() != 1 {
			t.Fatal("retried before the endpoint's Retry-After")
		}
		retryAfter = 6 * time.Hour // more patience than the cap grants
		e.clock.Advance(time.Second)
		e.step()
		if e.sender.n() != 2 {
			t.Fatal("no retry after Retry-After")
		}
		e.clock.Advance(15 * time.Minute)
		e.step()
		if e.sender.n() != 3 {
			t.Fatal("a Retry-After beyond the cap was honoured in full")
		}
	})
}

// A webhook whose deliveries fail for DisableAfter is switched off.
func TestWorkerAutoDisable(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Retention = -1; c.DisableAfter = 72 * time.Hour })
	e.hook("wh1")
	e.hook("healthy")
	e.commands(1)
	down := true
	e.sender.respond = func(s sent) Result {
		if down && strings.Contains(s.URL, "wh1") {
			return timedOut()
		}
		return ok()
	}
	e.step()
	// 71 hours of failure: still enabled, still trying.
	for h := 0; h < 71; h++ {
		e.clock.Advance(time.Hour)
		e.step()
	}
	row := e.st.Hook("wh1")
	if !row.Enabled || row.FailingSince == nil || row.Attempts < 70 {
		t.Fatalf("after 71h of failures: %+v", row)
	}
	e.clock.Advance(time.Hour)
	e.step()
	row = e.st.Hook("wh1")
	if row.Enabled || row.DisabledReason != store.WebhookDisabledFailed || row.DisabledAt == nil {
		t.Fatalf("after 72h of failures: %+v", row)
	}
	n := e.sender.n()
	e.commands(2)
	e.clock.Advance(24 * time.Hour)
	e.step()
	for _, s := range e.sender.all()[n:] {
		if strings.Contains(s.URL, "wh1") {
			t.Fatalf("a disabled webhook was sent %s", s.ID)
		}
	}
	if h := e.st.Hook("healthy"); !h.Enabled || h.CursorEventID != e.st.Head() {
		t.Errorf("the healthy webhook: %+v", h)
	}
	if snap := e.w.Metrics().Snapshot(); snap.Disabled[store.WebhookDisabledFailed] != 1 {
		t.Errorf("metrics: %+v", snap)
	}

	// Enabled again by its owner: a clean slate, starting at the present.
	on := true
	if _, err := e.st.UpdateWebhook(context.Background(), tenant, "wh1", store.WebhookUpdate{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	down = false
	fresh := e.commands(1)
	n = e.sender.n()
	e.step()
	var got []string
	for _, s := range e.sender.all()[n:] {
		if strings.Contains(s.URL, "wh1") {
			got = append(got, s.ID)
		}
	}
	eq(t, "after re-enabling", got, fresh)

	// A success in between ends the run of failures: 71h + 71h of trouble
	// with one good delivery in the middle does not disable.
	e2 := newWorkerEnv(t, func(c *Config) { c.Retention = -1; c.DisableAfter = 72 * time.Hour })
	e2.hook("wh1")
	bad := true
	e2.sender.respond = func(sent) Result {
		if bad {
			return status(500)
		}
		return ok()
	}
	for round := 0; round < 2; round++ {
		bad = true
		e2.commands(1)
		e2.step()
		for h := 0; h < 71; h++ {
			e2.clock.Advance(time.Hour)
			e2.step()
		}
		bad = false
		e2.clock.Advance(15 * time.Minute)
		e2.step()
		if row := e2.st.Hook("wh1"); !row.Enabled || row.FailingSince != nil {
			t.Fatalf("round %d: %+v", round, row)
		}
	}
}

// The backlog is bounded by age: events older than the retention window
// when their turn comes are dropped, counted and recorded, not sent.
func TestWorkerBoundedBacklog(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Retention = 24 * time.Hour; c.DisableAfter = -1; c.BatchSize = 10 })
	e.hook("wh1")
	down := true
	e.sender.respond = func(sent) Result {
		if down {
			return status(503)
		}
		return ok()
	}
	old := e.commands(35) // the endpoint is down when these happen
	e.step()
	e.clock.Advance(12 * time.Hour)
	mid := e.commands(2)
	e.step()
	if row := e.st.Hook("wh1"); row.DroppedEvents != 0 || row.CursorEventID != e.st.Head()-37 {
		t.Fatalf("inside the window nothing is dropped: %+v", row)
	}
	e.clock.Advance(12*time.Hour + time.Minute) // the first 35 are now older than 24h
	fresh := e.commands(1)
	down = false
	n := e.sender.n()
	e.step()

	var got []string
	for _, s := range e.sender.all()[n:] {
		got = append(got, s.ID)
	}
	eq(t, "sent after the outage", got, append(append([]string{}, mid...), fresh...))
	for _, s := range e.sender.all()[n:] {
		for _, o := range old {
			if s.ID == o {
				t.Fatalf("expired event %s was sent", o)
			}
		}
	}
	row := e.st.Hook("wh1")
	if row.DroppedEvents != 35 || row.CursorEventID != e.st.Head() || row.Attempts != 0 || row.FailingSince != nil {
		t.Fatalf("after the outage: %+v", row)
	}
	var droppedRows, droppedTotal int
	for _, d := range e.st.Log("wh1") {
		if d.Status == store.DeliveryDropped {
			droppedRows++
			droppedTotal += d.DroppedEvents
			if d.ErrorCode != CodeRetentionExpired || d.EventID == 0 || !strings.Contains(d.Error, "24h") {
				t.Errorf("gap row: %+v", d)
			}
		}
	}
	if droppedTotal != 35 || droppedRows < 1 || droppedRows > 4 {
		t.Errorf("the gap is recorded as %d rows for %d events", droppedRows, droppedTotal)
	}
	if snap := e.w.Metrics().Snapshot(); snap.Dropped[CodeRetentionExpired] != 35 {
		t.Errorf("metrics: %+v", snap)
	}

	// An endpoint that stays down: its backlog never grows past the
	// window, and nothing is sent for what fell out of it.
	e2 := newWorkerEnv(t, func(c *Config) { c.Retention = time.Hour; c.DisableAfter = -1 })
	e2.hook("wh1")
	e2.sender.respond = func(sent) Result { return status(500) }
	for i := 0; i < 48; i++ {
		e2.commands(10)
		e2.clock.Advance(30 * time.Minute)
		e2.step()
	}
	st, _ := e2.st.WebhookStats(context.Background())
	if st.Backlog > 30 {
		t.Errorf("backlog of an endpoint that is down for a day with a 1h window: %d events", st.Backlog)
	}
	if row := e2.st.Hook("wh1"); row.DroppedEvents < 440 {
		t.Errorf("dropped %d of 480", row.DroppedEvents)
	}
}

// One slow endpoint holds one slot, however many events it has; the
// other webhooks are served meanwhile, and no webhook ever has two
// requests in flight.
func TestWorkerConcurrencyAndFairness(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Concurrency = 3 })
	e.hook("slow")
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		e.hook(id)
	}
	e.commands(4)
	gate := make(chan struct{})
	entered := make(chan struct{}, 16)
	e.sender.respond = func(s sent) Result {
		if strings.Contains(s.URL, "slow") {
			entered <- struct{}{}
			<-gate
		}
		return ok()
	}
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	fastDone := func() bool {
		for _, id := range []string{"a", "b", "c", "d", "e"} {
			if e.st.Hook(id).CursorEventID != e.st.Head() {
				return false
			}
		}
		return true
	}
	slowEntered := false
	for !fastDone() || !slowEntered {
		if time.Now().After(deadline) {
			t.Fatalf("the fast webhooks were not served while one endpoint hung (slow endpoint reached: %v)", slowEntered)
		}
		e.w.Poll(ctx)
		select {
		case <-entered:
			slowEntered = true
		case <-time.After(time.Millisecond):
		}
	}
	if row := e.st.Hook("slow"); row.CursorEventID == e.st.Head() || row.LeaseUntil == nil {
		t.Fatalf("the slow webhook: %+v", row)
	}
	// More polls while it hangs do not start a second request to it.
	for i := 0; i < 20; i++ {
		e.w.Poll(ctx)
	}
	close(gate)
	e.w.Wait()
	e.step()
	e.sender.mu.Lock()
	defer e.sender.mu.Unlock()
	for url, n := range e.sender.maxInFlight {
		if n != 1 {
			t.Errorf("%s had %d requests in flight at once", url, n)
		}
	}
	if e.sender.max > 3 {
		t.Errorf("%d requests in flight with a concurrency of 3", e.sender.max)
	}
	if len(e.sender.log) != 6*4 {
		t.Errorf("%d requests, want %d", len(e.sender.log), 6*4)
	}
}

func TestWorkerConcurrencyIsBounded(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Concurrency = 2 })
	for i := 0; i < 6; i++ {
		e.hook(fmt.Sprintf("h%d", i))
	}
	e.commands(1)
	gate := make(chan struct{})
	e.sender.respond = func(sent) Result { <-gate; return ok() }
	for i := 0; i < 10; i++ {
		e.w.Poll(context.Background())
	}
	waitFor(t, "two requests in flight", func() bool { e.sender.mu.Lock(); defer e.sender.mu.Unlock(); return e.sender.total == 2 })
	time.Sleep(20 * time.Millisecond)
	e.sender.mu.Lock()
	total, claims := e.sender.total, len(e.sender.log)
	e.sender.mu.Unlock()
	if total != 2 || claims != 2 {
		t.Fatalf("%d in flight, %d started; the limit is 2", total, claims)
	}
	if snap := e.w.Metrics().Snapshot(); snap.InFlight != 2 {
		t.Errorf("in-flight gauge: %d", snap.InFlight)
	}
	close(gate)
	e.w.Wait()
	for i := 0; i < 5; i++ {
		e.step()
	}
	if e.sender.n() != 6 || e.sender.max != 2 {
		t.Fatalf("%d requests, at most %d at once", e.sender.n(), e.sender.max)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A webhook with a long backlog gives its slot up when its turn budget is
// spent and goes behind the webhooks that are waiting.
func TestWorkerTurnBudget(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Concurrency = 1; c.TurnBudget = 30 * time.Second })
	e.hook("busy")
	e.commands(10)
	e.sender.respond = func(sent) Result { e.clock.Advance(10 * time.Second); return ok() } // a slow, healthy endpoint
	e.step()
	if e.sender.n() != 3 {
		t.Fatalf("%d events sent in a 30s turn of 10s requests", e.sender.n())
	}
	if row := e.st.Hook("busy"); row.LeaseUntil != nil || row.Attempts != 0 || row.FailingSince != nil {
		t.Fatalf("after the turn: %+v", row)
	}
	// Another webhook has been waiting since before that turn ended.
	e.hook("other")
	head := e.st.Head()
	e.st.Edit("other", func(w *store.Webhook) {
		w.NextAttemptAt = e.clock.Now().Add(-time.Minute)
		w.CursorTxid, w.CursorEventID = head-1, head-1
	})
	e.step()
	if s := e.sender.all()[3]; !strings.Contains(s.URL, "other") {
		t.Fatalf("the next turn went to %s", s.URL)
	}
	for i := 0; i < 5; i++ {
		e.step()
	}
	if row := e.st.Hook("busy"); row.CursorEventID != e.st.Head() {
		t.Fatalf("the backlog was not finished: %+v", row)
	}
	seen := map[string]int{}
	for _, s := range e.sender.all() {
		seen[s.URL+s.ID]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s sent %d times", k, n)
		}
	}
}

// Drain sends everything that is due and returns; a webhook that is
// backing off is not waited for.
func TestWorkerDrain(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.Concurrency = 1; c.BatchSize = 2 })
	e.hook("good")
	e.hook("bad")
	ids := e.commands(5)
	e.sender.respond = func(s sent) Result {
		if strings.Contains(s.URL, "bad") {
			return status(500)
		}
		return ok()
	}
	done := make(chan struct{})
	go func() { e.w.Drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return")
	}
	var good []string
	bad := 0
	for _, s := range e.sender.all() {
		if strings.Contains(s.URL, "good") {
			good = append(good, s.ID)
		} else {
			bad++
		}
	}
	eq(t, "drained to the healthy webhook", good, ids)
	if bad != 1 {
		t.Errorf("%d attempts at the failing webhook during a drain", bad)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.w.Drain(ctx) // returns at once
}

// Stop lets the attempt in flight finish and records it; nothing new is
// started.
func TestWorkerGracefulStop(t *testing.T) {
	e := newWorkerEnv(t, nil)
	e.hook("wh1")
	ids := e.commands(3)
	gate := make(chan struct{})
	entered := make(chan struct{}, 4)
	e.sender.respond = func(sent) Result { entered <- struct{}{}; <-gate; return ok() }
	e.w.Start()
	<-entered

	stopped := make(chan struct{})
	go func() {
		e.w.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a request was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the request finished")
	}
	eq(t, "sent", e.sender.ids(), ids[:1])
	row := e.st.Hook("wh1")
	if row.CursorEventID != e.st.Head()-2 || row.LeaseUntil != nil || row.LastSuccessAt == nil || row.Attempts != 0 {
		t.Fatalf("after a graceful stop: %+v", row)
	}
	if log := e.st.Log("wh1"); len(log) != 1 || log[0].Status != store.DeliverySucceeded {
		t.Fatalf("log: %+v", log)
	}
	// Stopped means stopped.
	e.w.Nudge()
	e.w.Poll(e.w.runCtx)
	e.w.Wait()
	if e.sender.n() != 1 {
		t.Fatalf("%d requests after Stop", e.sender.n())
	}
	e.w.Stop(context.Background()) // idempotent
}

// When the shutdown budget runs out, the attempt in flight is aborted. It
// is not the endpoint's failure: nothing is counted against the webhook,
// the lease is given back, and the event is sent again later.
func TestWorkerStopAbortsWhenTheBudgetRunsOut(t *testing.T) {
	e := newWorkerEnv(t, nil)
	e.hook("wh1")
	ids := e.commands(2)
	entered := make(chan struct{}, 4)
	e.sender.respond = func(s sent) Result {
		entered <- struct{}{}
		<-s.ctx.Done() // an endpoint that never answers
		return Result{Code: CodeConnectionFailed, Error: "the attempt was cancelled"}
	}
	e.w.Start()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { e.w.Stop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after its context ended")
	}
	row := e.st.Hook("wh1")
	if row.Attempts != 0 || row.FailingSince != nil || row.LastFailureAt != nil || row.LeaseUntil != nil || row.CursorEventID != e.st.Head()-2 {
		t.Fatalf("after an aborted attempt: %+v", row)
	}
	if log := e.st.Log("wh1"); len(log) != 0 {
		t.Fatalf("an aborted attempt was logged: %+v", log)
	}
	if snap := e.w.Metrics().Snapshot(); snap.Failed != 0 {
		t.Errorf("metrics: %+v", snap)
	}

	// The next process picks up where this one left off.
	next := newWorkerEnv(t, nil)
	next.st, next.clock = e.st, e.clock
	w2, err := NewWorker(Config{Store: e.st, Sender: next.sender, Clock: e.clock, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	w2.Poll(context.Background())
	w2.Wait()
	eq(t, "sent by the next worker", next.sender.ids(), ids)
}

// The running loop: started, it delivers what arrives without being
// stepped, on the poll interval or sooner when nudged.
func TestWorkerLoop(t *testing.T) {
	e := newWorkerEnv(t, func(c *Config) { c.PollInterval = time.Second })
	e.hook("wh1")
	e.w.Start()
	first := e.commands(1)
	e.w.Nudge()
	waitFor(t, "the nudged delivery", func() bool { return e.sender.n() == 1 })
	second := e.commands(1)
	// No nudge: the poll interval brings it.
	waitFor(t, "the polled delivery", func() bool {
		e.clock.Advance(time.Second)
		return e.sender.n() == 2
	})
	eq(t, "sent", e.sender.ids(), append(first, second...))
	waitFor(t, "the stats refresh", func() bool { return e.w.Metrics().Snapshot().Stats.Enabled == 1 })
}

// A configuration change while a request is in flight: the step is
// discarded (the lease is gone) and the event is sent again under the new
// configuration. At least once, never lost.
func TestWorkerLeaseLostMidFlight(t *testing.T) {
	e := newWorkerEnv(t, nil)
	e.hook("wh1")
	ids := e.commands(2)
	first := true
	e.sender.respond = func(sent) Result {
		if first {
			first = false
			url := "https://new.example.com/hook"
			if _, err := e.st.UpdateWebhook(context.Background(), tenant, "wh1", store.WebhookUpdate{URL: &url}); err != nil {
				t.Error(err)
			}
		}
		return ok()
	}
	e.step()
	if e.sender.n() != 1 {
		t.Fatalf("%d requests in the turn that lost its lease", e.sender.n())
	}
	if row := e.st.Hook("wh1"); row.CursorEventID != e.st.Head()-2 {
		t.Fatalf("a fenced worker moved the cursor: %+v", row)
	}
	if snap := e.w.Metrics().Snapshot(); snap.LeaseLost != 1 {
		t.Errorf("metrics: %+v", snap)
	}
	e.step()
	all := e.sender.all()
	eq(t, "sent", e.sender.ids(), []string{ids[0], ids[0], ids[1]})
	if all[1].URL != "https://new.example.com/hook" || all[0].URL == all[1].URL {
		t.Errorf("urls: %s then %s", all[0].URL, all[1].URL)
	}
	if all[0].ID != all[1].ID {
		t.Errorf("the duplicate does not carry the same Webhook-Id: %s, %s", all[0].ID, all[1].ID)
	}
}

// The store fails after the endpoint said yes: the event is sent again
// once the lease has run out, with the same id.
func TestWorkerRedeliversWhenProgressCannotBeRecorded(t *testing.T) {
	e := newWorkerEnv(t, nil)
	e.hook("wh1")
	ids := e.commands(1)
	e.st.FailNextRecord(errors.New("connection reset"))
	e.step()
	if row := e.st.Hook("wh1"); row.CursorEventID == e.st.Head() || row.LeaseUntil == nil {
		t.Fatalf("after a failed commit: %+v", row)
	}
	e.step()
	if e.sender.n() != 1 {
		t.Fatal("the webhook was claimed again while its lease was running")
	}
	e.clock.Advance(e.w.cfg.lease())
	e.step()
	eq(t, "sent", e.sender.ids(), []string{ids[0], ids[0]})
	if row := e.st.Hook("wh1"); row.CursorEventID != e.st.Head() {
		t.Fatalf("after the redelivery: %+v", row)
	}
}

func TestWorkerSignsWithBothSecretsDuringRotation(t *testing.T) {
	e := newWorkerEnv(t, nil)
	wh := e.hook("wh1")
	newSecret, _ := NewSecret()
	if _, err := e.st.RotateWebhookSecret(context.Background(), tenant, "wh1", newSecret, time.Hour); err != nil {
		t.Fatal(err)
	}
	e.commands(1)
	e.step()
	s := e.sender.all()[0]
	if n := len(strings.Fields(s.Header.Get(HeaderSignature))); n != 2 {
		t.Fatalf("%d signatures during the grace period", n)
	}
	for _, secret := range []string{newSecret, wh.Secret} {
		if err := Verify(secret, s.Header, s.Body, e.clock.Now(), 0); err != nil {
			t.Errorf("during the grace period: %v", err)
		}
	}
	e.clock.Advance(time.Hour + time.Second)
	e.commands(1)
	e.step()
	s = e.sender.all()[1]
	if n := len(strings.Fields(s.Header.Get(HeaderSignature))); n != 1 {
		t.Fatalf("%d signatures after the grace period", n)
	}
	if err := Verify(newSecret, s.Header, s.Body, e.clock.Now(), 0); err != nil {
		t.Errorf("new secret: %v", err)
	}
	if err := Verify(wh.Secret, s.Header, s.Body, e.clock.Now(), 0); !errors.Is(err, ErrNoMatch) {
		t.Errorf("the old secret still verifies after its grace period: %v", err)
	}
}

func TestWorkerConfigAndMetricsOutput(t *testing.T) {
	if _, err := NewWorker(Config{}); err == nil {
		t.Error("a worker without a store was accepted")
	}
	if _, err := NewWorker(Config{Store: webhooktest.NewMemStore(time.Now), Sender: newFakeSender(RealClock{}), RetryBase: -time.Second}); err == nil {
		t.Error("a negative retry delay was accepted")
	}
	cfg := Config{Store: webhooktest.NewMemStore(time.Now), Sender: newFakeSender(RealClock{})}
	if err := cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	if cfg.lease() <= cfg.TurnBudget+cfg.RequestTimeout {
		t.Errorf("the lease (%s) does not outlast the longest turn (%s + %s)", cfg.lease(), cfg.TurnBudget, cfg.RequestTimeout)
	}

	e := newWorkerEnv(t, nil)
	e.hook("wh1")
	e.commands(1)
	e.sender.respond = func(sent) Result { r := status(500); r.Duration = 20 * time.Millisecond; return r }
	e.step()
	var buf bytes.Buffer
	e.w.Metrics().Write(&buf)
	out := buf.String()
	for _, want := range []string{
		`sbx_webhook_deliveries_total{outcome="failed"} 1`, `sbx_webhook_deliveries_total{outcome="succeeded"} 0`,
		`sbx_webhook_delivery_failures_total{code="http_status"} 1`, `sbx_webhook_dropped_events_total{reason="retention_expired"} 0`,
		`sbx_webhook_auto_disabled_total{reason="failing"} 0`, `sbx_webhook_delivery_duration_seconds_bucket{le="0.025"} 1`,
		`sbx_webhook_delivery_duration_seconds_count 1`, "sbx_webhook_backlog_events ", "sbx_webhook_lag_seconds ",
		`sbx_webhooks{state="enabled"}`, "sbx_webhook_in_flight 0", "# TYPE sbx_webhook_deliveries_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output lacks %q:\n%s", want, out)
		}
	}
}
