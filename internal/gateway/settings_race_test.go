package gateway

import (
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// Issue #32: the settings handler snapshots the config at request start and
// used to publish that whole snapshot at commit. Two overlapping writes then
// reverted each other's fields — a proxy saved in one tab came back
// "变回默认" after any other toggle committed later. commitSettings now
// publishes only the fields the request actually carried; this test pins that
// with a deliberately stale snapshot, which is exactly what a racing request
// holds.
func TestCommitSettingsPublishesOnlyDirtyFields(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := New(lane.NewLane(), st)

	// Live state: the user has just saved a custom proxy and enabled
	// subscriptions (subs/save owns SubsEnabled, the settings handler never
	// parses it).
	st.Mutate(func(c *store.Config) {
		c.ProxyMode = "custom"
		c.ProxyURL = "http://127.0.0.1:7897"
		c.SubsEnabled = true
		c.SingBoxPath = `C:\tools\sing-box.exe`
	})
	// A stale snapshot from a request that started BEFORE those saves:
	// defaults everywhere the live store now says otherwise.
	stale := *st.Config()
	stale.ProxyMode = "env"
	stale.ProxyURL = ""
	stale.SubsEnabled = false
	stale.SingBoxPath = ""

	// The racing request only toggled notifications.
	s.commitSettings(&stale, map[string]bool{"notifications": true})

	got := st.Config()
	if got.ProxyMode != "custom" || got.ProxyURL != "http://127.0.0.1:7897" {
		t.Errorf("proxy reverted by an unrelated settings commit: %q / %q", got.ProxyMode, got.ProxyURL)
	}
	if !got.SubsEnabled {
		t.Error("SubsEnabled reverted by the settings commit")
	}
	if got.SingBoxPath == "" {
		t.Error("SingBoxPath wiped by the settings commit")
	}
}
