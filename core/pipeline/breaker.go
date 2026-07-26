package pipeline

import (
	"sync"
	"time"
)

// Breaker states reported to metrics.
const (
	BreakerClosed   = 0
	BreakerHalfOpen = 1
	BreakerOpen     = 2
)

// Breaker is a minimal circuit breaker: it opens after `threshold`
// consecutive failures, stays open for `cooldown`, then admits a single
// half-open probe whose outcome closes or reopens the circuit.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu            sync.Mutex
	failures      int
	state         int
	openedAt      time.Time
	probeInFlight bool
}

// NewBreaker builds a breaker; zero threshold/cooldown get sane defaults.
func NewBreaker(threshold int, cooldown time.Duration, now func() time.Time) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 15 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: now}
}

// Allow reports whether a request may proceed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if b.now().Sub(b.openedAt) >= b.cooldown {
			b.state = BreakerHalfOpen
			b.probeInFlight = true
			return true
		}
		return false
	default: // half-open: only one probe at a time
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	}
}

// Success records a successful upstream exchange.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = BreakerClosed
	b.probeInFlight = false
}

// Failure records a failed upstream exchange.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerHalfOpen {
		b.state = BreakerOpen
		b.openedAt = b.now()
		b.probeInFlight = false
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.state = BreakerOpen
		b.openedAt = b.now()
	}
}

// State returns the current state for metrics.
func (b *Breaker) State() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
