package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/lane"
)

// Anthropic /v1/messages, for Claude-protocol clients (Claude Code and kin).

type anthropicContentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Title  string `json:"title"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    json.RawMessage    `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicToolDef `json:"tools"`
	Stream    bool               `json:"stream"`
	Thinking  *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

func convertAnthropicMessages(in []anthropicMessage, system json.RawMessage) []lane.Message {
	out := []lane.Message{}
	if len(system) > 0 {
		parts := anthropicParts(system, nil)
		if len(parts) > 0 {
			out = append(out, lane.Message{Role: lane.RoleSystem, Parts: parts})
		}
	}
	for _, m := range in {
		parts := anthropicParts(m.Content, nil)
		if m.Role == "tool" {
			// not an Anthropic role; defensive
			out = append(out, lane.Message{Role: lane.RoleUser, Parts: parts})
			continue
		}
		out = append(out, lane.Message{Role: m.Role, Parts: parts})
	}
	return out
}

func anthropicParts(raw json.RawMessage, wantText *string) []lane.Part {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if wantText != nil {
			*wantText = s
			return nil
		}
		if s != "" {
			return []lane.Part{lane.TextPart{Text: s}}
		}
		return nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	parts := []lane.Part{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, lane.TextPart{Text: b.Text})
			}
		case "image":
			if b.Source != nil && b.Source.Type == "base64" && b.Source.Data != "" {
				parts = append(parts, lane.ImagePart{DataURL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data})
			}
		case "document":
			// Anthropic document blocks are PDFs: base64 source today (url
			// sources would leak the gateway's egress to a third party).
			if b.Source != nil && b.Source.Type == "base64" && b.Source.Data != "" {
				media := b.Source.MediaType
				if media == "" {
					media = "application/pdf"
				}
				parts = append(parts, lane.FilePart{Name: b.Title, MediaType: media, Data: b.Source.Data})
			}
		case "tool_use":
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			parts = append(parts, lane.ToolCallPart{ID: b.ID, Name: b.Name, Arguments: args})
		case "tool_result":
			text := ""
			if len(b.Content) > 0 {
				var cs string
				if json.Unmarshal(b.Content, &cs) == nil {
					text = cs
				} else {
					var inner []anthropicContentBlock
					if json.Unmarshal(b.Content, &inner) == nil {
						for _, ib := range inner {
							if ib.Type == "text" {
								text += ib.Text
							}
						}
					}
				}
			}
			parts = append(parts, lane.ToolResultPart{ToolCallID: b.ToolUseID, Text: text, IsError: b.IsError})
		}
	}
	return parts
}

type anthropicStreamState struct {
	sse      *sseWriter
	index    int
	blockMap map[int]int // lane block index → anthropic block index
	sawAny   bool
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
		return
	}
	agent := s.agentOf(r)
	unified := convertAnthropicMessages(req.Messages, req.System)
	if len(unified) == 0 {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "messages must not be empty"}})
		return
	}
	user := ""
	if req.Metadata != nil {
		user = req.Metadata.UserID
	}
	seed := SessionSeed(agent, user, unified)
	turn := TurnSeed(unified)
	model := req.Model
	needs := lane.SummarizeNeeds(unified, len(req.Tools))
	effortDeclared := ""
	if req.Thinking != nil && req.Thinking.Type == "enabled" && req.Thinking.BudgetTokens > 0 {
		switch b := req.Thinking.BudgetTokens; {
		case b > 8192:
			effortDeclared = "deep"
		case b > 2048:
			effortDeclared = "balanced"
		default:
			effortDeclared = "light"
		}
	}
	effort := s.resolveEffort(model, effortDeclared)
	base := lane.BaseModelId(model)
	tools := []lane.ToolDef{}
	for _, t := range req.Tools {
		tools = append(tools, lane.ToolDef{Name: t.Name, Description: t.Description, Parameters: string(t.InputSchema)})
	}
	// Namespaced model id → user-added provider (openai-protocol providers get
	// the messages converted to their chat wire).
	if p, upstream, ok := s.providerRoute(req.Model); ok {
		if !p.Enabled {
			writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "message": "自定义供应商 " + p.Name + " 已停用，请到「自定义 API」开启"}})
			return
		}
		s.relayAnthropicMessages(w, r, req, p, upstream, agent, unified, tools)
		return
	}
	ctx := r.Context()
	st := &anthropicStreamState{blockMap: map[int]int{}}

	if !req.Stream {
		collector := newResponsesCollector()
		outcome, uerr := s.Lane.Complete(ctx, lane.Request{
			Model: model, Effort: effort, Messages: unified, Tools: tools,
			MaxTokens: req.MaxTokens, SessionSeed: seed, TurnSeed: turn,
			Agent: agent, Needs: needs,
		}, collector.consume)
		served := servedModel(outcome, base)
		if uerr != nil && !collector.hasAnything() {
			if uerr.Exhausted && s.fallbackEnabled() {
				if usage, finish, fbServed, _, ok := s.providerFallback(ctx,
					s.providerFallbackPicks(needs, base), unified, tools,
					req.MaxTokens, agent, collector.consume); ok {
					fbOutcome := lane.Outcome{Usage: usage, Finish: finish, ServedModel: fbServed}
					w.Header().Set("x-zen-gate-served-by", fbServed)
					writeJSON(w, 200, anthropicResponse(randomID("msg_"), fbServed, collector, fbOutcome))
					return
				}
			}
			setRetryAfter(w, uerr)
			writeJSON(w, errorStatus(uerr), map[string]any{"type": "error", "error": map[string]any{"type": errorType(uerr), "message": uerr.Message}})
			return
		}
		w.Header().Set("x-zen-gate-served-by", served)
		writeJSON(w, 200, anthropicResponse(randomID("msg_"), served, collector, outcome))
		return
	}

	sse, err := newSSE(w)
	if err != nil {
		writeJSON(w, 500, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		return
	}
	st.sse = sse
	msgID := randomID("msg_")
	sse.event(map[string]any{"type": "message_start", "message": map[string]any{
		"id": msgID, "type": "message", "role": "assistant", "model": base,
		"content": []any{}, "stop_sequence": nil, "stop_reason": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})

	outcome, uerr := s.Lane.Complete(ctx, lane.Request{
		Model: model, Effort: effort, Messages: unified, Tools: tools,
		MaxTokens: req.MaxTokens, SessionSeed: seed, TurnSeed: turn,
		Agent: agent, Needs: needs,
	}, st.consume)
	stop := "end_turn"
	switch {
	case outcome.Finish == lane.FinishToolCalls:
		stop = "tool_use"
	case outcome.Finish == lane.FinishMaxTokens || outcome.Finish == "":
		stop = "max_tokens"
	}
	if uerr != nil && outcome.Finish == "" && !st.sawAny {
		if uerr.Exhausted && s.fallbackEnabled() {
			fbMaxTok := req.MaxTokens
			if fbMaxTok <= 0 {
				fbMaxTok = 8192
			}
			if usage, finish, _, _, ok := s.providerFallback(ctx,
				s.providerFallbackPicks(needs, base), unified, tools,
				fbMaxTok, agent, st.consume); ok {
				stop := "end_turn"
				switch {
				case finish == lane.FinishToolCalls:
					stop = "tool_use"
				case finish == lane.FinishMaxTokens || finish == "":
					stop = "max_tokens"
				}
				sse.event(map[string]any{"type": "message_delta",
					"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
					"usage": map[string]any{"output_tokens": usage.Output}})
				sse.event(map[string]any{"type": "message_stop"})
				return
			}
		}
		sse.event(map[string]any{"type": "error", "error": map[string]any{"type": errorType(uerr), "message": uerr.Message}})
		return
	}
	sse.event(map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outcome.Usage.Output}})
	sse.event(map[string]any{"type": "message_stop"})
}

func (st *anthropicStreamState) consume(c lane.Chunk) {
	st.sawAny = true
	switch c.Kind {
	case lane.ChunkBlockStart:
		idx := st.index
		st.blockMap[c.Index] = idx
		st.index++
		switch c.BlockType {
		case "text":
			st.sse.event(map[string]any{"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "text", "text": ""}})
		case "reasoning":
			st.sse.event(map[string]any{"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""}})
		case "tool-call":
			st.sse.event(map[string]any{"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage("{}")}})
		}
	case lane.ChunkTextDelta:
		st.sse.event(map[string]any{"type": "content_block_delta", "index": st.blockMap[c.Index],
			"delta": map[string]any{"type": "text_delta", "text": c.Delta}})
	case lane.ChunkReasonDelta:
		st.sse.event(map[string]any{"type": "content_block_delta", "index": st.blockMap[c.Index],
			"delta": map[string]any{"type": "thinking_delta", "thinking": c.Delta}})
	case lane.ChunkToolCallDelta:
		st.sse.event(map[string]any{"type": "content_block_delta", "index": st.blockMap[c.Index],
			"delta": map[string]any{"type": "input_json_delta", "partial_json": c.Delta}})
	case lane.ChunkBlockEnd:
		st.sse.event(map[string]any{"type": "content_block_stop", "index": st.blockMap[c.Index]})
	}
}

func anthropicResponse(id, model string, c *responsesCollector, outcome lane.Outcome) map[string]any {
	content := []any{}
	for _, b := range c.blocks {
		switch b.kind {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.text})
		case "reasoning":
			content = append(content, map[string]any{"type": "thinking", "thinking": b.text, "signature": ""})
		case "tool-call":
			args := json.RawMessage(b.text)
			if strings.TrimSpace(b.text) == "" || !json.Valid([]byte(b.text)) {
				args = json.RawMessage("{}")
			}
			content = append(content, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": args})
		}
	}
	stop := "end_turn"
	switch outcome.Finish {
	case lane.FinishToolCalls:
		stop = "tool_use"
	case lane.FinishMaxTokens:
		stop = "max_tokens"
	case "":
		stop = "max_tokens"
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content":     content,
		"stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  outcome.Usage.Input + outcome.Usage.CacheRead,
			"output_tokens": outcome.Usage.Output,
		},
		"created": time.Now().Unix(),
	}
}
