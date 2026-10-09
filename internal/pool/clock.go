package pool

import "time"

// Clock is the controller's time source. Every interval, timeout and
// backoff in this package goes through it, so tests can drive time with
// pooltest.FakeClock instead of sleeping.
//
// The store keeps its own clock (Postgres's now()) for the timestamps in
// the sandboxes table; the two only have to agree roughly, because row
// deadlines are written and compared by the store alone.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once, d from now.
	After(d time.Duration) <-chan time.Time
	// AfterFunc runs f in its own goroutine d from now, unless the
	// returned stop function is called first. stop reports whether it
	// prevented the call.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (RealClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}
