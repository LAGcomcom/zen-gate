package lane

import (
	"encoding/json"
	"strings"
	"testing"
)

// A cache bucket key has to be a conversation id, not a request id: the whole
// point is that turn N+2 lands in the bucket turn N created. The helper writes
// the field only when the wire wants one — the Anthropic wire declares its
// cache inline, and the Zen/Kilo lanes are byte-checked, so a stray field there
// is a 403 rather than an optimisation.
func TestWithCacheKeyWritesOnlyOnTheChatWire(t *testing.T) {
	body := map[string]any{"model": "m", "messages": []any{}}
	if got := WithCacheKey(body, "chat", "conv-1"); got != "prompt_cache_key" {
		t.Fatalf("chat wire: wrote %q, want prompt_cache_key", got)
	}
	if body["prompt_cache_key"] != "conv-1" {
		t.Errorf("body = %v, want the key to be the conversation id", body["prompt_cache_key"])
	}

	for _, wire := range []string{"messages", "responses", ""} {
		b := map[string]any{"model": "m"}
		if got := WithCacheKey(b, wire, "conv-1"); got != "" {
			t.Errorf("wire %q: wrote %q, want nothing", wire, got)
		}
		if _, has := b["prompt_cache_key"]; has {
			t.Errorf("wire %q: field leaked into the body", wire)
		}
	}

	// An empty session (a probe, or a client that sends no hint) must not mint
	// a shared bucket: one bucket for every anonymous caller would make every
	// prefix collide.
	b := map[string]any{"model": "m"}
	if got := WithCacheKey(b, "chat", "  "); got != "" {
		t.Errorf("empty session: wrote %q, want nothing", got)
	}
}

// Anthropic's cache is prefix-shaped, so one breakpoint on the last block
// covers everything before it. Declaring 2 (last system block + last tool)
// rather than one-per-block is what keeps the request inside the documented
// 4-breakpoint budget while still covering the largest stable prefix there is:
// the system prompt plus a 20-tool schema resent on every turn.
func TestWithCacheBreakpointsMarksOnlyTheLastBlockAndTool(t *testing.T) {
	long := strings.Repeat("system rules ", 400) // ~5 KB, well past the threshold
	body := map[string]any{
		"model":  "claude-x",
		"system": long,
		"tools": []map[string]any{
			{"name": "read", "description": "read a file", "input_schema": map[string]any{"type": "object"}},
			{"name": "shell", "description": "run a command", "input_schema": map[string]any{"type": "object"}},
		},
	}
	if !WithCacheBreakpoints(body) {
		t.Fatal("a 5 KB system prompt with tools must be declared cacheable")
	}
	blocks, ok := body["system"].([]map[string]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("system = %#v, want a one-block array", body["system"])
	}
	if _, has := blocks[0]["cache_control"]; !has {
		t.Error("the last (only) system block carries no breakpoint")
	}
	tools, _ := body["tools"].([]map[string]any)
	if _, has := tools[0]["cache_control"]; has {
		t.Error("the FIRST tool carries a breakpoint; only the last one should")
	}
	if _, has := tools[1]["cache_control"]; !has {
		t.Error("the last tool carries no breakpoint, so the whole tool schema stays uncached")
	}
}

// A breakpoint below the threshold is a cache WRITE (billed above plain input)
// that can never be read: the prefix is too small for the 5-minute TTL to pay
// it back. Leaving the request alone is the cheaper answer.
func TestWithCacheBreakpointsSkipsTinyPrefixes(t *testing.T) {
	body := map[string]any{
		"model":  "claude-x",
		"system": "you are helpful",
		"tools":  []map[string]any{{"name": "read", "input_schema": map[string]any{}}},
	}
	if WithCacheBreakpoints(body) {
		t.Error("a 20-byte prefix was declared cacheable")
	}
	if _, isStr := body["system"].(string); !isStr {
		t.Errorf("system = %#v, want the untouched string", body["system"])
	}
}

// An array-shaped system prompt (what a client that already uses blocks sends)
// keeps its blocks; only the last one gains the breakpoint.
func TestWithCacheBreakpointsKeepsExistingBlocks(t *testing.T) {
	long := strings.Repeat("rule ", 900)
	body := map[string]any{
		"model": "claude-x",
		"system": []any{
			map[string]any{"type": "text", "text": "intro"},
			map[string]any{"type": "text", "text": long},
		},
	}
	if !WithCacheBreakpoints(body) {
		t.Fatal("a long block array must be declared cacheable")
	}
	blocks, ok := body["system"].([]map[string]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("system = %#v, want the two original blocks", body["system"])
	}
	if _, has := blocks[0]["cache_control"]; has {
		t.Error("the first block gained a breakpoint")
	}
	if _, has := blocks[1]["cache_control"]; !has {
		t.Error("the last block carries no breakpoint")
	}
}

// The fallback for an upstream that refuses the block shape has to be strictly
// more conservative than the hint: no breakpoints, and the system prompt back in
// exactly the bytes it started as. A retry that changed the prompt text would be
// testing something else.
func TestStripCacheBreakpointsRestoresThePlainShape(t *testing.T) {
	long := strings.Repeat("rule ", 900)
	body := map[string]any{
		"model":  "claude-x",
		"system": long,
		"tools": []map[string]any{
			{"name": "read", "input_schema": map[string]any{}},
			{"name": "shell", "input_schema": map[string]any{}},
		},
	}
	if !WithCacheBreakpoints(body) {
		t.Fatal("setup: the body was not declared cacheable")
	}
	if !StripCacheBreakpoints(body) {
		t.Fatal("StripCacheBreakpoints reported nothing to do on a hinted body")
	}
	sys, ok := body["system"].(string)
	if !ok {
		t.Fatalf("system = %#v, want a plain string after the strip", body["system"])
	}
	// Byte-for-byte: the single block was a wrapper around this exact string.
	if sys != long {
		t.Errorf("the stripped system prompt is not the original (%d vs %d bytes)", len(sys), len(long))
	}
	for _, tool := range body["tools"].([]map[string]any) {
		if _, has := tool["cache_control"]; has {
			t.Errorf("tool %v still carries a breakpoint", tool["name"])
		}
	}
	// Round-trip: stripping a clean body must be a no-op, so a caller can call
	// it unconditionally on the retry path and still learn whether the first
	// attempt differed.
	if StripCacheBreakpoints(body) {
		t.Error("a second strip found something to remove")
	}
	if body["system"].(string) != long {
		t.Error("the no-op strip damaged the already-plain system prompt")
	}
}

// A multi-block system prompt was already an array when it arrived, so stripping
// keeps it an array with the breakpoints deleted. Joining the blocks into one
// string would send a DIFFERENT request on the retry, which is the one thing a
// fallback must never do — the second attempt would then fail for an unrelated
// reason and the original verdict would never be learned.
func TestStripCacheBreakpointsKeepsBlocksAsBlocks(t *testing.T) {
	long := strings.Repeat("rule ", 900)
	body := map[string]any{
		"model": "claude-x",
		"system": []map[string]any{
			{"type": "text", "text": "intro"},
			{"type": "text", "text": long},
		},
	}
	if !WithCacheBreakpoints(body) {
		t.Fatal("setup: the block array was not declared cacheable")
	}
	if !StripCacheBreakpoints(body) {
		t.Fatal("nothing was stripped")
	}
	blocks, ok := body["system"].([]map[string]any)
	if !ok {
		t.Fatalf("system = %#v, want it to stay a block array", body["system"])
	}
	if len(blocks) != 2 {
		t.Fatalf("block count = %d, want the original 2", len(blocks))
	}
	for i, b := range blocks {
		if _, has := b["cache_control"]; has {
			t.Errorf("block %d still carries a breakpoint", i)
		}
	}
	if blocks[0]["text"] != "intro" || blocks[1]["text"] != long {
		t.Error("stripping changed the block text")
	}
}

// The helper must not mutate the caller's own blocks: a relayed turn builds its
// body from a projection that another path may hold onto, and a breakpoint
// written behind its back would leak into the next request.
func TestWithCacheBreakpointsDoesNotMutateCallerBlocks(t *testing.T) {
	long := strings.Repeat("rule ", 900)
	original := []map[string]any{
		{"type": "text", "text": "intro"},
		{"type": "text", "text": long},
	}
	body := map[string]any{"model": "claude-x", "system": original}
	if !WithCacheBreakpoints(body) {
		t.Fatal("setup: not declared cacheable")
	}
	for i, b := range original {
		if _, has := b["cache_control"]; has {
			t.Errorf("the caller's block %d was mutated", i)
		}
	}
	// And the body must hold the marked copies, not the caller's slice.
	hinted, _ := body["system"].([]map[string]any)
	if _, has := hinted[1]["cache_control"]; !has {
		t.Error("the body's own copy carries no breakpoint")
	}
}

// A chat-wire body must survive the Anthropic helper untouched: those bodies
// carry `tools` in the nested function shape, and rewriting them into blocks
// would corrupt a perfectly valid OpenAI request.
func TestWithCacheBreakpointsIgnoresChatShapedTools(t *testing.T) {
	long := strings.Repeat("rule ", 900)
	body := map[string]any{
		"model":    "gpt-x",
		"messages": []map[string]any{{"role": "system", "content": long}},
	}
	if WithCacheBreakpoints(body) {
		t.Error("a body with no system field was declared cacheable")
	}
	if _, has := body["system"]; has {
		t.Error("a system field appeared on a chat body that had none")
	}
}

// A half-applied hint is worse than none: with the tools marked but the system
// prompt not, the cached region would begin after the system prompt — the
// largest stable prefix in an agent loop would sit OUTSIDE the cache. The helper
// therefore writes both or neither.
func TestWithCacheBreakpointsWritesBothOrNeither(t *testing.T) {
	long := strings.Repeat("rule ", 900)
	// A system value the helper cannot normalize (a number) with tools present.
	body := map[string]any{
		"model":  "claude-x",
		"system": 12345,
		"tools":  []map[string]any{{"name": "read", "input_schema": map[string]any{}}},
	}
	if WithCacheBreakpoints(body) {
		t.Fatal("the helper reported a write it could not make")
	}
	if tools, ok := body["tools"].([]map[string]any); ok {
		for _, tl := range tools {
			if _, has := tl["cache_control"]; has {
				t.Error("a tool gained a breakpoint while the system prompt did not")
			}
		}
	}
	if body["system"] != 12345 {
		t.Errorf("system = %#v, want it untouched", body["system"])
	}
	_ = long
}

// prompt_cache_key must survive the body encoder: the Zen lane serializes chat
// bodies in a fixed key order, and an unknown top-level key that gets dropped
// would make the hint silently useless on exactly the path it was added for.
func TestMarshalUpstreamBodyKeepsCacheKey(t *testing.T) {
	body := map[string]any{
		"model":            "m",
		"messages":         []map[string]any{{"role": "user", "content": "hi"}},
		"stream":           true,
		"prompt_cache_key": "conv-42",
	}
	raw, err := MarshalUpstreamBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["prompt_cache_key"] != "conv-42" {
		t.Errorf("encoded body lost the key: %s", raw)
	}
}
