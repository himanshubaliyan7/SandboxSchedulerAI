package pool

import (
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

// claimWaitBuckets are the upper bounds, in seconds, of the claim-wait
// histogram. The last bucket is unbounded.
var claimWaitBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Histogram is a fixed-bucket histogram. Counts[i] counts observations
// <= Buckets[i] (not cumulative); Counts[len(Buckets)] is the overflow.
type Histogram struct {
	Buckets []float64 // upper bounds in seconds
	Counts  []uint64  // len(Buckets)+1
	Sum     float64   // seconds
	Count   uint64
}

func (h *Histogram) observe(d time.Duration) {
	s := d.Seconds()
	i := 0
	for i < len(h.Buckets) && s > h.Buckets[i] {
		i++
	}
	h.Counts[i]++
	h.Sum += s
	h.Count++
}

// TemplateMetrics is the per-template part of a snapshot. The state
// counts are those read from the store by the latest refill pass.
type TemplateMetrics struct {
	WarmTarget int
	Warm       int
	Creating   int
	Assigned   int
	Running    int
	Paused     int
	Recycling  int
	Failed     int

	InflightCreates           int // creates running in this process
	Waiters                   int // claims queued on an empty pool
	ConsecutiveCreateFailures int
	BreakerOpen               bool
	BreakerOpenUntil          time.Time
}

// Metrics is a point-in-time snapshot of the controller.
type Metrics struct {
	Templates map[string]TemplateMetrics

	Claims            uint64 // successful claims
	ClaimsWaited      uint64 // successful claims that had to queue
	ClaimsPoolEmpty   uint64 // claims that ended in ErrPoolEmpty
	ClaimsQuota       uint64 // claims that ended in ErrQuotaExceeded
	ClaimsFailed      uint64 // every other claim error, including failures after assignment
	ClaimWait         Histogram
	Releases          uint64
	Creates           uint64 // creates that reached WARM
	CreateFailures    uint64 // creates that ended FAILED, after every attempt
	CreateRetries     uint64 // create attempts that failed and were tried again
	Destroyed         uint64
	OrphansDestroyed  uint64 // instances destroyed by the reconciler
	InstancesLost     uint64 // rows failed because their instance vanished or died
	Adopted           uint64 // WARM instances re-adopted from an older agent epoch
	Terminations      map[core.TerminationReason]uint64
	LastRefillAt      time.Time
	LastReconcileAt   time.Time
	LastSweepAt       time.Time
	RefillPasses      uint64
	ReconcileFailures uint64
}

type counters struct {
	Metrics
	terminations map[core.TerminationReason]uint64
}

func newCounters() counters {
	return counters{
		Metrics: Metrics{ClaimWait: Histogram{
			Buckets: claimWaitBuckets, Counts: make([]uint64, len(claimWaitBuckets)+1),
		}},
		terminations: map[core.TerminationReason]uint64{},
	}
}

// Metrics returns a snapshot. It does not touch the store.
func (c *Controller) Metrics() Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.m.Metrics
	out.ClaimWait.Counts = append([]uint64(nil), c.m.ClaimWait.Counts...)
	out.ClaimWait.Buckets = append([]float64(nil), c.m.ClaimWait.Buckets...)
	out.Terminations = make(map[core.TerminationReason]uint64, len(c.m.terminations))
	for k, v := range c.m.terminations {
		out.Terminations[k] = v
	}
	now := c.clock.Now()
	out.Templates = make(map[string]TemplateMetrics, len(c.templates))
	for _, t := range c.templates {
		cs := c.counts[t.ID]
		tm := TemplateMetrics{
			WarmTarget: t.WarmTarget,
			Warm:       cs[core.StateWarm], Creating: cs[core.StateCreating], Assigned: cs[core.StateAssigned],
			Running: cs[core.StateRunning], Paused: cs[core.StatePaused], Recycling: cs[core.StateRecycling],
			Failed: cs[core.StateFailed],
		}
		for _, op := range c.creating {
			if op.templateID == t.ID {
				tm.InflightCreates++
			}
		}
		if q := c.queues[t.ID]; q != nil {
			tm.Waiters = q.len()
		}
		if b := c.breakers[t.ID]; b != nil {
			tm.ConsecutiveCreateFailures = b.failures
			tm.BreakerOpen = b.failures >= c.cfg.BreakerThreshold && now.Before(b.openUntil)
			tm.BreakerOpenUntil = b.openUntil
		}
		out.Templates[t.ID] = tm
	}
	return out
}
