// Package reliability implements the executor that walks a router's
// candidate chain: retries with backoff and jitter, per-key cooldowns,
// circuit breakers, timeout budgets, and pre-first-content streaming
// failover (DESIGN §6). Provider adapters stay dumb; this package owns
// every give-up/try-again decision.
package reliability

import (
	"sync"
	"time"
)

// DefaultCooldown is applied to a rate-limited key when the provider sends
// no Retry-After.
const DefaultCooldown = 30 * time.Second

// KeyPool rotates a provider's API keys round-robin, skipping keys in
// cooldown. A nil *KeyPool means the provider has no keys to manage.
type KeyPool struct {
	mu   sync.Mutex
	keys []string
	next int
	cool map[string]time.Time
	now  func() time.Time
}

// NewKeyPool builds a pool; returns nil when there is at most one key,
// since rotation is meaningless (a single key still gets cooldown handling
// through the executor's retry backoff).
func NewKeyPool(keys []string) *KeyPool {
	if len(keys) < 2 {
		return nil
	}
	return &KeyPool{keys: keys, cool: map[string]time.Time{}, now: time.Now}
}

// Next returns the next usable key. ok=false means every key is cooling —
// the candidate is down (DESIGN §6).
func (p *KeyPool) Next() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for i := 0; i < len(p.keys); i++ {
		k := p.keys[p.next]
		p.next = (p.next + 1) % len(p.keys)
		if until, cooling := p.cool[k]; !cooling || now.After(until) {
			delete(p.cool, k)
			return k, true
		}
	}
	return "", false
}

// Cooldown removes a key from rotation for d (a 429, per DESIGN §6).
func (p *KeyPool) Cooldown(key string, d time.Duration) {
	if d <= 0 {
		d = DefaultCooldown
	}
	p.mu.Lock()
	p.cool[key] = p.now().Add(d)
	p.mu.Unlock()
}
