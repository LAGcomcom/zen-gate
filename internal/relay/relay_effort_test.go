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

// relayOneTurn posts one turn through a capturing upstream and returns the body
// that upstream actually received. The response is a well-formed stream for the
// protocol under test, so a failure here is about the request, not the stub.
func relayOneTurn(t *testing.T, protocol, effort string) map[string]any {
	t.Helper()
	var (
		body map[string]any
		seen bool
	)
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = true
		w.Header().Set("content-type", "text/event-stream")
		if protocol == store.ProtocolAnthropic {
			fmt.Fprintln(w, `event: message_start`)
			fmt.Fprintln(w, `data: {"type":"message_start","message":{"id":"1","role":"assistant","content":[]}}`)
			fmt.Fprintln(w, `event: content_block_delta`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
			fmt.Fprintln(w, `event: message_delta`)
			fmt.Fprintln(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
			fmt.Fprintln(w, `event: message_stop`)
			fmt.Fprintln(w, `data: {"type":"message_stop"}`)
			return
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	t.Cleanup(capture.Close)

	p := &store.Provider{ID: "t", Name: "T", BaseURL: capture.URL, APIKey: "sk",
		Protocol: protocol, Enabled: true}
	_, _, uerr := Complete(context.Background(), Turn{
		Provider: p, Model: "m", ReasoningEffort: effort,
		Messages:  []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "hi"}}}},
		MaxTokens: 100,
	}, func(lane.Chunk) {})
	if !seen {
		t.Fatal("the upstream never saw a request")
	}
	if uerr != nil {
		t.Fatalf("the relayed turn did not complete: %s", uerr.Message)
	}
	return body
}

// A caller that picked a thinking rung must have it reach the upstream: the
// picker was a no-op while the relay silently dropped the field.
func TestCompleteCarriesReasoningEffort(t *testing.T) {
	body := relayOneTurn(t, store.ProtocolOpenAI, "xhigh")
	if got := body["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh verbatim; body was %v", got, body)
	}
}

// Stating nothing adds nothing: a level the caller never asked for would be an
// unclaimed change to a request that already works.
func TestCompleteOmitsReasoningEffortWhenUnstated(t *testing.T) {
	body := relayOneTurn(t, store.ProtocolOpenAI, "")
	if v, ok := body["reasoning_effort"]; ok {
		t.Errorf("body carries reasoning_effort %v although nothing was declared", v)
	}
}

// The Anthropic wire has no level names — its thinking control is a budget
// object — so a rung must not be smuggled in as a field that endpoint rejects.
func TestCompleteAnthropicProtocolSendsNoRung(t *testing.T) {
	body := relayOneTurn(t, store.ProtocolAnthropic, "xhigh")
	if v, ok := body["reasoning_effort"]; ok {
		t.Errorf("anthropic body carries reasoning_effort %v", v)
	}
	if body["messages"] == nil {
		t.Errorf("anthropic body lost its messages: %v", body)
	}
}
