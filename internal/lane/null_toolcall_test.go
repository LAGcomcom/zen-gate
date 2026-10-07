package lane

import "testing"

// Spec-compliant continuation frames carry only `index` + `function.arguments`.
// The decoder must leave id/name empty for them instead of turning the absent
// JSON fields into the literal string "null" (LAGcomcom/zen-gate#7).
func TestDecoderChatToolCallContinuationKeepsNameAndIDEmpty(t *testing.T) {
	var deltas []Chunk
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkToolCallDelta {
			deltas = append(deltas, c)
		}
	})
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc123","function":{"name":"bash","arguments":"{\"x\":"}}]}}]}`))
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`))
	dec.Finish()

	if len(deltas) < 2 {
		t.Fatalf("want first + continuation tool delta, got %d", len(deltas))
	}
	if first := deltas[0]; first.ID != "call_abc123" || first.Name != "bash" {
		t.Fatalf("first delta must carry the real id/name, got id=%q name=%q", first.ID, first.Name)
	}
	for i, d := range deltas {
		if d.Name == "null" || d.ID == "null" {
			t.Fatalf("delta %d leaked literal \"null\": id=%q name=%q", i, d.ID, d.Name)
		}
		if d.ID != "" && d.ID != "call_abc123" {
			t.Fatalf("delta %d advertises a fabricated call id %q (must be the original or empty)", i, d.ID)
		}
	}
	if cont := deltas[1]; cont.ID != "" || cont.Name != "" {
		t.Fatalf("continuation delta must leave id/name empty, got id=%q name=%q", cont.ID, cont.Name)
	}
}
