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
// masked credential into the stored accounts.
//
// The switch is now a whole-pool flag (the channel serves when any account is
// enabled), so it has to reach every account, not a single one.
func TestSettingsPATCHPersistsWorkBuddySwitch(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	s.Store.Mutate(func(c *store.Config) {
		c.WorkBuddyAccounts = []lane.WBAccount{
			{ID: "a", AccessToken: "access-token-of-record", RefreshToken: "refresh-token-of-record",
				UID: "uid-1", Enabled: true},
			{ID: "b", AccessToken: "second-token", RefreshToken: "second-refresh",
				UID: "uid-2", Enabled: true},
		}
	})

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL, `{"workBuddyEnabled":false}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}

	cfg := s.Store.Config()
	if len(cfg.WorkBuddyAccounts) != 2 {
		t.Fatalf("pool = %d accounts, want 2", len(cfg.WorkBuddyAccounts))
	}
	for _, a := range cfg.WorkBuddyAccounts {
		if a.Enabled {
			t.Errorf("account %s kept Enabled=true: the switch did not reach the whole pool", a.ID)
		}
	}
	if cfg.WorkBuddyEnabled() {
		t.Error("the channel still reports itself enabled")
	}
	if cfg.WorkBuddyAccounts[0].AccessToken != "access-token-of-record" ||
		cfg.WorkBuddyAccounts[0].UID != "uid-1" ||
		cfg.WorkBuddyAccounts[1].AccessToken != "second-token" {
		t.Errorf("credentials were clobbered by the settings write: %+v", cfg.WorkBuddyAccounts)
	}

	// And back on again: the branch is not a one-way door.
	if code := patchSettings(t, ts.URL, `{"workBuddyEnabled":true}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}
	if !s.Store.Config().WorkBuddyEnabled() {
		t.Errorf("switch did not persist when turned back on")
	}
}
