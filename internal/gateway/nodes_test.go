package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zen-gate/internal/store"
)

func newNodesServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, _, _ := newBareServer(t)
	return s, httptest.NewServer(s.mux)
}

func seedNodesProvider(t *testing.T, s *Server) {
	t.Helper()
	s.Store.Mutate(func(c *store.Config) {
		c.Providers = []store.Provider{{
			ID: "seed", Name: "用户自己起的名字", BaseURL: "https://seed.test/v1",
			APIKey: "sk-user-key", Protocol: store.ProtocolOpenAI, Enabled: true,
			Models: []string{"seed-model"}, Note: "手填备注",
		}}
	})
	_ = s.Store.Save()
}

// The core promise: one paste creates many nodes, and re-importing a node we
// already have refreshes its declaration without ever touching user state.
func TestNodesImportAddsUpdatesAndKeepsUserState(t *testing.T) {
	s, ts := newNodesServer(t)
	defer ts.Close()
	seedNodesProvider(t, s)

	text := `Seed 新名字 | https://seed.test/v1 | openai | free | sk-should-not-overwrite
Fresh | https://fresh.test/v1 | openai | freemium | sk-fresh
Fresh Again | https://fresh.test/v1 | openai | paid`
	out := postAdmin(t, ts.URL+"/admin/api/nodes/import", mustJSONNodes(t, map[string]any{"text": text}))
	if out["ok"] != true {
		t.Fatalf("import failed: %+v", out)
	}
	if out["added"].(float64) != 1 || out["updated"].(float64) != 1 || out["skipped"].(float64) != 1 {
		t.Fatalf("counts = add:%v update:%v skip:%v, want 1/1/1", out["added"], out["updated"], out["skipped"])
	}
	cfg := s.Store.Config()
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(cfg.Providers))
	}
	seed := cfg.Providers[0]
	if seed.Name != "用户自己起的名字" || seed.APIKey != "sk-user-key" ||
		len(seed.Models) != 1 || seed.Models[0] != "seed-model" {
		t.Errorf("re-import overwrote user state: %+v", seed)
	}
	if seed.Billing != store.BillingFree {
		t.Errorf("declaration not refreshed: billing = %q", seed.Billing)
	}
	fresh := cfg.Providers[1]
	if fresh.Billing != store.BillingFreemium || fresh.APIKey != "sk-fresh" || fresh.ID == "seed" {
		t.Errorf("new node = %+v", fresh)
	}
}

// A dry run must be a real preview: identical answer, zero writes.
func TestNodesImportDryRunDoesNotWrite(t *testing.T) {
	s, ts := newNodesServer(t)
	defer ts.Close()
	body := mustJSONNodes(t, map[string]any{
		"text": "One | https://one.test/v1\nTwo | https://two.test/v1", "dryRun": true,
	})
	out := postAdmin(t, ts.URL+"/admin/api/nodes/import", body)
	if out["dryRun"] != true || out["added"].(float64) != 2 {
		t.Fatalf("preview = %+v", out)
	}
	if got := len(s.Store.Config().Providers); got != 0 {
		t.Errorf("dry run wrote %d providers", got)
	}
	body = mustJSONNodes(t, map[string]any{"text": "One | https://one.test/v1\nTwo | https://two.test/v1"})
	out = postAdmin(t, ts.URL+"/admin/api/nodes/import", body)
	if out["added"].(float64) != 2 || len(s.Store.Config().Providers) != 2 {
		t.Fatalf("real import = %+v", out)
	}
	// Two new nodes with different names must not collide on one id.
	if cfg := s.Store.Config(); cfg.Providers[0].ID == cfg.Providers[1].ID {
		t.Errorf("duplicate provider id: %s", cfg.Providers[0].ID)
	}
}

// The exported list can be handed to another machine. It carries declarations
// but no credential, in any field.
func TestNodesExportIsPortableAndSecretFree(t *testing.T) {
	s, ts := newNodesServer(t)
	defer ts.Close()
	seedNodesProvider(t, s)
	out := postAdmin(t, ts.URL+"/admin/api/nodes/import", mustJSONNodes(t, map[string]any{
		"text": `Seed | https://seed.test/v1 | openai | free
Another | https://another.test/v1 | anthropic | paid | sk-another`,
	}))
	if out["ok"] != true {
		t.Fatalf("import: %+v", out)
	}
	resp, err := http.Get(ts.URL + "/admin/api/nodes/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var exp struct {
		OK    bool             `json:"ok"`
		Count int              `json:"count"`
		Nodes []store.NodeSpec  `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&exp); err != nil {
		t.Fatal(err)
	}
	if !exp.OK || exp.Count != 2 {
		t.Fatalf("export = %+v", exp)
	}
	for _, n := range exp.Nodes {
		if len(n.Keys) != 0 {
			t.Errorf("export leaked keys for %s: %v", n.Name, n.Keys)
		}
	}
	// The exported JSON can be pasted straight back into the import box.
	raw, _ := json.Marshal(exp.Nodes)
	again := postAdmin(t, ts.URL+"/admin/api/nodes/import", mustJSONNodes(t, map[string]any{"text": string(raw)}))
	if again["ok"] != true || again["updated"].(float64) != 2 {
		t.Errorf("round-trip import = %+v", again)
	}
	if len(s.Store.Config().Providers) != 2 {
		t.Error("round trip must not duplicate providers")
	}
}

func TestNodesImportReportsBadLinesWithoutLosingBatch(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/nodes/import", mustJSONNodes(t, map[string]any{
		"text": "Good | https://good.test/v1\nftp://nope.test/v1\nAlsoGood | https://also.test/v1",
	}))
	if out["ok"] != true || out["added"].(float64) != 2 {
		t.Fatalf("import = %+v", out)
	}
	problems, ok := out["problems"].([]any)
	if !ok || len(problems) != 1 {
		t.Errorf("bad line not reported: %+v", out["problems"])
	}
}

func TestNodesImportRejectsEmptyBatch(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/nodes/import", mustJSONNodes(t, map[string]any{"text": "# nothing here"}))
	if out["ok"] != false {
		t.Fatalf("empty batch must be refused: %+v", out)
	}
}

func mustJSONNodes(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
