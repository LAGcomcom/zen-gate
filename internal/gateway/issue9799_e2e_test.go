package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureUpstream records the JSON body the gateway actually sends upstream and
// answers with one text delta followed by a clean stop.
func captureUpstream(t *testing.T, sink *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("upstream body is not JSON: %v (%s)", err, raw)
		}
		*sink = body
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// Issue #97, end to end: the client's spelling must travel through the real
// HTTP handler into the real output budget. "max" has to buy more room than
// "low" -- before the fix both spellings fell back to the same default.
func TestReasoningEffortMaxTravelsEndToEnd(t *testing.T) {
	spent := func(effort string) int {
		var seen map[string]any
		up := captureUpstream(t, &seen)
		defer up.Close()
		s := newTestServer(t, up)

		body := fmt.Sprintf(
			`{"model":"mimo-v2.6-flash-free","reasoning_effort":%q,"stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			effort)
		resp := postChat(t, s, body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		v, _ := seen["max_tokens"].(float64)
		return int(v)
	}

	budget := map[string]int{}
	for _, e := range []string{"max", "low"} {
		budget[e] = spent(e)
	}
	if budget["max"] == 0 || budget["low"] == 0 {
		t.Fatalf("upstream saw no max_tokens: %v", budget)
	}
	if budget["max"] <= budget["low"] {
		t.Fatalf(`reasoning_effort "max" bought %d tokens, "low" bought %d: the deepest rung is not deeper`,
			budget["max"], budget["low"])
	}
}

// Issue #99, end to end: a replayed assistant turn that carries
// reasoning_content must reach the upstream with that field intact, otherwise
// a thinking-mode upstream rejects the turn.
func TestReplayReasoningReachesUpstream(t *testing.T) {
	var seen map[string]any
	up := captureUpstream(t, &seen)
	defer up.Close()
	s := newTestServer(t, up)

	body := `{"model":"mimo-v2.6-flash-free","stream":false,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","reasoning_content":"let me think","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"done"},
		{"role":"user","content":"go on"}]}`
	resp := postChat(t, s, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	msgs, _ := seen["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("upstream messages = %d: %#v", len(msgs), msgs)
	}
	assistant, _ := msgs[1].(map[string]any)
	if role, _ := assistant["role"].(string); role != "assistant" {
		t.Fatalf("msgs[1] role = %v", assistant["role"])
	}
	if rc, _ := assistant["reasoning_content"].(string); rc != "let me think" {
		t.Fatalf("upstream assistant reasoning_content = %q, want %q -- the replay would be rejected",
			rc, "let me think")
	}
	if _, ok := assistant["tool_calls"]; !ok {
		t.Fatalf("upstream assistant turn lost its tool_calls: %#v", assistant)
	}
}
