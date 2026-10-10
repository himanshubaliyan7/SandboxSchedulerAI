package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Clock is the worker's time source. Every interval, backoff and age it
// computes goes through it, so tests drive time with a fake
// (pooltest.FakeClock satisfies it).
//
// When a webhook is next due is stored and compared by the database with
// its own now(), like every other timestamp of the store; the worker
// only hands it durations.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Store is the part of the state store the worker needs. *store.Store
// satisfies it.
type Store interface {
	ClaimWebhooks(ctx context.Context, c store.WebhookClaim) ([]store.Webhook, error)
	PendingWebhookEvents(ctx context.Context, tenantID string, afterTxid, afterID int64, limit int) ([]store.WebhookEvent, error)
	RecordWebhookProgress(ctx context.Context, p store.WebhookProgress) error
	PruneWebhookDeliveries(ctx context.Context, olderThan time.Duration) (int64, error)
	RepairWebhookCursors(ctx context.Context) (int64, error)
	WebhookStats(ctx context.Context) (store.WebhookStats, error)
}

var _ Store = (*store.Store)(nil)

// Worker defaults.
const (
	DefaultPollInterval         = time.Second
	DefaultConcurrency          = 8
	DefaultBatchSize            = 100
	DefaultTurnBudget           = 30 * time.Second
	DefaultRetention            = 24 * time.Hour
	DefaultDisableAfter         = 72 * time.Hour
	DefaultMaxRejections        = 10
	DefaultDeliveryLogRetention = 7 * 24 * time.Hour
	DefaultHousekeepingInterval = 15 * time.Second

	// leaseMargin is added to the longest a turn can take to get the lease.
	leaseMargin = 30 * time.Second
	// pruneInterval is how often the delivery log is pruned.
	pruneInterval = 10 * time.Minute
)

// Config configures a Worker. Store and Sender are required.
type Config struct {
	Store  Store
	Sender Sender
	Clock  Clock
	Logger *slog.Logger
	// Metrics receives the worker's counters. Default: a private set.
	Metrics *Metrics

	// PollInterval is how often the worker looks for due webhooks when
	// it has no other reason to. It bounds how long a new event waits
	// before its first attempt. Default 1s.
	PollInterval time.Duration
	// Concurrency is how many webhooks are delivered to at the same
	// time. One webhook never takes more than one of these, so a slow
	// endpoint cannot starve the others. Default 8.
	Concurrency int
	// BatchSize is how many events are read per query. Default 100.
	BatchSize int
	// RequestTimeout bounds one attempt. It should be the sender's own
	// timeout; the worker enforces it again. Default 10s.
	RequestTimeout time.Duration
	// TurnBudget is how long a webhook may keep its slot while it has
	// events to send; then it goes to the back of the queue. Default 30s.
	TurnBudget time.Duration

	// RetryBase and RetryMax shape the backoff after a failed attempt:
	// RetryBase doubling to RetryMax, with jitter. Defaults 5s and 15m.
	RetryBase time.Duration
	RetryMax  time.Duration
	// Retention bounds the backlog: an event older than this when its
	// turn comes is dropped, not sent. Default 24h; negative means never.
	Retention time.Duration
	// DisableAfter disables a webhook whose deliveries have failed
	// continuously for this long. Default 72h; negative means never.
	DisableAfter time.Duration
	// MaxRejections is how many attempts an event gets when the endpoint
	// keeps answering with a 4xx status other than 408, 425 and 429,
	// before it is dropped. Default 10; negative means retry like any
	// other failure.
	MaxRejections int
	// DeliveryLogRetention is how long attempt-log rows are kept.
	// Default 7 days; negative means forever.
	DeliveryLogRetention time.Duration
	// HousekeepingInterval is how often the backlog gauges are refreshed.
	// Default 15s.
	HousekeepingInterval time.Duration

	// TenantID restricts the worker to one tenant's webhooks. A server
	// leaves it empty; tests that share a database set it.
	TenantID string
	// Rand returns a number in [0, 1) for the backoff jitter. Default:
	// math/rand.
	Rand func() float64
}

func (c *Config) defaults() error {
	switch {
	case c.Store == nil:
		return errors.New("webhook: Config.Store is required")
	case c.Sender == nil:
		return errors.New("webhook: Config.Sender is required")
	}
	if c.Clock == nil {
		c.Clock = RealClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Metrics == nil {
		c.Metrics = NewMetrics()
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&c.PollInterval, DefaultPollInterval}, {&c.RequestTimeout, DefaultRequestTimeout}, {&c.TurnBudget, DefaultTurnBudget},
		{&c.RetryBase, DefaultRetryBase}, {&c.RetryMax, DefaultRetryMax}, {&c.Retention, DefaultRetention},
		{&c.DisableAfter, DefaultDisableAfter}, {&c.DeliveryLogRetention, DefaultDeliveryLogRetention},
		{&c.HousekeepingInterval, DefaultHousekeepingInterval},
	} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if c.PollInterval < 0 || c.RequestTimeout < 0 || c.TurnBudget < 0 || c.RetryBase < 0 || c.RetryMax < 0 || c.HousekeepingInterval < 0 {
		return errors.New("webhook: intervals, timeouts and retry delays must not be negative")
	}
	if c.RetryMax < c.RetryBase {
		c.RetryMax = c.RetryBase
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	if c.MaxRejections == 0 {
		c.MaxRejections = DefaultMaxRejections
	}
	return nil
}

// lease is how long a claim holds a webhook: the longest turn (the budget,
// plus one attempt that started just inside it) and a margin. A worker is
// therefore always done before its lease can be taken over, which is what
// makes "never two deliveries of one webhook at once" hold across
// processes.
func (c *Config) lease() time.Duration {
	return c.TurnBudget + c.RequestTimeout + leaseMargin
}

// Worker delivers events to webhooks. Create it with NewWorker, call
// Start, and Stop on shutdown.
type Worker struct {
	cfg Config
	log *slog.Logger
	m   *Metrics

	// runCtx ends the poll loop; sendCtx aborts attempts in flight.
	runCtx     context.Context
	stopRun    context.CancelFunc
	sendCtx    context.Context
	abortSends context.CancelFunc

	wake  chan struct{} // a reason to poll before the interval is over
	slots chan struct{} // one token per webhook being delivered to
	wg    sync.WaitGroup
	done  chan struct{} // closed when the poll loop has returned

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewWorker validates cfg and returns a Worker that is not yet running.
func NewWorker(cfg Config) (*Worker, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	w := &Worker{cfg: cfg, log: cfg.Logger.With("component", "webhook"), m: cfg.Metrics,
		wake: make(chan struct{}, 1), slots: make(chan struct{}, cfg.Concurrency), done: make(chan struct{})}
	w.runCtx, w.stopRun = context.WithCancel(context.Background())
	w.sendCtx, w.abortSends = context.WithCancel(context.Background())
	return w, nil
}

// Metrics returns the worker's counters.
func (w *Worker) Metrics() *Metrics { return w.m }

// Start launches the poll loop. It returns at once.
func (w *Worker) Start() {
	w.startOnce.Do(func() { go w.run() })
}

// Nudge makes the worker poll now instead of at the end of the current
// interval. It never blocks; nudges that arrive while one is pending
// collapse into it.
func (w *Worker) Nudge() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Stop stops the worker: no new webhook is claimed, and the ones being
// delivered to finish the attempt they are in, record it and give their
// lease back. If ctx ends first, the attempts in flight are aborted; an
// aborted attempt is not counted as a failure, and its event is sent
// again by whoever claims the webhook next. Stop returns when every
// goroutine of the worker has ended. It is idempotent.
func (w *Worker) Stop(ctx context.Context) {
	w.stopOnce.Do(func() {
		w.stopRun()
		w.startOnce.Do(func() { close(w.done) }) // never started
		finished := make(chan struct{})
		go func() { <-w.done; w.wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-ctx.Done():
			w.abortSends()
			<-finished
		}
		w.abortSends()
	})
}

func (w *Worker) run() {
	defer close(w.done)
	var lastHousekeeping, lastPrune time.Time
	for {
		now := w.cfg.Clock.Now()
		if lastHousekeeping.IsZero() || now.Sub(lastHousekeeping) >= w.cfg.HousekeepingInterval {
			prune := lastPrune.IsZero() || now.Sub(lastPrune) >= pruneInterval
			w.housekeeping(prune)
			lastHousekeeping = now
			if prune {
				lastPrune = now
			}
		}
		// When every slot is taken, more webhooks may be due than were
		// claimed; a slot that frees up nudges the loop, so there is no
		// need to poll faster.
		w.Poll(w.runCtx)
		select {
		case <-w.runCtx.Done():
			return
		case <-w.wake:
		case <-w.cfg.Clock.After(w.cfg.PollInterval):
		}
	}
}

// housekeeping refreshes the gauges and, when prune is set, repairs
// cursors and prunes the delivery log.
func (w *Worker) housekeeping(prune bool) {
	ctx, cancel := context.WithTimeout(w.runCtx, 30*time.Second)
	defer cancel()
	if st, err := w.cfg.Store.WebhookStats(ctx); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not read webhook stats", "err", err)
		}
	} else {
		w.m.setStats(st, w.cfg.Clock.Now())
	}
	if !prune {
		return
	}
	if n, err := w.cfg.Store.RepairWebhookCursors(ctx); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not check webhook cursors", "err", err)
		}
	} else if n > 0 {
		w.log.Warn("webhook cursors pointed past the database's transaction counter and were moved to the present; "+
			"this is expected once after restoring the database from a dump", "webhooks", n)
	}
	if w.cfg.DeliveryLogRetention > 0 {
		if n, err := w.cfg.Store.PruneWebhookDeliveries(ctx, w.cfg.DeliveryLogRetention); err != nil {
			if ctx.Err() == nil {
				w.log.Warn("could not prune the webhook delivery log", "err", err)
			}
		} else if n > 0 {
			w.log.Debug("pruned the webhook delivery log", "rows", n)
		}
	}
}

// Poll claims as many due webhooks as there are free slots and starts
// delivering to each in its own goroutine. It reports whether it filled
// every free slot, in which case more webhooks may be due. The poll loop
// calls it; tests call it directly to step the worker.
func (w *Worker) Poll(ctx context.Context) (saturated bool) {
	free := cap(w.slots) - len(w.slots)
	if free <= 0 {
		return true
	}
	w.m.polled(w.cfg.Clock.Now())
	claimed, err := w.cfg.Store.ClaimWebhooks(ctx, store.WebhookClaim{Limit: free, Lease: w.cfg.lease(), TenantID: w.cfg.TenantID})
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not claim webhooks", "err", err)
		}
		return false
	}
	for _, wh := range claimed {
		w.slots <- struct{}{} // never blocks: at most `free` were claimed and only Poll adds
		w.wg.Add(1)
		w.m.flight(1)
		go func() {
			defer func() {
				<-w.slots
				w.m.flight(-1)
				w.wg.Done()
				w.Nudge()
			}()
			w.deliver(wh)
		}()
	}
	return len(claimed) == free
}

// Wait blocks until every delivery started by Poll has finished. Tests
// use it to step the worker.
func (w *Worker) Wait() { w.wg.Wait() }

// position is a cursor value.
type position struct{ txid, id int64 }

// skipped accumulates the events a turn passed over without sending:
// filtered out by the subscription, not public, or expired. It is
// written with the next progress record, or on its own at the end.
type skipped struct {
	to      position
	any     bool
	dropped int64
	lastID  int64 // last expired event
}

func (s *skipped) pass(e store.WebhookEvent, expired bool) {
	s.to, s.any = position{e.Txid, e.ID}, true
	if expired {
		s.dropped++
		s.lastID = e.ID
	}
}

// turn is the state of one webhook's turn in a slot.
type turn struct {
	wh           store.Webhook
	attempts     int        // failed attempts on the event after the recorded cursor
	failingSince *time.Time // start of the current run of failures
	skip         skipped
}

// progress starts a progress record that carries the pending skips.
func (w *Worker) progress(t *turn) store.WebhookProgress {
	p := store.WebhookProgress{WebhookID: t.wh.ID, LeaseToken: t.wh.LeaseToken, Attempts: t.attempts}
	if t.skip.any {
		p.Advance, p.CursorTxid, p.CursorEventID = true, t.skip.to.txid, t.skip.to.id
	}
	if t.skip.dropped > 0 {
		p.Dropped = t.skip.dropped
		p.Deliveries = append(p.Deliveries, store.WebhookDelivery{
			EventID: t.skip.lastID, Status: store.DeliveryDropped, DroppedEvents: int(t.skip.dropped), ErrorCode: CodeRetentionExpired,
			Error: fmt.Sprintf("%d event(s) older than the retention window of %s were not delivered", t.skip.dropped, w.cfg.Retention),
		})
	}
	return p
}

// record writes a progress record. It reports false when the turn must
// end: the lease was lost or the store failed.
func (w *Worker) record(t *turn, p store.WebhookProgress) bool {
	// Not the send context: an attempt that completed is recorded even
	// while the worker is being stopped.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.sendCtx), 10*time.Second)
	defer cancel()
	err := w.cfg.Store.RecordWebhookProgress(ctx, p)
	switch {
	case err == nil:
		if n := t.skip.dropped; n > 0 {
			w.m.drop(CodeRetentionExpired, n)
			w.log.Warn("webhook events dropped: older than the retention window", "webhook", t.wh.ID, "tenant", t.wh.TenantID,
				"events", n, "retention", w.cfg.Retention)
		}
		t.skip = skipped{}
		return true
	case errors.Is(err, store.ErrWebhookLeaseLost):
		w.m.lostLease()
		w.log.Info("webhook lease lost; the step is discarded and will be redone", "webhook", t.wh.ID)
	default:
		w.log.Warn("could not record webhook progress; the event will be sent again", "webhook", t.wh.ID, "err", err)
	}
	return false
}

// release ends the turn, writing whatever skips are pending.
func (w *Worker) release(t *turn) {
	p := w.progress(t)
	p.Release = true
	w.record(t, p)
}

// deliver is one webhook's turn: it sends the events after the cursor in
// order until there are none left, one fails, the turn budget is used up
// or the worker stops.
func (w *Worker) deliver(wh store.Webhook) {
	t := &turn{wh: wh, attempts: wh.Attempts, failingSince: wh.FailingSince}
	start := w.cfg.Clock.Now()
	cursor := position{wh.CursorTxid, wh.CursorEventID}
	for {
		qctx, cancel := context.WithTimeout(w.sendCtx, 10*time.Second)
		events, err := w.cfg.Store.PendingWebhookEvents(qctx, wh.TenantID, cursor.txid, cursor.id, w.cfg.BatchSize)
		cancel()
		if err != nil {
			if w.sendCtx.Err() == nil {
				w.log.Warn("could not read pending webhook events", "webhook", wh.ID, "err", err)
			}
			w.release(t)
			return
		}
		if len(events) == 0 {
			w.release(t)
			return
		}
		for _, e := range events {
			now := w.cfg.Clock.Now()
			if w.runCtx.Err() != nil || now.Sub(start) >= w.cfg.TurnBudget {
				w.release(t)
				return
			}
			env, ok := Translate(e)
			if !ok || !Subscribed(wh.EventTypes, env.Type) {
				t.skip.pass(e, false)
				t.attempts, cursor = 0, position{e.Txid, e.ID}
				continue
			}
			if w.cfg.Retention > 0 && now.Sub(e.At) > w.cfg.Retention {
				t.skip.pass(e, true)
				t.attempts, cursor = 0, position{e.Txid, e.ID}
				continue
			}
			advanced, goOn := w.attempt(t, e, env)
			if advanced {
				cursor = position{e.Txid, e.ID}
			}
			if !goOn {
				return
			}
		}
		// The batch is done. Persist a run of skips before reading on, so
		// that a long backlog of expired events is not redone after a crash.
		if t.skip.any {
			if !w.record(t, w.progress(t)) {
				return
			}
		}
	}
}

// outcome classes of an attempt.
type class int

const (
	classOK       class = iota
	classRetry          // transient: retry until the event expires
	classRejected       // the endpoint refused the event itself
	classGone           // 410: the endpoint asks not to be called again
)

func classify(res Result) class {
	switch {
	case res.OK():
		return classOK
	case res.Status == http.StatusGone:
		return classGone
	case res.Status == http.StatusRequestTimeout, res.Status == http.StatusTooEarly, res.Status == http.StatusTooManyRequests:
		return classRetry
	case res.Status >= 400 && res.Status < 500:
		return classRejected
	}
	return classRetry
}

// send signs and sends one event.
func (w *Worker) send(wh store.Webhook, env Envelope, attempt int) (Result, error) {
	body, err := env.Marshal()
	if err != nil {
		return Result{}, err
	}
	now := w.cfg.Clock.Now()
	secrets := []string{wh.Secret}
	if wh.PreviousSecret != "" && wh.PreviousSecretExpiresAt != nil && wh.PreviousSecretExpiresAt.After(now) {
		secrets = append(secrets, wh.PreviousSecret)
	}
	header, err := signatureHeaders(env.ID, now, body, secrets...)
	if err != nil {
		return Result{}, err
	}
	header.Set(HeaderEventType, env.Type)
	header.Set(HeaderAttempt, strconv.Itoa(attempt))
	ctx, cancel := context.WithTimeout(w.sendCtx, w.cfg.RequestTimeout)
	defer cancel()
	res := w.cfg.Sender.Send(ctx, Request{URL: wh.URL, Header: header, Body: body})
	return res, nil
}

// attempt sends one event and records what happened. advanced reports
// whether the cursor moved past the event; goOn whether the turn
// continues with the next one.
func (w *Worker) attempt(t *turn, e store.WebhookEvent, env Envelope) (advanced, goOn bool) {
	n := t.attempts + 1
	res, err := w.send(t.wh, env, n)
	if err != nil {
		// A stored secret that cannot sign: nothing a retry would fix,
		// and nothing the receiver did. Leave the event where it is.
		w.log.Error("could not sign a webhook event; the webhook is skipped until its secret is rotated", "webhook", t.wh.ID, "err", err)
		p := w.progress(t)
		p.Outcome, p.Attempts, p.Error, p.RetryIn, p.Release = store.WebhookFailed, t.attempts, "the signing secret is unusable; rotate it", w.cfg.RetryMax, true
		w.record(t, p)
		return false, false
	}
	if w.sendCtx.Err() != nil {
		// Aborted by Stop: not the endpoint's failure, so not counted.
		w.release(t)
		return false, false
	}
	w.m.attempt(res)
	now := w.cfg.Clock.Now()
	row := store.WebhookDelivery{EventID: e.ID, MessageID: env.ID, EventType: env.Type, Attempt: n,
		ResponseStatus: res.Status, DurationMs: int(res.Duration.Milliseconds()), ErrorCode: res.Code, Error: res.Error}
	p := w.progress(t)
	pos := position{e.Txid, e.ID}

	cls := classify(res)
	if cls == classOK {
		row.Status = store.DeliverySucceeded
		p.Advance, p.CursorTxid, p.CursorEventID = true, pos.txid, pos.id
		p.Outcome, p.Attempts = store.WebhookSucceeded, 0
		p.Deliveries = append(p.Deliveries, row)
		if !w.record(t, p) {
			return false, false
		}
		t.attempts, t.failingSince = 0, nil
		w.log.Debug("webhook delivered", "webhook", t.wh.ID, "event", env.ID, "type", env.Type, "attempt", n, "status", res.Status, "took", res.Duration)
		return true, true
	}

	row.Status = store.DeliveryFailed
	p.Outcome, p.Error = store.WebhookFailed, res.Error
	p.Deliveries = append(p.Deliveries, row)
	if t.failingSince == nil {
		t.failingSince = &now
	}
	w.log.Info("webhook delivery failed", "webhook", t.wh.ID, "tenant", t.wh.TenantID, "event", env.ID, "type", env.Type,
		"attempt", n, "status", res.Status, "code", res.Code, "detail", res.Detail, "took", res.Duration)

	switch {
	case cls == classGone:
		p.Attempts, p.Disable = n, store.WebhookDisabledGone
	case w.cfg.DisableAfter > 0 && now.Sub(*t.failingSince) >= w.cfg.DisableAfter:
		p.Attempts, p.Disable = n, store.WebhookDisabledFailed
	case cls == classRejected && w.cfg.MaxRejections > 0 && n >= w.cfg.MaxRejections:
		// Give up on this event only; the ones behind it get their turn.
		p.Advance, p.CursorTxid, p.CursorEventID = true, pos.txid, pos.id
		p.Attempts = 0
		p.Dropped++
		p.Deliveries = append(p.Deliveries, store.WebhookDelivery{EventID: e.ID, MessageID: env.ID, EventType: env.Type, Attempt: n,
			Status: store.DeliveryDropped, DroppedEvents: 1, ResponseStatus: res.Status, ErrorCode: CodeRejected,
			Error: fmt.Sprintf("the endpoint rejected the event %d times (last status %d); it will not be sent again", n, res.Status)})
		if !w.record(t, p) {
			return false, false
		}
		w.m.drop(CodeRejected, 1)
		w.log.Warn("webhook event dropped: rejected by the endpoint", "webhook", t.wh.ID, "tenant", t.wh.TenantID,
			"event", env.ID, "attempts", n, "status", res.Status)
		t.attempts = 0
		return true, true
	default:
		p.Attempts = n
	}
	if p.Disable != "" {
		p.Release = true
		if w.record(t, p) {
			w.m.disable(p.Disable)
			w.log.Warn("webhook disabled", "webhook", t.wh.ID, "tenant", t.wh.TenantID, "reason", p.Disable,
				"failing_since", t.failingSince.UTC().Format(time.RFC3339))
		}
		return false, false
	}
	delay := backoff(n, w.cfg.RetryBase, w.cfg.RetryMax, w.cfg.Rand())
	if res.RetryAfter > delay {
		// The endpoint asked for more patience; grant it up to the cap.
		delay = min(res.RetryAfter, w.cfg.RetryMax)
	}
	p.RetryIn, p.Release = delay, true
	w.record(t, p)
	return false, false
}
