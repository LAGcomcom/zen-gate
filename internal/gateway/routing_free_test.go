package gateway

import (
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func pickProviderIDs(picks []fallbackPick) []string {
	out := make([]string, 0, len(picks))
	for _, p := range picks {
		out = append(out, p.p.ID)
	}
	return out
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Billing is a tie-breaker, not a new capability rank. Turning it on must
// reorder candidates; turning it off must restore the exact catalog order.
func TestPreferFreeProvidersReordersFallbackCandidates(t *testing.T) {
	s, _, st := newBareServer(t)
	st.Mutate(func(c *store.Config) {
		c.Providers = []store.Provider{
			{ID: "paid1", Enabled: true, Models: []string{"shared"}, Billing: store.BillingPaid},
			{ID: "free1", Enabled: true, Models: []string{"shared"}, Billing: store.BillingFree},
			{ID: "unknown1", Enabled: true, Models: []string{"shared"}},
			{ID: "free2", Enabled: true, Models: []string{"shared"}, Billing: store.BillingFree},
		}
		c.SmartRouting = false
	})
	if ids := pickProviderIDs(s.providerFallbackPicks(lane.Needs{}, "")); !sameIDs(ids,
		[]string{"paid1", "free1", "unknown1", "free2"}) {
		t.Fatalf("default order = %v, want catalog order", ids)
	}

	st.Mutate(func(c *store.Config) { c.PreferFreeProviders = true })
	if ids := pickProviderIDs(s.providerFallbackPicks(lane.Needs{}, "")); !sameIDs(ids,
		[]string{"free1", "free2", "paid1", "unknown1"}) {
		t.Fatalf("free-first order = %v, want a stable free/paid partition", ids)
	}
}

// The setting has to survive the settings → state round trip, or the toggle
// would be a phantom that resets on the next boot.
func TestPreferFreeProvidersSettingPersists(t *testing.T) {
	s, _, _ := newBareServer(t)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/settings", `{"preferFreeProviders":true}`)
	if out["ok"] == false {
		t.Fatalf("settings save failed: %+v", out)
	}
	if !s.Store.Config().PreferFreeProviders {
		t.Fatal("config did not take the new setting")
	}
	_, state := getAdminJSON(t, ts.URL+"/admin/api/state")
	settings, _ := state["settings"].(map[string]any)
	if settings["preferFreeProviders"] != true {
		t.Errorf("state settings = %+v, want preferFreeProviders=true", settings)
	}
}
