package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// Every custom provider request now carries a prompt-cache hint, and both
// hints are guesses about what the upstream accepts. The retry path below is
// what keeps a guess cheap: a 400 that names the field costs ONE re-send, and
// the verdict is then remembered on the provider so it is never guessed again.

// A 400 that names a cache field must be retried in the plain shape: no
// prompt_cache_key, no cache_control, and the request still succeeds.
func TestCacheRefusalRetriesWithoutTheHint(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf strings.Builder
		b := make([]byte, 4096)
		for {
			n, err := r.Body.Read(b)
			buf.Write(b[:n])
			if err != nil {
				break
			}
		}
		bodies = append(bodies, buf.String())
		if strings.Contains(buf.String(), "prompt_cache_key") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unexpected field prompt_cache_key"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	prov := &store.Provider{
		ID: "p1", Name: "p1", BaseURL: srv.URL, Protocol: store.ProtocolOpenAI,
		Enabled: true, Models: []string{"m"},
	}
	usage, finish, err := Complete(t.Context(), Turn{
		Provider: prov, Model: "m", Session: "conv-1",
		Messages: []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "hi"}}}},
	}, func(lane.Chunk) {})
	if err != nil {
		t.Fatalf("a cache-field refusal must be retried, got %v", err)
	}
	if finish != lane.FinishStop || usage.Output != 2 {
		t.Errorf("finish/usage = %q/%+v, want the retry's answer", finish, usage)
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want exactly one hinted try plus one plain retry", len(bodies))
	}
	if !strings.Contains(bodies[0], "prompt_cache_key") {
		t.Errorf("first attempt carried no hint: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "prompt_cache_key") {
		t.Errorf("retry still carried the refused field: %s", bodies[1])
	}
}

// An Anthropic-protocol provider that refuses the block shape gets the plain
// string system prompt back — the shape that worked before the hint existed.
func TestCacheRefusalRetriesAnthropicWithoutBreakpoints(t *testing.T) {
	var systems []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		raw, _ := json.Marshal(got["system"])
		systems = append(systems, string(raw))
		if strings.Contains(string(raw), "cache_control") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"message":"system: unexpected field cache_control"}}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	long := strings.Repeat("rule ", 900) // past the breakpoint threshold
	prov := &store.Provider{
		ID: "p2", Name: "p2", BaseURL: srv.URL, Protocol: store.ProtocolAnthropic,
		Enabled: true, Models: []string{"m"},
	}
	_, _, err := Complete(t.Context(), Turn{
		Provider: prov, Model: "m", Session: "conv-2",
		Messages: []lane.Message{{Role: lane.RoleSystem, Parts: []lane.Part{lane.TextPart{Text: long}}}},
	}, func(lane.Chunk) {})
	if err != nil {
		t.Fatalf("the block-shape refusal must be retried: %v", err)
	}
	if len(systems) != 2 {
		t.Fatalf("attempts = %d, want 2", len(systems))
	}
	if !strings.Contains(systems[0], "cache_control") {
		t.Errorf("first attempt carried no breakpoint: %s", systems[0])
	}
	if strings.Contains(systems[1], "cache_control") {
		t.Errorf("retry still carried a breakpoint: %s", systems[1])
	}
}

// A provider already marked NoCache must never see a hint in the first place,
// which is what makes the remembered verdict worth persisting.
func TestNoCacheProviderSendsNoHint(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf strings.Builder
		b := make([]byte, 4096)
		for {
			n, err := r.Body.Read(b)
			buf.Write(b[:n])
			if err != nil {
				break
			}
		}
		body = buf.String()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	prov := &store.Provider{
		ID: "p3", Name: "p3", BaseURL: srv.URL, Protocol: store.ProtocolOpenAI,
		Enabled: true, Models: []string{"m"}, NoCache: true,
	}
	if _, _, err := Complete(t.Context(), Turn{
		Provider: prov, Model: "m", Session: "conv-3",
		Messages: []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "hi"}}}},
	}, func(lane.Chunk) {}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if strings.Contains(body, "prompt_cache_key") {
		t.Errorf("a NoCache provider still got the field: %s", body)
	}
}

// Only a 4xx that NAMES a cache-ish field is worth re-sending. Replaying a bad
// key, an unknown model or a broken tool schema doubles the request count for
// an answer that cannot change.
//
// Note the codes: ClassifyFailure files a 400 as CodeServer + Unavailable (a
// per-model verdict), not CodeClient, so the code alone cannot separate "the
// upstream disliked our field" from "this model will not serve". The payload
// is what carries the distinction.
func TestCacheRefusalIgnoresUnrelatedClientErrors(t *testing.T) {
	cases := []*lane.UpstreamError{
		{Code: lane.CodeClient, Message: "invalid api key"},
		{Code: lane.CodeClient, Message: "model not found"},
		{Code: lane.CodeServer, Message: "The model is unavailable for this account"},
		{Code: lane.CodeCredential, Message: "cache_control unsupported"},
		{Code: lane.CodeTransport, Message: "prompt_cache_key: boom"},
	}
	for _, e := range cases {
		if cacheRefusal(e) {
			t.Errorf("%q/%q was treated as a cache refusal", e.Code, e.Message)
		}
	}
	if cacheRefusal(nil) {
		t.Error("a nil error was treated as a cache refusal")
	}
	for _, e := range []*lane.UpstreamError{
		{Code: lane.CodeClient, Message: "system: unexpected field cache_control"},
		{Code: lane.CodeServer, Message: `{"error":{"message":"unexpected field prompt_cache_key"}}`},
	} {
		if !cacheRefusal(e) {
			t.Errorf("%q was not recognised as a cache refusal", e.Message)
		}
	}
}
