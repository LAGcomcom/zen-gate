// Package update checks a configurable feed for newer zen-gate builds.
// Two feed shapes are understood:
//   - GitHub Releases API ("https://api.github.com/repos/<o>/<r>/releases/latest")
//     — parsed natively (tag_name/html_url/body/assets)
//   - any URL returning {"version":"x.y.z","url":"https://…"}
package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SelfUpdateSupported reports whether 一键更新 can replace the running binary
// in place. Only Windows ships that flow: the app is a single .exe that can
// be renamed while running. Everywhere else the build is a bundle/zip, so the
// dashboard links to the release page and the user installs by hand.
var SelfUpdateSupported = runtime.GOOS == "windows"

// assetName is the release asset 一键更新 expects for this platform. An empty
// name means the platform has no in-place update, which keeps the updater from
// ever handing a foreign binary to the swap logic.
func assetName() string {
	switch runtime.GOOS {
	case "windows":
		return "zen-gate.exe"
	case "darwin":
		return "zen-gate-darwin"
	default:
		return ""
	}
}

// FeedURL is the default update feed, overridable via -ldflags at release
// build time and per-install through settings.
var FeedURL = ""

// Current is the running version, overridable via -ldflags.
var Current = "1.1.0"

// FeedJSON is the expected remote shape for plain feeds.
type FeedJSON struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Notes   string `json:"notes"`
}

// Check fetches the feed and compares against Current.
// Returns (hasUpdate, version, downloadURL, notes, error).
func Check(feedURL string, client getJSONer) (bool, string, string, string, error) {
	if strings.TrimSpace(feedURL) == "" {
		return false, "", "", "", nil
	}
	data, err := client.Get(feedURL)
	if err != nil {
		return false, "", "", "", err
	}
	if IsGitHubFeed(feedURL) {
		return parseGitHubRelease(data)
	}
	var feed FeedJSON
	if err := json.Unmarshal(data, &feed); err != nil {
		return false, "", "", "", err
	}
	return Newer(feed.Version, Current), strings.TrimSpace(feed.Version), strings.TrimSpace(feed.URL), feed.Notes, nil
}

// IsGitHubFeed reports whether the feed URL is a GitHub Releases API endpoint.
func IsGitHubFeed(feedURL string) bool {
	u := strings.TrimSuffix(strings.TrimSpace(feedURL), "/")
	return strings.HasPrefix(u, "https://api.github.com/repos/") && strings.HasSuffix(u, "/releases/latest")
}

// githubRelease mirrors the slice of the Releases API response zen-gate needs.
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// parseGitHubRelease maps a Releases/latest payload onto the feed shape,
// preferring this platform's binary asset. Without a matching asset the URL
// stays on the release page: the dashboard then shows the version and sends
// the user to the download, which is what a bundled build needs.
func parseGitHubRelease(data []byte) (bool, string, string, string, error) {
	var rel githubRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return false, "", "", "", err
	}
	url := rel.HTMLURL
	if want := assetName(); want != "" {
		for _, a := range rel.Assets {
			if strings.EqualFold(a.Name, want) {
				url = a.BrowserDownloadURL
				break
			}
		}
	}
	return Newer(rel.TagName, Current), strings.TrimSpace(rel.TagName), url, rel.Body, nil
}

// LatestAssetURL returns this platform's binary download URL from a
// Releases/latest payload. It deliberately does *not* fall back to an arbitrary
// asset: handing a Windows exe to a macOS build would replace the app with
// something it cannot run.
func LatestAssetURL(data []byte) (string, int64, error) {
	var rel githubRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", 0, err
	}
	want := assetName()
	if want == "" {
		return "", 0, fmt.Errorf("no in-place update is supported on %s", runtime.GOOS)
	}
	for _, a := range rel.Assets {
		if strings.EqualFold(a.Name, want) {
			return a.BrowserDownloadURL, a.Size, nil
		}
	}
	return "", 0, fmt.Errorf("release %s has no %s asset", rel.TagName, want)
}

type getJSONer interface {
	Get(url string) ([]byte, error)
}

// Newer compares dotted versions; "1.2.10" > "1.2.9".
func Newer(remote, current string) bool {
	a := parseVersion(remote)
	b := parseVersion(current)
	if len(a) == 0 {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.SplitN(v, ".", 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			return [3]int{}
		}
		out[i] = n
	}
	return out
}

// HTTP fetcher with a sane timeout. An optional Client (e.g. the lane's
// proxy-aware one) takes precedence over the default transport.
type HTTP struct {
	Timeout time.Duration
	Client  *http.Client
}

func (h HTTP) Get(url string) ([]byte, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("update feed returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
