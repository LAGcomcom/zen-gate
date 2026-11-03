package subs

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// An echo outage must not read as "every node is dead". The node carried a
// connection; the exit is merely unknown, so it stays in rotation with its
// last known identity.
func TestProbeVerdictEchoOutageKeepsNodeAlive(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP", LatencyMs: 120},
	})
	cur := m.health["n1"]
	m.applyProbeVerdict(cur, probeResult{Reachable: true, Reason: "出口回显不可用，节点可连通：超时"})

	if !cur.Alive {
		t.Fatal("回显不可用不应判节点死亡（会把整池清空并静默降级直连）")
	}
	if !cur.EgressStale {
		t.Fatal("回显不可用应标记 EgressStale，让面板能区分「出口未知」与「出口正常」")
	}
	if cur.IP != "203.0.113.7" || cur.Country != "JP" {
		t.Fatalf("未知出口应保留上次已知身份，得到 %q/%q", cur.IP, cur.Country)
	}
	if cur.FailReason == "" {
		t.Fatal("出口未知必须留下可读原因，否则面板只能说「不可用」，运维只能猜")
	}
}

// A node that cannot carry a connection at all is death — the only case that
// clears the known exit.
func TestProbeVerdictUnreachableRetiresNode(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP"},
	})
	cur := m.health["n1"]
	m.applyProbeVerdict(cur, probeResult{Reachable: false, Reason: "拨号不通：连接被拒"})

	if cur.Alive {
		t.Fatal("拨号不通必须判死")
	}
	if cur.IP != "" || cur.Country != "" {
		t.Fatal("死节点应清空出口信息，避免面板显示过期国家")
	}
	if cur.EgressStale {
		t.Fatal("死节点不应同时带 stale 标记")
	}
	if !strings.Contains(cur.FailReason, "拨号不通") {
		t.Fatalf("死节点原因应说明拨号不通，得到 %q", cur.FailReason)
	}
	if cur.FailAt == 0 {
		t.Fatal("失败原因应带时间戳")
	}
}

// A node that comes back on a different IP is a different exit: the drift is
// recorded and every conversation pinned to it is unpinned.
func TestExitDriftClearsSticky(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP"},
	})
	m.sticky["conv-a"] = "203.0.113.7"
	m.sticky["conv-b"] = "203.0.113.7"
	m.stickyAt["conv-a"] = 1
	m.stickyAt["conv-b"] = 1

	cur := m.health["n1"]
	m.reviveNode(cur, lane.Egress{IP: "198.51.100.9", Country: "SG"}, 90)

	if cur.Drifts != 1 || cur.DriftFrom != "203.0.113.7" {
		t.Fatalf("漂移未记录: drifts=%d from=%q", cur.Drifts, cur.DriftFrom)
	}
	if len(m.sticky) != 0 {
		t.Fatalf("出口换 IP 后会话粘性必须作废，仍剩 %d 条", len(m.sticky))
	}
	if cur.IP != "198.51.100.9" || cur.Country != "SG" {
		t.Fatalf("应更新为新出口，得到 %q/%q", cur.IP, cur.Country)
	}
	if cur.EgressStale {
		t.Fatal("拿到出口后应清除 stale 标记")
	}
	if cur.FailReason != "" {
		t.Fatal("探测成功后必须清除失败原因，否则面板会挂着已经过期的红字")
	}
}

// Same IP is not a drift: no sticky invalidation, no counter bump.
func TestExitStableKeepsSticky(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP"},
	})
	m.sticky["conv-a"] = "203.0.113.7"

	cur := m.health["n1"]
	m.reviveNode(cur, lane.Egress{IP: "203.0.113.7", Country: "JP"}, 88)

	if cur.Drifts != 0 {
		t.Fatalf("出口未变不应计漂移，得到 %d", cur.Drifts)
	}
	if m.sticky["conv-a"] != "203.0.113.7" {
		t.Fatal("出口未变不应打断会话粘性（会丢掉 prompt cache）")
	}
}

// Pin the exit, not the node. Two nodes behind one IP are one exit as far as
// quota and prompt cache are concerned; when the faster one cools down the
// sibling must take the conversation, because the exit did not change.
func TestStickyFollowsExitNotNode(t *testing.T) {
	m := newTestManager(
		[]Node{{ID: "n1"}, {ID: "n2"}},
		map[string]*NodeHealth{
			"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", LatencyMs: 100},
			"n2": {ID: "n2", Port: 2, Alive: true, IP: "203.0.113.7", LatencyMs: 140},
		})
	m.sticky["conv"] = "203.0.113.7"
	m.stickyAt["conv"] = 1

	m.coolNode("n1")
	if got := m.Pick("conv", "m"); got != "n2" {
		t.Fatalf("同 IP 兄弟节点应接管，得到 %q（换节点不该被当成换出口）", got)
	}

	// Only when the whole exit is gone may the conversation move.
	m.coolNode("n2")
	if got := m.Pick("conv", "m"); got != "" {
		t.Fatalf("该出口全部不可用时应返回空交给上层，得到 %q", got)
	}
}

// A different IP is a different exit: the stale pin must not survive it.
func TestStickyDoesNotCrossExits(t *testing.T) {
	m := newTestManager(
		[]Node{{ID: "n1"}, {ID: "n2"}},
		map[string]*NodeHealth{
			"n1": {ID: "n1", Port: 1, Alive: false, IP: "203.0.113.7"},
			"n2": {ID: "n2", Port: 2, Alive: true, IP: "198.51.100.9", LatencyMs: 120},
		})
	m.sticky["conv"] = "203.0.113.7"
	m.stickyAt["conv"] = 1

	if got := m.Pick("conv", "m"); got != "n2" {
		t.Fatalf("旧出口已死，应重新选到 n2，得到 %q", got)
	}
	if m.sticky["conv"] != "198.51.100.9" {
		t.Fatalf("粘性应改钉新出口 IP，得到 %q", m.sticky["conv"])
	}
}

// The round is one wave, not eight sequential waves. That property is the whole
// point of probeWave and is invisible in a diff, so pin it: 32 items sleeping
// 30ms each must overlap instead of queueing.
func TestProbeWaveRunsPoolAtOnce(t *testing.T) {
	const n = probeMaxParallel
	var live, peak int32
	start := time.Now()
	probeWave(n, func(int) {
		cur := atomic.AddInt32(&live, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&live, -1)
	})
	elapsed := time.Since(start)

	if peak < int32(n-2) {
		t.Fatalf("并发峰值只有 %d/%d，这一轮又排队了", peak, n)
	}
	// Queued, this would be n*30ms = 960ms. A wave is ~30ms; 400ms leaves room
	// for a loaded CI box without hiding a regression to a queue.
	if elapsed > 400*time.Millisecond {
		t.Fatalf("一轮耗时 %v，接近串行（应为 ~30ms）", elapsed)
	}
}

// The cap still holds: an oversized subscription must not open every node at
// once, it goes in a couple of waves.
func TestProbeWaveHonoursCap(t *testing.T) {
	var live, peak int32
	probeWave(probeMaxParallel*3, func(int) {
		cur := atomic.AddInt32(&live, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&live, -1)
	})
	if peak > probeMaxParallel {
		t.Fatalf("并发 %d 超过上限 %d", peak, probeMaxParallel)
	}
}

// A dead-node recheck must give way to a round already in flight instead of
// opening a second set of connections to the same nodes.
func TestProbeDeadYieldsToRunningRound(t *testing.T) {
	var m Manager
	m.probeGate.Lock()
	defer m.probeGate.Unlock()

	done := make(chan struct{})
	go func() {
		m.ProbeDead(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("全量轮次进行中时 ProbeDead 应让路，而不是排队等待")
	}
}
