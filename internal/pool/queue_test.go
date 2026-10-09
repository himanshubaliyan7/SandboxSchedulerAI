package pool

import (
	"testing"
	"time"
)

func granted(w *waiter) bool {
	select {
	case <-w.ch:
		return true
	default:
		return false
	}
}

func TestWaitQueueRoundRobinAcrossTenants(t *testing.T) {
	q := newWaitQueue(100)
	// Tenant a queues five claims before b and c queue one each.
	var a []*waiter
	for i := 0; i < 5; i++ {
		a = append(a, q.push("a"))
	}
	b := q.push("b")
	c := q.push("c")

	// FIFO would serve a,a,a,a,a,b,c. Round-robin serves a,b,c,a,a,a,a.
	want := []*waiter{a[0], b, c, a[1], a[2], a[3], a[4]}
	for i, w := range want {
		if !q.grant() {
			t.Fatalf("grant %d: queue reported empty", i)
		}
		if !granted(w) {
			t.Fatalf("grant %d went to the wrong waiter (tenant %s expected)", i, w.tenant)
		}
		for _, later := range want[i+1:] {
			if granted(later) {
				t.Fatalf("grant %d woke a waiter out of turn (tenant %s)", i, later.tenant)
			}
		}
	}
	if q.grant() || q.len() != 0 {
		t.Fatal("queue should be empty")
	}
}

func TestWaitQueueLateTenantDoesNotJumpTheRing(t *testing.T) {
	q := newWaitQueue(100)
	a1, a2 := q.push("a"), q.push("a")
	b1, b2 := q.push("b"), q.push("b")
	q.grant() // a1; b is next
	c1 := q.push("c")
	order := []*waiter{b1, a2, c1, b2}
	if !granted(a1) {
		t.Fatal("a1 should have been served first")
	}
	for i, w := range order {
		q.grant()
		if !granted(w) {
			t.Fatalf("step %d: wrong waiter served, expected tenant %s", i, w.tenant)
		}
	}
}

func TestWaitQueueBoundedAndRemove(t *testing.T) {
	q := newWaitQueue(3)
	a1, b1, a2 := q.push("a"), q.push("b"), q.push("a")
	if q.push("c") != nil {
		t.Fatal("push beyond the bound must fail")
	}
	if !q.remove(a1) || q.len() != 2 {
		t.Fatal("remove of a queued waiter failed")
	}
	if q.remove(a1) {
		t.Fatal("removing twice must report false")
	}
	if q.push("c") == nil {
		t.Fatal("a freed slot must be usable")
	}
	// a1 is gone: the order is now a2 (a's turn), b1, c.
	q.grant()
	if !granted(a2) || granted(b1) {
		t.Fatal("after removing a1, a2 is first in a's line")
	}
	q.grant()
	if !granted(b1) {
		t.Fatal("b1 should be second")
	}
	if q.remove(b1) {
		t.Fatal("remove of a granted waiter must report false so the grant is passed on")
	}
	if !q.remove(q.lines["c"][0]) || q.len() != 0 || len(q.ring) != 0 {
		t.Fatalf("queue not empty: n=%d ring=%v", q.len(), q.ring)
	}
	if q.grant() {
		t.Fatal("grant on an empty queue")
	}
}

func TestBreakerAllowance(t *testing.T) {
	now := time.Unix(1000, 0)
	b := &breaker{}
	if b.allowance(now, 3, 0) < 1000 {
		t.Fatal("closed breaker must not limit")
	}
	b.failures, b.openUntil = 3, now.Add(time.Second)
	if b.allowance(now, 3, 0) != 0 {
		t.Fatal("open breaker must block")
	}
	later := now.Add(2 * time.Second)
	if b.allowance(later, 3, 0) != 1 {
		t.Fatal("half-open breaker allows one probe")
	}
	if b.allowance(later, 3, 1) != 0 {
		t.Fatal("half-open breaker allows only one probe at a time")
	}
}

func TestHistogramObserve(t *testing.T) {
	c := newCounters()
	h := &c.ClaimWait
	h.observe(500 * time.Microsecond) // <= 1ms
	h.observe(30 * time.Millisecond)  // <= 50ms
	h.observe(time.Minute)            // overflow
	if h.Counts[0] != 1 || h.Counts[3] != 1 || h.Counts[len(h.Buckets)] != 1 || h.Count != 3 {
		t.Fatalf("counts = %v", h.Counts)
	}
}
