package gateway

import (
	"encoding/json"
	"testing"

	"zen-gate/internal/lane"
)

// Issue #99: a thinking-mode upstream refuses a replayed assistant turn whose
// reasoning_content was dropped. The inbound field must survive the trip
// through the unified model and come back out on the chat wire.
func TestReasoningContentRoundTrips(t *testing.T) {
	body := `[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"thinking about it","reasoning_content":"let me think"},
		{"role":"user","content":"go on"}
	]`
	var in []openaiMessage
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	unified := convertOpenAIMessages(in)

	var got string
	for _, part := range unified[1].Parts {
		if rp, ok := part.(lane.ReasoningPart); ok {
			got = rp.Text
		}
	}
	if got != "let me think" {
		t.Fatalf("assistant reasoning = %q, want %q", got, "let me think")
	}

	wire := lane.ToChatMessages(unified)
	assistant, _ := wire[1]["role"].(string)
	if assistant != lane.RoleAssistant {
		t.Fatalf("wire[1] role = %v", wire[1]["role"])
	}
	if rc, _ := wire[1]["reasoning_content"].(string); rc != "let me think" {
		t.Fatalf("chat wire reasoning_content = %q, want %q", rc, "let me think")
	}
}

// The "reasoning" spelling is accepted as a fallback.
func TestReasoningSpellingFallback(t *testing.T) {
	var in []openaiMessage
	if err := json.Unmarshal([]byte(`[{"role":"assistant","content":"x","reasoning":"because"}]`), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	unified := convertOpenAIMessages(in)
	wire := lane.ToChatMessages(unified)
	if rc, _ := wire[0]["reasoning_content"].(string); rc != "because" {
		t.Fatalf("chat wire reasoning_content = %q, want %q", rc, "because")
	}
}

// An assistant turn with no reasoning is left alone: the key is absent, so
// lanes that need an empty backfill (WorkBuddy) still add their own.
func TestNoReasoningKeepsKeyAbsent(t *testing.T) {
	var in []openaiMessage
	if err := json.Unmarshal([]byte(`[{"role":"assistant","content":"plain"}]`), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wire := lane.ToChatMessages(convertOpenAIMessages(in))
	if _, has := wire[0]["reasoning_content"]; has {
		t.Fatalf("reasoning_content must be absent when the client sent none: %v", wire[0])
	}
}

// A Responses-API reasoning item that precedes its assistant turn is carried
// back too — that is the wire Trae-class clients use.
func TestResponsesReasoningItemIsCarried(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"pondering"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
	]`)
	unified := convertResponsesInput(raw, "")
	wire := lane.ToChatMessages(unified)
	if len(wire) != 2 {
		t.Fatalf("wire = %#v", wire)
	}
	if rc, _ := wire[1]["reasoning_content"].(string); rc != "pondering" {
		t.Fatalf("chat wire reasoning_content = %q, want %q", rc, "pondering")
	}
}

// A reasoning item in front of a function call belongs to that call's turn.
func TestResponsesReasoningBeforeFunctionCall(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"reasoning","content":[{"type":"reasoning_text","text":"need a tool"}]},
		{"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"}
	]`)
	wire := lane.ToChatMessages(convertResponsesInput(raw, ""))
	if len(wire) != 1 {
		t.Fatalf("wire = %#v", wire)
	}
	if rc, _ := wire[0]["reasoning_content"].(string); rc != "need a tool" {
		t.Fatalf("chat wire reasoning_content = %q", rc)
	}
	if _, ok := wire[0]["tool_calls"]; !ok {
		t.Fatalf("tool call lost: %#v", wire[0])
	}
}
