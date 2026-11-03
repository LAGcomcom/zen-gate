package subs

import (
	"testing"

	"zen-gate/internal/lane"
)

// A node whose tunnel "connects but every turn dies" cannot be caught by the
// probe (one tiny round-trip succeeds), so real traffic must be allowed to
// retire it: deadStrikes tunnel-level failures inside failMemory flip the
// node out of rotation, and the periodic dead-node probe brings it back.
func TestReportDeadRetiresNodeAfterTwoStrikes(t *testing.T) {
	m, health := poolForPick()

	m.ReportDead("n3") // strike one: jitter, node stays in
	if !health["n3"].Alive {
		t.Fatal("one tunnel failure must not retire a node")
	}
	m.ReportDead("n3") // strike two
	if health["n3"].Alive {
		t.Fatal("two tunnel failures inside the fail-memory window must retire the node")
	}
	for i := 0; i < 80; i++ {
		if id := m.Pick("", "m"); id == "n3" {
			t.Fatal("a retired node was picked")
		}
	}

	// The periodic dead-node probe answers again: the node rejoins and, the
	// point of the counter, its strikes restart from zero.
	m.mu.Lock()
	m.reviveNode(health["n3"], lane.Egress{IP: "10.0.0.2"}, 100)
	m.mu.Unlock()
	if !health["n3"].Alive {
		t.Fatal("a successful probe must revive a retired node")
	}
	m.ReportDead("n3")
	if !health["n3"].Alive {
		t.Fatal("after a revival the first new strike must not retire the node again")
	}
}

// A real request that succeeds through the node is stronger evidence than the
// strikes — including the race where the node was retired while the request
// was still in flight.
func TestReviveResurrectsRetiredNode(t *testing.T) {
	m, health := poolForPick()
	m.ReportDead("n3")
	m.ReportDead("n3")
	if health["n3"].Alive {
		t.Fatal("setup: node should be retired")
	}
	m.Revive("n3", "m")
	if !health["n3"].Alive {
		t.Fatal("a served request must resurrect the node")
	}
	m.ReportDead("n3")
	if !health["n3"].Alive {
		t.Fatal("strikes must restart after a revival, not carry over")
	}
}

// Retired nodes must not hand their sticky sessions nowhere: pick re-homes
// the conversation on the next call instead of returning the dead node.
func TestStickyDoesNotPinARetiredNode(t *testing.T) {
	m, _ := poolForPick()
	m.Pick("conv-1", "m") // establish a sticky binding
	m.mu.Lock()
	for _, h := range m.health {
		if h.ID != "n3" {
			h.Alive = false // leave n3 as the only candidate
		}
	}
	m.mu.Unlock()
	m.ReportDead("n3")
	m.ReportDead("n3")
	if id := m.Pick("conv-1", "m"); id == "n3" {
		t.Fatal("sticky kept a conversation pinned to a retired node")
	}
}
