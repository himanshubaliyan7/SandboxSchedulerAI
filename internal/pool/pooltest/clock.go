// Package pooltest holds test doubles for the pool controller: a fake
// clock, an in-memory store, a fake agent connection and a fake egress
// proxy. Together with internal/runtime/fake they let the controller, and
// anything built on it, be tested without Postgres or Podman.
package pooltest

import (
	"sync"
	"time"
)

// FakeClock is a manually advanced pool.Clock. Time only moves when
// Advance is called. Unlike a real clock, AfterFunc callbacks run
// synchronously inside Advance, which keeps tests deterministic.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
	f  func()
}

// NewFakeClock starts at a fixed, arbitrary instant.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.ch <- c.now
		return t.ch
	}
	c.timers = append(c.timers, t)
	return t.ch
}

func (c *FakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, x := range c.timers {
			if x == t {
				c.timers = append(c.timers[:i], c.timers[i+1:]...)
				return true
			}
		}
		return false
	}
}

// Advance moves time forward and fires every timer that is now due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due, keep []*fakeTimer
	for _, t := range c.timers {
		if t.at.After(now) {
			keep = append(keep, t)
		} else {
			due = append(due, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	for _, t := range due {
		if t.f != nil {
			t.f()
		} else {
			t.ch <- now
		}
	}
}

// Pending is the number of timers not yet fired or stopped.
func (c *FakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}
