package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /healthz is the supervisor spelling of /health (docker healthchecks,
// systemd probes): same body, and like /health it must not ask for a key —
// a liveness probe that needs credentials fails on every restart.
func TestHealthzMatchesHealth(t *testing.T) {
	s := newPrefServer(t)
	for _, path := range []string{"/health", "/healthz"} {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s = %d, want 200", path, w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, `"ok":true`) {
			t.Fatalf("%s body = %s", path, body)
		}
	}
}
