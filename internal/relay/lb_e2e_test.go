package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// TestCompleteLoadBalancesKeys proves the end-to-end path: a provider with
// several keys actually spreads requests across them instead of using one. The
// mock upstream records the Authorization header of every turn.
func TestCompleteLoadBalancesKeys(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("authorization"))
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	p := &store.Provider{ID: "lb-e2e", Name: "lb", BaseURL: up.URL + "/v1",
		Protocol: store.ProtocolOpenAI, APIKeys: []string{"k1", "k2", "k3"}}

	msg := []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "hi"}}}}
	for i := 0; i < 9; i++ {
		_, _, uerr := Complete(context.Background(), Turn{
			Provider: p, Model: "m", Messages: msg,
		}, func(lane.Chunk) {})
		if uerr != nil {
			t.Fatalf("turn %d error: %v", i, uerr)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 9 {
		t.Fatalf("expected 9 requests, got %d", len(auths))
	}
	norm := map[string]int{}
	for _, a := range auths {
		switch a {
		case "Bearer k1", "Bearer k2", "Bearer k3":
			norm[a]++
		default:
			t.Fatalf("unexpected auth header %q", a)
		}
	}
	if norm["Bearer k1"] != 3 || norm["Bearer k2"] != 3 || norm["Bearer k3"] != 3 {
		t.Fatalf("keys not evenly balanced across 9 turns: %v", norm)
	}
}
