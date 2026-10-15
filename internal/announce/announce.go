// Package announce pulls the author's announcement feed — a small JSON file
// (default: announcements.json in the project's GitHub repo) — and parses it
// into entries the caller splits into active / ended. 404 means "no
// announcements published", a normal state for private or fresh installs.
package announce

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Level spellings for an announcement.
const (
	LevelInfo     = "info"
	LevelWarn     = "warn"
	LevelCritical = "critical"
)

// Item is one announcement in the feed.
type Item struct {
	ID    string `json:"id"` // stable id; the dashboard uses it for 已读 tracking
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Level string `json:"level,omitempty"` // info | warn | critical (default info)
	Start string `json:"start,omitempty"` // YYYY-MM-DD, inclusive
	End   string `json:"end,omitempty"`   // YYYY-MM-DD, inclusive; empty = no end
	URL   string `json:"url,omitempty"`   // optional "了解更多" link
}

type feedFile struct {
	Announcements []Item `json:"announcements"`
}

const dateLayout = "2006-01-02"

// DefaultFeedURL is the compiled-in announcement source: the GitHub Contents
// API for announcements.json on the project's master branch. It rides
// api.github.com — the same host the update checker already depends on —
// because raw.githubusercontent.com is unreachable from some networks.
// rawFeedURL is its mirror for the opposite failure: the Contents API is
// rate-limited per proxy IP (403 once a shared accelerator burns the
// anonymous quota), and raw keeps serving when it does.
const (
	DefaultFeedURL = "https://api.github.com/repos/LAGcomcom/zen-gate/contents/announcements.json?ref=master"
	rawFeedURL     = "https://raw.githubusercontent.com/LAGcomcom/zen-gate/master/announcements.json"
)

// Fetch pulls and parses the feed. GitHub feeds fail in two opposite ways:
// the Contents API answers 403 once the proxy's shared IP burns the
// anonymous 60/hour quota, while raw.githubusercontent.com is unreachable
// through no proxy at all on some networks. So this is a small ladder, not
// one request: the configured URL, then the complementary GitHub host, then
// each again with the proxy bypassed (a dead local proxy must not swallow
// the feed either). All-404 means "nothing published" (nil, nil), same as
// before; any other total failure returns the last error. Callers split the
// list with Active / EndedItems.
func Fetch(ctx context.Context, url string, client *http.Client) ([]Item, error) {
	return FetchWithMirror(ctx, url, mirror(url), client)
}

// FetchWithMirror runs the ladder over an explicit mirror pair — the seam
// the tests drive; Fetch wires the mirror from the URL itself. The proxy
// bypass round only runs for the known GitHub pair: a user-customized feed
// is fetched through their client, nothing else.
func FetchWithMirror(ctx context.Context, url, mirrorURL string, client *http.Client) ([]Item, error) {
	candidates := []string{url}
	if mirrorURL != "" && mirrorURL != url {
		candidates = append(candidates, mirrorURL)
	}
	clients := []*http.Client{client}
	if mirrorURL != "" && client != directClient {
		clients = append(clients, directClient)
	}
	var lastErr error
	saw404 := false
	for _, c := range clients {
		for _, u := range candidates {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			items, err := fetchOnce(cctx, u, c)
			cancel()
			switch {
			case err == nil:
				return items, nil
			case errors.Is(err, errNotFound):
				saw404 = true
			default:
				lastErr = err
			}
		}
	}
	if saw404 {
		return nil, nil
	}
	return nil, lastErr
}

var errNotFound = errors.New("announce: 404")

// directClient bypasses every proxy for the GitHub pair: the Contents-API
// quota that 403s belongs to the proxy's shared IP, and the machine's own
// IP usually holds a separate one (and reaches raw even when the proxy is
// down). Idle conns are recycled by the per-attempt deadline; the pull runs
// at boot and every 6h, so this small pool is never hot.
var directClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

// fetchOnce is the single-URL pull behind Fetch: a 404 is the "no feed
// published" state (errNotFound), a wrong status or an unparseable body is
// an error. Both shapes parse: the plain {"announcements":[…]} file and the
// GitHub Contents-API envelope.
func fetchOnce(ctx context.Context, url string, client *http.Client) ([]Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return Parse(data)
}

// mirror returns the complementary GitHub source for this project's feed
// URLs — Contents API on api.github.com ↔ raw.githubusercontent.com — or ""
// for anything else: a user-customized feed has no mirror and is fetched
// through their client only. A wrong ref on one side (v1.3.0 wrote
// ?ref=main) is recovered by the other, which always names master.
func mirror(url string) string {
	u := strings.ToLower(strings.TrimSpace(url))
	switch {
	case strings.Contains(u, "api.github.com") && strings.Contains(u, "/zen-gate/contents/announcements.json"):
		return rawFeedURL
	case strings.Contains(u, "raw.githubusercontent.com") && strings.Contains(u, "/zen-gate/") && strings.HasSuffix(u, "announcements.json"):
		return DefaultFeedURL
	}
	return ""
}

// Parse decodes a feed payload in either shape and returns every valid entry.
func Parse(data []byte) ([]Item, error) {
	var envelope struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	payload := data
	if json.Unmarshal(data, &envelope) == nil && envelope.Encoding == "base64" && envelope.Content != "" {
		clean := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, envelope.Content)
		decoded, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("公告 base64 解码失败: %v", err)
		}
		payload = decoded
	}
	var f feedFile
	if err := json.Unmarshal(payload, &f); err != nil {
		return nil, fmt.Errorf("公告格式不是合法 JSON: %v", err)
	}
	out := []Item{}
	for _, it := range f.Announcements {
		if it.ID == "" || it.Title == "" {
			continue
		}
		switch it.Level {
		case LevelInfo, LevelWarn, LevelCritical:
		default:
			it.Level = LevelInfo
		}
		out = append(out, it)
	}
	return out, nil
}

// Active filters items to those whose window contains now (start passed, end
// not passed), ordered critical → warn → info, newest first within a level.
// Items with an unparsable or future start are skipped.
func Active(items []Item, now time.Time) []Item {
	out := []Item{}
	for _, it := range items {
		if it.Start != "" {
			if _, err := time.ParseInLocation(dateLayout, it.Start, time.Local); err != nil {
				continue
			}
			// Zero-padded YYYY-MM-DD compares correctly as a string.
			if now.Format(dateLayout) < it.Start {
				continue
			}
		}
		if Ended(it, now) {
			continue
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := levelRank(out[i].Level), levelRank(out[j].Level)
		if ri != rj {
			return ri < rj
		}
		return out[i].Start > out[j].Start
	})
	return out
}

// Ended reports whether the item's window has closed (a parsable end date in
// the past). No end date = never ends.
func Ended(it Item, now time.Time) bool {
	if it.End == "" {
		return false
	}
	if _, err := time.ParseInLocation(dateLayout, it.End, time.Local); err != nil {
		return false
	}
	return now.Format(dateLayout) > it.End
}

// EndedItems returns window-closed entries, most recently ended first.
func EndedItems(items []Item, now time.Time) []Item {
	out := []Item{}
	for _, it := range items {
		if Ended(it, now) {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].End > out[j].End })
	return out
}

func levelRank(l string) int {
	switch l {
	case LevelCritical:
		return 0
	case LevelWarn:
		return 1
	default:
		return 2
	}
}
