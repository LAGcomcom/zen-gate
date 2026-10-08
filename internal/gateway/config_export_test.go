package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// 导出配置 exists so the file can be mailed to a friend or pasted into an issue.
// It used to be config.json verbatim, which carries the gateway key, every
// provider key, every agent key — and every subscription URL, which is a
// standing claim on a paid account.
func TestExportHidesCredentialsByDefault(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	s := newTestServer(t, up)
	s.Store.Replace(store.Config{
		Port:      8787,
		MainKey:   "sk-main-secret",
		AgentKeys: map[string]string{"claude": "sk-claude-secret"},
		Providers: []store.Provider{{
			ID: "nim", Name: "N", BaseURL: up.URL + "/v1",
			APIKey: "sk-provider-secret", Enabled: true, Models: []string{"m1"},
		}},
		Subscriptions: []store.Subscription{{ID: "sub", URL: "https://paid.example/token-abc123"}},
	})

	body := get(t, s, "/admin/api/export")
	for _, secret := range []string{"sk-main-secret", "sk-claude-secret", "sk-provider-secret", "token-abc123"} {
		if strings.Contains(body, secret) {
			t.Errorf("the default export leaked %s:\n%s", secret, body)
		}
	}
	var out store.Config
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("export is not config-shaped: %v\n%s", err, body)
	}
	if out.MainKey != store.MaskedValue {
		t.Errorf("MainKey = %q, want the mask %q", out.MainKey, store.MaskedValue)
	}
	if len(out.Providers) != 1 || out.Providers[0].APIKey != store.MaskedValue {
		t.Errorf("provider key not masked: %+v", out.Providers)
	}
	if len(out.Subscriptions) != 1 || out.Subscriptions[0].URL != store.MaskedValue {
		t.Errorf("subscription url not masked: %+v", out.Subscriptions)
	}
	// Everything that is not a secret has to survive, or the export is useless
	// for the support request it exists for.
	if out.Providers[0].BaseURL != up.URL+"/v1" || len(out.Providers[0].Models) != 1 {
		t.Errorf("non-secret provider fields lost: %+v", out.Providers)
	}
}

// Migration is the other half of the feature: an explicit opt-in still exports
// the real credentials.
func TestExportWithSecretsKeepsCredentials(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	s := newTestServer(t, up)
	s.Store.Replace(store.Config{
		Port:      8787,
		MainKey:   "sk-main-secret",
		AgentKeys: map[string]string{"claude": "sk-claude-secret"},
		Providers: []store.Provider{{ID: "nim", BaseURL: up.URL + "/v1", APIKey: "sk-provider-secret"}},
	})
	body := get(t, s, "/admin/api/export?secrets=1")
	for _, secret := range []string{"sk-main-secret", "sk-claude-secret", "sk-provider-secret"} {
		if !strings.Contains(body, secret) {
			t.Errorf("the opt-in export dropped %s", secret)
		}
	}
}

// Restoring a masked export has to leave the machine working: the local gateway
// key survives, and the placeholders become "not configured" rather than a
// credential string that fails every request with a confusing 401.
func TestImportOfMaskedExportKeepsLocalKey(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	s := newTestServer(t, up)
	s.Store.Replace(store.Config{
		Port:      8787,
		MainKey:   "sk-local",
		AgentKeys: map[string]string{"claude": "sk-a"},
		Providers: []store.Provider{{ID: "nim", Name: "N", BaseURL: up.URL + "/v1", APIKey: "sk-p"}},
	})
	masked := get(t, s, "/admin/api/export")

	resp := post(t, s, "/admin/api/import", masked)
	if resp.status != 200 {
		t.Fatalf("importing a masked export = %d: %s", resp.status, resp.body)
	}
	cfg := s.Store.Config()
	if cfg.MainKey != "sk-local" {
		t.Errorf("the masked import replaced the gateway key with a placeholder: %q", cfg.MainKey)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %d, want the exported one", len(cfg.Providers))
	}
	if cfg.Providers[0].APIKey != "" {
		t.Errorf("provider key imported as %q, want it emptied", cfg.Providers[0].APIKey)
	}
	if v, ok := cfg.AgentKeys["claude"]; !ok || v != "" {
		t.Errorf("agent key imported as %q (present=%v), want it emptied", v, ok)
	}
	if cfg.Providers[0].Name != "N" {
		t.Errorf("provider name lost in the round trip: %+v", cfg.Providers[0])
	}
}

type httpResp struct {
	status int
	body   string
}

func get(t *testing.T, s *Server, path string) string {
	t.Helper()
	body, status := adminRoundTrip(t, s, http.MethodGet, path, "")
	if status != 200 {
		t.Fatalf("GET %s = %d: %s", path, status, body)
	}
	return body
}

func post(t *testing.T, s *Server, path, body string) httpResp {
	t.Helper()
	got, status := adminRoundTrip(t, s, http.MethodPost, path, body)
	return httpResp{status, got}
}

func adminRoundTrip(t *testing.T, s *Server, method, path, body string) (string, int) {
	t.Helper()
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		out.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return out.String(), resp.StatusCode
}
