package gateway

import (
	"net/http/httptest"
	"testing"
)

// The WorkBuddy prefix-cache switch answered {"changed":true} and never
// reached disk: commitSettings copied exactly the fields it knew about, and
// workBuddyCacheKey was not one of them. A live panel run caught it — the
// button flipped to 已开启, config.json stayed empty, and a restart put the
// switch back to off. This test fails the moment that branch goes missing
// again.
func TestSettingsPATCHPersistsWorkBuddyCacheKey(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)

	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	if s.Store.Config().WorkBuddyCacheKey {
		t.Fatal("the switch starts on: the default is supposed to be off until a live A/B proves the field is accepted")
	}

	if code := patchSettings(t, ts.URL, `{"workBuddyCacheKey":true}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}
	if !s.Store.Config().WorkBuddyCacheKey {
		t.Error("workBuddyCacheKey did not persist: the panel would say 已开启 and the next boot would read false back")
	}

	// A second write of an unrelated field must not silently revert it — that
	// is what the dirty-map discipline exists to prevent.
	if code := patchSettings(t, ts.URL, `{"autoTagEnabled":false}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}
	if !s.Store.Config().WorkBuddyCacheKey {
		t.Error("an unrelated settings write reverted workBuddyCacheKey")
	}

	// And back off again: the branch is not a one-way door.
	if code := patchSettings(t, ts.URL, `{"workBuddyCacheKey":false}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}
	if s.Store.Config().WorkBuddyCacheKey {
		t.Error("workBuddyCacheKey did not persist when turned back off")
	}
}
