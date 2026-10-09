// Package tooltypes holds the shared tool data types (ToolDef / ToolChoice /
// ToolCall) and the request-side extractors that build them from OpenAI- and
// Anthropic-shaped JSON. The gateway supports native function-calling, so the
// historical prompt-injection / action-block emulation engine has been removed;
// only these types and extractors remain.
package tooltypes

import (
	"encoding/json"
	"strings"
)

type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type ToolChoice struct {
	Mode string
	Name string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

func ExtractTools(raw any) []ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}

	out := make([]ToolDef, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok && strings.EqualFold(strings.TrimSpace(stringFromAny(m["type"])), "function") {
			fn = m
			ok = true
		}
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringFromAny(fn["name"]))
		if name == "" {
			continue
		}
		schema, _ := fn["parameters"].(map[string]any)
		out = append(out, ToolDef{
			Name:        name,
			Description: strings.TrimSpace(stringFromAny(fn["description"])),
			InputSchema: cloneMap(schema),
		})
	}
	return out
}

func ExtractAnthropicTools(raw any) []ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}

	out := make([]ToolDef, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if IsAnthropicHostedTool(m) {
			continue
		}
		name := strings.TrimSpace(stringFromAny(m["name"]))
		if name == "" {
			continue
		}
		schema, _ := m["input_schema"].(map[string]any)
		out = append(out, ToolDef{
			Name:        name,
			Description: strings.TrimSpace(stringFromAny(m["description"])),
			InputSchema: cloneMap(schema),
		})
	}
	return out
}

func IsAnthropicHostedTool(tool map[string]any) bool {
	toolType := strings.TrimSpace(stringFromAny(tool["type"]))
	return IsAnthropicHostedToolType(toolType)
}

func IsAnthropicHostedToolType(toolType string) bool {
	toolType = strings.TrimSpace(toolType)
	return strings.HasPrefix(toolType, "web_search_")
}

func ExtractToolChoice(raw any) ToolChoice {
	if raw == nil {
		return ToolChoice{Mode: "auto"}
	}
	if s, ok := raw.(string); ok {
		s = strings.TrimSpace(s)
		switch s {
		case "", "auto":
			return ToolChoice{Mode: "auto"}
		case "none":
			return ToolChoice{Mode: "none"}
		case "required", "any":
			return ToolChoice{Mode: "any"}
		default:
			return ToolChoice{Mode: "tool", Name: s}
		}
	}

	m, ok := raw.(map[string]any)
	if !ok {
		return ToolChoice{Mode: "auto"}
	}
	typeName := strings.TrimSpace(stringFromAny(m["type"]))
	switch typeName {
	case "function", "tool":
		if fn, ok := m["function"].(map[string]any); ok {
			if name := strings.TrimSpace(stringFromAny(fn["name"])); name != "" {
				return ToolChoice{Mode: "tool", Name: name}
			}
		}
		if name := strings.TrimSpace(stringFromAny(m["name"])); name != "" {
			return ToolChoice{Mode: "tool", Name: name}
		}
	case "required", "any":
		return ToolChoice{Mode: "any"}
	case "auto", "none":
		return ToolChoice{Mode: "auto"}
	}
	return ToolChoice{Mode: "auto"}
}

func ExtractAnthropicToolChoice(raw any) ToolChoice {
	if raw == nil {
		return ToolChoice{Mode: "auto"}
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ExtractToolChoice(raw)
	}
	switch strings.TrimSpace(stringFromAny(m["type"])) {
	case "", "auto":
		return ToolChoice{Mode: "auto"}
	case "none":
		return ToolChoice{Mode: "none"}
	case "any", "required":
		return ToolChoice{Mode: "any"}
	case "tool":
		name := strings.TrimSpace(stringFromAny(m["name"]))
		if name != "" {
			return ToolChoice{Mode: "tool", Name: name}
		}
	}
	return ToolChoice{Mode: "auto"}
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}
