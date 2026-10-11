package webhook

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/promtext"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// attemptBuckets are the upper bounds, in seconds, of the attempt
// duration histogram.
var attemptBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Metrics counts what the worker and the test route do. One instance is
// shared by both; Write renders it for GET /metrics.
type Metrics struct {
	mu        sync.Mutex
	attempts  map[string]uint64 // by outcome: succeeded, failed
	failures  map[string]uint64 // failed attempts by error code
	dropped   map[string]uint64 // events given up on, by reason
	disabled  map[string]uint64 // webhooks auto-disabled, by reason
	tests     map[string]uint64 // test events by outcome
	leaseLost uint64
	inFlight  int
	buckets   []uint64
	sum       float64
	count     uint64
	stats     store.WebhookStats
	statsAt   time.Time
	lastPoll  time.Time
}

// NewMetrics returns an empty set of counters.
func NewMetrics() *Metrics {
	return &Metrics{
		attempts: map[string]uint64{store.DeliverySucceeded: 0, store.DeliveryFailed: 0},
		failures: map[string]uint64{}, dropped: map[string]uint64{CodeRetentionExpired: 0, CodeRejected: 0},
		disabled: map[string]uint64{store.WebhookDisabledFailed: 0, store.WebhookDisabledGone: 0},
		tests:    map[string]uint64{store.DeliverySucceeded: 0, store.DeliveryFailed: 0},
		buckets:  make([]uint64, len(attemptBuckets)+1),
	}
}

func (m *Metrics) attempt(res Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if res.OK() {
		m.attempts[store.DeliverySucceeded]++
	} else {
		m.attempts[store.DeliveryFailed]++
		m.failures[res.Code]++
	}
	sec := res.Duration.Seconds()
	i := 0
	for i < len(attemptBuckets) && sec > attemptBuckets[i] {
		i++
	}
	m.buckets[i]++
	m.sum += sec
	m.count++
}

func (m *Metrics) test(res Result) {
	m.mu.Lock()
	if res.OK() {
		m.tests[store.DeliverySucceeded]++
	} else {
		m.tests[store.DeliveryFailed]++
	}
	m.mu.Unlock()
}

func (m *Metrics) drop(reason string, n int64) {
	m.mu.Lock()
	m.dropped[reason] += uint64(n)
	m.mu.Unlock()
}

func (m *Metrics) disable(reason string) { m.mu.Lock(); m.disabled[reason]++; m.mu.Unlock() }
func (m *Metrics) lostLease()            { m.mu.Lock(); m.leaseLost++; m.mu.Unlock() }
func (m *Metrics) flight(d int)          { m.mu.Lock(); m.inFlight += d; m.mu.Unlock() }
func (m *Metrics) polled(at time.Time)   { m.mu.Lock(); m.lastPoll = at; m.mu.Unlock() }

func (m *Metrics) setStats(st store.WebhookStats, at time.Time) {
	m.mu.Lock()
	m.stats, m.statsAt = st, at
	m.mu.Unlock()
}

// Snapshot is a copy of the counters, for tests.
type Snapshot struct {
	Succeeded, Failed uint64
	Failures          map[string]uint64
	Dropped           map[string]uint64
	Disabled          map[string]uint64
	LeaseLost         uint64
	InFlight          int
	Stats             store.WebhookStats
}

// Snapshot copies the counters.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := func(in map[string]uint64) map[string]uint64 {
		out := make(map[string]uint64, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	return Snapshot{Succeeded: m.attempts[store.DeliverySucceeded], Failed: m.attempts[store.DeliveryFailed],
		Failures: cp(m.failures), Dropped: cp(m.dropped), Disabled: cp(m.disabled), LeaseLost: m.leaseLost,
		InFlight: m.inFlight, Stats: m.stats}
}

func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Write renders the metrics in the Prometheus text format.
func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	head := func(name, typ, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	byLabel := func(name, typ, help, label string, vals map[string]uint64) {
		head(name, typ, help)
		for _, k := range sortedKeys(vals) {
			fmt.Fprintf(w, "%s{%s=\"%s\"} %d\n", name, label, promtext.Label(k), vals[k])
		}
	}
	byLabel("sbx_webhook_deliveries_total", "counter", "Webhook delivery attempts by outcome (succeeded, failed).", "outcome", m.attempts)
	byLabel("sbx_webhook_delivery_failures_total", "counter", "Failed webhook delivery attempts by error code.", "code", m.failures)
	byLabel("sbx_webhook_dropped_events_total", "counter",
		"Events not delivered to a webhook: older than the retention window, or rejected by the endpoint too often.", "reason", m.dropped)
	byLabel("sbx_webhook_auto_disabled_total", "counter", "Webhooks disabled by the server, by reason.", "reason", m.disabled)
	byLabel("sbx_webhook_test_events_total", "counter", "Test events sent through POST /v1/webhooks/{id}/test, by outcome.", "outcome", m.tests)
	head("sbx_webhook_lease_lost_total", "counter", "Delivery steps discarded because another worker or a configuration change took the webhook over.")
	fmt.Fprintf(w, "sbx_webhook_lease_lost_total %d\n", m.leaseLost)

	head("sbx_webhook_delivery_duration_seconds", "histogram", "Duration of webhook delivery attempts.")
	var cum uint64
	for i, b := range attemptBuckets {
		cum += m.buckets[i]
		fmt.Fprintf(w, "sbx_webhook_delivery_duration_seconds_bucket{le=\"%s\"} %d\n", strconv.FormatFloat(b, 'g', -1, 64), cum)
	}
	fmt.Fprintf(w, "sbx_webhook_delivery_duration_seconds_bucket{le=\"+Inf\"} %d\n", m.count)
	fmt.Fprintf(w, "sbx_webhook_delivery_duration_seconds_sum %s\n", strconv.FormatFloat(m.sum, 'g', -1, 64))
	fmt.Fprintf(w, "sbx_webhook_delivery_duration_seconds_count %d\n", m.count)

	head("sbx_webhook_in_flight", "gauge", "Webhooks this process is delivering to right now.")
	fmt.Fprintf(w, "sbx_webhook_in_flight %d\n", m.inFlight)
	head("sbx_webhooks", "gauge", "Webhooks in the database by state (enabled, disabled, failing: enabled and in a run of failures), as of the last refresh.")
	fmt.Fprintf(w, "sbx_webhooks{state=\"enabled\"} %d\nsbx_webhooks{state=\"disabled\"} %d\nsbx_webhooks{state=\"failing\"} %d\n",
		m.stats.Enabled, m.stats.Disabled, m.stats.Failing)
	head("sbx_webhook_backlog_events", "gauge", "Events after the cursors of the enabled webhooks (each webhook counted up to 10000), as of the last refresh.")
	fmt.Fprintf(w, "sbx_webhook_backlog_events %d\n", m.stats.Backlog)
	head("sbx_webhook_lag_seconds", "gauge", "Age of the oldest event an enabled webhook has not dealt with, as of the last refresh.")
	fmt.Fprintf(w, "sbx_webhook_lag_seconds %s\n", strconv.FormatFloat(m.stats.Lag.Seconds(), 'g', -1, 64))
	head("sbx_webhook_stats_timestamp_seconds", "gauge", "Unix time of the last refresh of the backlog and lag gauges.")
	fmt.Fprintf(w, "sbx_webhook_stats_timestamp_seconds %s\n", unixOrZero(m.statsAt))
	head("sbx_webhook_last_poll_timestamp_seconds", "gauge", "Unix time of the worker's last poll for due webhooks.")
	fmt.Fprintf(w, "sbx_webhook_last_poll_timestamp_seconds %s\n", unixOrZero(m.lastPoll))
}

func unixOrZero(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}
