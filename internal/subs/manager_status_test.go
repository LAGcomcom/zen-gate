package subs

import "testing"

// Status is serialized after the manager lock is released. Returning the live
// *NodeHealth let a concurrent probe rewrite a row mid-encoding, and callers
// could mutate manager state by accident.
func TestStatusReturnsHealthCopies(t *testing.T) {
	live := &NodeHealth{ID: "a", Alive: true}
	m := newTestManager([]Node{{ID: "a"}}, map[string]*NodeHealth{"a": live})

	rows := m.Status()["nodes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("nodes = %d, want 1", len(rows))
	}
	live.Alive = false
	if !rows[0].(NodeHealth).Alive {
		t.Fatal("Status kept an alias to the live health row")
	}
}
