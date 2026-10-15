package announce

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchParsesAllAndSplits(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"announcements":[
			{"id":"a1","title":"维护","level":"warn","start":"2026-01-01"},
			{"id":"a2","title":"过期","level":"info","start":"2026-01-01","end":"2026-01-31"},
			{"id":"a3","title":"未来","level":"info","start":"2099-01-01"},
			{"id":"","title":"无id"},
			{"id":"a4","title":"紧急","level":"critical"}
		]}`))
	}))
	defer up.Close()
	items, err := Fetch(context.Background(), up.URL, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("fetch = %d items, want 4 valid", len(items))
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	active := Active(items, now)
	if len(active) != 2 || active[0].ID != "a4" || active[1].ID != "a1" {
		t.Fatalf("active = %+v", active)
	}
	ended := EndedItems(items, now)
	if len(ended) != 1 || ended[0].ID != "a2" {
		t.Fatalf("ended = %+v", ended)
	}
}

func TestFetch404IsEmpty(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()
	items, err := Fetch(context.Background(), up.URL, up.Client())
	if err != nil {
		t.Fatalf("404 must not be an error, got %v", err)
	}
	if items != nil {
		t.Fatalf("items = %v, want nil", items)
	}
}

func TestFetchBadJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer up.Close()
	if _, err := Fetch(context.Background(), up.URL, up.Client()); err == nil {
		t.Fatal("want error for non-JSON body")
	}
}

func TestFetchGitHubContentsEnvelope(t *testing.T) {
	inner := `{"announcements":[{"id":"c1","title":"来自 Contents API","level":"warn"}]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":"` + base64.StdEncoding.EncodeToString([]byte(inner)) + `","encoding":"base64"}`))
	}))
	defer up.Close()
	items, err := Fetch(context.Background(), up.URL, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "c1" || items[0].Level != LevelWarn {
		t.Fatalf("items = %+v", items)
	}
}

func TestParseDefaultsLevel(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	items, err := Parse([]byte(`{"announcements":[{"id":"x","title":"t","level":"weird"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out := Active(items, now)
	if len(out) != 1 || out[0].Level != LevelInfo {
		t.Fatalf("out = %+v", out)
	}
}

// --- dual-source ladder (#55-adjacent outage: Contents API 403s when a
// proxy's shared IP burns the anonymous quota; raw is unreachable from other
// networks — the feed must survive either failure) ---

func TestMirrorMapsGitHubPair(t *testing.T) {
	if got := mirror("https://api.github.com/repos/LAGcomcom/zen-gate/contents/announcements.json?ref=main"); got != rawFeedURL {
		t.Errorf("contents mirror = %q, want raw", got)
	}
	if got := mirror("https://raw.githubusercontent.com/LAGcomcom/zen-gate/master/announcements.json"); got != DefaultFeedURL {
		t.Errorf("raw mirror = %q, want contents", got)
	}
	if got := mirror("https://example.com/feed.json"); got != "" {
		t.Errorf("custom feed mirror = %q, want none", got)
	}
}

// The Contents API 403s (proxy quota spent); the raw mirror still serves —
// the ladder must land the feed through the mirror without touching the
// bypass round.
func TestFetchFallsBackToMirror(t *testing.T) {
	inner := `{"announcements":[{"id":"m1","title":"来自 raw","level":"info"}]}`
	var hits struct{ contents, raw int }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "api-contents") {
			hits.contents++
			w.WriteHeader(http.StatusForbidden) // "403 rate limit exceeded"
			return
		}
		hits.raw++
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(inner))
	}))
	defer up.Close()
	items, err := FetchWithMirror(context.Background(), up.URL+"/api-contents", up.URL+"/raw", up.Client())
	if err != nil {
		t.Fatalf("ladder must survive a 403 on one source: %v", err)
	}
	if len(items) != 1 || items[0].ID != "m1" {
		t.Fatalf("items = %+v", items)
	}
	if hits.raw != 1 {
		t.Fatalf("raw hits = %d, want exactly one (mirror answers, no bypass round needed)", hits.raw)
	}
}

// A 404 on the primary is not "no feed published" while the mirror still has
// content: the ?ref=main install class. And one source erroring plus the
// other 404-ing reads as "nothing published" (prior 404 semantics win).
func TestFetchLadder404ThenMirror(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "main") {
			w.WriteHeader(http.StatusNotFound) // wrong ref
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"announcements":[{"id":"ok","title":"t"}]}`))
	}))
	defer up.Close()
	items, err := FetchWithMirror(context.Background(), up.URL+"/main", up.URL+"/master", up.Client())
	if err != nil || len(items) != 1 || items[0].ID != "ok" {
		t.Fatalf("items = %+v err = %v, want the mirror's one item", items, err)
	}

	// A mirror that 403s must not mask a working primary.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer dead.Close()
	items, err = FetchWithMirror(context.Background(), up.URL+"/master", dead.URL, up.Client())
	if err != nil || len(items) != 1 || items[0].ID != "ok" {
		t.Fatalf("items = %+v err = %v, want the working primary to answer", items, err)
	}

	// Both sources 404: the published "no feed" state (nil, nil).
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gone.Close()
	items, err = FetchWithMirror(context.Background(), gone.URL, gone.URL+"/mirror", up.Client())
	if err != nil || items != nil {
		t.Fatalf("items = %+v err = %v, want (nil, nil) for an all-404 ladder", items, err)
	}
}

// Every source failing non-404 surfaces the last error, not a silent empty
// feed — and the bypass round must run (it also fails, here against the same
// dead server), proving all four attempts were spent.
func TestFetchLadderExhausted(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer up.Close()
	_, err := FetchWithMirror(context.Background(), up.URL, up.URL+"/mirror", up.Client())
	if err == nil {
		t.Fatal("want error when every source 403s")
	}
	// 2 rounds (proxy client + bypass) × 2 candidates = 4 attempts.
	if hits.Load() != 4 {
		t.Fatalf("attempts = %d, want 4 (mirror widens, bypass retries)", hits.Load())
	}
}
