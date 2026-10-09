package pool

// waitQueue holds the claims waiting for one template's pool to refill.
// It is fair across tenants: each tenant has its own FIFO line and grants
// go round-robin over the tenants that have someone waiting, so one tenant
// queueing a hundred claims cannot starve another tenant's single claim.
// It is bounded: push fails once max waiters are queued.
//
// A grant is a wake-up, not an assignment. The woken claimer still has to
// win the atomic store claim; if it does not use the grant it passes it on.
//
// waitQueue is not safe for concurrent use; the controller's mutex guards it.
type waitQueue struct {
	max   int
	n     int
	lines map[string][]*waiter // tenant -> FIFO
	ring  []string             // tenants with at least one waiter, in arrival order
	next  int                  // index in ring of the tenant served next
}

type waiter struct {
	tenant  string
	ch      chan struct{} // closed on grant
	granted bool
}

func newWaitQueue(max int) *waitQueue {
	return &waitQueue{max: max, lines: map[string][]*waiter{}}
}

func (q *waitQueue) len() int { return q.n }

// push queues a waiter for a tenant, or returns nil when the queue is full.
func (q *waitQueue) push(tenant string) *waiter {
	if q.n >= q.max {
		return nil
	}
	w := &waiter{tenant: tenant, ch: make(chan struct{})}
	if len(q.lines[tenant]) == 0 {
		// A tenant joining the ring goes to the position served last, so
		// it cannot jump ahead of tenants already waiting for their turn.
		q.ring = append(q.ring, "")
		copy(q.ring[q.next+1:], q.ring[q.next:])
		q.ring[q.next] = tenant
		q.next++
		if q.next >= len(q.ring) {
			q.next = 0
		}
	}
	q.lines[tenant] = append(q.lines[tenant], w)
	q.n++
	return w
}

// grant wakes the next waiter in round-robin order. It reports false when
// nobody is waiting.
func (q *waitQueue) grant() bool {
	if q.n == 0 {
		return false
	}
	if q.next >= len(q.ring) {
		q.next = 0
	}
	tenant := q.ring[q.next]
	line := q.lines[tenant]
	w := line[0]
	q.setLine(tenant, line[1:], q.next, true)
	q.n--
	w.granted = true
	close(w.ch)
	return true
}

// remove takes a waiter out of the queue (timeout or cancellation). It
// reports false when the waiter was already granted, in which case the
// caller holds a grant it must pass on.
func (q *waitQueue) remove(w *waiter) bool {
	if w.granted {
		return false
	}
	line := q.lines[w.tenant]
	for i, x := range line {
		if x != w {
			continue
		}
		rest := append(append([]*waiter{}, line[:i]...), line[i+1:]...)
		q.setLine(w.tenant, rest, q.ringIndex(w.tenant), false)
		q.n--
		return true
	}
	return false
}

func (q *waitQueue) ringIndex(tenant string) int {
	for i, t := range q.ring {
		if t == tenant {
			return i
		}
	}
	return -1
}

// setLine stores a tenant's remaining line. served says the tenant at ring
// index idx was just granted, so the turn moves on.
func (q *waitQueue) setLine(tenant string, line []*waiter, idx int, served bool) {
	if len(line) > 0 {
		q.lines[tenant] = line
		if served {
			q.next = idx + 1
		}
	} else {
		delete(q.lines, tenant)
		q.ring = append(q.ring[:idx], q.ring[idx+1:]...)
		// Removing an entry before the cursor shifts the cursor's target
		// left; removing the entry at the cursor makes the cursor point at
		// the following tenant already.
		if idx < q.next {
			q.next--
		}
	}
	if q.next >= len(q.ring) {
		q.next = 0
	}
}
