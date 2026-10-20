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

// nodeDeadAfter is how many consecutive transport failures downgrade a node to
// Alive=false. One is usually a transient stall (a cold-start first call has
// been measured at 62s here), so two in a row is the bar for calling an egress
// dead from live traffic.
const nodeDeadAfter = 2

// ReportDead records that a real request through nodeID failed at the transport
// layer — the egress tunnel itself is broken (a dead upstream, a connection
// reset, a silent stall) even though the node's local socks inbound still
// answers. That is exactly the "能连接但不能用" case: the connect succeeds,
// the request never returns. A probe is a sample and cannot catch it between
// probe rounds, and the per-(exit,model) ban alone only parks the pair for
// seconds and is keyed per model, so the node keeps being chosen for every
// request and each attempt hangs for up to responseHeaderTimeout (90s).
//
// Two consecutive transport failures within failMemory downgrade the node to
// Alive=false. That drops it from Pick/nextNode immediately AND makes it
// eligible for the 2-minute dead-node recheck (ProbeDead only re-tests
// !Alive nodes), so a genuinely dead egress leaves rotation within one or two
// requests and a merely flaky one self-heals on the next probe instead of
// lingering until the next full ProbeAll (~30 min).
func (m *Manager) ReportDead(nodeID string) {
	if nodeID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.nodeFailAt[nodeID]; ok && now.Sub(last) > failMemory {
		m.nodeFails[nodeID] = 0
	}
	m.nodeFails[nodeID]++
	m.nodeFailAt[nodeID] = now
	if m.nodeFails[nodeID] >= nodeDeadAfter {
		if h := m.health[nodeID]; h != nil {
			h.Alive = false
			h.CoolUntil = 0
		}
	}
}

// Revive clears an exit's failure state after a real request for one model
// succeeded through it. A served request is harder evidence than any probe
// sample, so there is no reason to sit out the rest of a cooldown.
//
// The node-level cooldown is transport state — model-independent — so a
// success clears it outright. The (exit, model) ban is not: one model working
// through an exit says nothing about another model that the exit region-gates
// or has a separate quota for, so clearing sibling bans here would let a
// model-B success re-arm a model-A wall. Only the pair that just succeeded is
// lifted.
//
// A served request is also proof the egress is live, so Revive re-arms the
// node's Alive flag and clears the transport-failure counter that ReportDead
// may have accumulated — a node that answers one real request is not dead, even
// if it had timed out twice just before.
func (m *Manager) Revive(nodeID, model string) {
	if nodeID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.health[nodeID]; h != nil {
		h.CoolUntil = 0
		h.LastOKMs = now.UnixMilli()
		h.Alive = true
	}
	delete(m.nodeFails, nodeID)
	delete(m.nodeFailAt, nodeID)
	if model == "" {
		return
	}
	k := banKey(nodeID, model)
	delete(m.bans, k)
	delete(m.fails, k)
	delete(m.failAt, k)
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
