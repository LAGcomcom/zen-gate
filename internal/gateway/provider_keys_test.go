package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Issue #104 end-to-end through the admin API: the save endpoint keeps the
// store invariant (one key in apiKey, two or more in apiKeys), an absent
// apiKeys field leaves the stored keys alone, and the state view masks the
// first key while reporting the pool size.
func TestProviderSaveNormalizesKeyCount(t *testing.T) {
	up := declaredListing()
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	j := postAdmin(t, ts.URL+"/admin/api/providers", `{"name":"NIM","baseUrl":"`+up.URL+`/v1",
		"apiKey":"k1","apiKeys":["k2","k1","  "],"protocol":"openai","enabled":true,"models":["m1"]}`)
	if !j["ok"].(bool) {
		t.Fatalf("create failed: %v", j)
	}
	p := s.Store.Config().Providers[0]
	if p.APIKey != "" || len(p.APIKeys) != 2 || p.APIKeys[0] != "k2" || p.APIKeys[1] != "k1" {
		t.Fatalf("multi-key save stored %+v / %+v, want apiKey empty + apiKeys [k2 k1]", p.APIKey, p.APIKeys)
	}

	// Editing without the apiKeys field must not touch the stored pool — the
	// form is write-only and never round-trips keys.
	j = postAdmin(t, ts.URL+"/admin/api/providers", `{"id":"`+p.ID+`","name":"NIM","baseUrl":"`+up.URL+`/v1",
		"protocol":"openai","enabled":true,"models":["m1"]}`)
	if !j["ok"].(bool) {
		t.Fatalf("update failed: %v", j)
	}
	p = s.Store.Config().Providers[0]
	if len(p.APIKeys) != 2 {
		t.Fatalf("update without apiKeys clobbered the pool: %+v", p.APIKeys)
	}

	// A single key folds back into the legacy field.
	j = postAdmin(t, ts.URL+"/admin/api/providers", `{"id":"`+p.ID+`","name":"NIM","baseUrl":"`+up.URL+`/v1",
		"apiKeys":["only-key"],"protocol":"openai","enabled":true,"models":["m1"]}`)
	if !j["ok"].(bool) {
		t.Fatalf("single-key update failed: %v", j)
	}
	p = s.Store.Config().Providers[0]
	if p.APIKey != "only-key" || len(p.APIKeys) != 0 {
		t.Fatalf("single-key save stored %q / %v, want legacy field only", p.APIKey, p.APIKeys)
	}
}

// The dashboard's single-key box posts apiKey only — apiKeys is filled only
// when the user pastes several keys into the multi-key textarea. Deciding
// "unchanged" from apiKeys alone therefore dropped every single-key save: the
// provider was created with no credential, and the card read 「Key 未设置」for a
// key the user had just typed. Both create and update must store it.
func TestProviderSaveStoresSingleAPIKeyField(t *testing.T) {
	up := declaredListing()
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// Create with apiKey only (no apiKeys field at all).
	j := postAdmin(t, ts.URL+"/admin/api/providers", `{"name":"NIM","baseUrl":"`+up.URL+`/v1",
		"apiKey":"sk-single-1","protocol":"openai","enabled":true,"models":["m1"]}`)
	if !j["ok"].(bool) {
		t.Fatalf("create failed: %v", j)
	}
	p := s.Store.Config().Providers[0]
	if p.APIKey != "sk-single-1" || len(p.APIKeys) != 0 {
		t.Fatalf("create stored %q / %v, want the single key in apiKey", p.APIKey, p.APIKeys)
	}

	// Update the key through the same field.
	j = postAdmin(t, ts.URL+"/admin/api/providers", `{"id":"`+p.ID+`","name":"NIM","baseUrl":"`+up.URL+`/v1",
		"apiKey":"sk-single-2","protocol":"openai","enabled":true,"models":["m1"]}`)
	if !j["ok"].(bool) {
		t.Fatalf("update failed: %v", j)
	}
	p = s.Store.Config().Providers[0]
	if p.APIKey != "sk-single-2" {
		t.Fatalf("update stored %q, want the new single key", p.APIKey)
	}

	// An edit that leaves both key fields blank keeps the stored key — this is
	// the write-only contract the form depends on.
	j = postAdmin(t, ts.URL+"/admin/api/providers", `{"id":"`+p.ID+`","name":"NIM2","baseUrl":"`+up.URL+`/v1",
		"protocol":"openai","enabled":true,"models":["m1"]}`)
	if !j["ok"].(bool) {
		t.Fatalf("blank-key update failed: %v", j)
	}
	p = s.Store.Config().Providers[0]
	if p.APIKey != "sk-single-2" {
		t.Fatalf("a blank key box cleared the stored key: %q", p.APIKey)
	}
	if p.Name != "NIM2" {
		t.Fatalf("the same update failed to change the name: %q", p.Name)
	}
}

func TestProviderStateMasksBalancedKeys(t *testing.T) {	up := declaredListing()
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	postAdmin(t, ts.URL+"/admin/api/providers", `{"name":"NIM","baseUrl":"`+up.URL+`/v1",
		"apiKeys":["sk-alpha-1234567890","sk-beta-1234567890"],"protocol":"openai","enabled":true,"models":["m1"]}`)

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, secret := range []string{"sk-alpha", "sk-beta"} {
		if strings.Contains(text, secret) {
			t.Fatal("/admin/api/state leaked a balanced key")
		}
	}
	if !strings.Contains(text, `"keyCount":2`) {
		t.Fatal("/admin/api/state did not report the key pool size")
	}
}
