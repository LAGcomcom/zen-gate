package subs

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"zen-gate/internal/lane"
)

// probeTimeout bounds one node's egress echo round-trip.
//
// It was 12s, which a pool-wide round multiplied into minutes. 5s is still
// several times a slow mobile-network round-trip, so it only fires on nodes
// that are genuinely broken rather than merely far away.
const probeTimeout = 5 * time.Second

// coolDuration parks a node after a dial failure before it rejoins rotation.
const coolDuration = time.Minute

// probeMaxParallel caps one probe round.
//
// This probe asks ipinfo/ipify/ipapi "what is my IP" through a node's local
// socks inbound. It never touches an upstream API, holds no account, spends no
// quota — the only thing being multiplied is connections to the node itself, so
// a wave is cheap. 32 keeps the peak bounded (at most 32 nodes × 3 echo sources
// in flight) while still finishing a 64-node pool in two waves instead of the
// old eight.
//
// Do not copy this number into the model probe: that one sends real requests to
// a real account (see lane.ProbeCatalog), where a wave is exactly what gets an
// account rate-limited or banned.
const probeMaxParallel = 32

// probeParallel is how many nodes one round probes at once. A fixed 8 turned a
// 64-node pool into 8 sequential waves, which is exactly where "the probe takes
// minutes" came from; a probe has no reason to queue behind another probe.
func probeParallel(n int) int {
	if n > probeMaxParallel {
		return probeMaxParallel
	}
	if n < 1 {
		return 1
	}
	return n
}

// probeRetryGap is the pause before a node's second try. It only has to let a
// transient local-port collision clear, not give a dead node time to recover.
const probeRetryGap = 150 * time.Millisecond

// probeNodeRoundtrips is how many tries one node gets before it is judged dead.
// ProbeAll's verdict is a sample, and a sample is cheap to get wrong: transient
// jitter, upstream rate limits and local port contention all read as "egress
// empty" on the first try, and a false death drops a healthy node out of the
// pool until the next probe. One re-try on a fresh connection clears those.
// A timeout is not re-tried — see probeOnce.
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
	// A full round waits its turn: it is what a refresh or a manual "probe now"
	// asks for, and returning early would make the button look broken. The
	// death loop, which fires every two minutes anyway, gives way instead.
	m.probeGate.Lock()
	defer m.probeGate.Unlock()
	m.probeNodes(ctx, m.snapshotNodes(func(h *NodeHealth) bool {
		return !(h.LastOKMs > 0 && time.Since(time.UnixMilli(h.LastOKMs)) < realTrafficTrust)
	}))
}

// ProbeDead re-checks only the nodes currently marked dead, so a node killed by
// a transient failure rejoins rotation without waiting for a full refresh.
func (m *Manager) ProbeDead(ctx context.Context) {
	// Give way to a round already in flight: that round is sampling these very
	// nodes, and a second wave would only double the connections to decide the
	// same question.
	if !m.probeGate.TryLock() {
		return
	}
	defer m.probeGate.Unlock()
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
	m.mu.Lock()
	m.probeTotal, m.probeDone = len(nodes), 0
	m.mu.Unlock()
	// Zeroing on the way out is what tells the panel "the round is over":
	// a leftover 64/64 would look identical to a round that just started.
	defer func() {
		m.mu.Lock()
		m.probeTotal, m.probeDone = 0, 0
		m.mu.Unlock()
	}()

	// One wave, not a queue. workers is already len(nodes) for any realistic
	// subscription (probeParallel only bites past 256), so the whole pool is
	// asked at once and the round costs the slowest single node instead of
	// slowest × ceil(n/8).
	probeWave(len(nodes), func(i int) {
		h := nodes[i]
		res := m.probeOnce(ctx, h.Port)
		m.mu.Lock()
		m.probeDone++
		if cur := m.health[h.ID]; cur != nil {
			cur.LastCheck = time.Now().UnixMilli()
			m.applyProbeVerdict(cur, res)
		}
		m.mu.Unlock()
	})
}

// probeWave runs fn over n items with the round's parallelism and returns when
// all of them are done. Split out of probeNodes so "the whole pool is one wave"
// is testable without real sockets — that property is the whole point of the
// function, and it is invisible in a diff.
func probeWave(n int, fn func(i int)) {
	if n <= 0 {
		return
	}
	workers := probeParallel(n)
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		idx <- i
	}
	close(idx)
	wg.Wait()
}

// probeResult is one node's probe outcome. Reachability, the exit and the
// reason travel together so a caller cannot report "dead" without also
// carrying why.
type probeResult struct {
	Egress    lane.Egress
	LatencyMs int
	Reachable bool
	Reason    string
}

// applyProbeVerdict folds one probe round into a node's health. Split out of
// probeNodes so the three-way decision is testable without a live socks port.
// Caller holds m.mu.
func (m *Manager) applyProbeVerdict(cur *NodeHealth, res probeResult) {
	switch {
	case !res.Reachable:
		// The node itself could not carry a connection: that is death, and it
		// is the only case that clears the known exit.
		cur.Alive = false
		cur.LatencyMs = 0
		cur.EgressStale = false
		cur.IP, cur.Country = "", ""
		cur.FailReason, cur.FailAt = res.Reason, time.Now().UnixMilli()
	case res.Egress.IP == "":
		// The node carried a connection but no echo service answered. Unknown
		// exit, not a dead node: keep it in rotation with its last known
		// identity rather than emptying the pool.
		cur.Alive = true
		cur.CoolUntil = 0
		cur.EgressStale = true
		cur.FailReason, cur.FailAt = res.Reason, time.Now().UnixMilli()
	default:
		m.reviveNode(cur, res.Egress, res.LatencyMs)
		cur.FailReason, cur.FailAt = "", 0
	}
}

// reviveNode marks a node alive after a successful probe and clears every
// failure counter, so it rejoins rotation with a clean slate. Caller holds
// m.mu.
func (m *Manager) reviveNode(cur *NodeHealth, eg lane.Egress, lat int) {
	// An exit that changed IP is a different exit as far as an upstream is
	// concerned: everything it learned about the previous address (session
	// affinity, rate-limit state, risk score) does not carry over. Two things
	// must follow from that — the drift is recorded, and any conversation
	// still pinned here is unpinned so it re-picks instead of silently
	// continuing from another country.
	if cur.IP != "" && eg.IP != "" && cur.IP != eg.IP {
		cur.DriftFrom = cur.IP
		cur.DriftAt = time.Now().UnixMilli()
		cur.Drifts++
		m.dropSticky(cur.ID, cur.IP)
	}
	cur.Alive = true
	cur.EgressStale = false
	cur.IP, cur.Country = eg.IP, eg.Country
	cur.LatencyMs = lat
	cur.CoolUntil = 0 // a live egress is proof enough to rejoin rotation now
	delete(m.nodeStreak, cur.ID)
	// The tunnel answered again: the strike counter from ReportDead must
	// restart, or the first post-revival failure would instantly retire the
	// node a second time and the "two consecutive strikes" rule would
	// silently become "one, forever".
	delete(m.nodeFails, cur.ID)
	delete(m.nodeFailAt, cur.ID)
}

// dropSticky unpins every conversation bound to one exit identity. Both forms
// have to be cleared because Pick keys a probed exit by its IP and an
// unprobed one by "node:<id>". Caller holds m.mu.
func (m *Manager) dropSticky(nodeID, oldIP string) {
	forms := map[string]bool{"node:" + nodeID: true}
	if oldIP != "" {
		forms[oldIP] = true
	}
	for seed, key := range m.sticky {
		if forms[key] {
			delete(m.sticky, seed)
			delete(m.stickyAt, seed)
		}
	}
}

// reachTimeout bounds the "can this node carry a TCP connection at all" check.
const reachTimeout = 3 * time.Second

// reachHost is the neutral address used to tell a dead node from an
// unreachable echo service. It is deliberately NOT one of the echo hosts: if
// it were, an echo outage would look exactly like a dead node, which is the
// confusion this check exists to remove.
const reachHost = "1.1.1.1:443"

// probeOnce dials out through one node's socks inbound, re-trying the whole
// round-trip before it reports an empty egress.
//
// Reachable is the part that must not be lost: false means the node could not
// carry a connection at all, true with an empty Egress means the node carried
// one but no echo service answered. Callers treat the first as death and the
// second as unknown — an echo outage must not empty the pool.
func (m *Manager) probeOnce(ctx context.Context, port int) probeResult {
	var res probeResult
	var lastErr error
	for try := 0; try < probeNodeRoundtrips; try++ {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		client := nodeClient(port)
		start := time.Now()
		eg, err := lane.DetectEgressViaErr(pctx, client)
		res.LatencyMs = int(time.Since(start).Milliseconds())
		cancel()
		// The client owns a per-probe transport; without this every round leaves
		// three idle connections per node in the pool until the process exits.
		client.CloseIdleConnections()
		if eg.IP != "" {
			res.Egress, res.Reachable = eg, true
			return res
		}
		if err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			res.Reason = "探测被取消"
			return res
		}
		// Burning the whole budget IS the timeout verdict. Retrying a node that
		// just used all 5s doubles the price of every dead node in the round —
		// and a dead node is the common case in a stale subscription. Only fast
		// failures (refused, reset, bad handshake) get a second look, because
		// those are the shapes a transient port collision actually produces.
		if res.LatencyMs >= int(probeTimeout/time.Millisecond) {
			break
		}
		if try+1 < probeNodeRoundtrips {
			time.Sleep(probeRetryGap)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, reachTimeout)
	defer cancel()
	res.Reachable = dialThroughNode(rctx, port)
	if res.Reachable {
		res.Reason = "出口回显不可用，节点可连通：" + shortReason(lastErr)
	} else {
		res.Reason = "拨号不通：" + shortReason(lastErr)
	}
	return res
}

// shortReason turns a transport error into the one phrase an operator can act
// on. The full chain stays in the log; the panel needs the category.
func shortReason(err error) string {
	if err == nil {
		return "无响应"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "deadline exceeded"), strings.Contains(s, "timeout"),
		strings.Contains(s, "timed out"):
		return "超时"
	case strings.Contains(s, "connection refused"):
		return "连接被拒"
	case strings.Contains(s, "connection reset"), strings.Contains(s, "broken pipe"):
		return "连接被重置"
	case strings.Contains(s, "certificate"), strings.Contains(s, "tls"):
		return "TLS 握手失败"
	case strings.Contains(s, "no such host"), strings.Contains(s, "dns"):
		return "域名解析失败"
	case strings.Contains(s, "socks"), strings.Contains(s, "eof"):
		return "SOCKS 无响应"
	}
	// Unknown shapes keep their first clause so the panel is never blank.
	if i := strings.IndexByte(err.Error(), ':'); i > 0 {
		return err.Error()[:i]
	}
	return err.Error()
}

// dialThroughNode asks the node to CONNECT to a neutral public address. A
// working node answers; a dead one does not. No payload is sent — the TCP
// handshake through the socks proxy is the whole question.
func dialThroughNode(ctx context.Context, port int) bool {
	d, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, proxy.Direct)
	if err != nil {
		return false
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return false
	}
	conn, err := cd.DialContext(ctx, "tcp", reachHost)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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

// Dial implements lane.Rotator: pick the next exit (round-robin over egress
// IPs, same unit as Pick — walking nodes here spent one shared IP's quota
// several times while other exits sat idle), connect to the target through its
// local socks inbound, and on failure park that node and hop to the next.
func (m *Manager) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var lastErr error
	for hop := 0; hop < 3; hop++ {
		id := m.Pick("", "")
		if id == "" {
			if lastErr != nil {
				return nil, fmt.Errorf("所有订阅节点都拨号失败(%d 个健康节点): %w", m.Healthy(), lastErr)
			}
			return nil, fmt.Errorf("没有健康节点——请刷新订阅或检查节点")
		}
		m.mu.Lock()
		h := m.health[id]
		var snap NodeHealth
		found := h != nil
		if found {
			snap = *h
		}
		m.mu.Unlock()
		if !found {
			continue
		}
		conn, derr := m.dialVia(ctx, network, addr, snap)
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
		if ctx.Err() != nil {
			return nil, derr
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("所有订阅节点都拨号失败(%d 个健康节点)", m.Healthy())
}

// nextNode walks the alive list round-robin by node. Production dialing goes
// through Pick instead: the rotation unit is the egress IP, because several
// nodes behind one IP spend one shared allowance. Kept as the plain primitive
// the rotation tests assert against.
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

// coolNode parks a node after a dial failure, escalating the park duration with
// consecutive failures so a persistently dead node stops costing a full
// dialTimeout on every request. One failure parks for coolDuration; each repeat
// doubles it (capped at 15m) -- the node-level mirror of the (exit, model)
// ledger's adaptive delay in ledger.go.
func (m *Manager) coolNode(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nodeStreak == nil {
		m.nodeStreak = make(map[string]int)
	}
	m.nodeStreak[id]++
	n := m.nodeStreak[id]
	d := coolDuration << min(n-1, 4) // 1m, 2m, 4m, 8m, 16m
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	if h := m.health[id]; h != nil {
		h.CoolUntil = time.Now().Add(d).UnixMilli()
	}
}

// warmNode clears a node's consecutive-failure streak after a successful dial
// (or a live probe / served request), so a node that recovers rejoins rotation
// at the base cooldown instead of carrying stale escalation.
func (m *Manager) warmNode(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nodeStreak, id)
}

var _ lane.Rotator = (*Manager)(nil)
