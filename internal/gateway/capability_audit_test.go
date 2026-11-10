package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func newAuditServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, ln, _ := newBareServer(t)
	ln.SetCatalog(lane.BuildCatalog([]string{"space-bunny-free"}))
	ts := httptest.NewServer(s.mux)
	return s, ts
}

func getAudit(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := http.Get(ts.URL + "/admin/api/capability/audit")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The desk must work offline: the diff comes from the last successful catalog
// snapshot, not from whatever the network happens to answer when the page
// opens. It must also show both catalogs' opinions separately.
func TestCapabilityAuditReturnsOfflineDiff(t *testing.T) {
	s, ts := newAuditServer(t)
	defer ts.Close()
	if err := s.Store.SetCapabilityRefs(store.CapOriginModelsDev, map[string]store.RefCap{
		"space-bunny-free": {ContextWindow: 1048576, MaxOutput: 524288},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetCapabilityRefs(store.CapOriginOpenRouter, map[string]store.RefCap{
		"space-bunny": {ContextWindow: 1000000, MaxOutput: 128000},
	}); err != nil {
		t.Fatal(err)
	}
	out := getAudit(t, ts)
	diff, ok := out["diff"].([]any)
	if !ok {
		t.Fatalf("diff missing: %+v", out)
	}
	if len(diff) != 2 {
		t.Fatalf("diff rows = %d, want one per catalog: %+v", len(diff), diff)
	}
	sources := map[string]bool{}
	for _, raw := range diff {
		row := raw.(map[string]any)
		sources[row["source"].(string)] = true
		if row["model"].(string) != "space-bunny-free" {
			t.Errorf("row model = %v", row["model"])
		}
		if row["locked"].(bool) {
			t.Error("nothing is locked yet")
		}
	}
	if !sources[store.CapOriginModelsDev] || !sources[store.CapOriginOpenRouter] {
		t.Errorf("both catalogs must appear: %+v", sources)
	}
}

func TestCapabilityLockEndpointPersistsVeto(t *testing.T) {
	s, ts := newAuditServer(t)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/capability/lock",
		`{"model":"space-bunny-free","locked":true}`)
	if out["ok"] != true {
		t.Fatalf("lock failed: %+v", out)
	}
	if !s.Store.CapabilityLocks()["space-bunny-free"] {
		t.Fatal("veto not stored")
	}
	out = postAdmin(t, ts.URL+"/admin/api/capability/lock",
		`{"model":"space-bunny-free","locked":false}`)
	if out["ok"] != true {
		t.Fatalf("unlock failed: %+v", out)
	}
	if s.Store.CapabilityLocks()["space-bunny-free"] {
		t.Fatal("veto not cleared")
	}
}

// Rolling back restores the previous number AND locks the model. Without the
// lock the next 6-hourly pull would simply put the catalogue's value back,
// which makes a rollback feel broken.
func TestCapabilityRollbackRestoresBeforeAndLocks(t *testing.T) {
	s, ts := newAuditServer(t)
	defer ts.Close()
	st := s.Store
	st.SetModelTagTracked("space-bunny-free", store.ModelTag{
		CapsOnly: true, ContextWindow: 1048576, MaxOutput: 65536, Source: store.TagSourceListing,
	}, store.CapOriginModelsDev)
	st.SetModelTagTracked("space-bunny-free", store.ModelTag{
		CapsOnly: true, ContextWindow: 1048576, MaxOutput: 524288, Source: store.TagSourceListing,
	}, store.CapOriginModelsDev)
	// Use the typed accessor rather than the JSON shape.
	latest := st.CapabilityAudit()[0]
	out := postAdmin(t, ts.URL+"/admin/api/capability/rollback",
		`{"seq":`+strconv.FormatInt(latest.Seq, 10)+`}`)
	if out["ok"] != true {
		t.Fatalf("rollback failed: %+v", out)
	}
	tag, ok := st.ModelTagOf("space-bunny-free")
	if !ok || tag.MaxOutput != 65536 {
		t.Fatalf("verdict after rollback = %+v (ok=%v), want 65536", tag, ok)
	}
	if !st.CapabilityLocks()["space-bunny-free"] {
		t.Error("rollback must lock the model against the next pull")
	}
	if got := len(st.CapabilityAudit()); got != 3 {
		t.Errorf("audit entries = %d, want 3 (two pulls + the rollback)", got)
	}
}

// When a change had no Before row, rolling back must delete the verdict so the
// curated table answers again — writing a zeroed row would publish a model
// with no capacity at all.
func TestCapabilityRollbackToNoStoredRow(t *testing.T) {
	s, ts := newAuditServer(t)
	defer ts.Close()
	st := s.Store
	st.SetModelTagTracked("space-bunny-free", store.ModelTag{
		CapsOnly: true, ContextWindow: 1048576, MaxOutput: 524288, Source: store.TagSourceListing,
	}, store.CapOriginModelsDev)
	latest := st.CapabilityAudit()[0]
	out := postAdmin(t, ts.URL+"/admin/api/capability/rollback",
		`{"seq":`+strconv.FormatInt(latest.Seq, 10)+`}`)
	if out["ok"] != true {
		t.Fatalf("rollback failed: %+v", out)
	}
	if _, ok := st.ModelTagOf("space-bunny-free"); ok {
		t.Error("verdict must be deleted, not zeroed")
	}
	if !st.CapabilityLocks()["space-bunny-free"] {
		t.Error("rollback must lock the model")
	}
	entries := st.CapabilityAudit()
	if !entries[0].Removed {
		t.Error("the rollback entry must be marked as a removal for the UI")
	}
}

// A hand-entered number is a decision: it takes the top trust rung and is
// shielded from the automatic pull without the user having to remember a
// second toggle.
func TestCapabilityCalibrateWritesManualVerdict(t *testing.T) {
	s, ts := newAuditServer(t)
	defer ts.Close()
	st := s.Store
	out := postAdmin(t, ts.URL+"/admin/api/capability/calibrate",
		`{"model":"space-bunny-free","contextWindow":300000,"maxOutput":65536}`)
	if out["ok"] != true {
		t.Fatalf("calibrate failed: %+v", out)
	}
	tag, ok := st.ModelTagOf("space-bunny-free")
	if !ok || tag.ContextWindow != 300000 || tag.MaxOutput != 65536 {
		t.Fatalf("verdict = %+v (ok=%v)", tag, ok)
	}
	if tag.Source != store.TagSourceManual {
		t.Errorf("Source = %q, want 人工校准", tag.Source)
	}
	if !st.CapabilityLocks()["space-bunny-free"] {
		t.Error("manual calibration must be shielded from the next pull")
	}
	if got := postAdmin(t, ts.URL+"/admin/api/capability/calibrate",
		`{"model":"","contextWindow":1,"maxOutput":1}`); got["ok"] == true {
		t.Error("an empty model id must be refused")
	}
}

