package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// rungUpstream answers a chat stream and reports the reasoning_effort it got.
func rungUpstream(t *testing.T) (*httptest.Server, func() any) {
	t.Helper()
	var got any
	seen := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got, seen = body["reasoning_effort"], true
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(up.Close)
	return up, func() any {
		if !seen {
			t.Fatal("the provider never saw a request")
		}
		return got
	}
}

func rungProvider(up *httptest.Server, meta store.ModelMeta) store.Provider {
	return store.Provider{
		ID: "qoder", Name: "Qoder 账号渠道", BaseURL: up.URL + "/v1",
		APIKey: "k", Protocol: store.ProtocolOpenAI, Enabled: true,
		Models:    []string{"Qwen3.8-Flash"},
		ModelMeta: map[string]store.ModelMeta{"Qwen3.8-Flash": meta},
	}
}

// askRelay posts one chat completion through the gateway's real router.
func askRelay(t *testing.T, s *Server, model, effort string) {
	t.Helper()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	body := `{"model":` + fmt.Sprintf("%q", model) + `,"messages":[{"role":"user","content":"hi"}],"stream":false`
	if effort != "" {
		body += `,"reasoning_effort":` + fmt.Sprintf("%q", effort)
	}
	body += `}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		sink, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, string(sink))
	}
}

// The picker's word must reach the vendor in the vendor's own naming: a request
// for "deep" on a source whose rungs are low/medium/xhigh is a request for
// xhigh, and one that already says xhigh must not be translated at all.
func TestRelaySendsTheVendorRungThePickerChose(t *testing.T) {
	cases := []struct {
		name, effort, want string
	}{
		{"gateway word maps up", "deep", "xhigh"},
		{"gateway middle maps to the vendor's middle", "balanced", "medium"},
		{"the vendor's own word stays", "xhigh", "xhigh"},
		// The dashboard promises the 设置 default applies to every agent when
		// the request states no level — and a source that publishes rungs can
		// honour that promise, so it is kept here instead of dropped.
		{"no word declared sends the configured default", "", "medium"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, got := rungUpstream(t)
			s := newTestServer(t, up)
			cfg := s.Store.Config()
			cfg.Providers = append(cfg.Providers, rungProvider(up, store.ModelMeta{
				Reasoning: true, InputDeclared: true,
				Efforts: []string{"low", "medium", "xhigh"}, EffortDefault: "medium",
			}))
			askRelay(t, s, "qoder/Qwen3.8-Flash", c.effort)
			if v, _ := got().(string); v != c.want {
				t.Fatalf("upstream reasoning_effort = %q, want %q", v, c.want)
			}
		})
	}
}

// "off" is not a lighter rung: a caller that switched thinking off must not be
// served the vendor's lightest level, which still thinks.
func TestRelayKeepsThinkingOff(t *testing.T) {
	up, got := rungUpstream(t)
	s := newTestServer(t, up)
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, rungProvider(up, store.ModelMeta{
		Reasoning: true, InputDeclared: true,
		Efforts: []string{"low", "medium", "xhigh"}, EffortDefault: "medium",
	}))
	askRelay(t, s, "qoder/Qwen3.8-Flash", "none")
	if v, _ := got().(string); v != "none" {
		t.Fatalf("upstream reasoning_effort = %q, want none", v)
	}
}

// A provider that publishes no rungs is nobody's business to relabel: the
// caller's own word goes through and the gateway invents nothing when the
// caller stated nothing.
func TestRelayPassesThroughForAProviderWithoutRungs(t *testing.T) {
	up, got := rungUpstream(t)
	s := newTestServer(t, up)
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, rungProvider(up, store.ModelMeta{
		Reasoning: true, InputDeclared: true, ContextWindow: 131072,
	}))
	askRelay(t, s, "qoder/Qwen3.8-Flash", "xhigh")
	if v, _ := got().(string); v != "xhigh" {
		t.Fatalf("upstream reasoning_effort = %q, want the caller's xhigh untouched", v)
	}
}
