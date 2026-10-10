package subs

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"zen-gate/internal/lane"
)

// probeTimeout bounds one node's egress echo round-trip.
const probeTimeout = 12 * time.Second

// coolDuration parks a node after a dial failure before it rejoins rotation.
const coolDuration = time.Minute

// probeNodeRoundtrips is how many tries one node gets before it is judged dead.
// ProbeAll's verdict is a sample, and a sample is cheap to get wrong: transient
// jitter, upstream rate limits and local port contention all read as "egress
// empty" on the first try, and a false death drops a healthy node out of the
// pool until the next probe. One re-try on a fresh connection clears those.
const probeNodeRoundtrips = 2

// ProbeAll health-checks every node concurrently: egress IP/country via the
// node's local socks inbound, latency from the echo round-trip. Nodes that
// fail lose their Alive flag and drop out of rotation until the next probe.
// realTrafficTrust is how long a real success keeps a node out of the probe
// rotation. A probe is a sample that can be wrong in both directions; a served
// request is proof, so re-sampling that node spends probe budget to risk
// overwriting a better verdict. The lane already applies this idea per model
// (Lane.realOK); this is the per-node counterpart it needs to pick an exit.
const realTrafficTrust = 5 * time.Minute

func (m *Manager) ProbeAll(ctx context.Context) {
	m.probeNodes(ctx, m.snapshotNodes(func(h *NodeHealth) bool {
		return !(h.LastOKMs > 0 && time.Since(time.UnixMilli(h.LastOKMs)) < realTrafficTrust)
	}))
}

// ProbeDead re-checks only the nodes currently marked dead, so a node killed by
// a transient failure rejoins rotation without waiting for a full refresh.
func (m *Manager) ProbeDead(ctx context.Context) {
	m.probeNodes(ctx, m.snapshotNodes(func(h *NodeHealth) bool { return !h.Alive }))
}

func (m *Manager) snapshotNodes(keep func(*NodeHealth) bool) []NodeHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodes := make([]NodeHealth, 0, len(m.nodes))
	for _, n := range m.nodes {
		if h := m.health[n.ID]; h != nil && keep(h) {
			nodes = append(nodes, *h)
		}
	}
	return nodes
}

func (m *Manager) probeNodes(ctx context.Context, nodes []NodeHealth) {
	if len(nodes) == 0 {
		return
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, h := range nodes {
		wg.Add(1)
		go func(h NodeHealth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			eg, lat := m.probeOnce(ctx, h.Port)
			m.mu.Lock()
			defer m.mu.Unlock()
			if cur := m.health[h.ID]; cur != nil {
				cur.LastCheck = time.Now().UnixMilli()
				if eg.IP == "" {
					cur.Alive = false
					cur.LatencyMs = 0
					cur.IP, cur.Country = "", ""
				} else {
					cur.Alive = true
					cur.IP, cur.Country = eg.IP, eg.Country
					cur.LatencyMs = lat
					cur.CoolUntil = 0 // a live egress is proof enough to rejoin rotation now
				}
			}
		}(h)
	}
	wg.Wait()
}

// probeOnce dials out through one node's socks inbound, re-trying the whole
// round-trip before it reports an empty egress.
func (m *Manager) probeOnce(ctx context.Context, port int) (lane.Egress, int) {
	var eg lane.Egress
	var lat int
	for try := 0; try < probeNodeRoundtrips; try++ {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		client := nodeClient(port)
		start := time.Now()
		eg = lane.DetectEgressVia(pctx, client)
		lat = int(time.Since(start).Milliseconds())
		cancel()
		if eg.IP != "" {
			return eg, lat
		}
		if ctx.Err() != nil {
			return eg, lat
		}
		if try+1 < probeNodeRoundtrips {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return eg, lat
}

// nodeClient builds an HTTP client whose egress is one node's local socks
// inbound (http.Transport speaks socks5 natively via the Proxy func).
func nodeClient(port int) *http.Client {
	u, _ := url.Parse(fmt.Sprintf("socks5://127.0.0.1:%d", port))
	return &http.Client{
		Timeout:   probeTimeout,
		Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return u, nil }},
	}
}

// Dial implements lane.Rotator: pick the next healthy node (atomic
// round-robin), connect to the target through its local socks inbound, and on
// failure park that node for a minute and hop to the next (up to 3 tries).
func (m *Manager) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	for hop := 0; hop < 3; hop++ {
		h, err := m.nextNode()
		if err != nil {
			return nil, err
		}
		// proxy.SOCKS5(network, proxyAddr, auth, forward): the target addr is
		// resolved at the node (the CONNECT request carries the hostname).
		dialer, derr := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", h.Port), nil, proxy.Direct)
		if derr != nil {
			m.park(h.ID)
			continue
		}
		cd, ok := dialer.(proxy.ContextDialer)
		if !ok {
			m.park(h.ID)
			continue
		}
		conn, derr := cd.DialContext(ctx, network, addr)
		if derr != nil {
			m.park(h.ID)
			if ctx.Err() != nil {
				return nil, derr
			}
			continue
		}
		return conn, nil
	}
	return nil, fmt.Errorf("所有订阅节点都拨号失败(%d 个健康节点)", m.Healthy())
}

// nextNode atomically walks the alive list round-robin.
func (m *Manager) nextNode() (*NodeHealth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	alive := make([]*NodeHealth, 0, len(m.health))
	for _, n := range m.nodes {
		if h := m.health[n.ID]; h != nil && h.Alive && (h.CoolUntil == 0 || time.Now().UnixMilli() >= h.CoolUntil) {
			alive = append(alive, h)
		}
	}
	if len(alive) == 0 {
		return nil, fmt.Errorf("没有健康节点——请刷新订阅或检查节点")
	}
	idx := int(m.rr.Add(1)-1) % len(alive)
	return alive[idx], nil
}

func (m *Manager) park(id string) {
	m.mu.Lock()
	if h := m.health[id]; h != nil {
		h.CoolUntil = time.Now().Add(coolDuration).UnixMilli()
	}
	m.mu.Unlock()
}

var _ lane.Rotator = (*Manager)(nil)
