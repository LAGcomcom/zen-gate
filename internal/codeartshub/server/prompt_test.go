package server

import (
	"strings"
	"testing"
)

func TestRenderFullPrompt(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "system", Text: "你是助手"},
		{Role: "user", Text: "你好"},
		{Role: "assistant", Text: "你好！"},
		{Role: "user", Text: "再来一次"},
	}
	p := renderFullPrompt(msgs, "")
	if !contains(p, "你是助手") || !contains(p, "对话历史") || !contains(p, "当前消息") || !contains(p, "再来一次") {
		t.Fatalf("full prompt: %s", p)
	}
	if strings.Index(p, "对话历史") > strings.Index(p, "当前消息") {
		t.Fatalf("history/current order wrong: %s", p)
	}
}

func TestRenderTailPrompt(t *testing.T) {
	tail := []openAIMessage{
		{Role: "tool", ToolCallID: "call_1", Text: "{\"ok\":true}"},
		{Role: "user", Text: "继续"},
	}
	p := renderTailPrompt(tail, "TOOLS")
	if !contains(p, "继续") || !contains(p, "TOOLS") || !contains(p, "call_1") {
		t.Fatalf("tail prompt: %s", p)
	}
}

func TestFormatMessageToolCall(t *testing.T) {
	m := openAIMessage{Role: "assistant", Text: "", ToolCalls: []openAIToolCall{{Name: "f1", Arguments: `{"a":1}`}}}
	if !contains(formatMessage(m), "f1") {
		t.Fatalf("format assistant: %s", formatMessage(m))
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
