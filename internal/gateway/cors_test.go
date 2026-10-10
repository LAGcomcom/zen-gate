package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issue #34: a browser page on another origin reads /v1/* cross-origin and
// fails with "Failed to fetch" unless the reply carries CORS. The API key
// stays the gate — this only makes the answer readable, never the call free.
func TestCORSOnV1API(t *testing.T) {
	up := fakeUpstream(t, []string{`{"data":[{"id":"m1"}]}`})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// A credentialed GET carries the origin back and exposes the gateway's
	// own response markers so a web client can read which model answered.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO = %q, want the echoed origin", got)
	}
	if !headerHas(resp.Header, "Vary", "Origin") {
		t.Errorf("Vary must list Origin: %q", resp.Header.Get("Vary"))
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got == "" {
		t.Error("exposed headers must include the served-by / request-id markers")
	}

	// No key: the browser must still be able to READ the 401 (CORS headers
	// are stamped before auth), so a misconfigured page gets a clear error
	// instead of an opaque "Failed to fetch".
	req, _ = http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Origin", "https://example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("401 must still be CORS-readable, ACAO = %q", got)
	}
}

// The preflight is what actually broke the formatter: an OPTIONS with no
// Authorization, previously answered 404 with no CORS, so the browser aborted
// before the real GET ever fired. It must now short-circuit to 204 and echo
// the requested headers.
func TestCORSPreflight(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("OPTIONS", ts.URL+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, x-api-key")
	// No Authorization header: a preflight must never require the key.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("preflight ACAO = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("preflight must list allowed methods")
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "authorization, content-type, x-api-key" {
		t.Errorf("preflight must echo the requested headers, got %q", got)
	}
	if resp.Header.Get("Access-Control-Max-Age") == "" {
		t.Error("preflight should cache with a max-age")
	}
}

// The management surface is the whole reason /v1 CORS is safe to open: /admin
// must stay unreadable cross-origin, or a random web page could probe the
// user's providers and keys.
func TestCORSNotOnAdmin(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("/admin must not carry CORS (would expose the management surface), got %q", got)
	}
}

func headerHas(h http.Header, key, want string) bool {
	for _, v := range h.Values(key) {
		if v == want {
			return true
		}
	}
	return false
}
