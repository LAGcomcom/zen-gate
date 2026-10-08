package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// addProvider wires a fake upstream into the store config as a custom
// provider; returns nothing — callers close the server themselves.
func addProvider(t *testing.T, s *Server, up *httptest.Server, protocol, key string, enabled bool, models ...string) {
	t.Helper()
	p := store.Provider{
		ID: "prov", Name: "测试供应商", BaseURL: up.URL + "/v1",
		APIKey: key, Protocol: protocol, Enabled: enabled, Models: models,
	}
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, p)
}

func TestProviderChatRelay(t *testing.T) {
	var gotAuth, gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"provider says hi"}}]}`)
		fmt.Fprintln(w, ``)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk-user-key", true, "deepseek-ai/deepseek-r1")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"prov/deepseek-ai/deepseek-r1","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotAuth != "Bearer sk-user-key" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
	if gotModel != "deepseek-ai/deepseek-r1" {
		t.Fatalf("upstream model = %q", gotModel)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content != "provider says hi" {
		t.Fatalf("content = %q", out.Choices[0].Message.Content)
	}
	if out.Model != "prov/deepseek-ai/deepseek-r1" {
		t.Fatalf("served model = %q", out.Model)
	}
}

func TestProviderAnthropicRelay(t *testing.T) {
	var gotKey, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		if v := r.Header.Get("anthropic-version"); v == "" {
			t.Error("missing anthropic-version header")
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":4}}}`)
		fmt.Fprintln(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"claude 好的"}}`)
		fmt.Fprintln(w, `data: {"type":"content_block_stop","index":0}`)
		fmt.Fprintln(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
	}))
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolAnthropic, "ak-user-key", true, "longcat-flash")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"prov/longcat-flash","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotKey != "ak-user-key" {
		t.Fatalf("upstream key = %q", gotKey)
	}
	var out struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Content) == 0 || out.Content[0].Text != "claude 好的" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Usage.InputTokens != 4 || out.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestProviderModelsMerged(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", true, "model-a", "model-b")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, m := range out.Data {
		found[m.ID] = m.OwnedBy
	}
	if found["prov/model-a"] != "测试供应商" || found["prov/model-b"] != "测试供应商" {
		t.Fatalf("provider models missing from listing: %v", found)
	}
}

func TestProviderDisabledRejected(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", false, "model-a")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"prov/model-a","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestMigrateProviderSelectionsTrimsDump(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	cfg.SchemaVersion = 3 // pre-selection build
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "nv", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1",
		Protocol: store.ProtocolOpenAI, Enabled: true,
		Models: []string{"deepseek-ai/deepseek-v4.1-flash", "moonshotai/kimi-k3",
			"z-ai/glm-5.3", "nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b",
			"nvidia/nemotron-3-embed-1b", "mistralai/mistral-7b-instruct-v0.3"},
	})
	if n := MigrateProviderSelections(st); n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}
	got := st.Config().Providers[0].Models
	if len(got) != 3 {
		t.Fatalf("models after migration = %v, want the 3 recommended", got)
	}
	if st.Config().SchemaVersion != 5 {
		t.Fatalf("schema = %d, want 5", st.Config().SchemaVersion)
	}
	if n := MigrateProviderSelections(st); n != 0 {
		t.Fatalf("second migration should no-op, got %d", n)
	}
}

func TestProviderModelsRefreshPrunesToSelection(t *testing.T) {
	// Upstream offers m1..m4; provider has m1, m3 checked. 刷新 must keep
	// m1 (still upstream) and m3, and NOT add m2/m4.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2"},{"id":"m3"},{"id":"m4"}]}`)
	}))
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", true, "m1", "m3")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/admin/api/providers/models",
		strings.NewReader(`{"id":"prov"}`))
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	models := s.Store.Config().Providers[0].Models
	if len(models) != 2 || models[0] != "m1" || models[1] != "m3" {
		t.Fatalf("models after refresh = %v, want [m1 m3]", models)
	}
}

// 刷新模型 must not rewrite a config snapshot it handed out earlier: a request
// already in flight reads its provider out of exactly such a snapshot, and the
// catalog merge also writes through the ModelMeta map the snapshot shares.
func TestProviderModelsRefreshLeavesHeldSnapshotAlone(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m1","max_input_tokens":131072}]}`)
	}))
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", true, "m1", "retired-model")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	held := s.Store.Config()
	postAdmin(t, ts.URL+"/admin/api/providers/models", `{"id":"prov"}`)

	got := held.Providers[0].Models
	if len(got) != 2 || got[1] != "retired-model" {
		t.Errorf("the refresh rewrote a config handed out before it: %v", got)
	}
	if held.Providers[0].ModelMeta != nil {
		t.Errorf("the refresh merged catalog metadata into a shared map: %v", held.Providers[0].ModelMeta)
	}
	// The published config must have changed, or nothing was tested.
	now := s.Store.Config().Providers[0].Models
	if len(now) != 1 || now[0] != "m1" {
		t.Fatalf("published models = %v, want [m1]", now)
	}
}

// declaredListing offers m1 with published token capacities, m2 without any.
func declaredListing() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m1","max_input_tokens":131072,"max_output_tokens":4096},{"id":"m2"}]}`)
	}))
}

func postAdmin(t *testing.T, baseURL, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", baseURL, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode admin response: %v", err)
	}
	return out
}

func TestProviderModelsRefreshPersistsDeclaredCapacities(t *testing.T) {
	up := declaredListing()
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", true, "m1")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	j := postAdmin(t, ts.URL+"/admin/api/providers/models", `{"id":"prov"}`)
	if !j["ok"].(bool) {
		t.Fatalf("response = %v", j)
	}
	meta := s.Store.Config().Providers[0].ModelMeta
	if meta["m1"].ContextWindow != 131072 || meta["m1"].MaxOutput != 4096 {
		t.Errorf("provider kept no capacities for m1: %+v", meta)
	}
	if _, ok := meta["m2"]; ok {
		t.Errorf("an unchecked model must not enter the provider's catalog: %+v", meta)
	}
	// The picker needs the whole listing, numbers included, so a model the
	// user has not ticked yet can be saved with its capacities in one step.
	cat, _ := j["catalog"].([]any)
	if len(cat) != 2 {
		t.Fatalf("catalog = %v, want both listed models", j["catalog"])
	}
	first := cat[0].(map[string]any)
	if first["id"] != "m1" || first["contextWindow"].(float64) != 131072 {
		t.Errorf("catalog row lost its numbers: %v", first)
	}
}

func TestProviderSaveStoresCatalogOfSelectedModels(t *testing.T) {
	up := declaredListing()
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	j := postAdmin(t, ts.URL+"/admin/api/providers", `{"name":"NIM","baseUrl":"`+up.URL+`/v1",
		"apiKey":"sk","protocol":"openai","enabled":true,"models":["m1"],
		"catalog":[{"id":"m1","contextWindow":131072,"maxOutput":4096},
			{"id":"m2","contextWindow":8192}]}`)
	if !j["ok"].(bool) {
		t.Fatalf("response = %v", j)
	}
	if len(s.Store.Config().Providers) != 1 {
		t.Fatalf("provider not created: %v", s.Store.Config().Providers)
	}
	meta := s.Store.Config().Providers[0].ModelMeta
	if meta["m1"].ContextWindow != 131072 || meta["m1"].MaxOutput != 4096 {
		t.Errorf("saved provider lost m1's capacities: %+v", meta)
	}
	if _, ok := meta["m2"]; ok {
		t.Errorf("m2 was not selected, so its numbers must not be kept: %+v", meta)
	}
}

func TestAdminStateShowsCustomModelCapacities(t *testing.T) {
	type cardRow struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Reasoning     bool   `json:"reasoning"`
		ContextWindow int    `json:"contextWindow"`
		MaxOutput     int    `json:"maxOutput"`
		Custom        bool   `json:"custom"`
	}
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	s.Store.SetModelTag("deepseek-r1", store.ModelTag{Reasoning: true, Source: store.TagSourceAI})
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "nim", Name: "N", BaseURL: "http://n", Protocol: store.ProtocolOpenAI,
		Enabled: true, Models: []string{"deepseek-r1", "nvidia/m1"},
		ModelMeta: map[string]store.ModelMeta{"nvidia/m1": {Name: "Nemotron M1", ContextWindow: 131072, MaxOutput: 4096}},
	})
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Models []cardRow `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	rows := map[string]cardRow{}
	for _, r := range st.Models {
		rows[r.ID] = r
	}
	nim := rows["nim/nvidia/m1"]
	if !nim.Custom {
		t.Fatalf("nim/nvidia/m1 missing: %v", st.Models)
	}
	if nim.ContextWindow != 131072 || nim.MaxOutput != 4096 {
		t.Errorf("card lost the provider's declared capacities: %+v", nim)
	}
	if nim.Name != "Nemotron M1" {
		t.Errorf("card title = %q, want the provider's own display name", nim.Name)
	}
	if r := rows["nim/deepseek-r1"]; !r.Reasoning {
		t.Errorf("card lost the AI reasoning verdict: %+v", r)
	}
}

func TestModelVisibilityFiltering(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	vis := func(body string) {
		req, _ := http.NewRequest("POST", ts.URL+"/admin/api/models/visibility", strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	advertised := func() map[string]bool {
		req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
		req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, m := range out.Data {
			set[m.ID] = true
		}
		return set
	}

	vis(`{"id":"mimo-v2.6-flash-free","hidden":true}`)
	if got := s.Store.Config().HiddenModels; len(got) != 1 || got[0] != "mimo-v2.6-flash-free" {
		t.Fatalf("hidden = %v", got)
	}
	if advertised()["mimo-v2.6-flash-free"] {
		t.Fatal("hidden model still advertised")
	}
	for _, m := range s.VisibleModels() {
		if m.ID == "mimo-v2.6-flash-free" {
			t.Fatal("hidden model still in VisibleModels")
		}
	}

	vis(`{"id":"mimo-v2.6-flash-free","hidden":false}`)
	if len(s.Store.Config().HiddenModels) != 0 {
		t.Fatalf("hidden after restore = %v", s.Store.Config().HiddenModels)
	}
	if !advertised()["mimo-v2.6-flash-free"] {
		t.Fatal("restored model not advertised")
	}
}

func TestCustomModelProbeFlow(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", true, "m1")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/admin/api/probe/"+url.PathEscape("prov/m1"), nil)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("probe status %d", resp.StatusCode)
	}
	if pr, ok := s.customProbeOf("prov/m1"); !ok || pr.State != "available" || pr.TTFTMs <= 0 {
		t.Fatalf("stored probe = %+v ok=%v", pr, ok)
	}
	if _, n := s.Store.TTFTStats("prov/m1"); n != 1 {
		t.Fatalf("ttft samples = %d, want 1", n)
	}

	// state reflects the verdict
	stReq, _ := http.NewRequest("GET", ts.URL+"/admin/api/state", nil)
	stReq.Header.Set("content-type", "application/json")
	stResp, err := http.DefaultClient.Do(stReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stResp.Body.Close()
	var st struct {
		Models []struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			TTFTMs    int64  `json:"ttftMs"`
			TTFTMedMs int64  `json:"ttftMedMs"`
			Custom    bool   `json:"custom"`
		} `json:"models"`
	}
	if err := json.NewDecoder(stResp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Models {
		if m.ID != "prov/m1" {
			continue
		}
		if !m.Custom || m.State != "available" || m.TTFTMs <= 0 || m.TTFTMedMs <= 0 {
			t.Fatalf("state row = %+v", m)
		}
		return
	}
	t.Fatal("custom model missing from state")
}

func TestProviderEffortSuffixStripped(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()
	s := newTestServer(t, up)
	addProvider(t, s, up, store.ProtocolOpenAI, "sk", true, "qwen/qwen3-coder")
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"prov/qwen/qwen3-coder (deep)","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotModel != "qwen/qwen3-coder" {
		t.Fatalf("upstream model = %q, want effort suffix stripped", gotModel)
	}
}
