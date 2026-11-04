package gateway

import (
	"net/http/httptest"
	"testing"

	"zen-gate/internal/store"
)

// A wrong "free" label is how someone gets a surprise bill. Every tier comes
// from an explicit declaration; anything else is unknown.
func TestBillingForModelNeverGuesses(t *testing.T) {
	cfg := &store.Config{Providers: []store.Provider{
		{ID: "freepool", Billing: store.BillingFree},
		{ID: "mixed", Billing: "部分免费"},
		{ID: "paidapi", Billing: store.BillingPaid},
		{ID: "unknownapi"},
		{ID: "broken", Billing: "maybe-free?"},
	}}
	cases := []struct {
		id   string
		free bool
		want string
	}{
		{"glm-4.5-free", true, store.BillingFree},
		{"freepool/m1", false, store.BillingFree},
		{"mixed/m1", false, store.BillingFreemium},
		{"paidapi/m1", false, store.BillingPaid},
		{"unknownapi/m1", false, store.BillingUnknown},
		{"broken/m1", false, store.BillingUnknown},
		{"ghost/m1", false, store.BillingUnknown},
		{"bare-id", false, store.BillingUnknown},
	}
	for _, c := range cases {
		if got := billingForModel(c.id, c.free, cfg); got != c.want {
			t.Errorf("billingForModel(%q, %v) = %q, want %q", c.id, c.free, got, c.want)
		}
	}
	if laneBilling("glm-4.5-free") != store.BillingFree {
		t.Error("a -free lane id must read as free")
	}
	if laneBilling("my-private-model") != store.BillingUnknown {
		t.Error("an unrecognised lane id must stay unknown, not be labelled paid or free")
	}
}

// /v1/models already publishes capability metadata; billing rides the same
// shape so a client can pick a tier without knowing our provider table.
func TestListModelsPublishesBilling(t *testing.T) {
	s, _, st := newBareServer(t)
	st.Mutate(func(c *store.Config) {
		c.Providers = []store.Provider{{
			ID: "paidapi", Name: "Paid API", BaseURL: "https://paid.test/v1",
			Protocol: store.ProtocolOpenAI, Enabled: true,
			Models: []string{"m1"}, Billing: store.BillingPaid,
		}}
	})
	byID := listModels(t, s)
	e := byID["paidapi/m1"]
	if e == nil {
		t.Fatalf("provider model missing from /v1/models: %+v", byID)
	}
	if e["billing"] != store.BillingPaid {
		t.Errorf("billing = %v, want paid", e["billing"])
	}
}

// The admin state feeds the 模型 page filter; it has to carry the same verdict
// as /v1/models or the two views disagree about which models are free.
func TestAdminStateExposesBilling(t *testing.T) {
	s, _, st := newBareServer(t)
	st.Mutate(func(c *store.Config) {
		c.Providers = []store.Provider{{
			ID: "mixed", Name: "Mixed API", BaseURL: "https://mixed.test/v1",
			Protocol: store.ProtocolOpenAI, Enabled: true,
			Models: []string{"m1"}, Billing: store.BillingFreemium,
		}}
	})
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	code, j := getAdminJSON(t, ts.URL+"/admin/api/state")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	rows, _ := j["models"].([]any)
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["id"] != "mixed/m1" {
			continue
		}
		found = true
		if row["billing"] != store.BillingFreemium {
			t.Errorf("billing = %v, want freemium", row["billing"])
		}
	}
	if !found {
		t.Fatal("custom provider model missing from admin state")
	}
}
