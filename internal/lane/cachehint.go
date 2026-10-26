package lane

import (
	"encoding/json"
	"strings"
)

// Prompt-cache hints.
//
// ## The problem this solves
//
// Reading `cache_read_input_tokens` out of a usage block only ever told us what
// the upstream had ALREADY cached on its own. Nothing in this gateway ever
// asked for a cache, so on every channel the answer was "whatever the vendor
// felt like" — usually nothing. One conversation re-sent as a fresh prompt on
// every turn means the provider re-prefills the whole history each time, and
// the biggest cost in an agent loop (a large system prompt plus a tool schema
// block that never changes) was paid over and over.
//
// ## Two dialects, two mechanisms
//
// OpenAI-compatible gateways bucket their prefix cache by a request field —
// a plain string that must repeat across a conversation. DeepSeek-family
// endpoints spell it `prompt_cache_key`; the same field name is what the
// reference plugin for this exact WorkBuddy/CodeArts backend uses, with a
// measured effect worth quoting: an 8k-token prefix went from
// `prompt_cache_hit_tokens: 0` to `7808` on the strength of that field alone,
// which moved the billed credit from 0.34 to 0.02 — about 17x cheaper for the
// same prompt. CodeArts' own reference body builder has carried the field all
// along (see codearts/upstream client `chatBodyV2`); the path this gateway
// actually takes just never called it.
//
// Anthropic-protocol endpoints have no such key. Their cache is declared
// inline with `cache_control` breakpoints on content blocks, and it is
// PREFIX-shaped: one breakpoint covers everything before it. That is why the
// two helpers below are shaped the way they are — one breakpoint on the last
// tool is equivalent to one on every tool, and it costs a single slot out of
// the 4 a request is allowed.
//
// ## What we deliberately do NOT touch
//
// The OpenCode Zen lane validates the request byte-for-byte (key order, exact
// tool descriptions, `strict:false`), and an extra top-level field is answered
// with 403 FreeTierError. Kilo has no measured behaviour for these fields at
// all. Neither gets a hint: an unverified field on a fingerprint-checked lane
// is a way to break working traffic in exchange for a guess.

// cacheBoundary is the smallest prefix worth declaring. Anthropic bills a cache
// WRITE at 1.25x the input price, so declaring a breakpoint under a 4-block
// tool schema costs more than the cache can ever save; 2048 bytes is roughly
// the point where the 25% write premium is repaid by a single read.
const cacheBoundary = 2048

// CacheFieldFor names the request field a given wire wants for cache bucketing,
// returning "" when that wire has no such field (Anthropic declares breakpoints
// inline instead, and the Zen/Kilo lanes must not be touched at all).
func CacheFieldFor(wire string) string {
	switch wire {
	case "chat":
		return "prompt_cache_key"
	}
	return ""
}

// WithCacheKey adds the conversation's cache bucket key to an
// OpenAI-compatible body. It returns the field it wrote, or "" when there was
// nothing to do (no key, empty session, or a wire that does not use one).
//
// The value must be stable for the WHOLE conversation and different across
// conversations: it is a bucket id, not a request id. Callers derive it from
// the conversation seed, which is exactly the lifetime a prefix cache wants.
func WithCacheKey(body map[string]any, wire, session string) string {
	field := CacheFieldFor(wire)
	if field == "" || strings.TrimSpace(session) == "" || body == nil {
		return ""
	}
	body[field] = session
	return field
}

// WithCacheBreakpoints upgrades an Anthropic-shaped body for prompt caching:
// `system` becomes a block array whose LAST block carries the breakpoint, and
// the LAST tool carries one too. Returns true when anything was written.
//
// Prefix semantics are what makes this cheap. A breakpoint covers every block
// before it, so:
//
//   - one on the final system block covers the whole system prompt, caller
//     content included — which is the largest stable prefix in an agent loop;
//   - one on the final tool covers system + every tool schema before it.
//
// Total: 2 of the 4 breakpoints a request may carry, leaving room for anything
// the caller adds later. Marking every block instead would burn all four and
// leave the tool schema — 20+ tools, tens of KB, resent on every single turn —
// permanently uncached.
//
// Skipped entirely when the combined prefix is tiny (see cacheBoundary): a
// breakpoint on a 200-byte prefix is a 25%-priced write that can never be read.
func WithCacheBreakpoints(body map[string]any) bool {
	if body == nil {
		return false
	}
	if prefixWeight(body) < cacheBoundary {
		return false
	}
	blocks, ok := systemBlocks(body["system"])
	if !ok {
		// No usable system prompt: leave the body alone entirely. Half a hint
		// (tools marked, system not) would put the breakpoint in front of the
		// largest prefix instead of behind it, so the tools themselves would be
		// the cached region and the system prompt would sit outside it.
		return false
	}
	blocks[len(blocks)-1]["cache_control"] = ephemeral()
	body["system"] = blocks
	// The tool breakpoint rides after the system one on purpose: prefix caching
	// covers everything BEFORE a breakpoint, so a breakpoint on the last tool
	// extends the cached region over every schema sent before it. Two of the
	// four slots a request may carry, spent on the two prefixes an agent loop
	// resends verbatim on every turn.
	if tools := toolsOf(body); len(tools) > 0 {
		tools[len(tools)-1]["cache_control"] = ephemeral()
		body["tools"] = tools
	}
	return true
}

// prefixWeight estimates the cacheable prefix in bytes: the system prompt plus
// every tool declaration. Bytes, not tokens — the decision only needs to know
// whether this is "a real prompt" or "a ping", and a byte count needs no
// tokenizer.
func prefixWeight(body map[string]any) int {
	n := 0
	if sys, ok := body["system"]; ok {
		if b, err := json.Marshal(sys); err == nil {
			n += len(b)
		}
	}
	if tools, ok := body["tools"]; ok {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b)
		}
	}
	return n
}

// systemBlocks normalizes `system` into a fresh block slice.
//
// The returned maps are COPIES, always. Both callers write into them (one adds a
// breakpoint, the other deletes one), and a caller that handed us blocks it
// keeps around — or a decoder that cached a body — must not see its own data
// mutated behind its back. A plain string is wrapped into a single text block;
// anything else is refused so the caller never writes a shape the upstream did
// not ask for.
func systemBlocks(sys any) ([]map[string]any, bool) {
	switch t := sys.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, false
		}
		return []map[string]any{{"type": "text", "text": t}}, true
	case []map[string]any:
		if len(t) == 0 {
			return nil, false
		}
		out := make([]map[string]any, 0, len(t))
		for _, b := range t {
			out = append(out, cloneMap(b))
		}
		return out, true
	case []any:
		if len(t) == 0 {
			return nil, false
		}
		out := make([]map[string]any, 0, len(t))
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, cloneMap(m))
		}
		return out, true
	}
	return nil, false
}

// StripCacheBreakpoints removes every breakpoint this package adds, so a caller
// that drew a 400 on the cache-declaring shape can retry the plain form.
//
// The shape is restored, not rebuilt:
//
//   - a one-block system prompt came from a plain string, so it unwraps back to
//     exactly those bytes;
//   - a multi-block prompt was ALREADY blocks when it arrived (a caller that
//     uses blocks explicitly), so the array stays an array with the breakpoints
//     deleted. Joining the blocks into one string would send a different request
//     on the retry — a second failure with an unrelated cause, which is the one
//     thing a fallback must never do.
//
// Idempotent on purpose: it reports true only when a breakpoint was actually
// removed, so a retry path can call it unconditionally and still know whether
// the first attempt differed from the second.
func StripCacheBreakpoints(body map[string]any) bool {
	if body == nil {
		return false
	}
	removed := false
	if blocks, ok := systemBlocks(body["system"]); ok {
		for _, b := range blocks {
			if _, has := b["cache_control"]; has {
				delete(b, "cache_control")
				removed = true
			}
		}
		if removed {
			// Blocks are copies (see systemBlocks), so the normalized slice is
			// the only place the deletions exist — publish it back. A one-block
			// prompt came from a plain string and unwraps to exactly those
			// bytes; a multi-block prompt was already an array and stays one.
			if len(blocks) == 1 {
				if s, ok := blocks[0]["text"].(string); ok {
					body["system"] = s
				} else {
					body["system"] = blocks
				}
			} else {
				body["system"] = blocks
			}
		}
	}
	for _, t := range toolsOf(body) {
		if _, has := t["cache_control"]; has {
			delete(t, "cache_control")
			removed = true
		}
	}
	return removed
}

func ephemeral() map[string]any { return map[string]any{"type": "ephemeral"} }
