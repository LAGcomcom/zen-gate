package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
)

// Issue #31: the streaming delta must carry the thinking under BOTH
// "reasoning" (OpenRouter-family readers) and "reasoning_content" (the
// DeepSeek/GLM-family de facto standard that ZCode-class clients parse).
// Emitting only "reasoning" while the non-streaming path emits only
// "reasoning_content" silently loses the thinking process on half the
// ecosystem.
func TestStreamReasonDeltaCarriesBothSpellings(t *testing.T) {
	e := &chatEmitter{declared: map[string]bool{}}
	out := e.stream(lane.Chunk{Kind: lane.ChunkReasonDelta, Delta: "The"})
	if len(out) != 1 {
		t.Fatalf("one delta chunk expected, got %d", len(out))
	}
	if got := out[0].delta["reasoning"]; got != "The" {
		t.Errorf(`delta["reasoning"] = %v, want "The"`, got)
	}
	if got := out[0].delta["reasoning_content"]; got != "The" {
		t.Errorf(`delta["reasoning_content"] = %v, want "The"`, got)
	}
}

// Issue #30: a chat body carrying base64 screenshots (the old 8MB cap
// rejected normal vision traffic) must be accepted, and the cap must be
// shared by all three wire handlers.
func TestRequestBodyCapAcceptsVisionTraffic(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	})
	defer up.Close()
	s := newTestServer(t, up)

	// ~12MB of base64 image payload — past the old 8MB cap, under 64MB.
	blob := strings.Repeat("A", 12<<20)
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "看这张图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + blob}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	req, err := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("12MB vision request rejected: %d", resp.StatusCode)
	}
}
