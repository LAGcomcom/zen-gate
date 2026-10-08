package subs

import (
	"context"
	"testing"
	"time"
)

func newTestManager(nodes []Node, health map[string]*NodeHealth) *Manager {
	m := NewManager(nil, nil)
	m.nodes = nodes
	m.health = health
	return m
}

func TestRotationRoundRobin(t *testing.T) {
	nodes := []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	health := map[string]*NodeHealth{
		"a": {ID: "a", Port: 1, Alive: true},
		"b": {ID: "b", Port: 2, Alive: true},
		"c": {ID: "c", Port: 3, Alive: true},
	}
	m := newTestManager(nodes, health)
	if m.Healthy() != 3 {
		t.Fatalf("Healthy = %d", m.Healthy())
	}
	seen := []string{}
	for i := 0; i < 3; i++ {
		h, err := m.nextNode()
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, h.ID)
	}
	if seen[0] == seen[1] || seen[1] == seen[2] {
		t.Fatalf("轮询应依次经过所有节点: %v", seen)
	}
}

func TestRotationSkipsParkedAndDead(t *testing.T) {
	until := time.Now().Add(time.Minute).UnixMilli()
	nodes := []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	health := map[string]*NodeHealth{
		"a": {ID: "a", Port: 1, Alive: false},                  // 探测失败
		"b": {ID: "b", Port: 2, Alive: true},                   // 健康
		"c": {ID: "c", Port: 3, Alive: true, CoolUntil: until}, // 拨号失败被暂缓
	}
	m := newTestManager(nodes, health)
	if m.Healthy() != 1 {
		t.Fatalf("Healthy = %d, 期望 1", m.Healthy())
	}
	for i := 0; i < 3; i++ {
		h, err := m.nextNode()
		if err != nil {
			t.Fatal(err)
		}
		if h.ID != "b" {
			t.Fatalf("只应选中 b, 得到 %s", h.ID)
		}
	}
}

func TestRotationNoHealthyNodes(t *testing.T) {
	m := newTestManager([]Node{{ID: "a"}}, map[string]*NodeHealth{"a": {ID: "a", Alive: false}})
	if _, err := m.nextNode(); err == nil {
		t.Fatal("无健康节点应报错")
	}
	if _, err := m.Dial(context.Background(), "tcp", "example.com:443"); err == nil {
		t.Fatal("无健康节点 Dial 应报错")
	}
}

func TestParkExpires(t *testing.T) {
	m := newTestManager([]Node{{ID: "a"}, {ID: "b"}}, map[string]*NodeHealth{
		"a": {ID: "a", Port: 1, Alive: true},
		"b": {ID: "b", Port: 2, Alive: true},
	})
	m.park("a")
	if m.Healthy() != 1 {
		t.Fatalf("park 后 Healthy = %d", m.Healthy())
	}
	m.mu.Lock()
	m.health["a"].CoolUntil = time.Now().Add(-time.Second).UnixMilli()
	m.mu.Unlock()
	if m.Healthy() != 2 {
		t.Fatalf("冷却过期后 Healthy = %d", m.Healthy())
	}
}
