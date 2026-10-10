package api

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

// latencyBuckets are the upper bounds, in seconds, of the request
// duration histogram.
var latencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type requestKey struct {
	route string // "METHOD /pattern"
	code  int
}

type histogram struct {
	counts []uint64 // per bucket, not cumulative; last is +Inf
	sum    float64
	count  uint64
}

// metrics holds the API's own counters. Everything is written in the
// Prometheus text exposition format by hand; there is no client library.
type metrics struct {
	mu        sync.Mutex
	requests  map[requestKey]uint64
	latency   map[string]*histogram // by route
	streams   map[string]int64      // active, by kind
	throttled map[rateClass]uint64
}

func newMetrics() *metrics {
	return &metrics{requests: map[requestKey]uint64{}, latency: map[string]*histogram{},
		streams: map[string]int64{"ndjson": 0, "ws": 0, "wait": 0}, throttled: map[rateClass]uint64{}}
}

func (m *metrics) observeRequest(route string, code int, took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{route, code}]++
	h := m.latency[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets)+1)}
		m.latency[route] = h
	}
	sec := took.Seconds()
	i := 0
	for i < len(latencyBuckets) && sec > latencyBuckets[i] {
		i++
	}
	h.counts[i]++
	h.sum += sec
	h.count++
}

func (m *metrics) streamStarted(kind string) { m.mu.Lock(); m.streams[kind]++; m.mu.Unlock() }
func (m *metrics) streamEnded(kind string)   { m.mu.Lock(); m.streams[kind]--; m.mu.Unlock() }
func (m *metrics) rateLimited(c rateClass)   { m.mu.Lock(); m.throttled[c]++; m.mu.Unlock() }

// label escapes a Prometheus label value.
func label(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func fnum(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

type promWriter struct{ w io.Writer }

func (p promWriter) head(name, typ, help string) {
	fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (p promWriter) val(name, labels string, v float64) {
	if labels != "" {
		labels = "{" + labels + "}"
	}
	fmt.Fprintf(p.w, "%s%s %s\n", name, labels, fnum(v))
}

// hist writes one histogram series. counts are per bucket (not
// cumulative) with the overflow bucket last.
func (p promWriter) hist(name, labels string, bounds []float64, counts []uint64, sum float64, count uint64) {
	sep := ""
	if labels != "" {
		sep = ","
	}
	var cum uint64
	for i, b := range bounds {
		cum += counts[i]
		fmt.Fprintf(p.w, "%s_bucket{%s%sle=\"%s\"} %d\n", name, labels, sep, fnum(b), cum)
	}
	fmt.Fprintf(p.w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, labels, sep, count)
	p.val(name+"_sum", labels, sum)
	if labels != "" {
		labels = "{" + labels + "}"
	}
	fmt.Fprintf(p.w, "%s_count%s %d\n", name, labels, count)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := promWriter{w}

	// --- HTTP ---
	s.m.mu.Lock()
	reqKeys := make([]requestKey, 0, len(s.m.requests))
	for k := range s.m.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool {
		if reqKeys[i].route != reqKeys[j].route {
			return reqKeys[i].route < reqKeys[j].route
		}
		return reqKeys[i].code < reqKeys[j].code
	})
	p.head("sbx_http_requests_total", "counter", "HTTP requests by route and status code.")
	for _, k := range reqKeys {
		method, path, _ := strings.Cut(k.route, " ")
		p.val("sbx_http_requests_total", fmt.Sprintf(`method="%s",route="%s",code="%d"`, label(method), label(path), k.code), float64(s.m.requests[k]))
	}
	routes := make([]string, 0, len(s.m.latency))
	for route := range s.m.latency {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	p.head("sbx_http_request_duration_seconds", "histogram", "HTTP request duration by route. Streams count for as long as they are open.")
	for _, route := range routes {
		h := s.m.latency[route]
		method, path, _ := strings.Cut(route, " ")
		p.hist("sbx_http_request_duration_seconds", fmt.Sprintf(`method="%s",route="%s"`, label(method), label(path)),
			latencyBuckets, h.counts, h.sum, h.count)
	}
	p.head("sbx_http_active_streams", "gauge", "Open long-lived responses by kind (ndjson, ws, wait).")
	for _, kind := range []string{"ndjson", "wait", "ws"} {
		p.val("sbx_http_active_streams", `kind="`+kind+`"`, float64(s.m.streams[kind]))
	}
	p.head("sbx_http_rate_limited_total", "counter", "Requests refused with 429 by the per-tenant rate limiter, by bucket.")
	for _, c := range []rateClass{classCreate, classDefault} {
		p.val("sbx_http_rate_limited_total", `class="`+string(c)+`"`, float64(s.m.throttled[c]))
	}
	s.m.mu.Unlock()
	p.head("sbx_api_sessions", "gauge", "Sandboxes the gateway currently holds an agent client for.")
	p.val("sbx_api_sessions", "", float64(len(s.sessions.snapshot())))

	// --- pool ---
	pm := s.pool.Metrics()
	tmplIDs := make([]string, 0, len(pm.Templates))
	for id := range pm.Templates {
		tmplIDs = append(tmplIDs, id)
	}
	sort.Strings(tmplIDs)
	p.head("sbx_pool_sandboxes", "gauge", "Sandboxes by template and state, as of the last refill pass.")
	for _, id := range tmplIDs {
		t := pm.Templates[id]
		for _, sc := range []struct {
			state core.State
			n     int
		}{{core.StateWarm, t.Warm}, {core.StateCreating, t.Creating}, {core.StateAssigned, t.Assigned}, {core.StateRunning, t.Running},
			{core.StatePaused, t.Paused}, {core.StateRecycling, t.Recycling}, {core.StateFailed, t.Failed}} {
			p.val("sbx_pool_sandboxes", fmt.Sprintf(`template="%s",state="%s"`, label(id), publicState(sc.state)), float64(sc.n))
		}
	}
	perTemplate := func(name, typ, help string, f func(id string) float64) {
		p.head(name, typ, help)
		for _, id := range tmplIDs {
			p.val(name, `template="`+label(id)+`"`, f(id))
		}
	}
	perTemplate("sbx_pool_warm_target", "gauge", "Configured warm pool size by template.", func(id string) float64 { return float64(pm.Templates[id].WarmTarget) })
	perTemplate("sbx_pool_inflight_creates", "gauge", "Sandbox creates in flight by template.", func(id string) float64 { return float64(pm.Templates[id].InflightCreates) })
	perTemplate("sbx_pool_claim_waiters", "gauge", "Claims queued on an empty pool by template.", func(id string) float64 { return float64(pm.Templates[id].Waiters) })
	perTemplate("sbx_pool_create_breaker_open", "gauge", "1 while the create circuit breaker of a template is open.", func(id string) float64 {
		if pm.Templates[id].BreakerOpen {
			return 1
		}
		return 0
	})
	p.head("sbx_pool_claims_total", "counter", "Claims by result.")
	for _, c := range []struct {
		result string
		n      uint64
	}{{"ok", pm.Claims}, {"pool_empty", pm.ClaimsPoolEmpty}, {"quota_exceeded", pm.ClaimsQuota}, {"failed", pm.ClaimsFailed}} {
		p.val("sbx_pool_claims_total", `result="`+c.result+`"`, float64(c.n))
	}
	p.head("sbx_pool_claims_waited_total", "counter", "Successful claims that had to queue for a sandbox.")
	p.val("sbx_pool_claims_waited_total", "", float64(pm.ClaimsWaited))
	p.head("sbx_pool_claim_wait_seconds", "histogram", "Time from the start of a successful claim to the sandbox running.")
	p.hist("sbx_pool_claim_wait_seconds", "", pm.ClaimWait.Buckets, pm.ClaimWait.Counts, pm.ClaimWait.Sum, pm.ClaimWait.Count)
	for _, c := range []struct {
		name, help string
		n          uint64
	}{
		{"sbx_pool_releases_total", "Sandboxes released by their tenant.", pm.Releases},
		{"sbx_pool_creates_total", "Sandbox creates that reached WARM.", pm.Creates},
		{"sbx_pool_create_failures_total", "Sandbox creates that failed.", pm.CreateFailures},
		{"sbx_pool_create_retries_total", "Sandbox create attempts that failed and were retried.", pm.CreateRetries},
		{"sbx_pool_destroyed_total", "Sandboxes destroyed.", pm.Destroyed},
		{"sbx_pool_orphans_destroyed_total", "Instances destroyed by the reconciler because no row accounted for them.", pm.OrphansDestroyed},
		{"sbx_pool_instances_lost_total", "Sandboxes failed because their instance vanished or died.", pm.InstancesLost},
		{"sbx_pool_adopted_total", "Warm sandboxes re-adopted after a restart.", pm.Adopted},
		{"sbx_pool_reconcile_failures_total", "Reconcile passes that failed.", pm.ReconcileFailures},
	} {
		p.head(c.name, "counter", c.help)
		p.val(c.name, "", float64(c.n))
	}
	reasons := make([]string, 0, len(pm.Terminations))
	for reason := range pm.Terminations {
		reasons = append(reasons, string(reason))
	}
	sort.Strings(reasons)
	p.head("sbx_pool_terminations_total", "counter", "Sandboxes moved to recycling, by reason.")
	for _, reason := range reasons {
		p.val("sbx_pool_terminations_total", `reason="`+label(reason)+`"`, float64(pm.Terminations[core.TerminationReason(reason)]))
	}
	p.head("sbx_pool_last_refill_timestamp_seconds", "gauge", "Unix time of the last refill pass.")
	p.val("sbx_pool_last_refill_timestamp_seconds", "", unixOrZero(pm.LastRefillAt))
	p.head("sbx_pool_last_reconcile_timestamp_seconds", "gauge", "Unix time of the last reconcile pass.")
	p.val("sbx_pool_last_reconcile_timestamp_seconds", "", unixOrZero(pm.LastReconcileAt))

	if s.cfg.ExtraMetrics != nil {
		s.cfg.ExtraMetrics(w)
	}
}

func unixOrZero(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}
