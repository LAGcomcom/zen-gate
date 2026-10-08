package gateway

import (
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
)

// rotate is a real mode — lane.SetProxy tunnels through the subscription pool
// and the sidecar manager implements it — but the settings whitelist predates
// it, so the dashboard's 「订阅轮询 (sing-box)」 choice fell through the switch:
// no 400, no save, no effect.
func TestSettingsAppliesRotateProxyMode(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	t.Cleanup(func() { lane.SetProxy("direct", "") })

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL, `{"proxyMode":"rotate"}`); code != 200 {
		t.Fatalf("rotate = %d, want 200", code)
	}
	if got := s.Store.Config().ProxyMode; got != "rotate" {
		t.Errorf("ProxyMode = %q, want rotate", got)
	}
}

// A typo in the mode must not read as success while nothing changed.
func TestSettingsRejectsUnknownProxyMode(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	t.Cleanup(func() { lane.SetProxy("direct", "") })

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL, `{"proxyMode":"roatete"}`); code != 400 {
		t.Fatalf("unknown mode = %d, want 400", code)
	}
	if got := s.Store.Config().ProxyMode; got == "roatete" {
		t.Errorf("rejected mode was still stored")
	}
}
