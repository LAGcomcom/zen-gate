package gateway

import (
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// The dashboard's WorkBuddy switch reported {"changed":true} but the field had
// no branch in commitSettings, so the next boot read the old value back and the
// channel came on again. The switch must persist — and must not carry the
// masked credential into the stored account.
func TestSettingsPATCHPersistsWorkBuddySwitch(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	s.Store.Mutate(func(c *store.Config) {
		c.WorkBuddy = lane.WBAccount{
			AccessToken:  "access-token-of-record",
			RefreshToken: "refresh-token-of-record",
			UID:          "uid-1",
			Enabled:      true,
		}
	})

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL, `{"workBuddyEnabled":false}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}

	cfg := s.Store.Config()
	if cfg.WorkBuddy.Enabled {
		t.Errorf("switch did not persist: %+v", cfg.WorkBuddy)
	}
	if cfg.WorkBuddy.AccessToken != "access-token-of-record" || cfg.WorkBuddy.UID != "uid-1" {
		t.Errorf("credential was clobbered by the settings write: %+v", cfg.WorkBuddy)
	}

	// And back on again: the branch is not a one-way door.
	if code := patchSettings(t, ts.URL, `{"workBuddyEnabled":true}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}
	if !s.Store.Config().WorkBuddy.Enabled {
		t.Errorf("switch did not persist when turned back on")
	}
}
