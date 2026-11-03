package subs

import (
	"testing"

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
	m.applyProbeVerdict(cur, lane.Egress{}, 0, true)

	if !cur.Alive {
		t.Fatal("回显不可用不应判节点死亡（会把整池清空并静默降级直连）")
	}
	if !cur.EgressStale {
		t.Fatal("回显不可用应标记 EgressStale，让面板能区分「出口未知」与「出口正常」")
	}
	if cur.IP != "203.0.113.7" || cur.Country != "JP" {
		t.Fatalf("未知出口应保留上次已知身份，得到 %q/%q", cur.IP, cur.Country)
	}
}

// A node that cannot carry a connection at all is death — the only case that
// clears the known exit.
func TestProbeVerdictUnreachableRetiresNode(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP"},
	})
	cur := m.health["n1"]
	m.applyProbeVerdict(cur, lane.Egress{}, 0, false)

	if cur.Alive {
		t.Fatal("拨号不通必须判死")
	}
	if cur.IP != "" || cur.Country != "" {
		t.Fatal("死节点应清空出口信息，避免面板显示过期国家")
	}
	if cur.EgressStale {
		t.Fatal("死节点不应同时带 stale 标记")
	}
}

// A node that comes back on a different IP is a different exit: the drift is
// recorded and every conversation pinned to it is unpinned.
func TestExitDriftClearsSticky(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP"},
	})
	m.sticky["conv-a"] = "n1"
	m.sticky["conv-b"] = "n1"
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
}

// Same IP is not a drift: no sticky invalidation, no counter bump.
func TestExitStableKeepsSticky(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "203.0.113.7", Country: "JP"},
	})
	m.sticky["conv-a"] = "n1"

	cur := m.health["n1"]
	m.reviveNode(cur, lane.Egress{IP: "203.0.113.7", Country: "JP"}, 88)

	if cur.Drifts != 0 {
		t.Fatalf("出口未变不应计漂移，得到 %d", cur.Drifts)
	}
	if m.sticky["conv-a"] != "n1" {
		t.Fatal("出口未变不应打断会话粘性（会丢掉 prompt cache）")
	}
}
