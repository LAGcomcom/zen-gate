package lane

import (
	"context"
	"testing"
)

// A channel disabled in its channels/<id>.json file empties its catalog
// bucket exactly like a logged-out account — deliberate absence publishes,
// its models leave every picker — and a re-enabled channel comes back on the
// next rebuild without a restart.
func TestChannelGateEmptiesDisabledSource(t *testing.T) {
	oldZen, oldKilo := fetchZenListing, fetchKiloListing
	fetchZenListing = func(context.Context) ([]string, error) {
		return []string{"deepseek-v4.1-flash-free", "glm-5.3-flash-free"}, nil
	}
	fetchKiloListing = func(context.Context) ([]map[string]any, error) {
		return []map[string]any{{"id": "kilo-auto/free", "isFree": true}}, nil
	}
	t.Cleanup(func() { fetchZenListing, fetchKiloListing = oldZen, oldKilo })

	l := NewLane()
	rosters := map[string]int{}
	l.OnChannelRoster = func(channel string, models []ModelInfo) { rosters[channel] = len(models) }

	l.SetChannelGate(func(id string) bool { return id != "zen" })
	l.RefreshCatalog(context.Background())
	catalog, _, _ := l.Snapshot()
	for _, m := range catalog {
		if m.Channel == "" {
			t.Fatalf("disabled zen channel still serves %s", m.ID)
		}
	}
	if rosters["zen"] != 0 {
		t.Fatalf("disabled channel published a roster of %d", rosters["zen"])
	}
	if rosters["kilo"] != 1 {
		t.Fatalf("kilo roster = %d, want the one fake row", rosters["kilo"])
	}

	// Re-enable: the same rebuild brings zen back.
	l.SetChannelGate(nil)
	l.RefreshCatalog(context.Background())
	catalog, _, _ = l.Snapshot()
	found := false
	for _, m := range catalog {
		if m.ID == "deepseek-v4.1-flash-free" {
			found = true
		}
	}
	if !found {
		t.Fatal("zen models did not come back after the channel was re-enabled")
	}
	if rosters["zen"] != 2 {
		t.Fatalf("zen roster after re-enable = %d, want 2", rosters["zen"])
	}
}
