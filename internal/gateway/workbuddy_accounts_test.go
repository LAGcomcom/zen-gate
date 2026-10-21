package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// wbPoolServer builds a test server whose lane already holds a two-account
// pool, matching what main wires at boot.
func wbPoolServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	up := fakeUpstream(t, []string{"[DONE]"})
	t.Cleanup(up.Close)
	s := newTestServer(t, up)
	accts := []lane.WBAccount{
		{ID: "a", AccessToken: "at-a", RefreshToken: "rt-a", UID: "u-a", Nickname: "甲",
			Enabled: true, Realm: lane.WBRealmCN},
		{ID: "b", AccessToken: "at-b", RefreshToken: "rt-b", UID: "u-b", Nickname: "乙",
			Enabled: true, Realm: lane.WBRealmIntl},
	}
	s.Store.Mutate(func(c *store.Config) { c.WorkBuddyAccounts = accts })
	s.Lane.SetWorkBuddyAccounts(accts)
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func wbGetJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("GET %s: body %q: %v", url, body, err)
	}
	return out
}

func wbPostJSON(t *testing.T, url, payload string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out
}

// The card lists one row per account, and the legacy top-level block still
// describes the serving account so an older card keeps rendering.
func TestWorkBuddyStatusListsAccounts(t *testing.T) {
	_, ts := wbPoolServer(t)
	st := wbGetJSON(t, ts.URL+"/admin/api/workbuddy/status")

	rows, _ := st["accounts"].([]any)
	if len(rows) != 2 {
		t.Fatalf("accounts = %d rows, want 2", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	if first["id"] != "a" || first["nickname"] != "甲" {
		t.Errorf("first row = %v", first)
	}
	if first["state"] != "ok" {
		t.Errorf("state = %v, want ok", first["state"])
	}
	// Legacy fields: present, and describing the first account.
	if st["loggedIn"] != true {
		t.Errorf("loggedIn = %v", st["loggedIn"])
	}
	if st["uid"] != "u-a" {
		t.Errorf("legacy uid = %v, want the serving account", st["uid"])
	}
	if st["activeId"] != "a" {
		t.Errorf("activeId = %v, want a", st["activeId"])
	}
}

// Logging out one account must leave the others serving — that is the whole
// difference between "exit this account" and "exit the channel".
func TestWorkBuddyLogoutById(t *testing.T) {
	s, ts := wbPoolServer(t)
	code, out := wbPostJSON(t, ts.URL+"/admin/api/workbuddy/logout", `{"id":"b"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("logout = %d %v", code, out)
	}
	accts := s.Store.Config().WorkBuddyAccounts
	if len(accts) != 1 || accts[0].ID != "a" {
		t.Fatalf("pool after logout = %+v, want only a", accts)
	}
	// The lane copy follows the store.
	if got := s.Lane.WorkBuddyAccounts(); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("lane pool = %+v", got)
	}
}

// An empty body is what the pre-pool card sent: it means "exit the channel".
func TestWorkBuddyLogoutWithoutIdClearsPool(t *testing.T) {
	s, ts := wbPoolServer(t)
	if code, _ := wbPostJSON(t, ts.URL+"/admin/api/workbuddy/logout", `{}`); code != 200 {
		t.Fatalf("logout = %d", code)
	}
	if got := s.Store.Config().WorkBuddyAccounts; len(got) != 0 {
		t.Fatalf("pool = %+v, want empty", got)
	}
	if got := s.Lane.WorkBuddyAccounts(); len(got) != 0 {
		t.Fatalf("lane pool = %+v, want empty", got)
	}
}

func TestWorkBuddyAccountToggle(t *testing.T) {
	s, ts := wbPoolServer(t)
	code, _ := wbPostJSON(t, ts.URL+"/admin/api/workbuddy/account/toggle", `{"id":"b","enabled":false}`)
	if code != 200 {
		t.Fatalf("toggle = %d", code)
	}
	for _, a := range s.Store.Config().WorkBuddyAccounts {
		if a.ID == "b" && a.Enabled {
			t.Fatal("b stayed enabled")
		}
		if a.ID == "a" && !a.Enabled {
			t.Fatal("toggling b disabled a")
		}
	}
	// A missing id is a client error, not a silent no-op.
	if code, _ := wbPostJSON(t, ts.URL+"/admin/api/workbuddy/account/toggle", `{"enabled":true}`); code != 400 {
		t.Fatalf("toggle without id = %d, want 400", code)
	}
}

func TestWorkBuddyPrimaryMovesFirst(t *testing.T) {
	s, ts := wbPoolServer(t)
	if code, _ := wbPostJSON(t, ts.URL+"/admin/api/workbuddy/account/primary", `{"id":"b"}`); code != 200 {
		t.Fatalf("primary = %d", code)
	}
	accts := s.Store.Config().WorkBuddyAccounts
	if len(accts) != 2 || accts[0].ID != "b" {
		t.Fatalf("order = %+v, want b first", accts)
	}
	if got := s.Lane.WorkBuddyActiveID(); got != "b" {
		t.Fatalf("active = %q, want b", got)
	}
}
