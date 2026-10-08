package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// laneUpstream429 refuses everything on the free lane.
func laneUpstream429(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"FreeUsageLimitError"}}`)
	}))
}

// laneUpstreamByModel answers per model id: refusals for `refuse`, SSE
// success for everything else.
func laneUpstreamByModel(t *testing.T, refuse map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if refuse[body.Model] {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"FreeUsageLimitError"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
}

func withProvider(t *testing.T, s *Server, providerURL string, fallback bool) {
	t.Helper()
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "pv1", Name: "PV One", BaseURL: providerURL, Protocol: store.ProtocolOpenAI,
		Enabled: true, Models: []string{"gpt-4o-mini"},
	})
	cfg.LaneFallbackToProviders = fallback
	if err := s.Store.Save(); err != nil {
		t.Fatal(err)
	}
}

// postChat POSTs an authenticated chat completion through the server mux.
func postChat(t *testing.T, s *Server, body string) *http.Response {
	t.Helper()
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestProviderFallbackOnExhaustedLane(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	provUp := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"from provider"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":3}}`,
		"[DONE]",
	})
	defer provUp.Close()
	s := newTestServer(t, laneUp)
	withProvider(t, s, provUp.URL, true)

	// Park every other lane model so the first 429 exhausts the pool and the
	// turn becomes offerable to the provider.
	cat, _, _ := s.Lane.Snapshot()
	for _, m := range cat {
		if m.ID != "mimo-v2.6-flash-free" {
			s.Lane.MarkThrottled(m.ID, 120)
		}
	}

	body := `{"model":"mimo-v2.6-flash-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	resp := postChat(t, s, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (fallback must rescue an exhausted lane)", resp.StatusCode)
	}
	if got := resp.Header.Get("x-zen-gate-served-by"); got != "pv1/gpt-4o-mini" {
		t.Fatalf("served-by = %q, want pv1/gpt-4o-mini", got)
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
	if out.Model != "pv1/gpt-4o-mini" || !strings.Contains(out.Choices[0].Message.Content, "provider") {
		t.Fatalf("body = %+v", out)
	}
}

func TestProviderFallbackOffByDefault(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	provUp := fakeUpstream(t, []string{`{"choices":[{"delta":{"role":"assistant","content":"x"}}]}`, "[DONE]"})
	defer provUp.Close()
	s := newTestServer(t, laneUp)
	withProvider(t, s, provUp.URL, false)

	cat, _, _ := s.Lane.Snapshot()
	for _, m := range cat {
		if m.ID != "mimo-v2.6-flash-free" {
			s.Lane.MarkThrottled(m.ID, 120)
		}
	}
	body := `{"model":"mimo-v2.6-flash-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	resp := postChat(t, s, body)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 (fallback is opt-in)", resp.StatusCode)
	}
}

func TestVisionAwareFailover(t *testing.T) {
	// mimo* are parked; the requested nemotron refuses. Smart routing must
	// skip the text-only ling and land on space-bunny (vision-capable).
	laneUp := laneUpstreamByModel(t, map[string]bool{
		"nemotron-3-ultra-free": true,
	})
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	s.Lane.MarkThrottled("mimo-v2.6-flash-free", 120)
	s.Lane.MarkThrottled("mimo-v2.5-free", 120)

	body := `{"model":"nemotron-3-ultra-free","stream":false,"messages":[{"role":"user","content":[
		{"type":"text","text":"这是什么"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	resp := postChat(t, s, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("x-zen-gate-served-by"); got != "space-bunny-free" {
		t.Fatalf("smart routing served-by = %q, want space-bunny-free", got)
	}
}

func TestSmartRoutingOffDropsModalityFilter(t *testing.T) {
	// Same scenario with the master switch off: legacy behaviour picks the
	// next catalog model regardless of the image in the request.
	laneUp := laneUpstreamByModel(t, map[string]bool{
		"nemotron-3-ultra-free": true,
	})
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	s.Lane.SetSmartRouting(false)
	s.Lane.MarkThrottled("mimo-v2.6-flash-free", 120)
	s.Lane.MarkThrottled("mimo-v2.5-free", 120)

	body := `{"model":"nemotron-3-ultra-free","stream":false,"messages":[{"role":"user","content":[
		{"type":"text","text":"这是什么"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	resp := postChat(t, s, body)
	if got := resp.Header.Get("x-zen-gate-served-by"); got != "ling-3.0-flash-fin-free" {
		t.Fatalf("legacy served-by = %q, want ling-3.0-flash-fin-free", got)
	}
}

func TestModalityCapsMerging(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	// Name heuristics only.
	caps := s.modalityCapsOf("gpt-4o-mini")
	if !caps.Known || !caps.Vision || caps.Source != store.TagSourceHeuris {
		t.Fatalf("heuristic caps = %+v", caps)
	}
	// A stored tag overrides the heuristics.
	s.Store.SetModelTag("gpt-4o-mini", store.ModelTag{Vision: false, Audio: false, File: false, Source: store.TagSourceAI})
	caps = s.modalityCapsOf("gpt-4o-mini")
	if !caps.Known || caps.Vision || caps.Source != store.TagSourceAI {
		t.Fatalf("AI tag caps = %+v", caps)
	}
	// Unknown id with no tag: unknown.
	if caps := s.modalityCapsOf("my-private-llm"); caps.Known {
		t.Fatalf("unknown caps = %+v", caps)
	}
}

func TestModalityCapsCarriesReasoningAndDeclaredNumbers(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	// An AI verdict about reasoning must survive into the merged caps.
	s.Store.SetModelTag("r1-lite", store.ModelTag{Reasoning: true, Source: store.TagSourceAI})
	if caps := s.modalityCapsOf("r1-lite"); !caps.Reasoning {
		t.Errorf("AI reasoning verdict dropped: %+v", caps)
	}
	// Name heuristics alone should still flag a reasoning family.
	if caps := s.modalityCapsOf("deepseek-r1-distill"); !caps.Reasoning {
		t.Errorf("heuristic reasoning dropped: %+v", caps)
	}

	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "nim", Name: "N", BaseURL: "http://n", Protocol: store.ProtocolOpenAI,
		Enabled: true, Models: []string{"nvidia/m1"},
		ModelMeta: map[string]store.ModelMeta{"nvidia/m1": {ContextWindow: 131072, MaxOutput: 4096}},
	})
	caps := s.modalityCapsOf("nvidia/m1")
	if caps.ContextWindow != 131072 || caps.MaxOutput != 4096 {
		t.Errorf("provider-declared capacities not merged: %+v", caps)
	}
	// Numbers merge per field: a probe that measured the window but not the
	// output cap must not throw away the provider's stated output cap.
	s.Store.SetModelTag("nvidia/m1", store.ModelTag{ContextWindow: 65536, Source: store.TagSourceProbe})
	if caps := s.modalityCapsOf("nvidia/m1"); caps.ContextWindow != 65536 || caps.MaxOutput != 4096 {
		t.Errorf("probe verdict must outrank the listing per field: %+v", caps)
	}
}

func TestModalityCapsListingOutranksNameGuess(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	// `^glm-5` is in the vision heuristics; this provider states its model is
	// text-in only, which is a better answer than a regex on the name.
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "intern", Name: "Intern", BaseURL: "http://i", Protocol: store.ProtocolAnthropic,
		Enabled: true, Models: []string{"glm-5.3"},
		ModelMeta: map[string]store.ModelMeta{"glm-5.3": {
			Name: "GLM-5.3", ContextWindow: 1048576, Reasoning: true,
			InputDeclared: true, OutputDeclared: true,
		}},
	})
	caps := s.modalityCapsOf("glm-5.3")
	if caps.Vision {
		t.Errorf("the listing says text-only but the name heuristic still won: %+v", caps)
	}
	if !caps.Reasoning || !caps.Known || caps.Source != store.TagSourceListing {
		t.Errorf("declared modalities not used: %+v", caps)
	}
	// A live probe still beats the provider's own claim.
	s.Store.SetModelTag("glm-5.3", store.ModelTag{Vision: true, Source: store.TagSourceProbe})
	if caps := s.modalityCapsOf("glm-5.3"); !caps.Vision || caps.Source != store.TagSourceProbe {
		t.Errorf("probe verdict must outrank the listing: %+v", caps)
	}
	// An AI guess must not outrank it either.
	s.Store.SetModelTag("glm-5.3", store.ModelTag{Vision: true, Source: store.TagSourceAI})
	if caps := s.modalityCapsOf("glm-5.3"); caps.Vision || caps.Source != store.TagSourceListing {
		t.Errorf("AI tag outranked the provider's own statement: %+v", caps)
	}
}

func TestProviderFallbackPicks(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers,
		store.Provider{ID: "pva", Name: "A", BaseURL: "http://a", Protocol: store.ProtocolOpenAI,
			Enabled: true, Models: []string{"text-only-llm", "gpt-4o"}},
	)
	cfg.Providers = append(cfg.Providers,
		store.Provider{ID: "pvb", Name: "B", BaseURL: "http://b", Protocol: store.ProtocolOpenAI,
			Enabled: true, Models: []string{"qwen2.5-vl-72b"}},
	)
	// A probe verdict can veto a model the heuristics would have allowed.
	s.Store.SetModelTag("gpt-4o", store.ModelTag{Vision: false, Source: store.TagSourceProbe})
	_ = s.Store.Save()

	needs := lane.Needs{Image: true}
	picks := s.providerFallbackPicks(needs, "some-lane-model")
	// gpt-4o is vetoed by the probe verdict (vision=false); qwen matches the
	// heuristics and leads; the unknown text-only-llm trails as a soft pick.
	if len(picks) != 2 || picks[0].p.ID != "pvb" || picks[0].model != "qwen2.5-vl-72b" ||
		picks[1].model != "text-only-llm" {
		t.Fatalf("picks = %+v, want pvb/qwen2.5-vl-72b then pva/text-only-llm", picks)
	}

	// The probe veto applies only to smart routing; off, everything is listed
	// in provider order.
	s.Store.Config().SmartRouting = false
	picks = s.providerFallbackPicks(needs, "some-lane-model")
	if len(picks) != 3 || picks[0].p.ID != "pva" || picks[0].model != "text-only-llm" {
		t.Fatalf("legacy picks = %+v", picks)
	}
}
