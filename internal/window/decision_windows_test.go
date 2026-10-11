//go:build windows

package window

import (
	"errors"
	"testing"
)

// The pre-flight verdict decides between the embedded window and the browser
// fallback. A failed query must leave the verdict open (try the real
// creation), because degrading on a bogus pre-flight is strictly worse.
func TestEmbeddedDecision(t *testing.T) {
	tests := []struct {
		name    string
		version string
		verr    error
		forced  bool
		wantOK  bool
		reason  string
	}{
		{"runtime installed", "139.0.3405.119", nil, false, true, ""},
		{"runtime missing", "", nil, false, false, "WebView2 Runtime 未安装"},
		{"forced off", "139.0.3405.119", nil, true, false, "ZEN_GATE_NO_WEBVIEW2=1（手动跳过内嵌窗口）"},
		{"forced off wins over installed", "", nil, true, false, "ZEN_GATE_NO_WEBVIEW2=1（手动跳过内嵌窗口）"},
		{"preflight error stays open", "", errors.New("HRESULT 0x80004005"), false, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := embeddedDecision(tt.version, tt.verr, tt.forced)
			if ok != tt.wantOK {
				t.Fatalf("embeddedDecision(%q, %v, %v) ok = %v, want %v", tt.version, tt.verr, tt.forced, ok, tt.wantOK)
			}
			if reason != tt.reason {
				t.Fatalf("embeddedDecision(%q, %v, %v) reason = %q, want %q", tt.version, tt.verr, tt.forced, reason, tt.reason)
			}
		})
	}
}
