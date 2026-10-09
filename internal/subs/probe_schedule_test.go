package subs

import (
	"net"
	"testing"
	"time"
)

// Issue #29: dead nodes must get re-probed on their own cadence instead of
// waiting for a full refresh (manual, or the hourly one) with every request
// failing meanwhile. ProbeDead selects exactly the dead set; ProbeAll selects
// everything.
func TestProbeDeadSelectsOnlyDeadNodes(t *testing.T) {
	nodes := []Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	health := map[string]*NodeHealth{
		"a": {ID: "a", Port: 1, Alive: true},
		"b": {ID: "b", Port: 2, Alive: false},
		"c": {ID: "c", Port: 3, Alive: false},
	}
	m := newTestManager(nodes, health)

	dead := m.snapshotNodes(func(h *NodeHealth) bool { return !h.Alive })
	if len(dead) != 2 {
		t.Fatalf("dead set = %d nodes, want 2", len(dead))
	}
	for _, h := range dead {
		if h.Alive {
			t.Errorf("node %s came back alive in the dead set", h.ID)
		}
	}
	all := m.snapshotNodes(func(*NodeHealth) bool { return true })
	if len(all) != 3 {
		t.Fatalf("full set = %d nodes, want 3", len(all))
	}
}

// waitPortReady returns as soon as the port accepts, and gives up at the
// deadline otherwise — the boot health pass must wait for sing-box's first
// inbound instead of probing into a not-yet-listening pool (issue #29 defect 1).
func TestWaitPortReadyBothPaths(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	start := time.Now()
	waitPortReady(port, 10*time.Second)
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("ready port took %v, want the fast path", el)
	}

	// A port nobody listens on must not hang past its deadline. Ask for a
	// closed port: bind once, release, and the port is reliably refused.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	start = time.Now()
	waitPortReady(deadPort, 1500*time.Millisecond)
	if el := time.Since(start); el < 1400*time.Millisecond || el > 3*time.Second {
		t.Errorf("dead port returned after %v, want the ~1.5s deadline", el)
	}
}
