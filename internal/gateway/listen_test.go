package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// routableIPv4 is this machine's LAN address, or "" when the box has no
// non-loopback interface (the LAN tests then skip instead of failing).
func routableIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	return lanIPv4(addrs)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// lanTestServer starts a real listener (not httptest) with the given LAN
// setting and returns it plus its port.
func lanTestServer(t *testing.T, allowLan bool) (*Server, int) {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	st.Config().Port = port
	st.Config().AllowLan = allowLan
	s := New(lane.NewLane(), st)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s, port
}

func getHealth(url string) (int, error) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func TestLoopbackOnlyByDefault(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	s, port := lanTestServer(t, false)
	if got, want := s.BaseURL(), fmt.Sprintf("http://127.0.0.1:%d/v1", port); got != want {
		t.Fatalf("BaseURL() = %q, 期望 %q", got, want)
	}
	if _, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port)); err == nil {
		t.Fatalf("默认配置下局域网地址 %s:%d 竟然可访问", ip, port)
	}
	if code, err := getHealth(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err != nil || code != 200 {
		t.Fatalf("本机回环不可用: code=%d err=%v", code, err)
	}
}

func TestLanServedWhenAllowed(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	_, port := lanTestServer(t, true)
	code, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port))
	if err != nil {
		t.Fatalf("开启 allowLan 后局域网访问失败: %v", err)
	}
	if code != 200 {
		t.Fatalf("局域网 /health = %d, 期望 200", code)
	}
	if code, err := getHealth(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err != nil || code != 200 {
		t.Fatalf("放开局域网后本机回环应照常可用: code=%d err=%v", code, err)
	}
}

// The agent adapters identify zen-gate's own entries by the literal
// 127.0.0.1 in the advertised base URL, so it must never become 0.0.0.0.
func TestBaseURLStaysLoopbackWhenLanAllowed(t *testing.T) {
	s, port := lanTestServer(t, true)
	if got, want := s.BaseURL(), fmt.Sprintf("http://127.0.0.1:%d/v1", port); got != want {
		t.Fatalf("BaseURL() = %q, 期望 %q", got, want)
	}
}

func TestAdminFenceHoldsOverLan(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	_, port := lanTestServer(t, true)
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/admin/api/state", ip, port))
	if err != nil {
		t.Fatalf("GET admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("局域网访问 admin = %d, 期望 403", resp.StatusCode)
	}
}

func TestRebindAppliesLanToggle(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	s, port := lanTestServer(t, false)
	if _, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port)); err == nil {
		t.Fatalf("Rebind 前局域网不应可用")
	}
	s.Store.Config().AllowLan = true
	if err := s.Rebind(); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if _, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port)); err != nil {
		t.Fatalf("Rebind 后局域网仍连不上: %v", err)
	}

	s.Store.Config().AllowLan = false
	if err := s.Rebind(); err != nil {
		t.Fatalf("Rebind back: %v", err)
	}
	if _, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port)); err == nil {
		t.Fatalf("Rebind 关闭后局域网不应可用")
	}
}

func TestSettingsAllowLanRebinds(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	s, port := lanTestServer(t, false)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/admin/api/settings", "application/json",
		strings.NewReader(`{"allowLan":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("settings = %d, 期望 200", resp.StatusCode)
	}
	if !s.Store.Config().AllowLan {
		t.Fatalf("allowLan 未写入配置")
	}
	if _, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port)); err != nil {
		t.Fatalf("开关后未重新监听局域网: %v", err)
	}
}

func TestStateExposesLanEndpoint(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	s, port := lanTestServer(t, true)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state struct {
		BaseURL    string `json:"baseURL"`
		LANBaseURL string `json:"lanBaseURL"`
		Settings   struct {
			AllowLan bool `json:"allowLan"`
		} `json:"settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.LANBaseURL != fmt.Sprintf("http://%s:%d/v1", ip, port) {
		t.Fatalf("lanBaseURL = %q", state.LANBaseURL)
	}
	if !state.Settings.AllowLan {
		t.Fatalf("settings.allowLan 未上报")
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d/v1", port); state.BaseURL != want {
		t.Fatalf("baseURL = %q, 期望 %q", state.BaseURL, want)
	}
}

// A refused routable bind must not leave the gateway with no listener: the
// switch has to fall back to loopback and say so.
func TestRebindFallsBackToLoopbackWhenBindFails(t *testing.T) {
	s, port := lanTestServer(t, false)
	real := net.Listen
	s.listen = func(network, addr string) (net.Listener, error) {
		if strings.HasPrefix(addr, "0.0.0.0:") {
			return nil, fmt.Errorf("bind refused")
		}
		return real(network, addr)
	}

	s.Store.Config().AllowLan = true
	if err := s.Rebind(); err == nil {
		t.Fatalf("绑定被拒绝时 Rebind 应报错")
	}
	if s.Store.Config().AllowLan {
		t.Fatalf("切换失败后 allowLan 应回退为 false")
	}
	if code, err := getHealth(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err != nil || code != 200 {
		t.Fatalf("回退后本机服务应可用: code=%d err=%v", code, err)
	}
}

func TestLanIPv4PrefersPrivateOverOverlay(t *testing.T) {
	overlay := &net.IPNet{IP: net.IPv4(100, 122, 246, 80), Mask: net.CIDRMask(32, 32)}
	priv := &net.IPNet{IP: net.IPv4(192, 168, 1, 68), Mask: net.CIDRMask(24, 24)}
	loop := &net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)}
	link := &net.IPNet{IP: net.IPv4(169, 254, 1, 20), Mask: net.CIDRMask(16, 32)}

	if got := lanIPv4([]net.Addr{overlay, priv, loop, link}); got != "192.168.1.68" {
		t.Fatalf("lanIPv4() = %q, 期望优先私有网段 192.168.1.68", got)
	}
	// 172.16-31 is this box's Hyper-V/WSL NAT range: never the LAN peer's path.
	nat := &net.IPNet{IP: net.IPv4(172, 21, 32, 1), Mask: net.CIDRMask(20, 32)}
	if got := lanIPv4([]net.Addr{nat, priv}); got != "192.168.1.68" {
		t.Fatalf("lanIPv4() = %q, 期望跳过虚拟机 NAT 段", got)
	}
	if got := lanIPv4([]net.Addr{overlay, loop, link}); got != "100.122.246.80" {
		t.Fatalf("lanIPv4() = %q, 无私有网段时应退回可用地址", got)
	}
	if got := lanIPv4([]net.Addr{loop, link}); got != "" {
		t.Fatalf("lanIPv4() = %q, 回环与 link-local 不应入选", got)
	}
}

// Rebind closes the listener that is serving the settings request itself, so
// the reply has to go out before the switchover — otherwise the dashboard
// reports a failed toggle even though the bind changed.
func TestSettingsAllowLanRepliesOverLiveListener(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	_, port := lanTestServer(t, false)
	url := fmt.Sprintf("http://127.0.0.1:%d/admin/api/settings", port)
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Post(url, "application/json", strings.NewReader(`{"allowLan":true}`))
	if err != nil {
		t.Fatalf("开关请求失败: %v", err)
	}
	defer resp.Body.Close()
	var j struct {
		OK      bool `json:"ok"`
		Changed bool `json:"changed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		t.Fatalf("响应体在重新监听时被切断: %v", err)
	}
	if resp.StatusCode != 200 || !j.OK || !j.Changed {
		t.Fatalf("settings = %d %+v, 期望 200 ok+changed", resp.StatusCode, j)
	}
	// The reply is flushed BEFORE Rebind runs (that ordering is the point of
	// the test), so the LAN listener may still be opening when the follow-up
	// request leaves: a single-shot assert loses that race on loaded runners
	// (v1.7.1's build-macos saw connection refused). Poll until reachable.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, err := getHealth(fmt.Sprintf("http://%s:%d/health", ip, port))
		if err == nil && code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("切换后局域网不可达: code=%d err=%v", code, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLANBaseURLOnlyWhenAllowed(t *testing.T) {
	ip := routableIPv4()
	if ip == "" {
		t.Skip("no LAN interface on this machine")
	}
	s, port := lanTestServer(t, false)
	if got := s.LANBaseURL(); got != "" {
		t.Fatalf("LANBaseURL() = %q, 关闭时应为空", got)
	}
	s.Store.Config().AllowLan = true
	want := fmt.Sprintf("http://%s:%d/v1", ip, port)
	if got := s.LANBaseURL(); got != want {
		t.Fatalf("LANBaseURL() = %q, 期望 %q", got, want)
	}
}
