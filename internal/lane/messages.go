package lane

import (
	"encoding/json"
	"strings"
)

// RepairToolPairing removes dangling tool calls (assistant requests with no
// later result) and orphan tool results, plus nameless call husks. A request
// replayed upstream with an unmatched tool call is rejected with 400 and then
// poisons the session for every subsequent turn — repair before sending.
// Paired history is left untouched.
func RepairToolPairing(messages []Message) []Message {
	called := map[string]bool{}
	resulted := map[string]bool{}
	for _, m := range messages {
		for _, p := range m.Parts {
			switch t := p.(type) {
			case ToolCallPart:
				// A nameless call is not executable; marking it here means its
				// result fails the paired-result test and goes with it.
				if t.ID != "" && t.Name != "" {
					called[t.ID] = true
				}
			case ToolResultPart:
				if t.ToolCallID != "" {
					resulted[t.ToolCallID] = true
				}
			}
		}
	}
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		kept := make([]Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch t := p.(type) {
			case ToolCallPart:
				// A call with no name is not executable. One that reached the
				// host was answered "unknown tool \"\"" — an *answered* call,
				// so the result-presence test alone kept the husk in the
				// history forever and every later turn 400'd on every model.
				// A nameless call is dropped like an unanswered one; its
				// result then fails the paired-result test and goes with it.
				if t.ID != "" && t.Name != "" && resulted[t.ID] {
					kept = append(kept, p)
				}
			case ToolResultPart:
				if t.ToolCallID != "" && called[t.ToolCallID] {
					kept = append(kept, p)
				}
			default:
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 && m.Role == RoleAssistant {
			// an assistant turn that carried only dangling calls is dropped
			// only if it also has no text — otherwise keep the text
			hasText := false
			for _, p := range m.Parts {
				if _, ok := p.(TextPart); ok {
					hasText = true
					break
				}
			}
			if !hasText {
				continue
			}
		}
		m.Parts = kept
		out = append(out, m)
	}
	return out
}

// chatContentPart renders one non-text part for the chat wire.
func chatContentPart(p Part) map[string]any {
	switch t := p.(type) {
	case ImagePart:
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": t.DataURL}}
	case AudioPart:
		return map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": t.Data, "format": t.Format}}
	case FilePart:
		return map[string]any{"type": "file", "file": map[string]any{
			"filename": t.Name, "file_data": "data:" + t.MediaType + ";base64," + t.Data}}
	}
	return nil
}

// ToChatMessages projects unified messages onto the chat wire.
func ToChatMessages(messages []Message) []map[string]any {
	out := []map[string]any{}
	pendingParts := []map[string]any{}
	flushParts := func() {
		if len(pendingParts) > 0 {
			out = append(out, map[string]any{"role": RoleUser, "content": pendingParts})
			pendingParts = nil
		}
	}
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			text := m.TextOf()
			if strings.TrimSpace(text) != "" {
				flushParts()
				out = append(out, map[string]any{"role": "system", "content": text})
			}
		case RoleUser:
			text := ""
			for _, p := range m.Parts {
				if t, ok := p.(TextPart); ok {
					text += t.Text
					continue
				}
				if part := chatContentPart(p); part != nil {
					pendingParts = append(pendingParts, part)
				}
			}
			content := any(text)
			if len(pendingParts) > 0 && text == "" {
				parts := pendingParts
				pendingParts = nil
				content = parts
			}
			// With both text and non-text parts, the text turn goes first and
			// the parts follow as their own user turn (the trailing flushParts).
			out = append(out, map[string]any{"role": RoleUser, "content": content})
			flushParts()
		case RoleAssistant:
			text := ""
			tools := []map[string]any{}
			for _, p := range m.Parts {
				switch t := p.(type) {
				case TextPart:
					text += t.Text
				case ToolCallPart:
					args := t.Arguments
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					tools = append(tools, map[string]any{
						"id":   t.ID,
						"type": "function",
						"function": map[string]any{
							"name":      t.Name,
							"arguments": args,
						},
					})
				}
			}
			msg := map[string]any{"role": RoleAssistant, "content": text}
			if len(tools) > 0 {
				msg["tool_calls"] = tools
			}
			flushParts()
			out = append(out, msg)
		case RoleTool:
			text := ""
			for _, p := range m.Parts {
				if t, ok := p.(ToolResultPart); ok {
					text += t.Text
				}
			}
			toolCallID := ""
			for _, p := range m.Parts {
				if t, ok := p.(ToolResultPart); ok {
					toolCallID = t.ToolCallID
					break
				}
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": toolCallID, "content": text})
		}
	}
	flushParts()
	return out
}

// ToClaudeMessages projects unified messages onto the Anthropic wire,
// returning {system, messages}. Consecutive same-role turns merge, and a
// tool_result block leads its user turn.
func ToClaudeMessages(messages []Message) (system string, out []map[string]any) {
	sysParts := []string{}
	type pendingTurn struct {
		role  string
		blocks []map[string]any
	}
	turns := []pendingTurn{}
	appendBlock := func(role string, block map[string]any) {
		if len(turns) > 0 && turns[len(turns)-1].role == role {
			turns[len(turns)-1].blocks = append(turns[len(turns)-1].blocks, block)
		} else {
			turns = append(turns, pendingTurn{role, []map[string]any{block}})
		}
	}

	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			sysParts = append(sysParts, m.TextOf())
		case RoleUser:
			resultsFirst := []map[string]any{}
			rest := []map[string]any{}
			for _, p := range m.Parts {
				switch t := p.(type) {
				case TextPart:
					if t.Text != "" {
						rest = append(rest, map[string]any{"type": "text", "text": t.Text})
					}
				case ImagePart:
					if media, data, ok := parseDataURL(t.DataURL); ok {
						rest = append(rest, map[string]any{"type": "image",
							"source": map[string]any{"type": "base64", "media_type": media, "data": data}})
					}
				case FilePart:
					// The Claude wire takes PDF documents; other media types
					// have no Anthropic representation and are dropped (the
					// modality filter keeps such requests off this wire's
					// models anyway).
					if t.Data != "" && t.MediaType == "application/pdf" {
						block := map[string]any{"type": "document",
							"source": map[string]any{"type": "base64", "media_type": t.MediaType, "data": t.Data}}
						if t.Name != "" {
							block["title"] = t.Name
						}
						rest = append(rest, block)
					}
				case AudioPart:
					// Anthropic accepts no audio input; skip.
				case ToolResultPart:
					block := map[string]any{"type": "tool_result", "tool_use_id": t.ToolCallID, "content": t.Text}
					if t.IsError {
						block["is_error"] = true
					}
					resultsFirst = append(resultsFirst, block)
				}
			}
			blocks := append(resultsFirst, rest...)
			if len(blocks) > 0 {
				appendBlock(RoleUser, blocks[0])
				for _, b := range blocks[1:] {
					appendBlock(RoleUser, b)
				}
			}
		case RoleAssistant:
			for _, p := range m.Parts {
				switch t := p.(type) {
				case TextPart:
					if strings.TrimSpace(t.Text) != "" {
						appendBlock(RoleAssistant, map[string]any{"type": "text", "text": t.Text})
					}
				case ToolCallPart:
					args := t.Arguments
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					appendBlock(RoleAssistant, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Name, "input": jsonRaw(args)})
				}
			}
		case RoleTool:
			for _, p := range m.Parts {
				if t, ok := p.(ToolResultPart); ok {
					block := map[string]any{"type": "tool_result", "tool_use_id": t.ToolCallID, "content": t.Text}
					if t.IsError {
						block["is_error"] = true
					}
					appendBlock(RoleUser, block)
				}
			}
		}
	}
	for _, t := range turns {
		out = append(out, map[string]any{"role": t.role, "content": t.blocks})
	}
	system = strings.TrimSpace(strings.Join(sysParts, "\n\n"))
	return system, out
}

// ToResponseInput projects unified messages onto the Responses wire,
// returning system instructions and input items.
func ToResponseInput(messages []Message) (instructions string, out []map[string]any) {
	sysParts := []string{}
	out = []map[string]any{}
	for _, m := range messages {
		switch m.Role {
		case RoleSystem:
			sysParts = append(sysParts, m.TextOf())
		case RoleUser:
			text := ""
			items := []map[string]any{}
			for _, p := range m.Parts {
				switch t := p.(type) {
				case TextPart:
					text += t.Text
				case ImagePart:
					items = append(items, map[string]any{"type": "input_image", "image_url": t.DataURL})
				case AudioPart:
					items = append(items, map[string]any{"type": "input_audio", "data": t.Data, "format": t.Format})
				case FilePart:
					item := map[string]any{"type": "input_file",
						"file_data": "data:" + t.MediaType + ";base64," + t.Data}
					if t.Name != "" {
						item["filename"] = t.Name
					}
					items = append(items, item)
				}
			}
			if strings.TrimSpace(text) != "" || len(items) > 0 {
				if strings.TrimSpace(text) != "" {
					items = append([]map[string]any{{"type": "input_text", "text": text}}, items...)
				}
				out = append(out, map[string]any{"role": RoleUser, "content": items})
			}
		case RoleAssistant:
			for _, p := range m.Parts {
				switch t := p.(type) {
				case TextPart:
					if strings.TrimSpace(t.Text) != "" {
						out = append(out, map[string]any{"role": RoleAssistant,
							"content": []map[string]any{{"type": "output_text", "text": t.Text}}})
					}
				case ToolCallPart:
					args := t.Arguments
					if strings.TrimSpace(args) == "" {
						args = "{}"
					}
					out = append(out, map[string]any{"type": "function_call", "call_id": t.ID, "name": t.Name, "arguments": args})
				}
			}
		case RoleTool:
			for _, p := range m.Parts {
				if t, ok := p.(ToolResultPart); ok {
					out = append(out, map[string]any{"type": "function_call_output", "call_id": t.ToolCallID, "output": t.Text})
				}
			}
		}
	}
	instructions = strings.TrimSpace(strings.Join(sysParts, "\n\n"))
	return instructions, out
}

// ToToolDefs converts unified tool defs to the requested wire shape.
// style: "chat" (nested function), "flat" (Responses), "claude".
func ToToolDefs(tools []ToolDef, style string) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := []map[string]any{}
	for _, t := range tools {
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if strings.TrimSpace(t.Parameters) != "" {
			_ = json.Unmarshal([]byte(t.Parameters), &params)
		}
		switch style {
		case "claude":
			out = append(out, map[string]any{"name": t.Name, "description": t.Description, "input_schema": params})
		case "flat":
			out = append(out, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": params})
		default:
			out = append(out, map[string]any{"type": "function",
				"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}})
		}
	}
	return out
}

// parseDataURL splits "data:<media>;base64,<data>".
func parseDataURL(dataURL string) (media, data string, ok bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(dataURL, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", "", false
	}
	media = rest[:semi]
	data = rest[semi+len(";base64,"):]
	switch media {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return media, data, true
	}
	return "", "", false
}

func jsonRaw(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
