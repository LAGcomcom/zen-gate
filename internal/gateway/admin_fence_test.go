package gateway

import (
	"net/http/httptest"
	"testing"
)

// The dashboard fence used substring matching for Origin/Referer and skipped
// the Host check when no port was present. Both let a hostile page (or DNS
// rebinding) reach mutating admin endpoints from the user's browser.
func TestAdminAllowedRejectsLookalikeOriginsAndBareHosts(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		origin  string
		referer string
		want    bool
	}{
		{name: "loopback origin", host: "127.0.0.1:8787", origin: "http://127.0.0.1:8787", want: true},
		{name: "localhost origin", host: "localhost:8787", origin: "http://localhost:8787", want: true},
		{name: "ipv6 referer", host: "[::1]:8787", referer: "http://[::1]:8787/admin", want: true},
		{name: "bare loopback host", host: "127.0.0.1", want: true},
		{name: "lookalike origin", host: "127.0.0.1:8787", origin: "http://127.0.0.1.attacker.com", want: false},
		{name: "lookalike localhost", host: "127.0.0.1:8787", origin: "http://localhost.attacker.com", want: false},
		{name: "bare foreign host", host: "evil.example", want: false},
		{name: "foreign origin", host: "127.0.0.1:8787", origin: "https://evil.example", want: false},
		{name: "opaque origin", host: "127.0.0.1:8787", origin: "null", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/admin/api/settings", nil)
			r.Host = tc.host
			r.RemoteAddr = "127.0.0.1:54321"
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			rec := httptest.NewRecorder()
			if got := adminAllowed(rec, r, true); got != tc.want {
				t.Fatalf("adminAllowed() = %v, want %v (status %d)", got, tc.want, rec.Code)
			}
		})
	}
}
