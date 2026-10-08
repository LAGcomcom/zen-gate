package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A streaming request that fails before the model says anything must answer
// with the same status the non-streaming request would: agents back off on 429
// + Retry-After. Writing the SSE headers first commits a 200, so the preamble
// has to wait until there is a byte of content to announce.
func laneRefusingStream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"FreeUsageLimitError","message":"usage limit"}}`)
	}))
}

// openStream POSTs a stream:true chat request and returns the raw reply.
func openStream(t *testing.T, s *Server, body string) (*http.Response, string) {
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

func parkAllModels(s *Server, keep string) {
	cat, _, _ := s.Lane.Snapshot()
	for _, m := range cat {
		if m.ID != keep {
			s.Lane.MarkThrottled(m.ID, 120)
		}
	}
}

func TestQuotaBeforeContentKeepsHttp429WhenStreaming(t *testing.T) {
	laneUp := laneRefusingStream(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	parkAllModels(s, "mimo-v2.6-flash-free")

	resp, raw := openStream(t, s, `{"model":"mimo-v2.6-flash-free","stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 — the agent's backoff reads the status, not the body:\n%s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("retry-after"); got == "" {
		t.Errorf("missing Retry-After header")
	}
	if ct := resp.Header.Get("content-type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want the JSON error document", ct)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("body is not the JSON error: %v\n%s", err, raw)
	}
	if out.Error.Message == "" {
		t.Errorf("error.message is empty: %s", raw)
	}
}

// The failure must not be swallowed for a provider turn either.
func TestQuotaBeforeContentKeepsHttp429ForProviderStream(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"quota exhausted"}}`)
	}))
	defer refuse.Close()
	s := newTestServer(t, fakeUpstream(t, nil))
	withProvider(t, s, refuse.URL, false)

	resp, raw := openStream(t, s, `{"model":"pv1/gpt-4o-mini","stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429:\n%s", resp.StatusCode, raw)
	}
	if strings.Contains(raw, "event-stream") {
		t.Errorf("the reply is an SSE stream: %s", raw)
	}
}

// Once content is on the wire the status is already committed, so the failure
// has to travel inside the stream — and the role preamble must still be the
// first chunk for clients that rely on it.
func TestStreamSuccessStillOpensWithRolePreamble(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	resp, raw := openStream(t, s, `{"model":"mimo-v2.6-flash-free","stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d\n%s", resp.StatusCode, raw)
	}
	first := strings.Split(strings.TrimSpace(raw), "\n")[0]
	var chunk struct {
		Choices []struct {
			Delta map[string]any `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(first, "data: ")), &chunk); err != nil {
		t.Fatalf("first frame %q: %v", first, err)
	}
	if chunk.Choices[0].Delta["role"] != "assistant" {
		t.Errorf("first delta = %v, want the assistant role preamble", chunk.Choices[0].Delta)
	}
}

// openMessages POSTs a /v1/messages request and returns the raw reply.
func openMessages(t *testing.T, s *Server, body string) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(s.mux)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(raw)
}

const anthropicStreamBody = `{"model":"mimo-v2.6-flash-free","stream":true,"max_tokens":64,` +
	`"messages":[{"role":"user","content":"hi"}]}`

// The Anthropic stream opened with message_start, which committed the 200 the
// same way the chat preamble did.
func TestAnthropicQuotaBeforeContentKeepsHttp429(t *testing.T) {
	laneUp := laneRefusingStream(t)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	parkAllModels(s, "mimo-v2.6-flash-free")

	resp, raw := openMessages(t, s, anthropicStreamBody)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429:\n%s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("retry-after"); got == "" {
		t.Errorf("missing Retry-After header")
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("body is not the Anthropic error document: %v\n%s", err, raw)
	}
	if out.Type != "error" || out.Error.Message == "" {
		t.Errorf("error document = %+v", out)
	}
}

// A successful stream still opens with message_start, and one that carries no
// content at all must not emit deltas without it.
func TestAnthropicStreamStillOpensWithMessageStart(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	resp, raw := openMessages(t, s, anthropicStreamBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d\n%s", resp.StatusCode, raw)
	}
	frames := strings.Split(strings.TrimSpace(raw), "\n")
	if !strings.Contains(frames[0], `"type":"message_start"`) {
		t.Fatalf("first frame = %q, want message_start", frames[0])
	}
}
