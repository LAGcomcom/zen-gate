package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// parallelToolReply drives a chat-wire upstream that streams two parallel tool
// calls and returns the client-side tool_calls aggregated by the gateway.
func parallelToolReply(t *testing.T, frames []string) ([]struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}, string) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	s := newTestServer(t, up)
	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "prov", Name: "e2e", BaseURL: up.URL + "/v1",
		APIKey: "sk-user-key", Protocol: store.ProtocolOpenAI, Enabled: true,
		Models: []string{"tool-model"},
	})
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	body := `{"model":"prov/tool-model","stream":false,"messages":[{"role":"user","content":"search and grep"}]}`
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
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d", len(out.Choices))
	}
	return out.Choices[0].Message.ToolCalls, out.Choices[0].FinishReason
}

func assertSplitCalls(t *testing.T, tcs []struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}, finish string) {
	t.Helper()
	if len(tcs) != 2 {
		t.Fatalf("want 2 tool calls, got %d: %#v", len(tcs), tcs)
	}
	got := map[string]string{}
	for _, tc := range tcs {
		got[tc.Function.Name] = tc.Function.Arguments
	}
	if want := `{"query":"golang"}`; got["web_search"] != want {
		t.Fatalf("web_search args = %q, want %q (names/args got mixed)", got["web_search"], want)
	}
	if want := `{"pattern":"todo"}`; got["grep"] != want {
		t.Fatalf("grep args = %q, want %q (names/args got mixed)", got["grep"], want)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls (a clean split must not read as broken)", finish)
	}
}

// Issue #96, the common wire: the upstream numbers its parallel calls (index 0
// and 1). Nothing here pre-splits the chunks -- the decoder and the emitter
// both have to do it.
func TestParallelToolCallsEndToEnd(t *testing.T) {
	tcs, finish := parallelToolReply(t, []string{
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"web_search","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"go"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"lang\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"grep","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"pattern\":\"t"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"odo\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
	})
	assertSplitCalls(t, tcs, finish)
}

// Issue #96, the harder wire: the upstream sends no index at all and drops the
// id on the argument continuations. v1.8.1 dropped those frames entirely, so
// the call arrived with truncated arguments ("{\"pattern\":\"t") and the turn
// was reported as length instead of tool_calls. They must follow the call that
// was opened last.
func TestParallelToolCallsWithoutIndexEndToEnd(t *testing.T) {
	tcs, finish := parallelToolReply(t, []string{
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"id":"call_a","function":{"name":"web_search","arguments":"{\"query\":\"golang\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"grep","arguments":"{\"pattern\":\"t"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"odo\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
	})
	assertSplitCalls(t, tcs, finish)
}
