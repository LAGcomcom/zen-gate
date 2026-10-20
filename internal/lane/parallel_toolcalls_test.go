package lane

import (
	"testing"
)

// Issue #96: the chat wire has no block-stop frame, so a second parallel tool
// call used to stay inside the first block until Finish(). Every call therefore
// landed on one block index, and clients that aggregate deltas by index glued
// the names and argument strings together.
func TestDecoderChatParallelToolCallsSeparateBlocks(t *testing.T) {
	type event struct {
		kind  string
		index int
		name  string
	}
	var events []event
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkBlockStart || c.Kind == ChunkBlockEnd {
			events = append(events, event{c.Kind, c.Index, c.Name})
		}
	})
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"web_search","arguments":"{\"query\":\"a\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"grep","arguments":"{\"pattern\":\"b\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, f := range frames {
		dec.Decode([]byte(f))
	}
	res := dec.Finish()
	if res.BrokenToolCall {
		t.Fatalf("valid parallel calls must not break: %+v", res)
	}

	var starts, ends []event
	for _, e := range events {
		if e.kind == ChunkBlockStart {
			starts = append(starts, e)
		} else {
			ends = append(ends, e)
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("want 2 blocks, got starts=%v ends=%v", starts, ends)
	}
	if starts[0].index == starts[1].index {
		t.Fatalf("parallel calls share block index %d", starts[0].index)
	}
	if starts[0].name != "web_search" || starts[1].name != "grep" {
		t.Fatalf("block names got mixed: %v %v", starts[0].name, starts[1].name)
	}
	// The first call must be closed before the second one opens, not both at
	// Finish in random map order.
	if ends[0].index != starts[0].index {
		t.Fatalf("first block closed out of order: end=%d start=%d", ends[0].index, starts[0].index)
	}
}

// Finish closes whatever is still open in the order the blocks were opened.
func TestDecoderFinishClosesBlocksInOrder(t *testing.T) {
	var ends []int
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkBlockEnd {
			ends = append(ends, c.Index)
		}
	})
	// One frame opening two calls without any stop frame: both end at Finish.
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"one","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"two","arguments":"{}"}}]}}]}`))
	dec.Finish()
	if len(ends) != 2 || ends[0] >= ends[1] {
		t.Fatalf("blocks must close in opening order, got %v", ends)
	}
}
