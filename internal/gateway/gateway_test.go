package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func fakeUpstream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if f == "[DONE]" {
				return
			}
		}
	}))
}

func newTestServer(t *testing.T, upstream *httptest.Server) *Server {
	t.Helper()
	old := lane.UpstreamBase
	lane.UpstreamBase = upstream.URL
	t.Cleanup(func() { lane.UpstreamBase = old })

	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return New(lane.NewLane(), st)
}

func TestChatCompletionsNonStream(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"！"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":false}`
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
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content != "你好！" {
		t.Fatalf("content = %q", out.Choices[0].Message.Content)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish = %q", out.Choices[0].FinishReason)
	}
	if out.Usage["prompt_tokens"] != 7 || out.Usage["completion_tokens"] != 2 {
		t.Fatalf("usage = %v", out.Usage)
	}
}

func TestChatCompletionsStreamShape(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var gotUsage, gotFinish, gotDone bool
	var text strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			gotDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]int `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
			if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
				gotFinish = true
			}
		}
		if chunk.Usage != nil {
			gotUsage = true
		}
	}
	if text.String() != "ab" {
		t.Fatalf("streamed text = %q", text.String())
	}
	if !gotFinish || !gotUsage || !gotDone {
		t.Fatalf("finish=%v usage=%v done=%v", gotFinish, gotUsage, gotDone)
	}
}

func TestAuthRequired(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated request must 401, got %d", resp.StatusCode)
	}

	// Wrong key too.
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("authorization", "Bearer ofm-wrong")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("bad key must 401, got %d", resp2.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`})
	defer up.Close()
	s := newTestServer(t, up)
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
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) == 0 {
		t.Fatal("model list must never be empty")
	}
}
