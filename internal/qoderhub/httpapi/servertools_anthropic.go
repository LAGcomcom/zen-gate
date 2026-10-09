package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/qoderhub/remote"
	"zen-gate/internal/qoderhub/service"
	"zen-gate/internal/qoderhub/tooltypes"
)

// Server-side tool injection: the proxy advertises gateway-only tools and runs
// them itself in an agentic loop, never surfacing those calls to the client —
// EXCEPT web_search, which is surfaced as native Anthropic server-tool blocks so
// clients (e.g. Claude Code) render their built-in "searched the web" UI.
// Genuine client tools (Bash, etc.) pass through untouched.

const maxMediaToolRounds = 4

func mediaToolsEnabled() bool { return truthyEnv("QODERCN_INJECT_MEDIA_TOOLS") }

// isServerTool reports whether a tool name is one the proxy executes itself.
func isServerTool(name string) bool {
	switch strings.TrimSpace(name) {
	case webSearchToolName, imageSearchToolName, textPolishToolName:
		return true
	}
	return false
}

// anthropicServerToolDefs returns which server tools to advertise: web_search if
// the client declares a hosted web_search tool AND replacement is enabled
// (QODERCN_REPLACE_WEB_SEARCH, default on), the rest when injection is on.
func (s *Server) anthropicServerToolDefs(req anthropicRequest) ([]any, bool) {
	var defs []any
	if replaceWebSearchEnabled() && hasAnthropicHostedWebSearchTool(req.Tools) {
		defs = append(defs, webSearchSpec.anthropicDef())
	}
	if mediaToolsEnabled() {
		defs = append(defs, imageSearchSpec.anthropicDef(), textPolishSpec.anthropicDef())
	}
	return defs, len(defs) > 0
}

// injectAnthropicServerTools replaces the client's hosted web_search def with our
// callable server-tool defs, skipping any the client already declares by name.
func injectAnthropicServerTools(req anthropicRequest, defs []any) anthropicRequest {
	stripped := stripAnthropicHostedWebSearchTool(req.Tools)
	existing := map[string]bool{}
	var items []any
	if arr, ok := stripped.([]any); ok {
		items = append(items, arr...)
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				existing[strings.TrimSpace(stringFromAny(m["name"]))] = true
			}
		}
	}
	for _, d := range defs {
		if m, ok := d.(map[string]any); ok && existing[stringFromAny(m["name"])] {
			continue
		}
		items = append(items, d)
	}
	req.Tools = items
	return req
}

// stripInjectedServerTools removes our injected server tools, leaving client tools.
func stripInjectedServerTools(raw any) any {
	arr, ok := raw.([]any)
	if !ok {
		return raw
	}
	kept := make([]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok && isServerTool(stringFromAny(m["name"])) {
			continue
		}
		kept = append(kept, it)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func partitionServerToolCalls(calls []tooltypes.ToolCall) (ours, others []tooltypes.ToolCall) {
	for _, c := range calls {
		if isServerTool(c.Name) {
			ours = append(ours, c)
		} else {
			others = append(others, c)
		}
	}
	return ours, others
}

func isWebSearchCall(c tooltypes.ToolCall) bool {
	return strings.TrimSpace(c.Name) == webSearchToolName
}

// argBool reads a boolean tool argument, falling back to def when absent (so an
// omitted flag keeps its default rather than becoming false).
func argBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

// executeServerTool runs one injected tool call and returns its result as text.
func (s *Server) executeServerTool(ctx context.Context, call tooltypes.ToolCall) string {
	switch call.Name {
	case webSearchToolName:
		_, _, modelText, _ := s.runWebSearch(ctx, call)
		return modelText
	case imageSearchToolName:
		query := strings.TrimSpace(stringFromAny(call.Arguments["query"]))
		count := 0
		if v, ok := call.Arguments["count"].(float64); ok {
			count = int(v)
		}
		results, err := s.svc.ImageSearch(ctx, query, count)
		if err != nil {
			return "Image search failed: " + err.Error()
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Image search results for %q:\n", query)
		for i, r := range results {
			fmt.Fprintf(&b, "[%d] %s — %s (%dx%d)\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.ImageURL), r.Width, r.Height)
		}
		return b.String()
	case textPolishToolName:
		text := strings.TrimSpace(stringFromAny(call.Arguments["text"]))
		if text == "" {
			return "Text polish failed: empty text"
		}
		polished, err := s.svc.PolishText(ctx, text)
		if err != nil {
			return "Text polish failed: " + err.Error()
		}
		return polished
	}
	return "unknown tool"
}

// runWebSearch executes one web_search server-tool call ONCE, returning the
// native web_search_result items (for the client UI), the text form (fed back to
// the model), and ok=false on a hard error (empty query / API failure).
func (s *Server) runWebSearch(ctx context.Context, call tooltypes.ToolCall) (query string, items []map[string]any, modelText string, ok bool) {
	query = strings.TrimSpace(stringFromAny(call.Arguments["query"]))
	if query == "" {
		return "", nil, "Web search failed: empty query", false
	}
	opts := remote.WebSearchOptions{
		TimeRange:    strings.TrimSpace(stringFromAny(call.Arguments["timeRange"])),
		MainText:     argBool(call.Arguments, "mainText", false),
		MarkdownText: argBool(call.Arguments, "markdownText", false),
		Summary:      argBool(call.Arguments, "summary", true), // default on: richer than snippet
	}
	results, err := s.svc.WebSearch(ctx, query, opts)
	if err != nil {
		return query, nil, "Web search failed: " + err.Error(), false
	}
	items = make([]map[string]any, 0, len(results))
	for _, r := range results {
		item := map[string]any{"type": "web_search_result", "title": strings.TrimSpace(r.Title), "url": strings.TrimSpace(r.Link)}
		if pt := strings.TrimSpace(r.PublishedTime); pt != "" {
			item["page_age"] = pt
		}
		items = append(items, item)
	}
	if len(results) == 0 {
		modelText = fmt.Sprintf("No web search results for %q.", query)
	} else {
		modelText = formatWebSearchResults(query, results)
	}
	return query, items, modelText, true
}

// webSearchResultID reuses the model's tool-call id (matching server_tool_use and
// web_search_tool_result), synthesizing one only if the model left it empty.
func webSearchResultID(call tooltypes.ToolCall) string {
	if id := strings.TrimSpace(call.ID); id != "" {
		return id
	}
	return fmt.Sprintf("srvtoolu_%d", time.Now().UnixNano())
}

// webSearchBlocks builds the native server_tool_use + web_search_tool_result
// block pair (non-streaming shape, full input inline).
func webSearchBlocks(id, query string, items []map[string]any, ok bool) []map[string]any {
	var content any = items
	if !ok {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	}
	return []map[string]any{
		{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": query}},
		{"type": "web_search_tool_result", "tool_use_id": id, "content": content},
	}
}

// withWebSearchCount adds usage.server_tool_use.web_search_requests when any
// web_search ran, matching Anthropic's native usage shape.
func withWebSearchCount(usage map[string]any, n int) map[string]any {
	if n > 0 {
		usage["server_tool_use"] = map[string]any{"web_search_requests": n}
	}
	return usage
}

// foldServerToolResults runs our server tools and formats their results as text
// to fold into the assistant message at hand-off (the client echoes it back).
// Used by the OpenAI server-tools path; the Anthropic path surfaces web_search
// natively and folds only the remaining tools.
func (s *Server) foldServerToolResults(ctx context.Context, ours []tooltypes.ToolCall) string {
	var b strings.Builder
	for _, c := range ours {
		label := strings.TrimSuffix(c.Name, serverToolSuffix)
		out := s.executeServerTool(ctx, c)
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%s results]\n%s", label, out)
	}
	return b.String()
}

// appendText joins base and extra with a blank line, tolerating empty inputs.
func appendText(base, extra string) string {
	if strings.TrimSpace(extra) == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + "\n\n" + extra
}

// serverToolUsage sums usage across the agentic loop's rounds; each round is a
// separate billed call, so reporting only the last round under-counts credits.
type serverToolUsage struct {
	input, output, cached, cacheCreate, reasoning, total int
	credits, originalCredits                             float64
	billable                                             bool
}

func (u *serverToolUsage) add(r *service.ChatResult) {
	if r == nil {
		return
	}
	u.input += r.InputTokens
	u.output += r.OutputTokens
	u.cached += r.CachedInputTokens
	u.cacheCreate += r.CacheCreationInputTokens
	u.reasoning += r.ReasoningTokens
	u.total += r.UsedTokens
	u.credits += r.Credits
	u.originalCredits += r.OriginalCredits
	if r.Billable {
		u.billable = true
	}
}

// applyTo copies content with its usage fields replaced by the accumulated totals.
func (u *serverToolUsage) applyTo(content *service.ChatResult) *service.ChatResult {
	c := *content
	c.InputTokens = u.input
	c.OutputTokens = u.output
	c.CachedInputTokens = u.cached
	c.CacheCreationInputTokens = u.cacheCreate
	c.ReasoningTokens = u.reasoning
	c.UsedTokens = u.total
	c.Credits = u.credits
	c.OriginalCredits = u.originalCredits
	c.Billable = u.billable
	return &c
}

// result projects the totals onto an empty ChatResult for usage-only frames.
func (u *serverToolUsage) result() *service.ChatResult {
	return u.applyTo(&service.ChatResult{})
}

// maybeServeAnthropicServerTools runs the injected server-tool loop when any
// server tool applies to this request, and reports whether it handled the
// response. This is the single seam the Anthropic handler calls into.
func (s *Server) maybeServeAnthropicServerTools(w http.ResponseWriter, r *http.Request, req anthropicRequest) bool {
	defs, ok := s.anthropicServerToolDefs(req)
	if !ok {
		return false
	}
	req = injectAnthropicServerTools(req, defs)
	s.handleAnthropicServerTools(w, r, req)
	return true
}

// appendToolResultTurn runs our tool calls once and appends the assistant
// tool_use turn + user tool_result turn so the model can continue. web_search
// calls also contribute their native block pair (accumulated in searchBlocks),
// so they are executed exactly once. Returns the grown request and the appended
// web_search block pairs plus how many web searches ran.
func (s *Server) appendToolResultTurn(ctx context.Context, req anthropicRequest, result *service.ChatResult, ours []tooltypes.ToolCall, lastRound bool) (anthropicRequest, []map[string]any, int) {
	assistantContent := make([]any, 0, len(ours)+1)
	if strings.TrimSpace(result.Text) != "" {
		assistantContent = append(assistantContent, map[string]any{"type": "text", "text": result.Text})
	}
	userContent := make([]any, 0, len(ours))
	var searchBlocks []map[string]any
	searches := 0
	for _, c := range ours {
		var modelText string
		if isWebSearchCall(c) {
			query, items, txt, ok := s.runWebSearch(ctx, c)
			searchBlocks = append(searchBlocks, webSearchBlocks(webSearchResultID(c), query, items, ok)...)
			searches++
			modelText = txt
		} else {
			modelText = s.executeServerTool(ctx, c)
		}
		assistantContent = append(assistantContent, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Arguments})
		userContent = append(userContent, map[string]any{"type": "tool_result", "tool_use_id": c.ID, "content": modelText})
	}
	req.Messages = append(req.Messages, rawMessage{Role: "assistant", Content: assistantContent})
	req.Messages = append(req.Messages, rawMessage{Role: "user", Content: userContent})
	if lastRound {
		req.Tools = stripInjectedServerTools(req.Tools)
	}
	return req, searchBlocks, searches
}

// handleAnthropicServerTools runs the agentic loop that advertises and executes
// our injected server tools. Tools must already be injected into req by caller.
func (s *Server) handleAnthropicServerTools(w http.ResponseWriter, r *http.Request, req anthropicRequest) {
	if req.Stream {
		s.streamAnthropicServerTools(w, r, req)
		return
	}
	ctx := r.Context()
	var usageAcc serverToolUsage
	var searchBlocks []map[string]any // native web_search blocks accumulated across rounds
	webSearchRequests := 0
	for round := 0; ; round++ {
		if ctx.Err() != nil {
			return // client disconnected; stop issuing more rounds / gateway calls
		}
		normalized, err := normalizeAnthropicRequest(req)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.applyDefaultModel(&normalized)
		result, err := s.svc.Generate(ctx, normalized)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		usageAcc.add(result)
		ours, others := partitionServerToolCalls(result.ToolCalls)
		if len(ours) == 0 || len(others) > 0 || round >= maxMediaToolRounds {
			if len(ours) > 0 {
				// Surface web_search natively; fold the remaining server tools into text.
				var folded string
				for _, c := range ours {
					if isWebSearchCall(c) {
						query, items, _, ok := s.runWebSearch(ctx, c)
						searchBlocks = append(searchBlocks, webSearchBlocks(webSearchResultID(c), query, items, ok)...)
						webSearchRequests++
						continue
					}
					label := strings.TrimSuffix(c.Name, serverToolSuffix)
					folded = appendText(folded, fmt.Sprintf("[%s results]\n%s", label, s.executeServerTool(ctx, c)))
				}
				result.Text = appendText(result.Text, folded)
				result.ToolCalls = others
			}
			s.emitAnthropicResultJSON(w, normalized, usageAcc.applyTo(result), searchBlocks, webSearchRequests)
			return
		}
		var rounds []map[string]any
		var n int
		req, rounds, n = s.appendToolResultTurn(ctx, req, result, ours, round+1 >= maxMediaToolRounds)
		searchBlocks = append(searchBlocks, rounds...)
		webSearchRequests += n
	}
}

// streamAnthropicServerTools streams the agentic loop as one Anthropic message:
// deltas flow live, our tool calls run server-side, continuation stays in-message.
// web_search calls stream as native server_tool_use + web_search_tool_result
// blocks; other server tools stay hidden (only fed back to the model).
func (s *Server) streamAnthropicServerTools(w http.ResponseWriter, r *http.Request, req anthropicRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return
	}
	ctx := r.Context()
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "lingma"
	}
	streamingHeaders(w)
	_ = writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": fmt.Sprintf("msg_%d", time.Now().UnixNano()), "type": "message", "role": "assistant",
			"content": []any{}, "model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	index := 0
	var usageAcc serverToolUsage
	webSearchRequests := 0
	blockStart := func(block map[string]any) {
		_ = writeSSEEvent(w, flusher, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
	}
	blockDelta := func(delta map[string]any) {
		_ = writeSSEEvent(w, flusher, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
	}
	blockStop := func() {
		_ = writeSSEEvent(w, flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	// emitWebSearch runs one web_search call and streams it as native server-tool
	// blocks, returning the text form to feed back to the model.
	emitWebSearch := func(call tooltypes.ToolCall) string {
		query, items, modelText, ok := s.runWebSearch(ctx, call)
		id := webSearchResultID(call)
		input, _ := json.Marshal(map[string]any{"query": query})
		blockStart(map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{}})
		blockDelta(map[string]any{"type": "input_json_delta", "partial_json": string(input)})
		blockStop()
		var content any = items
		if !ok {
			content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
		}
		blockStart(map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content})
		blockStop()
		webSearchRequests++
		return modelText
	}
	endStream := func(result *service.ChatResult, stopReason string, stopSequence any) {
		_ = writeSSEEvent(w, flusher, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": stopSequence}, "usage": withWebSearchCount(anthropicFinalUsage(result), webSearchRequests)})
		_ = writeSSEEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
	}

	for round := 0; ; round++ {
		if ctx.Err() != nil {
			return // client disconnected; stop issuing more rounds / gateway calls
		}
		normalized, err := normalizeAnthropicRequest(req)
		if err != nil {
			endStream(usageAcc.result(), "end_turn", nil)
			return
		}
		s.applyDefaultModel(&normalized)
		emitThinking := reasoningEffortEnabled(normalized.ReasoningEffort)

		events, done, err := s.svc.GenerateStream(ctx, normalized)
		if err != nil {
			endStream(usageAcc.result(), "end_turn", nil)
			return
		}

		// Strip emulated text action blocks from the visible text stream (native
		// tool calls surface via result.ToolCalls and are handled after the round).
		filter := newToolStreamFilter(s.svc.EmulatesTextTools(normalized))
		thinkingOpen, textOpen := false, false
		emitText := func(piece string) {
			if piece == "" {
				return
			}
			if thinkingOpen {
				blockStop()
				thinkingOpen = false
			}
			if !textOpen {
				blockStart(map[string]any{"type": "text", "text": ""})
				textOpen = true
			}
			blockDelta(map[string]any{"type": "text_delta", "text": piece})
		}
		for ev := range events {
			switch ev.Type {
			case service.StreamEventThinking:
				if !emitThinking {
					continue
				}
				if textOpen {
					blockStop()
					textOpen = false
				}
				if !thinkingOpen {
					blockStart(map[string]any{"type": "thinking", "thinking": ""})
					thinkingOpen = true
				}
				blockDelta(map[string]any{"type": "thinking_delta", "thinking": ev.Delta})
			case service.StreamEventText:
				for _, piece := range filter.Push(ev.Delta) {
					emitText(piece)
				}
				// StreamEventToolCall fragments are buffered by the service and
				// surface in the final result; we decide on them there.
			}
		}
		for _, piece := range filter.Flush() {
			emitText(piece)
		}
		if thinkingOpen {
			blockStop()
		}
		if textOpen {
			blockStop()
		}

		res := <-done
		if res.Err != nil || res.Result == nil {
			endStream(usageAcc.result(), "end_turn", nil)
			return
		}
		result := res.Result
		usageAcc.add(result)
		ours, others := partitionServerToolCalls(result.ToolCalls)

		if len(ours) == 0 || len(others) > 0 || round >= maxMediaToolRounds {
			// Finalize: surface web_search natively, fold remaining server tools to text.
			if len(ours) > 0 {
				var folded strings.Builder
				for _, c := range ours {
					if isWebSearchCall(c) {
						emitWebSearch(c)
						continue
					}
					label := strings.TrimSuffix(c.Name, serverToolSuffix)
					if folded.Len() > 0 {
						folded.WriteString("\n\n")
					}
					fmt.Fprintf(&folded, "[%s results]\n%s", label, s.executeServerTool(ctx, c))
				}
				if folded.Len() > 0 {
					blockStart(map[string]any{"type": "text", "text": ""})
					blockDelta(map[string]any{"type": "text_delta", "text": folded.String()})
					blockStop()
				}
				result.ToolCalls = others
			}
			for _, tc := range others {
				argsJSON, _ := json.Marshal(tc.Arguments)
				blockStart(map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}})
				blockDelta(map[string]any{"type": "input_json_delta", "partial_json": string(argsJSON)})
				blockStop()
			}
			stopReason, stopSequence := anthropicStopReason(result)
			endStream(usageAcc.result(), stopReason, stopSequence)
			return
		}
		// Execute our tools server-side and continue the same message next round.
		// web_search streams as native blocks now; the rest are fed back silently.
		assistantContent := make([]any, 0, len(ours)+1)
		if strings.TrimSpace(result.Text) != "" {
			assistantContent = append(assistantContent, map[string]any{"type": "text", "text": result.Text})
		}
		userContent := make([]any, 0, len(ours))
		for _, c := range ours {
			var modelText string
			if isWebSearchCall(c) {
				modelText = emitWebSearch(c)
			} else {
				modelText = s.executeServerTool(ctx, c)
			}
			assistantContent = append(assistantContent, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Arguments})
			userContent = append(userContent, map[string]any{"type": "tool_result", "tool_use_id": c.ID, "content": modelText})
		}
		req.Messages = append(req.Messages, rawMessage{Role: "assistant", Content: assistantContent})
		req.Messages = append(req.Messages, rawMessage{Role: "user", Content: userContent})
		if round+1 >= maxMediaToolRounds {
			req.Tools = stripInjectedServerTools(req.Tools)
		}
	}
}

func (s *Server) emitAnthropicResultJSON(w http.ResponseWriter, req service.ChatRequest, result *service.ChatResult, searchBlocks []map[string]any, webSearchRequests int) {
	content := make([]map[string]any, 0, 2+len(searchBlocks)+len(result.ToolCalls))
	if shouldEmitAnthropicThinking(req, result) {
		content = append(content, map[string]any{"type": "thinking", "thinking": result.ThoughtText})
	}
	// Native web_search server-tool blocks (from any round) precede the answer.
	content = append(content, searchBlocks...)
	if strings.TrimSpace(result.Text) != "" {
		content = append(content, map[string]any{"type": "text", "text": result.Text})
	}
	for _, tc := range result.ToolCalls {
		// Never surface our injected tools; only genuine client tools (Bash, etc.)
		// pass through for the client to execute.
		if isServerTool(tc.Name) {
			continue
		}
		content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": tc.Arguments})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stopReason, stopSequence := anthropicStopReason(result)
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "lingma"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
		"usage":         withWebSearchCount(anthropicFinalUsage(result), webSearchRequests),
	})
}
