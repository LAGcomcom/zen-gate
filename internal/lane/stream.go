package lane

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// CheckpointLimit caps the reasoning text kept for stream-cut recovery.
const CheckpointLimit = 131072

// Decoder turns upstream payloads (SSE frames or one JSON body) into
// normalized Chunks, across the three wire protocols.
type Decoder struct {
	wire     string // chat | messages | responses
	rename   map[string]string
	emit     func(Chunk)
	mu       sync.Mutex
	blocks   map[string]int // key → block index
	next     int
	toolArgs map[int]string // block index → accumulated tool arguments
	toolName map[int]string // block index → latest non-empty tool name
	kind     map[int]string // block index → block type
	result   StreamResult
	dsml     dsmlScrubber
	// messages-wire provider block index → key
	pidx map[float64]string
	// responses-wire output_index → key
	oidx map[float64]string
	// chat-wire tool calls without index, grouped by id
	chatTool map[string]int // provider id → block index
}

// StreamResult accumulates what one upstream call produced.
//
// DeepSeek v4-family models occasionally leak internal DSML control markup
// into visible content at the reasoning→action boundary: a literal
// "<｜DSML｜ calls>" marker streamed as text right before a legitimate tool
// call. Scrub <｜DSML｜…> fragments from visible text as it flows; a tail that
// could still complete into the opening is held back until the next delta
// resolves it, and Finish flushes what is left. A user quoting the token
// verbatim loses those characters — the trade is deliberate: leaking control
// tokens routinely is worse.
const dsmlOpening = "<｜DSML｜"

var dsmlTag = regexp.MustCompile("<｜DSML｜[^>]*>")

type dsmlScrubber struct{ held string }

func (s *dsmlScrubber) push(delta string) string {
	text := s.held + delta
	s.held = ""
	if cut := strings.LastIndex(text, "<"); cut != -1 {
		tail := text[cut:]
		if strings.HasPrefix(dsmlOpening, tail) ||
			(strings.HasPrefix(tail, dsmlOpening) && !strings.Contains(tail, ">")) {
			s.held = tail
			text = text[:cut]
		}
	}
	return dsmlTag.ReplaceAllString(text, "")
}

func (s *dsmlScrubber) flush() string {
	out := s.held
	s.held = ""
	return dsmlTag.ReplaceAllString(out, "")
}

type StreamResult struct {
	Usage  Usage
	Finish string // "" = upstream closed without a terminal frame
	// FinishToken is the raw terminal token the wire carried ("stop",
	// "end_turn", "failed", …) before mapping. The mapped Finish collapses
	// unknown spellings into stop, so a normal-ending judgement must consult
	// the token, not the mapping.
	FinishToken         string
	SawFinish           bool
	SawReasoning        bool
	SawText             bool
	SawToolCall         bool
	BrokenToolCall      bool
	ReasoningText       string
	// AnswerText is the concatenated answer, capped like the reasoning
	// checkpoint — it feeds the max-tokens continuation without buffering
	// more than one cap's worth.
	AnswerText          string
	CheckpointTruncated bool
	MaxTokens           int // echo of what was requested, for stats
}

// NewDecoder creates a decoder for one upstream call.
func NewDecoder(wire string, rename map[string]string, emit func(Chunk)) *Decoder {
	return &Decoder{
		wire:     wire,
		rename:   rename,
		emit:     emit,
		blocks:   map[string]int{},
		toolArgs: map[int]string{},
		toolName: map[int]string{},
		kind:     map[int]string{},
		pidx:     map[float64]string{},
		oidx:     map[float64]string{},
		chatTool: map[string]int{},
	}
}

// reasoningOf extracts the thinking one chat delta carries, in whichever
// spelling the upstream used. `reasoning` is the spelling this lane sends;
// `reasoning_content`, `reasoning_text` and `thinking` are the other spellings
// in the wild — a turn whose thinking arrived under one of those used to reach
// the caller as a turn with no reasoning at all: no thinking shown, an empty
// ReasoningText that keeps the recovery path from firing, and an apparently
// empty answer. Only the first non-empty spelling is taken: some relays report
// the same text under two names at once, and reading every name would double
// the block.
func reasoningOf(delta map[string]any) (string, bool) {
	for _, field := range []string{"reasoning", "reasoning_content", "reasoning_text", "thinking"} {
		if c, ok := delta[field].(string); ok && c != "" {
			return c, true
		}
	}
	if parts, ok := delta["reasoning_details"].([]any); ok {
		text := ""
		for _, part := range parts {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					text += t
				}
			}
		}
		if text != "" {
			return text, true
		}
	}
	return "", false
}

// toolDelta records and emits one tool-argument fragment.
func (d *Decoder) toolDelta(idx int, id, name, delta string) {
	if name != "" {
		d.toolName[idx] = name
	}
	if delta == "" {
		return
	}
	d.toolArgs[idx] += delta
	d.emit(Chunk{Kind: ChunkToolCallDelta, Index: idx, ID: id, Name: name, Delta: delta})
}

func (d *Decoder) startBlock(key, blockType, id, name string) int {
	if idx, ok := d.blocks[key]; ok {
		return idx
	}
	idx := d.next
	d.next++
	d.blocks[key] = idx
	d.kind[idx] = blockType
	if blockType == "tool-call" && name != "" {
		d.toolName[idx] = name
	}
	d.emit(Chunk{Kind: ChunkBlockStart, Index: idx, BlockType: blockType, ID: id, Name: name})
	return idx
}

func (d *Decoder) endBlock(key string) {
	if idx, ok := d.blocks[key]; ok {
		d.emit(Chunk{Kind: ChunkBlockEnd, Index: idx})
		delete(d.blocks, key)
	}
}

func (d *Decoder) endAllBlocks() {
	for key := range d.blocks {
		d.endBlock(key)
	}
}

// Decode consumes one upstream payload frame.
func (d *Decoder) Decode(payload []byte) {
	var frame map[string]any
	if json.Unmarshal(payload, &frame) != nil {
		return
	}
	switch d.wire {
	case "messages":
		d.decodeMessages(frame)
	case "responses":
		d.decodeResponses(frame)
	default:
		d.decodeChat(frame)
	}
}

// Finish finalizes the call: closes open blocks, applies finish mapping and
// records terminal flags. Returns the result snapshot.
func (d *Decoder) Finish() StreamResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Spill any DSML hold-back: a tail that looked like the opening of the
	// control marker but never completed is ordinary text after all.
	if rest := d.dsml.flush(); rest != "" {
		idx := d.startBlock("text", "text", "", "")
		d.emit(Chunk{Kind: ChunkTextDelta, Index: idx, Delta: rest})
		d.result.SawText = true
	}
	// Chat-wire tool blocks have no explicit stop frame; validate every tool
	// block still open before closing it.
	for key, idx := range d.blocks {
		if strings.HasPrefix(key, "tool:") {
			d.checkToolBlock(idx)
		}
	}
	d.endAllBlocks()
	if d.result.SawFinish && d.result.BrokenToolCall {
		// A broken tool call (unparseable arguments, or a call that never
		// learned its name) must not read as a clean success on any finish.
		d.result.Finish = FinishMaxTokens
	}
	return d.result
}

// --- chat wire ------------------------------------------------------------

func (d *Decoder) decodeChat(frame map[string]any) {
	if errObj, has := frame["error"]; has {
		// surfaced again by readSSE; nothing to project
		_ = errObj
		return
	}
	if choices, ok := frame["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice != nil {
			if delta, ok := choice["delta"].(map[string]any); ok {
				d.chatDelta(delta)
			}
			if msg, ok := choice["message"].(map[string]any); ok {
				// Non-streamed JSON answer delivered on a stream request.
				d.chatDelta(msg)
			}
			if fr, ok := choice["finish_reason"]; ok && fr != nil {
				d.setFinish(jsonString(fr))
			}
		}
	}
}

func (d *Decoder) chatDelta(delta map[string]any) {
	if c, ok := delta["content"].(string); ok && c != "" {
		idx := d.startBlock("text", "text", "", "")
		scrubbed := d.dsml.push(c)
		if scrubbed != "" {
			d.appendAnswer(scrubbed)
			d.emit(Chunk{Kind: ChunkTextDelta, Index: idx, Delta: scrubbed})
			d.result.SawText = true
		}
	}
	if reasoning, ok := reasoningOf(delta); ok {
		idx := d.startBlock("reasoning", "reasoning", "", "")
		d.emit(Chunk{Kind: ChunkReasonDelta, Index: idx, Delta: reasoning})
		d.result.SawReasoning = true
		d.appendCheckpoint(reasoning)
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			m, _ := tc.(map[string]any)
			if m == nil {
				continue
			}
			d.chatToolCall(m)
		}
	}
}

func (d *Decoder) chatToolCall(m map[string]any) {
	key := ""
	if v, ok := m["index"].(float64); ok {
		key = "tool:" + jsonString(v)
	}
	id := jsonString(m["id"])
	if id == "" {
		if fn, ok := m["function"].(map[string]any); ok {
			id = jsonString(fn["id"])
		}
	}
	if key == "" {
		// Parallel calls without index are chunked by id; a bare argument
		// continuation belongs to the previous block for that id.
		if id != "" {
			key = "tool:" + id
		} else if len(d.chatTool) > 0 {
			// no id and no index: attach to the last open tool block
			for k, idx := range d.blocks {
				if strings.HasPrefix(k, "tool:") {
					_ = idx
					key = k
					break
				}
			}
			if key == "" {
				return
			}
		} else {
			return
		}
	}
	name := ""
	args := ""
	if fn, ok := m["function"].(map[string]any); ok {
		name = jsonString(fn["name"])
		args = jsonString(fn["arguments"])
	}
	// Mint an id only when this frame opens a new tool block. A continuation
	// frame (same key, no id of its own) must stay id-less: minting a fresh id
	// there would advertise a different call id than the first frame sent.
	if id == "" {
		if _, open := d.blocks[key]; !open {
			id = "call_" + mintHex(12)
		}
	}
	idx := d.startBlock(key, "tool-call", id, RestoreToolName(name, d.rename))
	d.result.SawToolCall = true
	d.toolDelta(idx, RestoreToolName(id, d.rename), RestoreToolName(name, d.rename), args)
}

// --- messages (Anthropic) wire ---------------------------------------------

func (d *Decoder) decodeMessages(frame map[string]any) {
	typ := jsonString(frame["type"])
	switch typ {
	case "message_start":
		// usage merged by readSSE
	case "content_block_start":
		idx, _ := frame["index"].(float64)
		cb, _ := frame["content_block"].(map[string]any)
		cbType := jsonString(cb["type"])
		key := "p" + jsonString(idx)
		d.pidx[idx] = key
		switch cbType {
		case "text":
			d.startBlock(key, "text", "", "")
		case "thinking":
			d.startBlock(key, "reasoning", "", "")
		case "tool_use":
			id := jsonString(cb["id"])
			name := RestoreToolName(jsonString(cb["name"]), d.rename)
			if id == "" {
				id = "call_" + mintHex(12)
			}
			d.startBlock(key, "tool-call", id, name)
			d.result.SawToolCall = true
		}
	case "content_block_delta":
		idx, _ := frame["index"].(float64)
		key, ok := d.pidx[idx]
		if !ok {
			return
		}
		blockIdx, ok := d.blocks[key]
		if !ok {
			return
		}
		delta, _ := frame["delta"].(map[string]any)
		switch jsonString(delta["type"]) {
		case "text_delta":
			t := jsonString(delta["text"])
			if t != "" {
				scrubbed := d.dsml.push(t)
				if scrubbed != "" {
					d.appendAnswer(scrubbed)
					d.emit(Chunk{Kind: ChunkTextDelta, Index: blockIdx, Delta: scrubbed})
					d.result.SawText = true
				}
			}
		case "thinking_delta":
			t := jsonString(delta["thinking"])
			if t != "" {
				d.emit(Chunk{Kind: ChunkReasonDelta, Index: blockIdx, Delta: t})
				d.result.SawReasoning = true
				d.appendCheckpoint(t)
			}
		case "input_json_delta":
			t := jsonString(delta["partial_json"])
			d.toolDelta(blockIdx, "", "", t)
		}
	case "content_block_stop":
		idx, _ := frame["index"].(float64)
		if key, ok := d.pidx[idx]; ok {
			if blockIdx, ok2 := d.blocks[key]; ok2 {
				// validate accumulated tool args when the block ends
				d.checkToolBlock(blockIdx)
				d.endBlock(key)
			}
		}
	case "message_delta":
		if sr, ok := frame["delta"].(map[string]any); ok {
			if reason := jsonString(sr["stop_reason"]); reason != "" {
				d.setFinish(reason)
			}
		}
	case "message_stop":
		// This frame carries no token of its own — an ending whose
		// normal-ness only the caller can vouch for.
		d.setFinish("")
	case "error":
		// handled by readSSE
	}
}

// appendAnswer records answer text for the continuation path, capped at the
// checkpoint limit.
func (d *Decoder) appendAnswer(text string) {
	if d.result.CheckpointTruncated && len(d.result.AnswerText) >= CheckpointLimit {
		return
	}
	if len(d.result.AnswerText)+len(text) > CheckpointLimit {
		text = text[:CheckpointLimit-len(d.result.AnswerText)]
	}
	d.result.AnswerText += text
}

// checkToolBlock validates an ending tool-call block; arguments that never
// parse, or a call that never learned its name (a relayed model leaked its
// tool markup mid-text and the stream half-parsed it into an empty-named
// shell — executing it would answer "unknown tool \"\"" and, being
// *answered*, the pairing repair would keep the husk forever, failing every
// later turn on every model), mark the turn broken and downgrade it to
// max-tokens. Other block kinds validate nothing.
func (d *Decoder) checkToolBlock(blockIdx int) {
	if d.kind[blockIdx] != "tool-call" {
		return
	}
	if d.toolName[blockIdx] == "" {
		d.result.BrokenToolCall = true
	}
	args, ok := d.toolArgs[blockIdx]
	if !ok {
		return
	}
	if strings.TrimSpace(args) == "" {
		d.result.BrokenToolCall = true
		return
	}
	if !json.Valid([]byte(args)) {
		d.result.BrokenToolCall = true
	}
}

// --- responses wire ---------------------------------------------------------

func (d *Decoder) decodeResponses(frame map[string]any) {
	typ := jsonString(frame["type"])
	switch {
	case typ == "response.output_item.added":
		oi, _ := frame["output_index"].(float64)
		item, _ := frame["item"].(map[string]any)
		itemType := jsonString(item["type"])
		switch itemType {
		case "message":
			key := "o" + jsonString(oi)
			d.oidx[oi] = key
			d.startBlock(key, "text", "", "")
		case "reasoning":
			key := "o" + jsonString(oi)
			d.oidx[oi] = key
			d.startBlock(key, "reasoning", "", "")
		case "function_call":
			key := "o" + jsonString(oi)
			d.oidx[oi] = key
			id := jsonString(item["call_id"])
			if id == "" {
				id = jsonString(item["id"])
			}
			if id == "" {
				id = "call_" + mintHex(12)
			}
			name := RestoreToolName(jsonString(item["name"]), d.rename)
			d.startBlock(key, "tool-call", id, name)
			d.result.SawToolCall = true
		}
	case typ == "response.output_text.delta":
		if key, ok := d.oidx[oIdxOf(frame)]; ok {
			if idx, ok2 := d.blocks[key]; ok2 {
				t := jsonString(frame["delta"])
				if t != "" {
					scrubbed := d.dsml.push(t)
					if scrubbed != "" {
						d.appendAnswer(scrubbed)
						d.emit(Chunk{Kind: ChunkTextDelta, Index: idx, Delta: scrubbed})
						d.result.SawText = true
					}
				}
			}
		}
	case strings.HasPrefix(typ, "response.reasoning") && strings.HasSuffix(typ, ".delta"):
		if key, ok := d.oidx[oIdxOf(frame)]; ok {
			if idx, ok2 := d.blocks[key]; ok2 {
				t := jsonString(frame["delta"])
				if t != "" {
					d.emit(Chunk{Kind: ChunkReasonDelta, Index: idx, Delta: t})
					d.result.SawReasoning = true
					d.appendCheckpoint(t)
				}
			}
		}
	case typ == "response.function_call_arguments.delta":
		if key, ok := d.oidx[oIdxOf(frame)]; ok {
			if idx, ok2 := d.blocks[key]; ok2 {
				d.toolDelta(idx, "", "", jsonString(frame["delta"]))
			}
		}
	case typ == "response.output_item.done":
		oi, _ := frame["output_index"].(float64)
		if key, ok := d.oidx[oi]; ok {
			if blockIdx, ok2 := d.blocks[key]; ok2 {
				d.checkToolBlock(blockIdx)
				d.endBlock(key)
			}
			delete(d.oidx, oi)
		}
	case typ == "response.completed" || typ == "response.done":
		d.setFinish("stop")
	case typ == "response.incomplete":
		reason := ""
		if det, ok := frame["response"].(map[string]any); ok {
			if idet, ok := det["incomplete_details"].(map[string]any); ok {
				reason = jsonString(idet["reason"])
			}
		}
		if reason == "max_output_tokens" {
			d.setFinish("max_output_tokens")
		} else {
			d.setFinish("stop")
		}
	case typ == "response.failed":
		// A terminal shape on this wire; the raw token reaches the caller so a
		// failed turn is never mistaken for a clean stop.
		d.setFinish("failed")
	}
}

func oIdxOf(frame map[string]any) float64 {
	if v, ok := frame["output_index"].(float64); ok {
		return v
	}
	return -1
}

// --- shared ------------------------------------------------------------------

func (d *Decoder) setFinish(rawToken string) {
	d.result.SawFinish = true
	if d.result.FinishToken == "" {
		d.result.FinishToken = rawToken
	}
	d.result.Finish = finishReason(rawToken)
}

func (d *Decoder) appendCheckpoint(text string) {
	if d.result.CheckpointTruncated {
		return
	}
	if len(d.result.ReasoningText)+len(text) > CheckpointLimit {
		d.result.CheckpointTruncated = true
		return
	}
	d.result.ReasoningText += text
}

func finishReason(token string) string {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "tool_calls", "tool_use", "function_call":
		return FinishToolCalls
	case "length", "max_tokens", "max_output_tokens", "incomplete":
		return FinishMaxTokens
	default:
		return FinishStop
	}
}

func mintHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// WindowTokens subtracts reasoning tokens that never streamed from the
// throughput numerator, so "think time" is never counted as "write speed".
func WindowTokens(u Usage, sawReasoning bool) int {
	if sawReasoning && u.Reasoning > 0 {
		n := u.Output - u.Reasoning
		if n < 0 {
			return 0
		}
		return n
	}
	return u.Output
}
