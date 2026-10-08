package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func TestFetchModelsOpenAI(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("authorization") != "Bearer sk-abc" {
			t.Errorf("auth = %q", r.Header.Get("authorization"))
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"m-one"},{"id":"m-two"}]}`)
	}))
	defer up.Close()
	ids, err := FetchModels(context.Background(), up.URL+"/v1", "sk-abc", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "m-one" || ids[1] != "m-two" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestFetchModelsAnthropic(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "ak-1" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version")
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-x","type":"model"}]}`)
	}))
	defer up.Close()
	ids, err := FetchModels(context.Background(), up.URL+"/v1", "ak-1", "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "claude-x" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestFetchModelCatalogKeepsDeclaredCapacities(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"nvidia/m1","max_input_tokens":131072,"max_output_tokens":4096},
			{"id":"nvidia/m2"}]}`)
	}))
	defer up.Close()
	rows, err := FetchModelCatalog(context.Background(), up.URL+"/v1", "sk-abc", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0].ContextWindow != 131072 || rows[0].MaxOutput != 4096 {
		t.Errorf("declared capacities dropped: %+v", rows[0])
	}
	if rows[1].ID != "nvidia/m2" || rows[1].ContextWindow != 0 || rows[1].MaxOutput != 0 {
		t.Errorf("a model without numbers must still come back, unnumbered: %+v", rows[1])
	}
}

func TestFetchModelCatalogRejectsEmptyListing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer up.Close()
	if _, err := FetchModelCatalog(context.Background(), up.URL+"/v1", "sk-abc", "openai"); err == nil {
		t.Fatal("an empty listing must error — it means the Base URL or Key is wrong")
	}
}

func TestRecommendedModelsNVIDIA(t *testing.T) {
	// Real NVIDIA NIM catalog subset (Oct 2026) incl. junk and small models.
	cat := []string{
		"deepseek-ai/deepseek-v4.1-flash", "moonshotai/kimi-k3", "z-ai/glm-5.3",
		"nvidia/nemotron-3-ultra-550b-a55b", "nvidia/nemotron-3-super-120b-a12b",
		"openai/gpt-oss-20b", "poolside/laguna-xs-2.1", "meta/llama-3.2-90b-vision-instruct",
		"mistralai/mistral-7b-instruct-v0.3", "deepseek-ai/deepseek-coder-6.7b-instruct",
		"nvidia/llama-3.1-nemoguard-8b-content-safety", "nvidia/nemotron-3-embed-1b",
		"nvidia/nemotron-parse", "google/codegemma-7b", "meta/codellama-70b", "meta/llama2-70b",
	}
	rec := RecommendedModels("", "https://integrate.api.nvidia.com/v1", cat)
	want := map[string]bool{
		"deepseek-ai/deepseek-v4.1-flash":   true,
		"nvidia/nemotron-3-super-120b-a12b": true,
		"nvidia/nemotron-3-ultra-550b-a55b": true,
	}
	if len(rec) != len(want) {
		t.Fatalf("recommended = %v", rec)
	}
	for _, m := range rec {
		if !want[m] {
			t.Fatalf("unexpected recommendation %q in %v", m, rec)
		}
	}
}

func TestRecommendedModelsZhipu(t *testing.T) {
	cat := []string{"glm-4.5", "glm-4.6", "glm-5.3", "glm-5.3-flash", "glm-5.3-flashx",
		"glm-4-flash", "glm-4-flash-250414", "glm-4-flashx", "glm-4.5-flash", "glm-4v-flash"}
	rec := RecommendedModels("zhipu", "https://open.bigmodel.cn/api/paas/v4", cat)
	want := map[string]bool{"glm-4-flash": true, "glm-4-flash-250414": true,
		"glm-4.5-flash": true, "glm-4v-flash": true}
	if len(rec) != len(want) {
		t.Fatalf("recommended = %v", rec)
	}
	for _, m := range rec {
		if !want[m] {
			t.Fatalf("unexpected recommendation %q (paid glm-4-flashx must not match)", m)
		}
	}
}

func TestRecommendedModelsSiliconFlow(t *testing.T) {
	cat := []string{"Qwen/Qwen2.5-7B-Instruct", "Qwen/Qwen3-8B", "Qwen/Qwen3-14B",
		"Qwen/Qwen3-32B", "Qwen/Qwen2.5-72B-Instruct-128K", "Qwen/Qwen3.5-4B",
		"THUDM/GLM-4-9B-0414", "deepseek-ai/DeepSeek-V4-Flash", "Pro/deepseek-ai/DeepSeek-V3",
		"Qwen/Qwen3-Embedding-8B", "FunAudioLLM/SenseVoiceSmall"}
	rec := RecommendedModels("", "https://api.siliconflow.cn/v1", cat)
	want := map[string]bool{"Qwen/Qwen2.5-7B-Instruct": true, "Qwen/Qwen3-8B": true,
		"Qwen/Qwen3-14B": true, "Qwen/Qwen3-32B": true}
	if len(rec) != len(want) {
		t.Fatalf("recommended = %v", rec)
	}
	for _, m := range rec {
		if !want[m] {
			t.Fatalf("unexpected recommendation %q (72B/3.5/Pro/embed/audio must not match)", m)
		}
	}
}

func TestRecommendedModelsModelScope(t *testing.T) {
	cat := []string{"deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4-Pro-0813",
		"deepseek-ai/DeepSeek-V4.1-Flash", "deepseek-ai/DeepSeek-V4-Flash-0731",
		"ZhipuAI/GLM-5.2", "ZhipuAI/GLM-4.7-Flash", "Qwen/Qwen3.5-397B-A17B",
		"Qwen/Qwen3.5-122B-A10B", "Qwen/Qwen3.8-Flash-Next", "stepfun-ai/Step-3.7-Flash",
		"MiniMax/MiniMax-M3", "meituan-longcat/LongCat-Flash-Lite",
		"PaddlePaddle/ERNIE-4.5-300B-A47B-PT", "mistralai/Mistral-Large-Instruct-2407",
		"Shanghai_AI_Laboratory/Intern-S2-Preview"}
	rec := RecommendedModels("", "https://api-inference.modelscope.cn/v1", cat)
	for _, m := range []string{"deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4-Pro-0813",
		"deepseek-ai/DeepSeek-V4.1-Flash", "ZhipuAI/GLM-5.2", "Qwen/Qwen3.5-397B-A17B",
		"Qwen/Qwen3.5-122B-A10B", "Qwen/Qwen3.8-Flash-Next", "stepfun-ai/Step-3.7-Flash"} {
		found := false
		for _, r := range rec {
			if r == m {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %q in %v", m, rec)
		}
	}
	for _, m := range rec {
		if m == "MiniMax/MiniMax-M3" || m == "PaddlePaddle/ERNIE-4.5-300B-A47B-PT" ||
			m == "mistralai/Mistral-Large-Instruct-2407" {
			t.Fatalf("stale model %q recommended", m)
		}
	}
}

func TestRecommendedModelsUnknownHeuristic(t *testing.T) {
	cat := []string{"acme/embed-3b", "acme/safety-mini", "acme/llama2-70b",
		"acme/something-70b-instruct", "acme/tiny-7b"}
	rec := RecommendedModels("", "https://api.example.com/v1", cat)
	if len(rec) != 1 || rec[0] != "acme/something-70b-instruct" {
		t.Fatalf("recommended = %v", rec)
	}
}

func TestFetchModelsHTTPError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer up.Close()
	_, err := FetchModels(context.Background(), up.URL, "nope", "openai")
	if err == nil {
		t.Fatal("want error for 401")
	}
}

func TestProbeModelVerdicts(t *testing.T) {
	// available: SSE stream answers
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "m1" {
			t.Errorf("model = %v", body["model"])
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	p := &store.Provider{ID: "t", BaseURL: up.URL, APIKey: "sk", Protocol: store.ProtocolOpenAI}
	r := ProbeModel(context.Background(), p, "m1")
	if r.State != lane.StateAvailable || r.TTFTMs <= 0 {
		t.Fatalf("probe = %+v", r)
	}

	// unavailable: bad key → 401
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer bad.Close()
	p2 := &store.Provider{ID: "t", BaseURL: bad.URL, APIKey: "nope", Protocol: store.ProtocolOpenAI}
	r2 := ProbeModel(context.Background(), p2, "m1")
	if r2.State != lane.StateUnavailable {
		t.Fatalf("probe bad key = %+v", r2)
	}

	// throttled: 429
	t429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded"}}`)
	}))
	defer t429.Close()
	p3 := &store.Provider{ID: "t", BaseURL: t429.URL, APIKey: "sk", Protocol: store.ProtocolOpenAI}
	r3 := ProbeModel(context.Background(), p3, "m1")
	if r3.State != lane.StateThrottled {
		t.Fatalf("probe 429 = %+v", r3)
	}
}

func TestCompleteOpenAIWire(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "target-model" {
			t.Errorf("model = %v", body["model"])
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 2 { // system + user
			t.Errorf("messages = %d, want 2", len(msgs))
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"hello"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	p := &store.Provider{ID: "t", Name: "T", BaseURL: up.URL, APIKey: "sk", Protocol: store.ProtocolOpenAI, Enabled: true}
	var text string
	usage, finish, uerr := Complete(context.Background(), Turn{
		Provider: p, Model: "target-model",
		Messages: []lane.Message{
			{Role: lane.RoleSystem, Parts: []lane.Part{lane.TextPart{Text: "sys"}}},
			{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "hi"}}},
		},
		MaxTokens: 100,
	}, func(c lane.Chunk) {
		if c.Kind == lane.ChunkTextDelta {
			text += c.Delta
		}
	})
	if uerr != nil {
		t.Fatalf("uerr: %v", uerr)
	}
	if text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if finish != lane.FinishStop {
		t.Fatalf("finish = %q", finish)
	}
	if usage.Input != 9 || usage.Output != 1 {
		t.Fatalf("usage = %+v", usage)
	}
}
