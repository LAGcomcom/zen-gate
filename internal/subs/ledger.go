package subs

import (
	"time"
)

// The (exit, model) ledger.
//
// Rotation alone is not enough: an exit can be perfectly healthy and still
// refuse one particular model (region gating, a per-model quota, a family the
// node's provider does not carry). Without a ledger every request re-discovers
// that, one failure at a time, and the client sees the retry storm as a stall.
//
// Two rules, both learned the hard way on a real pool:
//
//  1. **The ledger only ever skips a pair known to be bad.** It is never used
//     to *prefer* a pair known to be good: an implementation that did pinned
//     every concurrent request onto the first success, which then burned that
//     exit's allowance and left the rest of the pool idle (concurrency
//     collapsed to a single exit within one window).
//  2. **The first delay is short.** A node's failure is usually transient —
//     jitter, a local speed bump, port contention — so the transport class
//     parks the pair for seconds. A fixed minute-long park empties the pool
//     while the nodes are in fact fine.
//
// Real traffic is the strongest evidence there is, so a served request clears
// the pair immediately rather than waiting the delay out.

// banBase* are the first delays; *Ceil caps the growth; the count doubles the
// delay per consecutive failure.
const (
	banBaseOther   = 3 * time.Second
	banBaseLimited = 30 * time.Second
	banRegion      = 15 * time.Minute
	banCeilOther   = time.Minute
	banCeilLimited = 10 * time.Minute
	// failMemory is how long a failure keeps counting toward the growth; past
	// it the count restarts, so a node that misbehaves once an hour is never
	// treated as persistently broken.
	failMemory = 10 * time.Minute
)

// banKey names one (exit, model) pair in the ledger.
func banKey(nodeID, model string) string { return nodeID + "\x00" + model }

// Report books one failure against an (exit, model) pair.
func (m *Manager) Report(nodeID, model, class string) {
	if nodeID == "" || model == "" {
		return
	}
	k := banKey(nodeID, model)
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.failAt[k]; ok && now.Sub(last) > failMemory {
		m.fails[k] = 0
	}
	m.fails[k]++
	m.failAt[k] = now
	delete(m.good, k)
	n := m.fails[k]

	var d time.Duration
	switch class {
	case "region":
		// A deterministic verdict: no growth, and no point probing around it
		// again soon.
		d = banRegion
	case "limited":
		d = banBaseLimited << min(n-1, 5)
		if d > banCeilLimited {
			d = banCeilLimited
		}
	default:
		d = banBaseOther << min(n-1, 5)
		if d > banCeilOther {
			d = banCeilOther
		}
	}
	m.bans[k] = now.Add(d)
}

// Revive clears an exit's failure state after a real request succeeded through
// it. A served request is harder evidence than any probe sample, so there is no
// reason to sit out the rest of a cooldown.
func (m *Manager) Revive(nodeID string) {
	if nodeID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.health[nodeID]; h != nil {
		h.CoolUntil = 0
		h.LastOKMs = now.UnixMilli()
	}
	prefix := nodeID + "\x00"
	for k := range m.bans {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(m.bans, k)
			delete(m.fails, k)
			delete(m.failAt, k)
			m.good[k] = now
		}
	}
}

// bannedFor reports whether this pair is inside its ban window, clearing the
// entry once it has expired. The caller must hold m.mu.
func (m *Manager) bannedFor(nodeID, model string, now time.Time) bool {
	if model == "" {
		return false
	}
	k := banKey(nodeID, model)
	until, ok := m.bans[k]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(m.bans, k)
	return false
}
