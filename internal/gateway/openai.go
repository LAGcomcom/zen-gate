package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/lane"
)

// --- OpenAI request shapes ---------------------------------------------------

type openaiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
	File *struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
	} `json:"file"`
}

type openaiToolCall struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Function  struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []openaiToolCall `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type openaiToolDef struct {
	Type string `json:"type"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiChatRequest struct {
	Model               string           `json:"model"`
	Messages            []openaiMessage  `json:"messages"`
	Tools               []openaiToolDef  `json:"tools"`
	Stream              bool             `json:"stream"`
	MaxTokens           int              `json:"max_tokens"`
	MaxCompletionTokens int              `json:"max_completion_tokens"`
	ReasoningEffort     string           `json:"reasoning_effort"`
	User                string           `json:"user"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// convertOpenAI builds unified messages from an OpenAI chat body.
func convertOpenAIMessages(in []openaiMessage) []lane.Message {
	out := []lane.Message{}
	for _, m := range in {
		switch m.Role {
		case "system", "developer":
			out = append(out, lane.Message{Role: lane.RoleSystem, Parts: partsOfContent(m.Content, nil)})
		case "user":
			out = append(out, lane.Message{Role: lane.RoleUser, Parts: partsOfContent(m.Content, nil)})
		case "assistant":
			parts := partsOfContent(m.Content, nil)
			for _, tc := range m.ToolCalls {
				parts = append(parts, lane.ToolCallPart{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			out = append(out, lane.Message{Role: lane.RoleAssistant, Parts: parts})
		case "tool":
			text := ""
			parts := partsOfContent(m.Content, &text)
			if text != "" {
				parts = []lane.Part{lane.ToolResultPart{ToolCallID: m.ToolCallID, Text: text}}
			} else {
				wrapped := []lane.Part{}
				for _, p := range parts {
					if tp, ok := p.(lane.TextPart); ok {
						wrapped = append(wrapped, lane.ToolResultPart{ToolCallID: m.ToolCallID, Text: tp.Text})
					}
				}
				parts = wrapped
			}
			out = append(out, lane.Message{Role: lane.RoleTool, Parts: parts})
		}
	}
	return out
}

// responsesContentParts decodes a Responses-API content array. Codex and other
// responses clients label parts input_text / output_text / input_image rather
// than the chat wire's text / image_url, and input_image carries the URL as a
// plain string. Anything unrecognized is skipped rather than failing the whole
// array parse.
func responsesContentParts(raw json.RawMessage) []lane.Part {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			return []lane.Part{lane.TextPart{Text: s}}
		}
		return nil
	}
	var arr []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
		// Responses-API audio/file parts carry their payload flat on the part
		// ({"type":"input_audio","data":…,"format":…}), unlike the chat wire's
		// nested input_audio/file objects.
		Data     string `json:"data"`
		Format   string `json:"format"`
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	parts := []lane.Part{}
	for _, p := range arr {
		switch p.Type {
		case "text", "input_text", "output_text", "summary_text":
			if p.Text != "" {
				parts = append(parts, lane.TextPart{Text: p.Text})
			}
		case "image_url", "input_image":
			if len(p.ImageURL) == 0 {
				continue
			}
			var url string
			if err := json.Unmarshal(p.ImageURL, &url); err != nil {
				var obj struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(p.ImageURL, &obj); err != nil || obj.URL == "" {
					continue
				}
				url = obj.URL
			}
			if url != "" {
				parts = append(parts, lane.ImagePart{DataURL: url})
			}
		case "input_audio":
			if p.Data != "" {
				format := p.Format
				if format == "" {
					format = "wav"
				}
				parts = append(parts, lane.AudioPart{Data: p.Data, Format: format})
			}
		case "input_file":
			if p.FileData != "" {
				media, data := splitFileData(p.FileData)
				if data != "" {
					if media == "" {
						media = "application/octet-stream"
					}
					parts = append(parts, lane.FilePart{Name: p.Filename, MediaType: media, Data: data})
				}
			}
		}
	}
	return parts
}

// splitFileData splits a "data:<media>;base64,<data>" payload into its media
// type and raw base64 body.
func splitFileData(fileData string) (media, data string) {
	rest := strings.TrimPrefix(fileData, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", ""
	}
	return rest[:semi], rest[semi+len(";base64,"):]
}

// partsOfContent decodes string-or-parts content; when wantText is set the
// plain-string form is returned through it instead.
func partsOfContent(raw json.RawMessage, wantText *string) []lane.Part {
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
	var arr []openaiContentPart
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	parts := []lane.Part{}
	for _, p := range arr {
		switch p.Type {
		case "text":
			if p.Text != "" {
				parts = append(parts, lane.TextPart{Text: p.Text})
			}
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				parts = append(parts, lane.ImagePart{DataURL: p.ImageURL.URL})
			}
		case "input_audio":
			if p.InputAudio != nil && p.InputAudio.Data != "" {
				format := p.InputAudio.Format
				if format == "" {
					format = "wav"
				}
				parts = append(parts, lane.AudioPart{Data: p.InputAudio.Data, Format: format})
			}
		case "file":
			if p.File != nil && p.File.FileData != "" {
				media, data := splitFileData(p.File.FileData)
				if data != "" {
					if media == "" {
						media = "application/octet-stream"
					}
					parts = append(parts, lane.FilePart{Name: p.File.Filename, MediaType: media, Data: data})
				}
			}
		}
	}
	return parts
}

func convertOpenAITools(in []openaiToolDef) []lane.ToolDef {
	out := []lane.ToolDef{}
	for _, t := range in {
		if t.Type == "function" && t.Function != nil {
			out = append(out, lane.ToolDef{Name: t.Function.Name, Description: t.Function.Description, Parameters: string(t.Function.Parameters)})
		} else if t.Name != "" {
			out = append(out, lane.ToolDef{Name: t.Name, Description: t.Description, Parameters: string(t.Parameters)})
		}
	}
	return out
}

// --- chat completions ---------------------------------------------------------

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openaiChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, openaiError("invalid request body: "+err.Error(), "invalid_request_error"))
		return
	}
	agent := s.agentOf(r)
	unified := convertOpenAIMessages(req.Messages)
	if len(unified) == 0 {
		writeJSON(w, 400, openaiError("messages must not be empty", "invalid_request_error"))
		return
	}
	needs := lane.SummarizeNeeds(unified, len(req.Tools))
	// Namespaced model id → user-added provider; the turn never touches the
	// free lane in that case.
	if p, upstream, ok := s.providerRoute(req.Model); ok {
		if !p.Enabled {
			writeJSON(w, 400, openaiError("自定义供应商 "+p.Name+" 已停用，请到「自定义 API」开启", "invalid_request_error"))
			return
		}
		s.relayChatCompletions(w, r, req, p, upstream, agent, unified)
		return
	}
	seed := SessionSeed(agent, req.User, unified)
	turn := TurnSeed(unified)
	ctx := r.Context()
	emit := &chatEmitter{declared: declaredNames(req.Tools)}

	model := req.Model
	effort := s.resolveEffort(model, req.ReasoningEffort)
	base := lane.BaseModelId(model)
	maxTok := req.MaxTokens
	if req.MaxCompletionTokens > 0 {
		maxTok = req.MaxCompletionTokens
	}

	id := randomID("chatcmpl-")
	created := time.Now().Unix()

	if !req.Stream {
		var text strings.Builder
		var reasoning strings.Builder
		var toolCalls []map[string]any
		outcome, uerr := s.Lane.Complete(ctx, lane.Request{
			Model: model, Effort: effort, Messages: unified,
			Tools: convertOpenAITools(req.Tools), MaxTokens: maxTok,
			SessionSeed: seed, TurnSeed: turn, Agent: agent, Needs: needs,
		}, func(c lane.Chunk) {
			emit.consume(c, &text, &reasoning, &toolCalls)
		})
		if uerr != nil && text.Len() == 0 && len(toolCalls) == 0 {
			// Free lane exhausted and the user opted in: offer the turn to
			// user-added providers before reporting the lane's failure.
			if uerr.Exhausted && s.fallbackEnabled() {
				if usage, finish, served, _, ok := s.providerFallback(ctx,
					s.providerFallbackPicks(needs, base), unified,
					convertOpenAITools(req.Tools), maxTok, agent,
					func(c lane.Chunk) { emit.consume(c, &text, &reasoning, &toolCalls) }); ok {
					w.Header().Set("x-zen-gate-served-by", served)
					msg := map[string]any{"role": "assistant", "content": text.String()}
					if reasoning.Len() > 0 {
						msg["reasoning_content"] = reasoning.String()
					}
					if len(toolCalls) > 0 {
						msg["tool_calls"] = toolCalls
					}
					outFinish := "stop"
					if finish == "" {
						outFinish = "length"
					} else {
						outFinish = mapFinish(finish)
					}
					writeJSON(w, 200, map[string]any{
						"id": id, "object": "chat.completion", "created": created, "model": served,
						"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": outFinish}},
						"usage":   usagePayload(usage),
					})
					return
				}
			}
			setRetryAfter(w, uerr)
			body := openaiError(uerr.Message, errorType(uerr))
			if uerr.Code == lane.CodeQuota {
				body = withSuggestions(body, s.Lane.Candidates(needs, base, 3))
			}
			writeJSON(w, errorStatus(uerr), body)
			return
		}
		served := servedModel(outcome, base)
		w.Header().Set("x-zen-gate-served-by", served)
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if reasoning.Len() > 0 {
			msg["reasoning_content"] = reasoning.String()
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		finish := "stop"
		if outcome.Finish == "" {
			finish = "length" // a cut stream is not a clean stop
		} else {
			finish = mapFinish(outcome.Finish)
		}
		resp := map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": served,
			"choices": []any{map[string]any{
				"index": 0, "message": msg,
				"finish_reason": finish,
			}},
			"usage": usagePayload(outcome.Usage),
		}
		writeJSON(w, 200, resp)
		return
	}

	sse, err := newSSE(w)
	if err != nil {
		writeJSON(w, 500, openaiError(err.Error(), "internal_error"))
		return
	}
	sendChunk := func(delta map[string]any, finish any, usage any) {
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
		payload := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": base, "choices": []any{choice},
		}
		if usage != nil {
			payload["usage"] = usage
		}
		sse.event(payload)
	}
	sendChunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)

	outcome, uerr := s.Lane.Complete(ctx, lane.Request{
		Model: model, Effort: effort, Messages: unified,
		Tools: convertOpenAITools(req.Tools), MaxTokens: maxTok,
		SessionSeed: seed, TurnSeed: turn, Agent: agent, Needs: needs,
	}, func(c lane.Chunk) {
		for _, d := range emit.stream(c) {
			sendChunk(d.delta, nil, nil)
		}
	})
	served := servedModel(outcome, base)
	if uerr != nil && !emit.hasAny() {
		// Free lane exhausted and the user opted in: continue the same SSE
		// stream from a user-added provider.
		if uerr.Exhausted && s.fallbackEnabled() {
			if usage, finish, fbServed, _, ok := s.providerFallback(ctx,
				s.providerFallbackPicks(needs, base), unified,
				convertOpenAITools(req.Tools), maxTok, agent,
				func(c lane.Chunk) {
					for _, d := range emit.stream(c) {
						sendChunk(d.delta, nil, nil)
					}
				}); ok {
				outFinish := "stop"
				if finish == "" {
					outFinish = "length"
				} else {
					outFinish = mapFinish(finish)
				}
				sendChunk(map[string]any{}, outFinish, nil)
				if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
					sse.event(map[string]any{
						"id": id, "object": "chat.completion.chunk", "created": created, "model": fbServed,
						"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
						"usage":   usagePayload(usage),
					})
				}
				sse.done()
				return
			}
		}
		setRetryAfter(w, uerr)
		body := openaiError(uerr.Message, errorType(uerr))
		if uerr.Code == lane.CodeQuota {
			body = withSuggestions(body, s.Lane.Candidates(needs, base, 3))
		}
		writeJSON(w, errorStatus(uerr), body)
		return
	}
	if uerr != nil {
		// mid-stream failure after content went out: report as an SSE error
		sse.event(map[string]any{"error": map[string]any{
			"message": uerr.Message, "type": errorType(uerr), "code": uerr.Code}})
		sse.done()
		return
	}
	finish := "stop"
	if outcome.Finish == "" {
		finish = "length" // a cut stream is not a clean stop
	} else {
		finish = mapFinish(outcome.Finish)
	}
	sendChunk(map[string]any{}, finish, nil)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		// The final chunk carries the model that actually served this turn —
		// it differs from the requested one when failover took over.
		finalModel := served
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}
		payload := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": finalModel, "choices": []any{choice}, "usage": usagePayload(outcome.Usage),
		}
		sse.event(payload)
	}
	sse.done()
}

// servedModel picks the model id to advertise: the one that actually served
// the turn when failover took over, the requested one otherwise.
func servedModel(outcome lane.Outcome, requested string) string {
	if outcome.ServedModel != "" {
		return outcome.ServedModel
	}
	return requested
}

// chatEmitter translates lane chunks into OpenAI deltas, deferring tool-call
// blocks so fingerprint decoys can be suppressed whole and OpenAI tool_calls
// indexes renumber from 0 per request.
type chatEmitter struct {
	declared map[string]bool
	toolIdx  int
	openTool map[int]*chatToolBuf
	sawAny   bool
}

// declaredNames collects the tool names an OpenAI request declared.
func declaredNames(tools []openaiToolDef) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if t.Type == "function" && t.Function != nil && t.Function.Name != "" {
			out[t.Function.Name] = true
		} else if t.Name != "" {
			out[t.Name] = true
		}
	}
	return out
}

type chatToolBuf struct {
	id, name   string
	args       string
	suppressed bool
}

func (e *chatEmitter) hasAny() bool { return e.sawAny }

func (e *chatEmitter) consume(c lane.Chunk, text, reasoning *strings.Builder, toolCalls *[]map[string]any) {
	e.sawAny = true
	switch c.Kind {
	case lane.ChunkTextDelta:
		text.WriteString(c.Delta)
	case lane.ChunkReasonDelta:
		reasoning.WriteString(c.Delta)
	case lane.ChunkBlockStart:
		if c.BlockType == "tool-call" {
			if e.openTool == nil {
				e.openTool = map[int]*chatToolBuf{}
			}
			e.openTool[c.Index] = &chatToolBuf{id: c.ID, name: c.Name,
				suppressed: isDecoy(c.Name) && !e.declared[c.Name]}
		}
	case lane.ChunkToolCallDelta:
		if e.openTool == nil {
			return
		}
		if buf, ok := e.openTool[c.Index]; ok && !buf.suppressed {
			if buf.id == "" {
				buf.id = c.ID
			}
			if buf.name == "" {
				buf.name = c.Name
			}
			buf.args += c.Delta
		}
	case lane.ChunkBlockEnd:
		if e.openTool == nil {
			return
		}
		if buf, ok := e.openTool[c.Index]; ok {
			delete(e.openTool, c.Index)
			if !buf.suppressed {
				*toolCalls = append(*toolCalls, map[string]any{
					"id":   buf.id,
					"type": "function",
					"function": map[string]any{"name": buf.name, "arguments": buf.args},
				})
			}
		}
	}
}

// stream is the streaming counterpart: returns the deltas to send now.
func (e *chatEmitter) stream(c lane.Chunk) []struct{ delta map[string]any } {
	out := []struct{ delta map[string]any }{}
	e.sawAny = true
	switch c.Kind {
	case lane.ChunkTextDelta:
		out = append(out, struct{ delta map[string]any }{map[string]any{"content": c.Delta}})
	case lane.ChunkReasonDelta:
		out = append(out, struct{ delta map[string]any }{map[string]any{"reasoning": c.Delta}})
	case lane.ChunkBlockStart:
		if c.BlockType == "tool-call" {
			if e.openTool == nil {
				e.openTool = map[int]*chatToolBuf{}
			}
			e.openTool[c.Index] = &chatToolBuf{id: c.ID, name: c.Name,
				suppressed: isDecoy(c.Name) && !e.declared[c.Name]}
		}
	case lane.ChunkToolCallDelta:
		if e.openTool != nil {
			if buf, ok := e.openTool[c.Index]; ok && !buf.suppressed {
				delta := map[string]any{
					"tool_calls": []any{map[string]any{
						"index": e.toolIdx,
						"id":    firstNonEmpty(buf.id, c.ID),
						"function": map[string]any{
							"name":      firstNonEmpty(buf.name, c.Name),
							"arguments": c.Delta,
						},
					}},
				}
				if buf.id != "" {
					buf.id = ""
				}
				if buf.name != "" {
					buf.name = ""
				}
				out = append(out, struct{ delta map[string]any }{delta})
			}
		}
	case lane.ChunkBlockEnd:
		if e.openTool != nil {
			if _, ok := e.openTool[c.Index]; ok {
				delete(e.openTool, c.Index)
				e.toolIdx++
			}
		}
	}
	return out
}

func isDecoy(name string) bool {
	for _, t := range lane.FingerprintTools {
		if t == name {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mapFinish(f string) string {
	switch f {
	case lane.FinishToolCalls:
		return "tool_calls"
	case lane.FinishMaxTokens:
		return "length"
	default:
		return "stop"
	}
}

func usagePayload(u lane.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.Input + u.CacheRead,
		"completion_tokens": u.Output,
		"total_tokens":      u.Input + u.CacheRead + u.Output,
	}
}

func errorStatus(e *lane.UpstreamError) int {
	switch e.Code {
	case lane.CodeQuota:
		return 429
	case lane.CodeRegion:
		return 403
	case lane.CodeCredential:
		return 401
	case lane.CodeClient:
		return 400
	case lane.CodeTimeout:
		return 504
	case lane.CodeTransport, lane.CodeServer, lane.CodeEmpty:
		return 502
	default:
		return 502
	}
}

func errorType(e *lane.UpstreamError) string {
	switch e.Code {
	case lane.CodeQuota:
		return "rate_limit_error"
	case lane.CodeRegion, lane.CodeCredential:
		return "authentication_error"
	case lane.CodeClient:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// --- responses ----------------------------------------------------------------

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Stream bool            `json:"stream"`
	Tools  []openaiToolDef `json:"tools"`
}

type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	Output  string          `json:"output,omitempty"`
}

func convertResponsesInput(raw json.RawMessage, instructions string) []lane.Message {
	out := []lane.Message{}
	if instructions != "" {
		out = append(out, lane.Message{Role: lane.RoleSystem, Parts: []lane.Part{lane.TextPart{Text: instructions}}})
	}
	if len(raw) == 0 {
		return out
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			out = append(out, lane.Message{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: s}}})
		}
		return out
	}
	var items []responsesItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return out
	}
	for _, it := range items {
		switch it.Type {
		case "", "message":
			role := it.Role
			if role == "" {
				role = "user"
			}
			parts := responsesContentParts(it.Content)
			out = append(out, lane.Message{Role: role, Parts: parts})
		case "function_call":
			out = append(out, lane.Message{Role: lane.RoleAssistant,
				Parts: []lane.Part{lane.ToolCallPart{ID: it.CallID, Name: it.Name, Arguments: it.Arguments}}})
		case "function_call_output":
			out = append(out, lane.Message{Role: lane.RoleTool,
				Parts: []lane.Part{lane.ToolResultPart{ToolCallID: it.CallID, Text: it.Output}}})
		default:
			// reasoning items are never echoed upstream
		}
	}
	return out
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, openaiError("invalid request body: "+err.Error(), "invalid_request_error"))
		return
	}
	agent := s.agentOf(r)
	unified := convertResponsesInput(req.Input, req.Instructions)
	if len(unified) == 0 {
		writeJSON(w, 400, openaiError("input must not be empty", "invalid_request_error"))
		return
	}
	needs := lane.SummarizeNeeds(unified, len(req.Tools))
	// Namespaced model id → user-added provider (converted to its chat wire).
	if p, upstream, ok := s.providerRoute(req.Model); ok {
		if !p.Enabled {
			writeJSON(w, 400, openaiError("自定义供应商 "+p.Name+" 已停用，请到「自定义 API」开启", "invalid_request_error"))
			return
		}
		s.relayResponses(w, r, req, p, upstream, agent, unified)
		return
	}
	model := req.Model
	effortDeclared := ""
	if req.Reasoning != nil {
		effortDeclared = req.Reasoning.Effort
	}
	effort := s.resolveEffort(model, effortDeclared)
	base := lane.BaseModelId(model)
	seed := SessionSeed(agent, "", unified)
	turn := TurnSeed(unified)

	ctx := r.Context()
	collector := newResponsesCollector()
	outcome, uerr := s.Lane.Complete(ctx, lane.Request{
		Model: model, Effort: effort, Messages: unified,
		Tools: convertOpenAITools(req.Tools), MaxTokens: req.MaxOutputTokens,
		SessionSeed: seed, TurnSeed: turn,
			Agent:       agent, Needs: needs,
	}, collector.consume)
	served := servedModel(outcome, base)
	if uerr != nil && !collector.hasAnything() {
		if uerr.Exhausted && s.fallbackEnabled() {
			if usage, finish, fbServed, _, ok := s.providerFallback(ctx,
				s.providerFallbackPicks(needs, base), unified,
				convertOpenAITools(req.Tools), req.MaxOutputTokens, agent, collector.consume); ok {
				fbOutcome := lane.Outcome{Usage: usage, Finish: finish, ServedModel: fbServed}
				w.Header().Set("x-zen-gate-served-by", fbServed)
				if req.Stream {
					s.writeResponsesEvents(w, fbServed, collector, fbOutcome)
				} else {
					writeJSON(w, 200, collector.finalResponse(fbServed, fbOutcome, false))
				}
				return
			}
		}
		setRetryAfter(w, uerr)
		body := openaiError(uerr.Message, errorType(uerr))
		if uerr.Code == lane.CodeQuota {
			body = withSuggestions(body, s.Lane.Candidates(needs, base, 3))
		}
		writeJSON(w, errorStatus(uerr), body)
		return
	}
	if req.Stream {
		s.writeResponsesEvents(w, served, collector, outcome)
	} else {
		writeJSON(w, 200, collector.finalResponse(served, outcome, false))
	}
}

func (s *Server) writeResponsesEvents(w http.ResponseWriter, model string, c *responsesCollector, outcome lane.Outcome) {
	sse, err := newSSE(w)
	if err != nil {
		writeJSON(w, 500, openaiError(err.Error(), "internal_error"))
		return
	}
	respID := randomID("resp_")
	sse.event(map[string]any{"type": "response.created", "response": map[string]any{
		"id": respID, "object": "response", "status": "in_progress", "model": model}})
	oidx := 0
	for _, block := range c.blocks {
		switch block.kind {
		case "text":
			itemID := fmt.Sprintf("msg_%d", oidx)
			sse.event(map[string]any{"type": "response.output_item.added", "output_index": oidx,
				"item": map[string]any{"type": "message", "role": "assistant", "id": itemID, "status": "in_progress", "content": []any{}}})
			sse.event(map[string]any{"type": "response.output_text.delta", "output_index": oidx, "item_id": itemID, "delta": block.text})
			sse.event(map[string]any{"type": "response.output_item.done", "output_index": oidx,
				"item": map[string]any{"type": "message", "role": "assistant", "id": itemID, "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": block.text}}}})
		case "reasoning":
			itemID := fmt.Sprintf("rs_%d", oidx)
			sse.event(map[string]any{"type": "response.output_item.added", "output_index": oidx,
				"item": map[string]any{"type": "reasoning", "id": itemID, "summary": []any{}}})
			if block.text != "" {
				sse.event(map[string]any{"type": "response.reasoning_text.delta", "output_index": oidx, "item_id": itemID, "delta": block.text})
			}
			sse.event(map[string]any{"type": "response.output_item.done", "output_index": oidx,
				"item": map[string]any{"type": "reasoning", "id": itemID, "summary": []any{}}})
		case "tool-call":
			itemID := fmt.Sprintf("fc_%d", oidx)
			sse.event(map[string]any{"type": "response.output_item.added", "output_index": oidx,
				"item": map[string]any{"type": "function_call", "id": itemID, "call_id": block.id, "name": block.name, "arguments": "", "status": "in_progress"}})
			if block.text != "" {
				sse.event(map[string]any{"type": "response.function_call_arguments.delta", "output_index": oidx, "item_id": itemID, "delta": block.text})
			}
			sse.event(map[string]any{"type": "response.output_item.done", "output_index": oidx,
				"item": map[string]any{"type": "function_call", "id": itemID, "call_id": block.id, "name": block.name, "arguments": block.text, "status": "completed"}})
		}
		oidx++
	}
	status := "completed"
	event := "response.completed"
	if outcome.Finish == lane.FinishMaxTokens {
		status = "incomplete"
		event = "response.incomplete"
	}
	sse.event(map[string]any{"type": event, "response": map[string]any{
		"id": respID, "object": "response", "status": status, "model": model,
		"output": c.outputItems(), "usage": responsesUsage(outcome.Usage)}})
	sse.done()
}

func responsesUsage(u lane.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  u.Input + u.CacheRead,
		"output_tokens": u.Output,
		"total_tokens":  u.Input + u.CacheRead + u.Output,
	}
}

// responsesCollector buffers lane chunks into ordered output blocks so both
// the streaming and buffered paths share one shape.
type responsesCollector struct {
	blocks []*responsesBlock
	open   map[int]*responsesBlock
}

type responsesBlock struct {
	kind string // text | reasoning | tool-call
	id   string
	name string
	text string
}

func newResponsesCollector() *responsesCollector {
	return &responsesCollector{open: map[int]*responsesBlock{}}
}

func (c *responsesCollector) hasAnything() bool { return len(c.blocks) > 0 }

func (c *responsesCollector) consume(chunk lane.Chunk) {
	switch chunk.Kind {
	case lane.ChunkBlockStart:
		b := &responsesBlock{kind: chunk.BlockType, id: chunk.ID, name: chunk.Name}
		c.open[chunk.Index] = b
		c.blocks = append(c.blocks, b)
	case lane.ChunkTextDelta, lane.ChunkReasonDelta, lane.ChunkToolCallDelta:
		if b, ok := c.open[chunk.Index]; ok {
			b.text += chunk.Delta
		}
	case lane.ChunkBlockEnd:
		delete(c.open, chunk.Index)
	}
}

func (c *responsesCollector) outputItems() []any {
	out := []any{}
	for _, b := range c.blocks {
		switch b.kind {
		case "text":
			out = append(out, map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": b.text}}})
		case "reasoning":
			out = append(out, map[string]any{"type": "reasoning", "summary": []any{}})
		case "tool-call":
			args := b.text
			if args == "" {
				args = "{}"
			}
			out = append(out, map[string]any{"type": "function_call", "call_id": b.id, "name": b.name, "arguments": args})
		}
	}
	return out
}

func (c *responsesCollector) finalResponse(model string, outcome lane.Outcome, stream bool) map[string]any {
	status := "completed"
	if outcome.Finish == lane.FinishMaxTokens {
		status = "incomplete"
	}
	return map[string]any{
		"id": randomID("resp_"), "object": "response", "status": status, "model": model,
		"output": c.outputItems(), "usage": responsesUsage(outcome.Usage),
	}
}
