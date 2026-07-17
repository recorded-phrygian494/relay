package reliability

import (
	"sync"
	"time"
)

// Breaker tuning. Values are deliberately conservative: the fallback chain
// is the primary failure response; the breaker only short-circuits
// candidates that are demonstrably down.
const (
	breakerWindow     = 30 * time.Second
	breakerMinSamples = 5
	breakerThreshold  = 0.5 // failure rate to trip
	breakerOpenFor    = 30 * time.Second
)

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Breaker is a circuit breaker for one (provider, model): closed → open
// when the failure rate over a sliding window crosses the threshold;
// after a hold-off, half-open admits exactly one probe (DESIGN §6).
type Breaker struct {
	mu        sync.Mutex
	state     breakerState
	failures  int
	successes int
	windowAt  time.Time
	openedAt  time.Time
	probing   bool
	now       func() time.Time
}

// Allow reports whether a request may proceed. In half-open it admits a
// single probe; concurrent callers are refused until the probe reports.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case stateClosed:
		return true
	case stateOpen:
		if now.Sub(b.openedAt) < breakerOpenFor {
			return false
		}
		b.state = stateHalfOpen
		b.probing = true
		return true
	default: // half-open
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// Record reports an attempt outcome.
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.state == stateHalfOpen {
		b.probing = false
		if success {
			b.reset()
		} else {
			b.state = stateOpen
			b.openedAt = now
		}
		return
	}
	if now.Sub(b.windowAt) > breakerWindow {
		b.failures, b.successes = 0, 0
		b.windowAt = now
	}
	if success {
		b.successes++
		return
	}
	b.failures++
	total := b.failures + b.successes
	if total >= breakerMinSamples && float64(b.failures)/float64(total) >= breakerThreshold {
		b.state = stateOpen
		b.openedAt = now
	}
}

func (b *Breaker) reset() {
	b.state = stateClosed
	b.failures, b.successes = 0, 0
	b.windowAt = b.now()
}

// Open reports whether the breaker is currently refusing traffic (for
// metrics and the dashboard).
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == stateOpen && b.now().Sub(b.openedAt) < breakerOpenFor
}

// BreakerSet lazily allocates one Breaker per (provider, model).
type BreakerSet struct {
	mu  sync.Mutex
	m   map[string]*Breaker
	now func() time.Time
}

// NewBreakerSet builds an empty set.
func NewBreakerSet() *BreakerSet {
	return &BreakerSet{m: map[string]*Breaker{}, now: time.Now}
}

// For returns the breaker for a (provider, model) pair.
func (s *BreakerSet) For(providerName, model string) *Breaker {
	key := providerName + "/" + model
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[key]
	if !ok {
		b = &Breaker{now: s.now, windowAt: s.now()}
		s.m[key] = b
	}
	return b
}

// States snapshots breaker openness for observability.
func (s *BreakerSet) States() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.m))
	for k, b := range s.m {
		out[k] = b.Open()
	}
	return out
}
