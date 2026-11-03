package subs

import (
	"net/url"
	"testing"
)

// An airport writes fp=unsafe (some clients accept arbitrary uTLS aliases);
// sing-box refuses the whole generated config over one unknown fingerprint —
// "initialize outbound[N]: unknown uTLS fingerprint: unsafe" — so every node in
// the subscription died at check time (#124). Unknown values are dropped.
func TestTLSBlockDropsUnknownUTLSFingerprint(t *testing.T) {
	q, _ := url.ParseQuery("security=tls&fp=unsafe&sni=a.example")
	tls := tlsBlock(q, true)
	if _, has := tls["utls"]; has {
		t.Fatalf("unknown fingerprint was forwarded: %v", tls["utls"])
	}
	if tls["server_name"] != "a.example" || tls["enabled"] != true {
		t.Fatalf("the rest of the TLS block must survive: %v", tls)
	}
}

// Known fingerprints still pass through, and the "*_auto" spelling some clients
// emit normalizes to the plain name sing-box accepts.
func TestTLSBlockKeepsKnownUTLSFingerprint(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"chrome", "chrome"},
		{"Chrome", "chrome"},
		{"firefox_auto", "firefox"},
		{" Safari ", "safari"},
	} {
		q, _ := url.ParseQuery("security=tls&fp=" + url.QueryEscape(tc.in))
		tls := tlsBlock(q, true)
		ut, ok := tls["utls"].(map[string]any)
		if !ok || ut["fingerprint"] != tc.want || ut["enabled"] != true {
			t.Fatalf("fp=%q → %v, want fingerprint %q", tc.in, tls["utls"], tc.want)
		}
	}
}

// reality cannot run without uTLS, so a rejected fingerprint falls back to
// chrome rather than emitting a reality block sing-box would refuse.
func TestRealityFallsBackToAWorkingFingerprint(t *testing.T) {
	q, _ := url.ParseQuery("security=reality&fp=unsafe&pbk=K&sid=ab&sni=www.apple.com")
	tls := tlsBlock(q, false)
	ut, ok := tls["utls"].(map[string]any)
	if !ok || ut["fingerprint"] != "chrome" {
		t.Fatalf("reality utls = %v, want chrome", tls["utls"])
	}
	if re, ok := tls["reality"].(map[string]any); !ok || re["public_key"] != "K" {
		t.Fatalf("reality block = %v", tls["reality"])
	}
}
