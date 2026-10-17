package gateway

import (
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// The CodeArts switch follows the WorkBuddy contract: it must persist, and it
// must never carry the masked credential copy into the stored account.
func TestSettingsPATCHPersistsCodeArtsSwitch(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	s.Store.Mutate(func(c *store.Config) {
		c.CodeArts = lane.CAAccount{
			SecurityToken:   "sts-token-of-record",
			SecretAccessKey: "secret-of-record",
			RefreshToken:    "refresh-of-record",
			CodeVerifier:    "verifier-of-record",
			DPoPKey:         map[string]string{"d": "dpop-of-record"},
			UserID:          "uid-1",
			Enabled:         true,
		}
	})

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL, `{"codeArtsEnabled":false}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}

	cfg := s.Store.Config()
	if cfg.CodeArts.Enabled {
		t.Errorf("switch did not persist: %+v", cfg.CodeArts)
	}
	if cfg.CodeArts.SecurityToken != "sts-token-of-record" ||
		cfg.CodeArts.DPoPKey["d"] != "dpop-of-record" || cfg.CodeArts.UserID != "uid-1" {
		t.Errorf("credentials clobbered by the settings write: %+v", cfg.CodeArts)
	}

	if code := patchSettings(t, ts.URL, `{"codeArtsEnabled":true}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}
	if !s.Store.Config().CodeArts.Enabled {
		t.Errorf("switch did not persist when turned back on")
	}
}
