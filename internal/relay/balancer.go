// Package relay forwards requests to user-added upstream providers. This file
// carries the multi-key load balancer for issue #104: a provider may hold
// several API keys, and the gateway spreads requests across them instead of
// forcing the user to register the same endpoint N times. A key that starts
// returning auth/quota/server errors is parked for a cooldown so the next
// picks skip it and a dead key does not keep eating traffic.
package relay

import (
	"sync"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// keyBalancer holds per-provider key state. It is in-memory only and resets on
// restart, which is acceptable: a failed key is re-learned within a handful of
// requests, and a persisted cooldown would just be stale after a relaunch.
type keyBalancer struct {
	mu   sync.Mutex
	rr   map[string]int                  // providerID -> next round-robin index
	cool map[string]map[string]time.Time // providerID -> key -> cooldown expiry
}

var bal = &keyBalancer{
	rr:   make(map[string]int),
	cool: make(map[string]map[string]time.Time),
}

// pickKey chooses a key for one request. It round-robins among keys that are
// not currently cooled; if every key is cooled it returns the least-bad one
// (the earliest expiry) so a request still goes out rather than erroring. The
// returned index is the key's position in `keys`, used only for diagnostics.
func pickKey(providerID string, keys []string) (string, int) {
	if len(keys) == 0 {
		return "", -1
	}
	if len(keys) == 1 {
		return keys[0], 0
	}
	bal.mu.Lock()
	defer bal.mu.Unlock()
	now := time.Now()
	start := bal.rr[providerID] % len(keys)
	idx := start
	for {
		if !isCooledLocked(providerID, keys[idx], now) {
			bal.rr[providerID] = (idx + 1) % len(keys)
			return keys[idx], idx
		}
		idx = (idx + 1) % len(keys)
		if idx == start {
			break // every key is cooled
		}
	}
	// All cooled: send to the key whose cooldown lifts soonest.
	best := start
	var earliest time.Time
	if m := bal.cool[providerID]; m != nil {
		for i, k := range keys {
			until := m[k]
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
				best = i
			}
		}
	}
	return keys[best], best
}

// isCooledLocked reads the cooldown map; callers hold bal.mu.
func isCooledLocked(providerID, key string, now time.Time) bool {
	m := bal.cool[providerID]
	if m == nil {
		return false
	}
	until, ok := m[key]
	return ok && until.After(now)
}

// coolKey parks a key for d so subsequent picks avoid it. A non-positive d is
// a no-op: callers pass a cooldown only when the failure actually warrants one.
func coolKey(providerID, key string, d time.Duration) {
	if key == "" || d <= 0 {
		return
	}
	bal.mu.Lock()
	defer bal.mu.Unlock()
	if bal.cool[providerID] == nil {
		bal.cool[providerID] = make(map[string]time.Time)
	}
	bal.cool[providerID][key] = time.Now().Add(d)
}

// cooldownFor maps a classified upstream error to how long the key that caused
// it should be parked. Credential failures (401/403) mean the key is dead, so
// they get a long cooldown; rate-limit (429) and server (5xx) errors are
// transient, so a short one. Anything else is not the key's fault.
func cooldownFor(code string) time.Duration {
	switch code {
	case lane.CodeCredential:
		return 10 * time.Minute
	case lane.CodeQuota:
		return 10 * time.Second
	case lane.CodeServer:
		return 15 * time.Second
	default:
		return 0
	}
}

// firstKey returns the first effective credential, or "" if the provider has
// none. Used by paths that only need one key and must not load-balance: the
// /models catalog fetch and the capability/availability probes.
func firstKey(p *store.Provider) string {
	keys := p.EffectiveKeys()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}
