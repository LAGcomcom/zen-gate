// 提示词组装：把 OpenAI 消息数组折叠为适合上游的单条文本（工具模拟注入用）。
//
// 上游上下文按 chat_id 会话维持；工具模拟需要把工具清单注入提示词，
// 因此提供全量折叠（renderFullPrompt）与尾部折叠（renderTailPrompt）两种形态。
//   - 新会话：system + 完整历史转录折叠进首条消息（renderFullPrompt）
//   - 续接会话：只发送未吸收的增量消息（renderTailPrompt）
package server

import (
	"strings"
)

// renderFullPrompt 新会话：折叠全部消息（system 提前，其余按序转录）。
func renderFullPrompt(msgs []openAIMessage, toolsBlock string) string {
	var sys, dialogue []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if strings.TrimSpace(m.Text) != "" {
				sys = append(sys, m.Text)
			}
			continue
		}
		if s := formatMessage(m); s != "" {
			dialogue = append(dialogue, s)
		}
	}
	var sb strings.Builder
	if len(sys) > 0 {
		sb.WriteString("[系统指令]\n")
		sb.WriteString(strings.Join(sys, "\n\n"))
		sb.WriteString("\n\n")
	}
	if len(dialogue) > 1 {
		sb.WriteString("[对话历史]\n")
		sb.WriteString(strings.Join(dialogue[:len(dialogue)-1], "\n\n"))
		sb.WriteString("\n\n[当前消息]\n")
		sb.WriteString(dialogue[len(dialogue)-1])
	} else if len(dialogue) == 1 {
		sb.WriteString(dialogue[0])
	}
	sb.WriteString(toolsBlock)
	return sb.String()
}

// renderTailPrompt 续接会话：只折叠增量消息（通常是 tool 结果 + 最新 user 消息）。
func renderTailPrompt(tail []openAIMessage, toolsBlock string) string {
	var parts []string
	for _, m := range tail {
		if s := formatMessage(m); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n") + toolsBlock
}

// formatMessage 单条消息转录文本。
func formatMessage(m openAIMessage) string {
	switch m.Role {
	case "user":
		return m.Text
	case "system", "developer":
		if strings.TrimSpace(m.Text) == "" {
			return ""
		}
		return "[系统指令]\n" + m.Text
	case "assistant":
		var sb strings.Builder
		sb.WriteString("[助手]")
		if strings.TrimSpace(m.Text) != "" {
			sb.WriteString("\n" + m.Text)
		}
		for _, c := range m.ToolCalls {
			sb.WriteString("\n[助手调用工具 " + c.Name + " 参数 " + normalizeArgs(c.Arguments) + "]")
		}
		if sb.Len() == len("[助手]") {
			return ""
		}
		return sb.String()
	case "tool":
		name := m.Name
		if name == "" {
			name = m.ToolCallID
		}
		if name == "" {
			name = "unknown"
		}
		return "[工具 " + name + " 返回结果]\n" + m.Text
	default:
		return "[" + m.Role + "]\n" + m.Text
	}
}
