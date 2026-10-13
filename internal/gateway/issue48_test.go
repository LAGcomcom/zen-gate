package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingUpstream answers chat-style SSE success and remembers the decoded
// body of every request, so a test can assert what actually went upstream.
func recordingUpstream(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"role":"assistant","content":"ok"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	return up, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

// Issue #48: an Anthropic client feeding a tool_result back in the user turn
// must see its content reach the upstream. Before the fix the block parsed
// into a ToolResultPart carried by a RoleUser message, which ToChatMessages
// had no branch for — the content vanished on the way to the chat wire and
// every tool-using Claude session collapsed to "tool interrupted".
func TestAnthropicToolResultReachesUpstream(t *testing.T) {
	up, seen := recordingUpstream(t)
	defer up.Close()
	s := newTestServer(t, up)

	body := `{"model":"mimo-v2.6-flash-free","max_tokens":120,` +
		`"tools":[{"name":"lookup","description":"x","input_schema":{"type":"object","properties":{"k":{"type":"string"}}}}],` +
		`"messages":[` +
		`{"role":"user","content":"查一下 k=1"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01A","name":"lookup","input":{"k":"1"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":[{"type":"text","text":"ZEBRA-7741-MAGENTA"}]}]},` +
		`{"role":"user","content":"工具返回了什么？"}]}`

	resp, raw := openMessages(t, s, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d\n%s", resp.StatusCode, raw)
	}

	requests := seen()
	if len(requests) == 0 {
		t.Fatal("upstream saw no request")
	}
	// Assert on the last attempt: an exit retry may post more than once.
	msgs, _ := json.Marshal(requests[len(requests)-1]["messages"])
	if !strings.Contains(string(msgs), "ZEBRA-7741-MAGENTA") {
		t.Fatalf("tool_result content lost upstream: %s", msgs)
	}
	if !strings.Contains(string(msgs), `"tool_call_id":"toolu_01A"`) {
		t.Fatalf("tool result not projected onto the chat wire: %s", msgs)
	}
}

// The same content arrives as a plain string content (not a block array):
// both shapes must survive the conversion.
func TestAnthropicToolResultStringContentReachesUpstream(t *testing.T) {
	up, seen := recordingUpstream(t)
	defer up.Close()
	s := newTestServer(t, up)

	body := `{"model":"mimo-v2.6-flash-free","max_tokens":120,` +
		`"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02B","name":"lookup","input":{"k":"2"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02B","content":"STRING-RESULT-42","is_error":true}]}]}`

	resp, raw := openMessages(t, s, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d\n%s", resp.StatusCode, raw)
	}
	requests := seen()
	if len(requests) == 0 {
		t.Fatal("upstream saw no request")
	}
	msgs, _ := json.Marshal(requests[len(requests)-1]["messages"])
	if !strings.Contains(string(msgs), "STRING-RESULT-42") {
		t.Fatalf("string-shaped tool_result lost upstream: %s", msgs)
	}
}

// #48 second symptom: /v1/messages stream frames must carry the `event:` line
// naming the top-level type — the Anthropic SDK dispatches on it, and a
// data-only stream yields zero events (clients fall back to non-streaming or
// fail outright). A nested "type" (inside message/content_block/delta) must
// never leak into the event name.
func TestAnthropicStreamFramesCarryEventNames(t *testing.T) {
	laneUp := laneUpstreamByModel(t, nil)
	defer laneUp.Close()
	s := newTestServer(t, laneUp)

	resp, raw := openMessages(t, s, `{"model":"mimo-v2.6-flash-free","stream":true,"max_tokens":64,`+
		`"tools":[{"name":"lookup","description":"x","input_schema":{"type":"object"}}],`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d\n%s", resp.StatusCode, raw)
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var eventNames []string
	for i, line := range lines {
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		name := strings.TrimPrefix(line, "event: ")
		if name != "message_start" && name != "content_block_start" &&
			name != "content_block_delta" && name != "content_block_stop" &&
			name != "message_delta" && name != "message_stop" && name != "ping" && name != "error" {
			t.Errorf("unknown event name %q", name)
		}
		// the following data frame's top-level type must match the name
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "data: ") {
			t.Errorf("event %q has no data line", name)
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[i+1], "data: ")), &head); err != nil {
			t.Errorf("data after event %q is not JSON: %v", name, err)
			continue
		}
		if head.Type != name {
			t.Errorf("event %q carries data type %q", name, head.Type)
		}
		eventNames = append(eventNames, name)
	}
	if len(eventNames) == 0 {
		t.Fatalf("stream has no event: lines at all:\n%s", raw)
	}
	if eventNames[0] != "message_start" {
		t.Errorf("first event = %q, want message_start", eventNames[0])
	}
	last := eventNames[len(eventNames)-1]
	if last != "message_stop" {
		t.Errorf("last event = %q, want message_stop", last)
	}
	// The content_block_delta for text has a nested delta.type — the name
	// must be the envelope's, not the nested one.
	for _, n := range eventNames {
		if n == "text_delta" || n == "input_json_delta" || n == "thinking_delta" {
			t.Errorf("nested delta type leaked into the event name: %q", n)
		}
	}
}
