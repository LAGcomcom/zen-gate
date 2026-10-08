package subs

import (
	"strings"
	"testing"
)

// AnyTLS is the same shape as trojan — proto://password@host:port?<query>#name,
// TLS always on — and many providers now ship it as their only node type. A
// skipped scheme is not a harmless parse miss: in the reporter's subscription
// 74 of 80 nodes were anytls, so the rotation pool silently became 6 exits and
// "more nodes = more quota" stopped being true.
func TestParseAnyTLSURI(t *testing.T) {
	n, err := ParseURI("anytls://s3cr3t@any.example.com:443?sni=any.example.com&alpn=h3&fp=chrome&insecure=1#HK-AnyTLS-01")
	if err != nil {
		t.Fatalf("anytls URI refused: %v", err)
	}
	if n.Proto != "anytls" {
		t.Errorf("Proto = %q, want anytls", n.Proto)
	}
	if got := n.Outbound["type"]; got != "anytls" {
		t.Errorf("outbound type = %v, want anytls", got)
	}
	if got := n.Outbound["password"]; got != "s3cr3t" {
		t.Errorf("password = %v", got)
	}
	if n.Outbound["server"] != "any.example.com" || n.Outbound["server_port"] != 443 {
		t.Errorf("server/port = %v/%v", n.Outbound["server"], n.Outbound["server_port"])
	}
	tls, ok := n.Outbound["tls"].(map[string]any)
	if !ok {
		t.Fatalf("no tls block: %#v", n.Outbound["tls"])
	}
	// TLS is structural for AnyTLS: a URI with no security= param is still TLS.
	if tls["enabled"] != true {
		t.Errorf("tls.enabled = %v, want true without any security= hint", tls["enabled"])
	}
	if tls["server_name"] != "any.example.com" {
		t.Errorf("sni not carried: %#v", tls)
	}
	if tls["utls"] == nil {
		t.Errorf("fp=chrome was dropped: %#v", tls)
	}
	if n.Name != "HK-AnyTLS-01" {
		t.Errorf("Name = %q", n.Name)
	}
}

func TestAnyTLSNodesAreNotSkippedFromASubscription(t *testing.T) {
	lines := []string{
		"anytls://p1@a.example.com:443?sni=a.example.com#A1",
		"anytls://p2@b.example.com:443?sni=b.example.com#A2",
		"trojan://p3@c.example.com:443#C1",
		"vless://uuid@d.example.com:443?security=tls&sni=d.example.com#D1",
	}
	nodes, err := ParseBody(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 {
		t.Fatalf("parsed %d of 4 nodes: %v", len(nodes), nodeNames(nodes))
	}
	protos := map[string]int{}
	for _, n := range nodes {
		protos[n.Proto]++
	}
	if protos["anytls"] != 2 {
		t.Errorf("anytls count = %d, want 2 (%v)", protos["anytls"], protos)
	}
}

// security=none is an explicit instruction, so it has to switch TLS off even
// for an always-TLS protocol — otherwise the generated config tries to talk TLS
// to a plaintext port and the node is unhealthy forever.
func TestAnyTLSTLLOutExplicitly(t *testing.T) {
	n, err := ParseURI("anytls://p@h.example.com:8443?security=none#Plain")
	if err != nil {
		t.Fatal(err)
	}
	tls := n.Outbound["tls"].(map[string]any)
	if tls["enabled"] != false {
		t.Errorf("security=none ignored: %#v", tls)
	}
}

func nodeNames(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}
