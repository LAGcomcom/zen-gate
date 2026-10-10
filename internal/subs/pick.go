package subs

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/proxy"
)

// Before this file the pool was walked node-by-node on an atomic counter. Two
// things make that the wrong unit and the wrong rhythm:
//
//   - **Quota is accounted per egress IP, not per node.** An airport routinely
//     sells several nodes that leave through the same IP (measured: 55 live
//     nodes, 21 distinct IPs, one IP carrying four of them). Walking nodes
//     spends that shared IP four times as fast while other IPs sit idle, so the
//     pool's total allowance is never actually used.
//   - **A fresh exit every request throws away the upstream's prompt cache.**
//     Measured over three days: 720M input tokens against 4.8M output, and zero
//     cache reads on any of it. Every turn re-prefilled the whole conversation.
//
// So Pick rotates over **distinct egress IPs** and prefers the exit a
// conversation already used.

// stickyConversations bounds the conversation→exit table; the least recently
// used entry is evicted past this.
const stickyConversations = 512

// Pick chooses an exit for one model.
//
// The unit is the **egress IP**: each distinct IP gets an equal share of the
// requests, and inside one IP the node with the best measured latency wins —
// that part is free, because nodes behind one IP spend the same allowance.
func (m *Manager) Pick(seed, model string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.nodes) == 0 {
		return ""
	}
	now := time.Now()
	usable := func(node Node) bool {
		h := m.health[node.ID]
		if h == nil || !h.Alive {
			return false
		}
		return h.CoolUntil == 0 || now.UnixMilli() >= h.CoolUntil
	}

	// Sticky: a conversation prefers the exit it already used. This is a
	// preference, never a lock — an exit that is dead or cooling is dropped
	// here and the rotation below picks a new one, so an exhausted exit cannot
	// strand a conversation.
	if seed != "" {
		if id, ok := m.sticky[seed]; ok {
			if node, found := m.nodeByID(id); found && usable(node) {
				m.stickyAt[seed] = now.UnixMilli()
				return id
			}
			delete(m.sticky, seed)
			delete(m.stickyAt, seed)
		}
	}

	// Group by egress IP. Nodes whose IP was never probed stay separate (keyed
	// by node id) so "unknown" is not mistaken for "same exit".
	type group struct {
		best Node
		late int
	}
	groups := map[string]*group{}
	order := []string{}
	for _, node := range m.nodes {
		if !usable(node) {
			continue
		}
		h := m.health[node.ID]
		key := h.IP
		if key == "" {
			key = "node:" + node.ID
		}
		late := h.LatencyMs
		if late <= 0 {
			late = 1 << 30 // never measured → last within its group
		}
		if g, ok := groups[key]; ok {
			if late < g.late {
				g.best, g.late = node, late
			}
		} else {
			groups[key] = &group{best: node, late: late}
			order = append(order, key)
		}
	}
	if len(order) == 0 {
		return ""
	}
	idx := int(m.rr.Add(1)-1) % len(order)
	chosen := groups[order[idx]].best.ID

	if seed != "" {
		if len(m.sticky) >= stickyConversations {
			oldest, oldestAt := "", int64(1<<62-1)
			for k, v := range m.stickyAt {
				if v < oldestAt {
					oldest, oldestAt = k, v
				}
			}
			if oldest != "" {
				delete(m.sticky, oldest)
				delete(m.stickyAt, oldest)
			}
		}
		m.sticky[seed] = chosen
		m.stickyAt[seed] = now.UnixMilli()
	}
	return chosen
}

// DialThrough dials one specific exit. The transport calls it when the request
// already carries an ExitPlan, so the session bound to that exit is the session
// actually used.
func (m *Manager) DialThrough(ctx context.Context, network, addr, nodeID string) (net.Conn, error) {
	m.mu.Lock()
	h := m.health[nodeID]
	var snap NodeHealth
	found := h != nil
	if found {
		snap = *h
	}
	m.mu.Unlock()
	if !found {
		return nil, fmt.Errorf("出口不存在: %s", nodeID)
	}
	return m.dialVia(ctx, network, addr, snap)
}

// ExitKey returns an exit's stable identity: its egress IP, or the node id when
// the IP is unknown. The upstream session is derived from this, so several
// nodes sharing one IP present one session instead of one each.
func (m *Manager) ExitKey(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.health[nodeID]; h != nil && h.IP != "" {
		return h.IP
	}
	return nodeID
}

// nodeByID looks a node up by id. The caller must hold m.mu.
func (m *Manager) nodeByID(id string) (Node, bool) {
	for _, n := range m.nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// dialVia opens one TCP connection through one node's socks inbound.
func (m *Manager) dialVia(ctx context.Context, network, addr string, h NodeHealth) (net.Conn, error) {
	// proxy.SOCKS5(network, proxyAddr, auth, forward): the target is resolved
	// at the node, because the CONNECT request carries the hostname.
	dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", h.Port), nil, proxy.Direct)
	if err != nil {
		m.park(h.ID)
		return nil, err
	}
	cd, ok := dialer.(proxy.ContextDialer)
	if !ok {
		m.park(h.ID)
		return nil, fmt.Errorf("socks5 dialer 不支持 context")
	}
	conn, derr := cd.DialContext(ctx, network, addr)
	if derr != nil {
		m.park(h.ID)
		return nil, derr
	}
	return conn, nil
}
