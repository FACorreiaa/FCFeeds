// Package poll schedules and performs feed fetches: which feed is due, how
// long to wait after success or failure, and the worker loop that does it.
package poll

import "time"

// Config holds the scheduling knobs.
type Config struct {
	// MinInterval is the floor for any poll interval; publishers asking for
	// less are ignored. Default 5m.
	MinInterval time.Duration
	// DefaultInterval applies when the publisher gives no ttl. Default 15m.
	DefaultInterval time.Duration
	// MaxInterval caps adaptive growth. Default 2h.
	MaxInterval time.Duration
	// QuietPollsPerDouble is how many consecutive polls with no new items
	// double the interval. Default 6.
	QuietPollsPerDouble int
	// MaxBackoff caps error backoff. Default 6h.
	MaxBackoff time.Duration
	// GoneAfterFailures is how many consecutive 404/410s pin a feed to a
	// daily poll. Default 5.
	GoneAfterFailures int
	// Workers is the fetch concurrency. Default 4.
	Workers int
	// Tick is how often the scheduler looks for due feeds. Default 5s.
	Tick time.Duration
	// ClaimBatch is how many due feeds one tick claims. Default 32.
	ClaimBatch int
	// LockFor is how long a claim holds. Default 60s.
	LockFor time.Duration
	// HostGap is the minimum spacing between requests to one host. Default 2s.
	HostGap time.Duration
	// KeepItems is the per-feed retention count. Default 300.
	KeepItems int
	// KeepFor is the per-feed retention age. Default 14d.
	KeepFor time.Duration
	// EvictAfter deletes feeds nobody requested for this long. Default 14d.
	EvictAfter time.Duration
	// EvictEvery is how often eviction runs. Default 24h.
	EvictEvery time.Duration
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		MinInterval:         5 * time.Minute,
		DefaultInterval:     15 * time.Minute,
		MaxInterval:         2 * time.Hour,
		QuietPollsPerDouble: 6,
		MaxBackoff:          6 * time.Hour,
		GoneAfterFailures:   5,
		Workers:             4,
		Tick:                5 * time.Second,
		ClaimBatch:          32,
		LockFor:             60 * time.Second,
		HostGap:             2 * time.Second,
		KeepItems:           300,
		KeepFor:             14 * 24 * time.Hour,
		EvictAfter:          14 * 24 * time.Hour,
		EvictEvery:          24 * time.Hour,
	}
}

// NextInterval is the poll interval after a successful fetch: the
// publisher's ttl floored at MinInterval, doubled once per
// QuietPollsPerDouble consecutive quiet polls, capped at MaxInterval.
func NextInterval(publisherTTL time.Duration, quietPolls int, c Config) time.Duration {
	base := publisherTTL
	if base <= 0 {
		base = c.DefaultInterval
	}
	if base < c.MinInterval {
		base = c.MinInterval
	}
	if c.QuietPollsPerDouble > 0 {
		for n := quietPolls / c.QuietPollsPerDouble; n > 0 && base < c.MaxInterval; n-- {
			base *= 2
		}
	}
	if base > c.MaxInterval {
		base = c.MaxInterval
	}
	return base
}

// Backoff is the wait after a failed fetch: exponential in failCount from the
// feed's base interval with ±10% jitter (rnd in [0,1)), capped at MaxBackoff.
// A larger Retry-After wins. Repeated 404/410 pins the feed to a daily poll.
func Backoff(base time.Duration, failCount int, retryAfter time.Duration, status int, c Config, rnd func() float64) time.Duration {
	if (status == 404 || status == 410) && failCount >= c.GoneAfterFailures {
		return 24 * time.Hour
	}
	if base <= 0 {
		base = c.DefaultInterval
	}
	d := base
	for i := 0; i < failCount && d < c.MaxBackoff; i++ {
		d *= 2
	}
	if d > c.MaxBackoff {
		d = c.MaxBackoff
	}
	if retryAfter > d {
		d = retryAfter
	}
	// jitter: d * (0.9 + 0.2*rnd)
	j := 0.9 + 0.2*rnd()
	return time.Duration(float64(d) * j)
}
