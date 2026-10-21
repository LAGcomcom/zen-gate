package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// loginStub answers the grant endpoint the way the vendor does: 404 until the
// user has "finished", then a token. userinfo answers the display identity.
func loginStub(t *testing.T) *httptest.Server {
	t.Helper()
	var polls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/deviceToken/poll", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&polls, 1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"token":"dt-http","refresh_token":"drt-http","expires_in":2592000000}`))
	})
	mux.HandleFunc("/api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"uid-http","name":"第二账号"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The card's whole login loop, end to end: start returns a URL to open, the
// first poll says pending, the second imports the account into the pool.
func TestPoolDeviceLoginImportsAccount(t *testing.T) {
	s, pool := newPoolServer(t)
	s.logins.OpenAPI = loginStub(t).URL

	code, start := callJSON(t, s, http.MethodPost, "/pool/account", `{"action":"login-start"}`)
	if code != http.StatusOK || start["ok"] != true {
		t.Fatalf("login-start = %d %v", code, start)
	}
	state, _ := start["state"].(string)
	if state == "" {
		t.Fatalf("login-start returned no state: %v", start)
	}
	if authURL, _ := start["authUrl"].(string); !strings.HasPrefix(authURL, "https://qoder.com.cn/device/selectAccounts?") {
		t.Fatalf("authUrl = %q", authURL)
	}
	if _, ok := start["verifier"]; ok {
		t.Fatal("the PKCE verifier must never leave the process")
	}

	code, pending := callJSON(t, s, http.MethodPost, "/pool/account",
		`{"action":"login-poll","state":"`+state+`"}`)
	if code != http.StatusOK || pending["status"] != "pending" {
		t.Fatalf("first poll = %d %v", code, pending)
	}
	if _, ok := pending["account"]; ok {
		t.Fatal("a pending poll must not report an account")
	}
	if len(pool.Snapshot()) != 0 {
		t.Fatal("a pending poll must not touch the pool")
	}

	code, done := callJSON(t, s, http.MethodPost, "/pool/account",
		`{"action":"login-poll","state":"`+state+`"}`)
	if code != http.StatusOK || done["status"] != "ok" {
		t.Fatalf("second poll = %d %v", code, done)
	}
	acct, ok := done["account"].(map[string]any)
	if !ok {
		t.Fatalf("second poll reported no account: %v", done)
	}
	if acct["label"] != "第二账号" {
		t.Fatalf("label = %v, want the account name", acct["label"])
	}
	if acct["expiresAt"] == "" || acct["expiresAt"] == nil {
		t.Fatal("an authorized account must state when its token dies")
	}

	rows := pool.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("pool has %d accounts, want 1", len(rows))
	}
	if rows[0].UID != "uid-http" || rows[0].Label != "第二账号" {
		t.Fatalf("imported row = %+v", rows[0])
	}
	if rows[0].ExpiresAt.IsZero() {
		t.Fatal("imported row lost its expiry: the card would never warn")
	}

	// The status endpoint must show it, with the expiry the card renders.
	code, listed := callJSON(t, s, http.MethodGet, "/pool", "")
	if code != http.StatusOK {
		t.Fatalf("pool status = %d", code)
	}
	accounts, _ := listed["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("pool status has %d rows: %v", len(accounts), listed)
	}
	row := accounts[0].(map[string]any)
	if row["expiresAt"] == nil || row["expiresAt"] == "" {
		t.Fatalf("status row lost expiresAt: %v", row)
	}
}

func TestPoolDeviceLoginRejectsUnknownRealm(t *testing.T) {
	s, _ := newPoolServer(t)
	code, body := callJSON(t, s, http.MethodPost, "/pool/account", `{"action":"login-start","realm":"mars"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d %v", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "区域") {
		t.Fatalf("error should name the realm problem, got %q", msg)
	}
}

func TestPoolDeviceLoginCancelDropsWindow(t *testing.T) {
	s, _ := newPoolServer(t)
	s.logins.OpenAPI = loginStub(t).URL
	_, start := callJSON(t, s, http.MethodPost, "/pool/account", `{"action":"login-start"}`)
	state, _ := start["state"].(string)
	if code, body := callJSON(t, s, http.MethodPost, "/pool/account",
		`{"action":"login-cancel","state":"`+state+`"}`); code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, body)
	}
	_, after := callJSON(t, s, http.MethodPost, "/pool/account",
		`{"action":"login-poll","state":"`+state+`"}`)
	if after["status"] != "unknown" {
		t.Fatalf("poll after cancel = %v, want unknown", after["status"])
	}
}
