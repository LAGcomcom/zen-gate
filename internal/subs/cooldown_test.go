package subs

import (
	"testing"
	"time"
)

// One failure, one rule: a node that keeps failing must get progressively more
// expensive to retry, and a single success must undo the escalation. Without
// the second half, a node that recovered would keep its old streak and the next
// isolated failure would park it for minutes.
func TestCoolNodeEscalatesAndWarmNodeResets(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true},
	})
	cur := m.health["n1"]

	m.coolNode("n1")
	first := time.Until(time.UnixMilli(cur.CoolUntil))
	if first < 55*time.Second || first > 61*time.Second {
		t.Fatalf("首次失败应冷却约 1 分钟，得到 %v", first)
	}

	m.coolNode("n1")
	second := time.Until(time.UnixMilli(cur.CoolUntil))
	if second < 115*time.Second || second > 121*time.Second {
		t.Fatalf("连续第二次失败应升级到约 2 分钟，得到 %v", second)
	}

	m.warmNode("n1")
	m.coolNode("n1")
	reset := time.Until(time.UnixMilli(cur.CoolUntil))
	if reset > 61*time.Second {
		t.Fatalf("成功一次后应回到基础冷却，得到 %v", reset)
	}
}

// The escalation must stop somewhere: a node that is simply gone should cost
// one attempt per 15 minutes, not one per hour or one per request.
func TestCoolNodeCapsAtFifteenMinutes(t *testing.T) {
	m := newTestManager(nil, map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true},
	})
	for i := 0; i < 10; i++ {
		m.coolNode("n1")
	}
	d := time.Until(time.UnixMilli(m.health["n1"].CoolUntil))
	if d > 15*time.Minute+time.Second {
		t.Fatalf("冷却应封顶 15 分钟，得到 %v", d)
	}
	if d < 14*time.Minute {
		t.Fatalf("多次连续失败后应达到封顶值，得到 %v", d)
	}
}

// A cooling node drops out of rotation; once the park expires it comes back
// without needing a successful probe first.
func TestCoolNodeRemovesFromRotation(t *testing.T) {
	m := newTestManager(
		[]Node{{ID: "n1"}, {ID: "n2"}},
		map[string]*NodeHealth{
			"n1": {ID: "n1", Port: 1, Alive: true},
			"n2": {ID: "n2", Port: 2, Alive: true},
		})
	if m.Healthy() != 2 {
		t.Fatalf("初始 Healthy = %d", m.Healthy())
	}
	m.coolNode("n1")
	if m.Healthy() != 1 {
		t.Fatalf("冷却中的节点应离开轮转，Healthy = %d", m.Healthy())
	}
	m.mu.Lock()
	m.health["n1"].CoolUntil = time.Now().Add(-time.Second).UnixMilli()
	m.mu.Unlock()
	if m.Healthy() != 2 {
		t.Fatalf("冷却到期应自动回到轮转，Healthy = %d", m.Healthy())
	}
}
