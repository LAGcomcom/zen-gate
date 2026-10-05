// End-to-end check: a real local OpenAI-compatible server behind the gateway.
//
// Run manually against a live local model:
//
//	go test ./internal/gateway/ -run TestLiveLocalUpstream -v -timeout 120s
//
// with ZEN_E2E_UPSTREAM set (e.g. http://127.0.0.1:8090). Skipped otherwise, so
// it never makes the normal suite depend on a model being loaded.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// liveUpstream returns the endpoint under test, or "" to skip.
func liveUpstream(t *testing.T) (string, string) {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("ZEN_E2E_UPSTREAM"))
	if base == "" {
		t.Skip("set ZEN_E2E_UPSTREAM to run (e.g. http://127.0.0.1:8090)")
	}
	model := strings.TrimSpace(os.Getenv("ZEN_E2E_MODEL"))
	if model == "" {
		model = "meissa-local"
	}
	return base, model
}

func e2eServer(t *testing.T, base, model string) *Server {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	cfg.Upstreams = []store.Upstream{{
		ID: "local", Name: "本地模型", BaseURL: base,
		Models: []store.UpstreamModel{{
			ID: model, Name: "Meissa", Reasoning: true,
			ContextWindow: 32768, MaxOutput: 4096,
		}},
	}}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return New(lane.NewLane(), st)
}

func post(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if k := s.Store.Config().MainKey; k != "" {
		req.Header.Set("authorization", "Bearer "+k)
	}
	ctx, cancel := context.WithTimeout(req.Context(), 90*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	s.route(rec, req.WithContext(ctx))
	return rec
}

// TestLiveLocalUpstreamStream is the whole point of the feature: a Codex-style
// streaming request for a local model goes out verbatim and comes back as real
// generated tokens.
func TestLiveLocalUpstreamStream(t *testing.T) {
	base, model := liveUpstream(t)
	s := e2eServer(t, base, model)

	rec := post(t, s, "/v1/chat/completions",
		`{"model":"`+model+`","stream":true,"max_tokens":48,`+
			`"messages":[{"role":"user","content":"用一句话说明什么是网关。"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	if got := rec.Header().Get("x-zen-gate-upstream"); got != "local" {
		t.Errorf("x-zen-gate-upstream = %q, want local", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Fatalf("no SSE frames: %s", truncateForLog(body))
	}
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if p == "[DONE]" {
			continue
		}
		var f struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(p), &f) == nil {
			text.WriteString(f.Choices[0].Delta.Content)
		}
	}
	if text.Len() == 0 {
		t.Fatalf("no content deltas decoded; raw: %s", truncateForLog(body))
	}
	t.Logf("PASS 流式生成 %d 字节: %s", text.Len(), text.String())
}

// TestLiveLocalUpstreamNonStream checks the buffered path an SDK would use.
func TestLiveLocalUpstreamNonStream(t *testing.T) {
	base, model := liveUpstream(t)
	s := e2eServer(t, base, model)

	rec := post(t, s, "/v1/chat/completions",
		`{"model":"`+model+`","max_tokens":32,`+
			`"messages":[{"role":"user","content":"说一个数字。"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Model string         `json:"model"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not OpenAI-shaped: %v — %s", err, truncateForLog(rec.Body.String()))
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		t.Fatalf("empty content: %s", truncateForLog(rec.Body.String()))
	}
	t.Logf("PASS 非流式: model=%s finish=%s usage=%v", out.Model, out.Choices[0].FinishReason, out.Usage)
	t.Logf("内容: %s", out.Choices[0].Message.Content)
}

// TestLiveLocalUpstreamCatalog checks the local model reaches the Codex picker
// catalog with every field Codex's schema demands — the "方案 B" outcome.
func TestLiveLocalUpstreamCatalog(t *testing.T) {
	base, model := liveUpstream(t)
	s := e2eServer(t, base, model)

	req := httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var mine map[string]any
	for _, m := range out.Models {
		if m["slug"] == model {
			mine = m
		}
	}
	if mine == nil {
		t.Fatalf("%s missing from catalog", model)
	}
	required := []string{"base_instructions", "supported_reasoning_levels", "shell_type",
		"truncation_policy", "experimental_supported_tools", "input_modalities",
		"visibility", "supported_in_api", "priority", "provider_id",
		"context_window", "max_output_tokens", "display_name", "description"}
	var missing []string
	for _, k := range required {
		if _, ok := mine[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("catalog entry missing %v — Codex rejects the whole catalog", missing)
	}
	if mine["provider_id"] != codexProviderID {
		t.Errorf("provider_id = %v, want %s (must match [model_providers.%s] in config.toml)",
			mine["provider_id"], codexProviderID, codexProviderID)
	}
	t.Logf("PASS 目录条目: display=%v ctx=%v max=%v priority=%v",
		mine["display_name"], mine["context_window"], mine["max_output_tokens"], mine["priority"])

	// And it must be discoverable through the OpenAI listing too.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	rec2 := httptest.NewRecorder()
	s.route(rec2, req2)
	if !strings.Contains(rec2.Body.String(), model) {
		t.Errorf("%s missing from /v1/models", model)
	}
	t.Logf("PASS /v1/models 含 %s", model)
}

func truncateForLog(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
