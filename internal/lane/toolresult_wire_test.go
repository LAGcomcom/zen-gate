package lane

import (
	"encoding/json"
	"strings"
	"testing"
)

// Issue #48: an Anthropic client feeds tool results back inside the user turn
// (a tool_result block), so a unified RoleUser message can carry ToolResultPart.
// On the chat wire those results must survive as role:"tool" messages — before
// the fix they fell through chatContentPart (nil) and vanished: the upstream
// saw an empty tool result and Claude clients reported every tool as
// interrupted, while the same content on the OpenAI path forwarded fine.

func TestChatWireCarriesUserToolResults(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{TextPart{Text: "查一下 k=1"}}},
		{Role: RoleAssistant, Parts: []Part{
			ToolCallPart{ID: "toolu_01A", Name: "lookup", Arguments: `{"k":"1"}`},
		}},
		{Role: RoleUser, Parts: []Part{
			ToolResultPart{ToolCallID: "toolu_01A", Text: "ZEBRA-7741-MAGENTA"},
		}},
		{Role: RoleUser, Parts: []Part{TextPart{Text: "工具返回了什么？"}}},
	}
	out := ToChatMessages(msgs)
	if len(out) != 4 {
		t.Fatalf("turns = %d, want 4: %+v", len(out), out)
	}
	tool, ok := out[2]["role"].(string)
	if !ok || tool != "tool" {
		t.Fatalf("turn 2 = %+v, want the tool result", out[2])
	}
	if out[2]["tool_call_id"] != "toolu_01A" {
		t.Errorf("tool_call_id = %v", out[2]["tool_call_id"])
	}
	if out[2]["content"] != "ZEBRA-7741-MAGENTA" {
		t.Errorf("content = %v, want the tool output verbatim", out[2]["content"])
	}
	if out[3]["role"] != RoleUser || out[3]["content"] != "工具返回了什么？" {
		t.Errorf("following user turn = %+v", out[3])
	}
}

func TestChatWireUserResultsWithTextAndImage(t *testing.T) {
	// A user turn carrying a result, free text and an image must emit the
	// tool message first, then the text turn, then the image parts turn.
	msgs := []Message{{Role: RoleUser, Parts: []Part{
		ToolResultPart{ToolCallID: "c1", Text: "ok", IsError: true},
		TextPart{Text: "顺带问一句"},
		ImagePart{DataURL: "data:image/png;base64,QUJD"},
	}}}
	out := ToChatMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("turns = %d, want 3: %+v", len(out), out)
	}
	if out[0]["role"] != "tool" || out[0]["content"] != "ok" || out[0]["tool_call_id"] != "c1" {
		t.Errorf("tool turn = %+v", out[0])
	}
	if out[1]["role"] != RoleUser || out[1]["content"] != "顺带问一句" {
		t.Errorf("text turn = %+v", out[1])
	}
	parts, ok := out[2]["content"].([]map[string]any)
	if !ok || len(parts) != 1 || parts[0]["type"] != "image_url" {
		t.Fatalf("parts turn = %#v", out[2]["content"])
	}
}

func TestChatWireUserResultsKeepOrder(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Parts: []Part{
		ToolResultPart{ToolCallID: "c1", Text: "first"},
		ToolResultPart{ToolCallID: "c2", Text: "second"},
	}}}
	out := ToChatMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("turns = %d, want 2: %+v", len(out), out)
	}
	if out[0]["tool_call_id"] != "c1" || out[1]["tool_call_id"] != "c2" {
		t.Errorf("order broken: %+v %+v", out[0], out[1])
	}
}

func TestResponsesWireCarriesUserToolResults(t *testing.T) {
	// The Responses wire has the same hole: a user turn's ToolResultPart must
	// project onto a function_call_output item before the turn's own input.
	msgs := []Message{
		{Role: RoleAssistant, Parts: []Part{
			ToolCallPart{ID: "c1", Name: "lookup", Arguments: `{"k":"1"}`},
		}},
		{Role: RoleUser, Parts: []Part{
			ToolResultPart{ToolCallID: "c1", Text: "42"},
			TextPart{Text: "结果如何？"},
		}},
	}
	_, out := ToResponseInput(msgs)
	if len(out) != 3 {
		t.Fatalf("items = %d, want 3 (call, output, user message): %+v", len(out), out)
	}
	if out[1]["type"] != "function_call_output" {
		t.Fatalf("item 1 = %+v", out[1])
	}
	if out[1]["call_id"] != "c1" || out[1]["output"] != "42" {
		t.Errorf("output item = %+v", out[1])
	}
	user := out[2]["content"].([]map[string]any)
	if len(user) != 1 || user[0]["text"] != "结果如何？" {
		t.Errorf("user turn items = %+v", out[2]["content"])
	}
}

func TestClaudeWireStillCarriesUserToolResults(t *testing.T) {
	// Regression guard for the path the bug did NOT touch: the native Claude
	// wire already kept tool_result blocks in the user turn.
	msgs := []Message{{Role: RoleUser, Parts: []Part{
		ToolResultPart{ToolCallID: "c1", Text: "42", IsError: true},
	}}}
	_, out := ToClaudeMessages(msgs)
	blocks := out[0]["content"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
		t.Fatalf("blocks = %#v", out[0]["content"])
	}
	if blocks[0]["content"] != "42" || blocks[0]["is_error"] != true {
		t.Errorf("block = %#v", blocks[0])
	}
	serialized, _ := json.Marshal(out)
	if !strings.Contains(string(serialized), "42") {
		t.Errorf("content lost from wire: %s", serialized)
	}
}
