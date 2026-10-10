package lane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// countRotator stands in for the sing-box node pool: it records every address
// it was asked to reach and can be told to fail, so the rotate branch's
// decisions are observable without a real egress.
type countRotator struct {
	mu      sync.Mutex
	addrs   []string
	fail    bool
	forward bool
}

func (r *countRotator) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, addr)
	r.mu.Unlock()
	if r.fail {
		return nil, errors.New("没有健康节点——请刷新订阅或检查节点")
	}
	if r.forward {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return nil, errors.New("no egress configured")
}

func (r *countRotator) Healthy() int { return 3 }

// The extended Rotator surface. This double only asserts that a rotate-mode
// transport dials through the installed rotator, so the egress-decision
// methods can stay trivial.
func (r *countRotator) Pick(seed, model string) string { return "test-exit" }

func (r *countRotator) DialThrough(ctx context.Context, network, addr, nodeID string) (net.Conn, error) {
	return r.Dial(ctx, network, addr)
}

func (r *countRotator) ExitKey(nodeID string) string { return nodeID }

func (r *countRotator) Report(nodeID, model, class string) {}

func (r *countRotator) Revive(nodeID string) {}

func (r *countRotator) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addrs...)
}

// rotateTransport installs rot and switches the shared client to rotate mode,
// restoring the previous state when the test ends.
func rotateTransport(t *testing.T, rot Rotator) *http.Transport {
	t.Helper()
	t.Cleanup(func() { SetRotator(nil); SetProxy("direct", "") })
	SetRotator(rot)
	SetProxy("rotate", "")
	tr, ok := laneHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("rotate mode did not install an http.Transport")
	}
	return tr
}

func localNonLoopbackIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("interfaces unreadable: %v", err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	t.Skip("this machine has no non-loopback IPv4 address")
	return ""
}

// Quota is accounted per egress IP, so rotation only pays off when every
// request gets a fresh dial — keep-alive would reuse one connection and pin the
// whole process to a single exit.
func TestRotateDialsEveryRequestInsteadOfReusing(t *testing.T) {
	tr := rotateTransport(t, &countRotator{forward: true})
	if !tr.DisableKeepAlives {
		t.Errorf("rotate transport keeps connections alive: DialContext runs once and every request leaves from the same IP")
	}
}

// The subscription itself is often served from this machine
// (http://127.0.0.1:21000/sub). Handing that address to a remote node means the
// node dials its own loopback, so the pool can never fill and rotate mode
// self-locks at boot.
func TestRotateKeepsLoopbackOffTheNodes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	rot := &countRotator{fail: true}
	tr := rotateTransport(t, rot)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, derr := tr.DialContext(ctx, "tcp", ln.Addr().String())
	if derr != nil {
		t.Fatalf("dial: %v", derr)
	}
	_ = c.Close()
	if got := rot.seen(); len(got) != 0 {
		t.Errorf("loopback reached a node: %v", got)
	}
}

func TestLoopbackAddrRecognition(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:21000", true},
		{"127.5.3.1:80", true},
		{"[::1]:21000", true},
		{"localhost:21000", true},
		{"LOCALHOST:443", true},
		{"192.168.1.7:11434", false},
		{"api.example.com:443", false},
		{"100.84.121.115:22", false},
	} {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// An empty or wholly failing pool must degrade to the system egress rather than
// answer 502 to everything.
func TestRotateFallsBackToDirectWhenNoNodeWorks(t *testing.T) {
	host := localNonLoopbackIP(t)
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Skipf("cannot bind %s: %v", host, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	rot := &countRotator{fail: true}
	tr := rotateTransport(t, rot)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, derr := tr.DialContext(ctx, "tcp", ln.Addr().String())
	if derr != nil {
		t.Fatalf("a failing pool must fall back to a direct dial, got: %v", derr)
	}
	_ = c.Close()
	if len(rot.seen()) == 0 {
		t.Errorf("the rotator was never asked for this egress")
	}
}

func TestRotateUsesANodeForRemoteTargets(t *testing.T) {
	host := localNonLoopbackIP(t)
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Skipf("cannot bind %s: %v", host, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	rot := &countRotator{forward: true}
	tr := rotateTransport(t, rot)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, derr := tr.DialContext(ctx, "tcp", ln.Addr().String())
	if derr != nil {
		t.Fatalf("dial through the pool: %v", derr)
	}
	_ = c.Close()
	if len(rot.seen()) == 0 {
		t.Errorf("a non-local target went straight out instead of through a node")
	}
}

// A 429 under rotation is one exit's answer, not the model's: with the pool
// live, the same request must be re-offered to other exits before the model is
// declared throttled.
func TestQuotaUnderRotationTriesOtherEgressesFirst(t *testing.T) {
	old := UpstreamBase
	t.Cleanup(func() { UpstreamBase = old; SetRotator(nil) })

	var mu sync.Mutex
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"FreeUsageLimitError"}}`)
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	UpstreamBase = up.URL

	SetRotator(&countRotator{forward: true})
	l := NewLane()
	l.catalog = []ModelInfo{{ID: "space-bunny-free", Wire: "chat"}}
	l.SetFailover(false, 1)

	var got []string
	_, uerr := l.Complete(context.Background(), Request{Model: "space-bunny-free"}, func(c Chunk) {
		if c.Kind == ChunkTextDelta {
			got = append(got, c.Delta)
		}
	})
	if uerr != nil {
		t.Fatalf("rotation retry should have found a funded exit: %v", uerr)
	}
	if len(got) == 0 || got[0] != "ok" {
		t.Errorf("text = %v", got)
	}
	mu.Lock()
	n := hits
	mu.Unlock()
	if n != 3 {
		t.Errorf("attempts = %d, want 3 (two 429 exits, then success)", n)
	}
	if l.RecoveryETA("space-bunny-free") != 0 {
		t.Errorf("a model that answered successfully got marked throttled")
	}
}
