package relay

import (
	"net/http"
	"sync"
	"time"

	"zen-gate/internal/store"
)

// Per-provider API-key rotation (issue #104): one upstream account is a rate
// limit, so a provider configured with several keys spreads requests over
// them. Rotation is round-robin with per-key cooldowns — a key that just
// earned a 429 or a credential rejection steps out for a bit instead of
// absorbing the next request too. Balancers live in memory only: a restart
// forgets cooldowns, which is the right bias (a fresh process should trust
// every key again).

const (
	// keyCoolCredential parks a key that the upstream rejected as a credential
	// (401/403): retrying it sooner only burns the rotation's other slots.
	keyCoolCredential = 10 * time.Minute
	// keyCoolRate is the default park for a 429 without a usable Retry-After.
	keyCoolRate = 10 * time.Second
	// keyCoolServer parks a key after a 5xx: the key is probably fine, but
	// letting the others absorb the next few requests is free insurance.
	keyCoolServer = 15 * time.Second
)

// keyBalancer is one provider's rotation state.
type keyBalancer struct {
	mu   sync.Mutex
	rr   uint64
	cool []time.Time
}

// pick returns the next key index to present. Indices inside their cooldown
// are skipped; when every key is cooling it returns whichever unfreezes
// first — queueing behind the best key beats spinning on a dead one.
func (b *keyBalancer) pick(n int) int {
	if n <= 1 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cool) != n {
		b.cool = make([]time.Time, n)
	}
	now := time.Now()
	start := int(b.rr % uint64(n))
	fallback, fallbackAt := -1, time.Time{}
	for off := 0; off < n; off++ {
		i := (start + off) % n
		if now.Before(b.cool[i]) {
			if fallback < 0 || b.cool[i].Before(fallbackAt) {
				fallback, fallbackAt = i, b.cool[i]
			}
			continue
		}
		b.rr = uint64(i) + 1
		return i
	}
	return fallback
}

// penalize parks one key index for d. A shorter new cooldown never shortens
// an existing one.
func (b *keyBalancer) penalize(i, n int, d time.Duration) {
	if n <= 1 || d <= 0 || i < 0 || i >= n {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cool) != n {
		b.cool = make([]time.Time, n)
	}
	if t := time.Now().Add(d); t.After(b.cool[i]) {
		b.cool[i] = t
	}
}

var (
	balMu     sync.Mutex
	balancers = map[string]*keyBalancer{}
)

func balancerFor(providerID string, n int) *keyBalancer {
	balMu.Lock()
	defer balMu.Unlock()
	b := balancers[providerID]
	if b == nil {
		b = &keyBalancer{}
		balancers[providerID] = b
	}
	return b
}

// keyFor picks the key one request should present: round-robin over the
// provider's effective keys. Single-key providers take the fast path and
// never touch balancer state.
func keyFor(p *store.Provider) string {
	keys := p.EffectiveKeys()
	if len(keys) == 0 {
		return ""
	}
	if len(keys) == 1 {
		return keys[0]
	}
	return keys[balancerFor(p.ID, len(keys)).pick(len(keys))]
}

// FirstKey is the stable key for non-traffic calls (listing fetches, timing
// probes): keeping them on one key stops probe traffic from churning the
// rotation's cooldown state.
func FirstKey(p *store.Provider) string {
	keys := p.EffectiveKeys()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// penalizeKey parks the key a failed request presented, for a duration that
// matches why it failed. model-level failures (4xx bodies on a 200, region
// walls) say nothing about the key and park nothing.
func penalizeKey(p *store.Provider, key string, status int, retryAfter time.Duration) {
	keys := p.EffectiveKeys()
	if len(keys) <= 1 {
		return
	}
	var d time.Duration
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		d = keyCoolCredential
	case status == http.StatusTooManyRequests:
		if retryAfter > 0 {
			d = retryAfter
		} else {
			d = keyCoolRate
		}
	case status >= 500:
		d = keyCoolServer
	default:
		return
	}
	for i, k := range keys {
		if k == key {
			balancerFor(p.ID, len(keys)).penalize(i, len(keys), d)
			return
		}
	}
}
