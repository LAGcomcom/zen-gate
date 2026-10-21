package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zen-gate/internal/qoderhub/remote"
	"zen-gate/internal/qoderhub/service"
)

// newPoolServer builds a server whose service owns a real, empty account pool.
// The IDE cache is pointed at a directory that does not exist, so the live
// login contributes nothing and each test sees only what it imports.
func newPoolServer(t *testing.T) (*Server, *remote.Pool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("QODERCN_CACHE_DIR", filepath.Join(dir, "no-ide-cache"))
	pool := remote.NewPool(filepath.Join(dir, "accounts"))
	if err := pool.Load(); err != nil {
		t.Fatalf("load pool: %v", err)
	}
	svc := service.New(service.Config{RemotePool: pool})
	return NewServer("127.0.0.1:0", svc), pool
}

func callJSON(t *testing.T, s *Server, method, target, body string) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// An empty pool is a normal state, not an error: the card must be able to say
// "nothing imported yet" and show the import button.
func TestPoolStatusOnEmptyPool(t *testing.T) {
	s, _ := newPoolServer(t)
	code, body := callJSON(t, s, http.MethodGet, "/pool", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", code, body)
	}
	if body["enabled"] != true {
		t.Errorf("enabled = %v, want true when the pool is wired", body["enabled"])
	}
	if accounts, _ := body["accounts"].([]any); len(accounts) != 0 {
		t.Errorf("accounts = %v, want an empty list", body["accounts"])
	}
	if body["dir"] == "" {
		t.Error("the card cannot explain where the pool lives without dir")
	}
}

// Import snapshots the IDE login. With no IDE login present the endpoint must
// explain that, not fail with a bare 500.
func TestPoolImportWithoutIDELoginExplainsItself(t *testing.T) {
	s, _ := newPoolServer(t)
	code, body := callJSON(t, s, http.MethodPost, "/pool/account", `{"action":"import"}`)
	if code == http.StatusOK {
		t.Fatalf("import succeeded with no IDE login: %v", body)
	}
	msg, _ := body["error"].(map[string]any)
	if msg == nil {
		t.Fatalf("no error envelope: %v", body)
	}
	if text, _ := msg["message"].(string); !strings.Contains(text, "登录") {
		t.Errorf("error does not say the IDE login is missing: %q", text)
	}
}

// Actions on a missing id must be refused before they touch the pool, and the
// refusal must name what was missing.
func TestPoolAccountActionsValidateInput(t *testing.T) {
	s, _ := newPoolServer(t)
	cases := []struct{ name, body, want string }{
		{"toggle without id", `{"action":"toggle","enabled":false}`, "id"},
		{"toggle without enabled", `{"action":"toggle","id":"acc-1"}`, "enabled"},
		{"primary without id", `{"action":"primary"}`, "id"},
		{"delete without id", `{"action":"delete"}`, "id"},
		{"unknown action", `{"action":"explode"}`, "未知操作"},
		{"malformed body", `{`, "JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := callJSON(t, s, http.MethodPost, "/pool/account", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%v)", code, body)
			}
			errObj, _ := body["error"].(map[string]any)
			text, _ := errObj["message"].(string)
			if !strings.Contains(text, tc.want) {
				t.Errorf("error %q does not mention %q", text, tc.want)
			}
		})
	}
}

// The roster must never carry credential material: this response is rendered in
// a page and pasted into bug reports.
func TestPoolStatusCarriesNoSecrets(t *testing.T) {
	s, pool := newPoolServer(t)
	acct, err := pool.Import(remote.Credential{
		CosyKey:         "cosy-secret-value",
		EncryptUserInfo: "enc-secret-value",
		UserID:          "uid-123456",
		MachineID:       "machine-secret-value",
	}, "work")
	if err != nil {
		t.Fatal(err)
	}
	code, body := callJSON(t, s, http.MethodGet, "/pool", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	raw, _ := json.Marshal(body)
	for _, secret := range []string{"cosy-secret-value", "enc-secret-value", "machine-secret-value"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("/pool leaks %q:\n%s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), acct.ID) {
		t.Errorf("/pool lost the account id:\n%s", raw)
	}
}

// A server whose service has no pool must answer the endpoint rather than
// panic: the card still polls it on a single-login install.
func TestPoolEndpointWithoutPool(t *testing.T) {
	svc := service.New(service.Config{})
	s := NewServer("127.0.0.1:0", svc)
	code, body := callJSON(t, s, http.MethodGet, "/pool", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false without a pool", body["enabled"])
	}
	if code, _ := callJSON(t, s, http.MethodPost, "/pool/account", `{"action":"import"}`); code != http.StatusBadRequest {
		t.Errorf("import on a pool-less server = %d, want 400", code)
	}
}
