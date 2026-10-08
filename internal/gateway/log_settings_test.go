package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/logx"
)

// patchSettings sends a settings change the way the dashboard does.
func patchSettings(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url+"/admin/api/settings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The switches have to work without a restart: 排障 means turning the noisy
// classes on, replaying the failing request, and turning them back off.
func TestSettingsPATCHAppliesLogSwitchesLive(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	l.SetCategories(logx.DefaultCategories())

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL,
		`{"logCategories":{"upstream":true,"access":false},"logLevel":"warn","logKeepDays":30}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}

	if !l.Enabled("upstream") {
		t.Errorf("upstream class still off after the switch")
	}
	if l.Enabled("access") {
		t.Errorf("access class still on after the switch")
	}
	cfg := s.Store.Config()
	if !cfg.LogCategories["upstream"] || cfg.LogCategories["access"] {
		t.Errorf("logCategories not persisted: %+v", cfg.LogCategories)
	}
	if cfg.LogLevel != "warn" || cfg.LogKeepDays != 30 {
		t.Errorf("logLevel/logKeepDays = %q/%d, want warn/30", cfg.LogLevel, cfg.LogKeepDays)
	}

	// The level filter trims the file only: the live viewer keeps everything,
	// a disabled class records nothing anywhere.
	s.logCat(logx.CatProbe, "info", "探测信息")
	s.logCat(logx.CatProbe, "warn", "探测告警")
	s.logCat(logx.CatAccess, "warn", "不该出现")
	today := time.Now().Format("2006-01-02")
	ring := l.Query(logx.Query{Limit: 500})
	if !hasMsg(ring, "探测信息") || !hasMsg(ring, "探测告警") {
		t.Errorf("ring lost lines at info level: %+v", ring)
	}
	file := l.Query(logx.Query{Day: today, Limit: 500})
	if !hasMsg(file, "探测告警") {
		t.Errorf("file lost the warn line: %+v", file)
	}
	if hasMsg(file, "探测信息") {
		t.Errorf("file kept an info line below the warn threshold: %+v", file)
	}
	if hasMsg(file, "不该出现") || hasMsg(ring, "不该出现") {
		t.Errorf("a silenced class still recorded: file %+v ring %+v", file, ring)
	}
}

// The dashboard chips read their initial state from /admin/api/state, which
// must show the effective switches — the shipped defaults filled in — not the
// raw (possibly empty) config.
func TestStateEchoesEffectiveLogSettings(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	wireLog(t, s)

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	if code := patchSettings(t, ts.URL, `{"logCategories":{"content":true}}`); code != 200 {
		t.Fatalf("settings = %d, want 200", code)
	}

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state struct {
		Settings struct {
			LogCategories map[string]bool `json:"logCategories"`
			LogLevel      string          `json:"logLevel"`
			LogKeepDays   int             `json:"logKeepDays"`
		} `json:"settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	got := state.Settings.LogCategories
	if !got["content"] {
		t.Errorf("logCategories = %+v, want content on", got)
	}
	if !got["access"] {
		t.Errorf("logCategories = %+v, want the untouched default classes still on", got)
	}
	if got["upstream"] {
		t.Errorf("logCategories = %+v, want upstream at its default off", got)
	}
	if state.Settings.LogLevel != "debug" {
		t.Errorf("logLevel = %q, want the default debug", state.Settings.LogLevel)
	}
	if state.Settings.LogKeepDays != logx.DefaultKeepDays {
		t.Errorf("logKeepDays = %d, want the default %d", state.Settings.LogKeepDays, logx.DefaultKeepDays)
	}
}

// At boot the persisted switches must reach the logger, and an empty config
// must land on the shipped defaults — the noisy classes off, the audit trail
// on, the file unfiltered.
func TestApplyLogSettingsAtStartup(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	l.SetCategories(map[string]bool{"content": true, "upstream": true, "access": true})

	s.ApplyLogSettings(s.Store.Config())

	for _, cat := range []string{"upstream", "content"} {
		if l.Enabled(cat) {
			t.Errorf("%s class on after startup with an empty config, want the shipped default off", cat)
		}
	}
	if !l.Enabled("access") {
		t.Errorf("access class silenced by the startup defaults")
	}
	s.logCat(logx.CatProbe, "info", "开机信息")
	today := time.Now().Format("2006-01-02")
	if !hasMsg(l.Query(logx.Query{Day: today, Limit: 500}), "开机信息") {
		t.Errorf("an info line did not reach the file with the default debug level")
	}
}

func hasMsg(entries []logx.Entry, want string) bool {
	for _, e := range entries {
		if strings.Contains(e.Msg, want) {
			return true
		}
	}
	return false
}
