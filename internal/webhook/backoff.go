package webhook

import "time"

// Retry policy defaults.
const (
	DefaultRetryBase = 5 * time.Second
	DefaultRetryMax  = 15 * time.Minute
	// retryJitter spreads each delay over [1-j, 1+j] times its nominal
	// value, so that the webhooks of an endpoint that comes back do not
	// all fire in the same second.
	retryJitter = 0.2
)

// backoff returns how long to wait after the n-th consecutive failed
// attempt (n >= 1): base doubled n-1 times, capped at max, then jittered
// by rnd in [0, 1) and capped again.
func backoff(n int, base, max time.Duration, rnd float64) time.Duration {
	if n < 1 {
		n = 1
	}
	d := base
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	if d > max || d <= 0 {
		d = max
	}
	d = time.Duration(float64(d) * (1 - retryJitter + 2*retryJitter*rnd))
	if d > max {
		d = max
	}
	return d
}
