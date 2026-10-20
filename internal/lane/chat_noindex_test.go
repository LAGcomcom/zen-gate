package lane

import "testing"

// Issue #96, the harder wire: an upstream that sends parallel tool calls with
// neither an index nor an id on the argument frames. The continuation must
// attach to the most recently opened call, not to whichever block a Go map
// happens to yield first.
func TestChatBareContinuationFollowsLastOpenedCall(t *testing.T) {
	// Run many times: Go map iteration order is randomised per pass, so a
	// single run could get lucky.
	for attempt := 0; attempt < 64; attempt++ {
		names := map[int]string{}
		args := map[int]string{}
		dec := NewDecoder("chat", nil, func(c Chunk) {
			switch c.Kind {
			case ChunkBlockStart:
				if c.BlockType == "tool-call" {
					names[c.Index] = c.Name
				}
			case ChunkToolCallDelta:
				args[c.Index] += c.Delta
			}
		})
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"id":"call_a","function":{"name":"web_search","arguments":"{\"query\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"one\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"id":"call_b","function":{"name":"grep","arguments":"{\"pattern\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"two\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		for _, f := range frames {
			dec.Decode([]byte(f))
		}
		dec.Finish()

		byName := map[string]string{}
		for idx, name := range names {
			if name != "" {
				byName[name] = args[idx]
			}
		}
		if got := byName["web_search"]; got != `{"query":"one"}` {
			t.Fatalf("attempt %d: web_search args = %q, want %q (args glued onto the wrong call)",
				attempt, got, `{"query":"one"}`)
		}
		if got := byName["grep"]; got != `{"pattern":"two"}` {
			t.Fatalf("attempt %d: grep args = %q, want %q (args glued onto the wrong call)",
				attempt, got, `{"pattern":"two"}`)
		}
	}
}
