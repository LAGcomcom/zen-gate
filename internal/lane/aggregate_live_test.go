package lane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// LiveCatalogAggregatesRealChannelFiles groups the catalog that this machine's
// channel files actually describe, and prints what it found. It is a diagnostic
// (not an assertion of specific production rows): the point is to see the
// aggregation applied to real data with `go test -v`, and to fail loudly if the
// grouping ever collapses a pair whose windows are far apart.
//
// Skipped when the data dir is absent, so CI and other machines stay green.
func TestLiveCatalogAggregatesRealChannelFiles(t *testing.T) {
	home := os.Getenv("ZEN_GATE_HOME")
	if home == "" {
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			t.Skip("no APPDATA")
		}
		home = filepath.Join(appdata, "zen-gate")
	}
	dir := filepath.Join(home, "channels")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("no channel dir:", err)
	}

	var catalog []ModelInfo
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var file struct {
			Models []ModelInfo `json:"models"`
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		kind := trimExt(e.Name())
		for _, m := range file.Models {
			if m.Channel == "" {
				m.Channel = kind
			}
			catalog = append(catalog, m)
		}
	}
	if len(catalog) == 0 {
		t.Skip("no models in channel files")
	}

	groups := AggregateGroups(catalog)
	// Distinct groups (an id appears in every group it belongs to).
	seen := map[*AggregateGroup]bool{}
	var uniq []*AggregateGroup
	for _, g := range groups {
		if !seen[g] {
			seen[g] = true
			uniq = append(uniq, g)
		}
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Key < uniq[j].Key })

	fmt.Printf("catalog=%d rows, aggregate groups=%d\n", len(catalog), len(uniq))
	for _, g := range uniq {
		fmt.Printf("  %-30s members=%v windows=", g.Key, g.Members)
		for _, m := range g.Members {
			fmt.Printf("%s:%d ", m, g.Windows[m])
		}
		fmt.Println()
		// The invariant: members of one group must be window-compatible.
		for i := 1; i < len(g.Members); i++ {
			a := g.Windows[g.Members[0]]
			b := g.Windows[g.Members[i]]
			if !aggregateCompatible(a, b) {
				t.Fatalf("group %q merged incompatible windows %d and %d", g.Key, a, b)
			}
		}
	}
}

func trimExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}
