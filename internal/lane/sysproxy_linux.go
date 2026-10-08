//go:build linux

package lane

import (
	"net/url"
	"os"
)

// SystemProxyURL reads the standard proxy environment variables, which most
// Linux desktops (and AppImage launchers) populate from the desktop's network
// settings. Order: HTTPS_PROXY/HTTPS_PROXY first, then http_proxy/HTTP_PROXY.
// Returns nil when no proxy is configured, matching the direct-connection
// fallback of the other platforms.
func SystemProxyURL() *url.URL {
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY"} {
		if v := os.Getenv(k); v != "" {
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				return u
			}
		}
	}
	return nil
}
