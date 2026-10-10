package subs

import (
	"testing"
	"time"
)

// Probing skips a node a real request just validated: the probe is a sample and
// the served request is proof.
func TestProbeAllSkipsRecentlyValidatedNodes(t *testing.T) {
	m, health := poolForPick()
	health["n3"].LastOKMs = time.Now().UnixMilli()
	keep := func(h *NodeHealth) bool {
		return !(h.LastOKMs > 0 && time.Since(time.UnixMilli(h.LastOKMs)) < realTrafficTrust)
	}
	got := m.snapshotNodes(keep)
	for _, h := range got {
		if h.ID == "n3" {
			t.Fatal("a node validated by real traffic seconds ago was scheduled for probing")
		}
	}
	if len(got) != 3 {
		t.Fatalf("probe set = %d nodes, want 3", len(got))
	}
	// Past the trust window it returns to the probe rotation.
	health["n3"].LastOKMs = time.Now().Add(-2 * realTrafficTrust).UnixMilli()
	if got = m.snapshotNodes(keep); len(got) != 4 {
		t.Fatalf("probe set after the trust window = %d nodes, want 4", len(got))
	}
	// A node that has never served traffic is probed as usual.
	health["n3"].LastOKMs = 0
	if got = m.snapshotNodes(keep); len(got) != 4 {
		t.Fatalf("probe set with no real traffic = %d nodes, want 4", len(got))
	}
}
