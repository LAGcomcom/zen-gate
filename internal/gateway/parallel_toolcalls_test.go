package gateway

import (
	"testing"

	"zen-gate/internal/lane"
)

// Issue #96: parallel tool calls must reach the client with distinct
// tool_calls.index values, not all 0. The index is allocated when the block
// opens, so it is already distinct on the very first delta of each call.
func TestChatEmitterParallelToolIndexes(t *testing.T) {
	e := &chatEmitter{declared: map[string]bool{"web_search": true, "grep": true}}
	chunks := []lane.Chunk{
		{Kind: lane.ChunkBlockStart, Index: 0, BlockType: "tool-call", ID: "a", Name: "web_search"},
		{Kind: lane.ChunkToolCallDelta, Index: 0, Delta: `{"query":"a"}`},
		{Kind: lane.ChunkBlockStart, Index: 1, BlockType: "tool-call", ID: "b", Name: "grep"},
		{Kind: lane.ChunkToolCallDelta, Index: 1, Delta: `{"pattern":"b"}`},
		{Kind: lane.ChunkBlockEnd, Index: 0},
		{Kind: lane.ChunkBlockEnd, Index: 1},
	}

	var indexes []int
	var names []string
	for _, c := range chunks {
		for _, d := range e.stream(c) {
			tcs, _ := d.delta["tool_calls"].([]any)
			for _, raw := range tcs {
				m, _ := raw.(map[string]any)
				idx, _ := m["index"].(int)
				fn, _ := m["function"].(map[string]any)
				name, _ := fn["name"].(string)
				indexes = append(indexes, idx)
				names = append(names, name)
			}
		}
	}
	if len(indexes) != 2 {
		t.Fatalf("want 2 tool deltas, got %d (%v)", len(indexes), indexes)
	}
	if indexes[0] == indexes[1] {
		t.Fatalf("parallel calls share index %d", indexes[0])
	}
	if indexes[0] != 0 || indexes[1] != 1 {
		t.Fatalf("indexes must be 0 and 1 in call order, got %v", indexes)
	}
	if names[0] != "web_search" || names[1] != "grep" {
		t.Fatalf("call names got mixed: %v", names)
	}
}

// A suppressed fingerprint decoy must not consume an index: the client never
// sees it, so the real call after it still starts at 0.
func TestChatEmitterDecoyLeavesNoIndexGap(t *testing.T) {
	e := &chatEmitter{declared: map[string]bool{"bash": true}}
	chunks := []lane.Chunk{
		{Kind: lane.ChunkBlockStart, Index: 0, BlockType: "tool-call", ID: "x", Name: lane.FingerprintTools[0]},
		{Kind: lane.ChunkToolCallDelta, Index: 0, Delta: `{}`},
		{Kind: lane.ChunkBlockEnd, Index: 0},
		{Kind: lane.ChunkBlockStart, Index: 1, BlockType: "tool-call", ID: "y", Name: "bash"},
		{Kind: lane.ChunkToolCallDelta, Index: 1, Delta: `{}`},
	}
	var indexes []int
	for _, c := range chunks {
		for _, d := range e.stream(c) {
			for _, raw := range d.delta["tool_calls"].([]any) {
				m, _ := raw.(map[string]any)
				idx, _ := m["index"].(int)
				indexes = append(indexes, idx)
			}
		}
	}
	if len(indexes) != 1 || indexes[0] != 0 {
		t.Fatalf("decoy must not shift real call index, got %v", indexes)
	}
}
