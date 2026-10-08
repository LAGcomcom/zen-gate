//go:build !windows && !darwin && !linux

package lane

import neturl "net/url"

// SystemProxyURL returns nil on platforms with no supported system-proxy
// lookup (the "system" proxy mode falls back to a direct connection).
func SystemProxyURL() *neturl.URL { return nil }
