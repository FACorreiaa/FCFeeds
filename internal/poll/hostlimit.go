package poll

import (
	"sync"
	"time"
)

// hostLimiter serializes requests per host and enforces a minimum gap between
// them, so one publisher never sees us as a burst.
type hostLimiter struct {
	gap   time.Duration
	mu    sync.Mutex
	slots map[string]*hostSlot
}

type hostSlot struct {
	mu   sync.Mutex
	last time.Time
}

func newHostLimiter(gap time.Duration) *hostLimiter {
	return &hostLimiter{gap: gap, slots: map[string]*hostSlot{}}
}

// acquire blocks until the host is free and the gap has elapsed, then returns
// a release function.
func (l *hostLimiter) acquire(host string) func() {
	l.mu.Lock()
	s, ok := l.slots[host]
	if !ok {
		s = &hostSlot{}
		l.slots[host] = s
	}
	l.mu.Unlock()
	s.mu.Lock()
	if wait := l.gap - time.Since(s.last); wait > 0 {
		time.Sleep(wait)
	}
	return func() {
		s.last = time.Now()
		s.mu.Unlock()
	}
}
