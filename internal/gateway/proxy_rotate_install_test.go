package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
)

// fakeSubs is the dashboard-facing sidecar surface with a stub rotator: the
// only thing #121 needs is whether a rotator gets installed, and whether the
// config keeps the mode the user picked.
type fakeSubs struct {
	healthy int
	applied int
	rotate  bool
}

func (f *fakeSubs) Apply(context.Context) error   { f.applied++; return nil }
func (f *fakeSubs) Refresh(context.Context) error { f.applied++; return nil }
func (f *fakeSubs) ProbeAll(context.Context)      {}
func (f *fakeSubs) Stop()                         {}
func (f *fakeSubs) Download(context.Context) (string, error) {
	return "", nil
}
func (f *fakeSubs) SetPath(string) error   { return nil }
func (f *fakeSubs) Healthy() int           { return f.healthy }
func (f *fakeSubs) Status() map[string]any { return map[string]any{} }
func (f *fakeSubs) AsRotator() lane.Rotator {
	f.rotate = true
	return nil
}

func postSettings(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/admin/api/settings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// Choosing 订阅轮询 must install a rotator. Saving the mode alone left the
// transport dialing through a nil rotator, which degrades to a plain dial: the
// panel said 订阅轮询 while every request left from the machine's own IP, and
// the next settings write (any field) put env back — the "自动切回跟随环境变量"
// report (#121).
func TestSettingsRotateInstallsTheRotator(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	t.Cleanup(func() { lane.SetProxy("direct", ""); lane.SetRotator(nil) })

	fake := &fakeSubs{healthy: 1}
	s.SetSubs(fake)

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	code, _ := postSettings(t, ts.URL, `{"proxyMode":"rotate"}`)
	if code != 200 {
		t.Fatalf("rotate = %d, want 200", code)
	}
	if !fake.rotate {
		t.Error("the rotate mode was saved without installing a rotator")
	}
	if got := s.Store.Config().ProxyMode; got != "rotate" {
		t.Errorf("ProxyMode = %q, want rotate persisted", got)
	}
}

// With no healthy node there is nothing to rotate through: the mode is still
// saved (the user may fill the subscription in next), the reply explains why,
// and no rotator is installed over an empty pool.
func TestSettingsRotateWithoutNodesExplainsItself(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	t.Cleanup(func() { lane.SetProxy("direct", ""); lane.SetRotator(nil) })

	s.SetSubs(&fakeSubs{healthy: 0})

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	code, out := postSettings(t, ts.URL, `{"proxyMode":"rotate"}`)
	if code != 200 {
		t.Fatalf("rotate = %d, want 200", code)
	}
	if out["note"] == nil || out["note"] == "" {
		t.Error("a saved-but-not-dialing rotate mode reported plain success")
	}
	if cur := lane.CurrentRotator(); cur != nil {
		t.Error("a rotator was installed over an empty node pool")
	}
}
