package subs

import (
	"testing"
)

// Node pool for these tests. Four nodes, three distinct egress IPs, with one IP
// carrying two nodes — the shape a real airport hands out (measured: 55 live
// nodes behind 21 IPs).
func poolForPick() (*Manager, map[string]*NodeHealth) {
	nodes := []Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}, {ID: "n4"}}
	health := map[string]*NodeHealth{
		"n1": {ID: "n1", Port: 1, Alive: true, IP: "10.0.0.1", LatencyMs: 900},
		"n2": {ID: "n2", Port: 2, Alive: true, IP: "10.0.0.1", LatencyMs: 200}, // same IP, faster
		"n3": {ID: "n3", Port: 3, Alive: true, IP: "10.0.0.2", LatencyMs: 500},
		"n4": {ID: "n4", Port: 4, Alive: true, IP: "10.0.0.3", LatencyMs: 400},
	}
	return newTestManager(nodes, health), health
}

// Rotation is counted per egress IP, not per node: walking nodes would spend an
// IP shared by several nodes faster than the others and never use the pool's
// whole allowance.
func TestPickRotatesByEgressIP(t *testing.T) {
	m, health := poolForPick()
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		id := m.Pick("", "m")
		if id == "" {
			t.Fatal("Pick returned empty with a healthy pool")
		}
		seen[health[id].IP]++
	}
	if len(seen) != 3 {
		t.Fatalf("distinct egress IPs used = %d, want 3 (one IP is shared by two nodes)", len(seen))
	}
	for ip, n := range seen {
		if n != 100 {
			t.Errorf("IP %s took %d of 300 picks, want an equal share (100)", ip, n)
		}
	}
}

// Inside one IP the nodes spend the same allowance, so picking the fastest is
// free.
func TestPickPrefersFastestNodeWithinAnIP(t *testing.T) {
	m, _ := poolForPick()
	for i := 0; i < 120; i++ {
		if id := m.Pick("", "m"); id == "n1" {
			t.Fatalf("n1 (900ms) was chosen inside an IP that also has n2 (200ms)")
		}
	}
}

// One conversation prefers the exit it already used, so the upstream has a
// chance to reuse its prompt cache.
func TestPickIsStickyPerConversation(t *testing.T) {
	m, _ := poolForPick()
	first := m.Pick("conversation-a", "m")
	for i := 0; i < 40; i++ {
		if got := m.Pick("conversation-a", "m"); got != first {
			t.Fatalf("conversation-a moved between exits: %s then %s", first, got)
		}
	}
	// A different conversation is free to land elsewhere.
	other := map[string]bool{}
	for i := 0; i < 40; i++ {
		other[m.Pick("conversation-b", "m")] = true
	}
	if len(other) == 0 {
		t.Fatal("conversation-b never resolved to an exit")
	}
}

// Sticky is a preference, never a lock: an exit that dies or cools down must be
// dropped immediately, so an exhausted exit cannot strand a conversation.
func TestStickyDropsUnusableExit(t *testing.T) {
	m, health := poolForPick()
	first := m.Pick("c", "m")

	health[first].Alive = false
	if got := m.Pick("c", "m"); got == first {
		t.Fatalf("conversation kept a dead exit %s", first)
	}

	// Re-bind, then park it — same expectation.
	m.Pick("c-2", "m")
	second := m.Pick("c-2", "m")
	m.park(second)
	if got := m.Pick("c-2", "m"); got == second {
		t.Fatalf("conversation kept a parked exit %s", second)
	}
}

// The session is derived from the exit's stable identity, so nodes sharing one
// IP present one session rather than one each.
func TestExitKeyPrefersEgressIP(t *testing.T) {
	m, _ := poolForPick()
	if got := m.ExitKey("n1"); got != "10.0.0.1" {
		t.Errorf("ExitKey(n1) = %q, want the egress IP", got)
	}
	if m.ExitKey("n1") != m.ExitKey("n2") {
		t.Error("nodes sharing an IP must share one exit identity")
	}
	if m.ExitKey("n4") == m.ExitKey("n3") {
		t.Error("different IPs must not collapse to one identity")
	}
	if got := m.ExitKey("missing"); got != "missing" {
		t.Errorf("ExitKey(unknown) = %q, want the node id", got)
	}
	if got := m.ExitKey(""); got != "" {
		t.Errorf("ExitKey(empty) = %q, want empty", got)
	}
}
