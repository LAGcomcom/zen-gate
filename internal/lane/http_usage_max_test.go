package lane

import (
	"strings"
	"testing"
)

// Regression for issue #105: several upstreams repeat the running usage
// snapshot on terminal (sometimes every) frame. The old aggregation summed
// the snapshots, compounding input tokens 30-1400x on affected calls. The
// per-field max must collapse the repeats to the final snapshot.
func TestReadSSEUsageRepeatedSnapshots(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105}}`,
		`data: {"choices":[{"delta":{"content":"b"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":12,"total_tokens":112}}`,
		"data: [DONE]",
		"",
	}, "\n")
	usage, err := readSSE(strings.NewReader(body), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if usage == nil {
		t.Fatal("no usage parsed")
	}
	if usage.Input != 100 || usage.Output != 12 || usage.TotalTokens != 112 {
		t.Fatalf("usage = %+v, want input=100 output=12 total=112", usage)
	}
}

// Anthropic splits usage across frames: message_start carries input on the
// message object, message_delta carries the running output at the top level.
// A terminal frame that omits input must not erase what earlier frames said.
func TestReadSSEUsageSplitFrames(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":1}}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":9}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	usage, err := readSSE(strings.NewReader(body), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if usage == nil {
		t.Fatal("no usage parsed")
	}
	if usage.Input != 120 || usage.Output != 9 {
		t.Fatalf("usage = %+v, want input=120 output=9", usage)
	}
}

// A partial frame (some fields absent, i.e. zero on the incoming side) must
// keep the larger values already accumulated, and a smaller late snapshot
// must not shrink the total either.
func TestUsageMergeMaxKeepsLargerFields(t *testing.T) {
	u := &Usage{Input: 100, Output: 5, CacheRead: 40}
	u.MergeMax(&Usage{Output: 9})
	if u.Input != 100 || u.Output != 9 || u.CacheRead != 40 {
		t.Fatalf("after partial frame: %+v, want input=100 output=9 cacheRead=40", u)
	}
	u.MergeMax(&Usage{Input: 60, Output: 3, TotalTokens: 63})
	if u.Input != 100 || u.Output != 9 || u.CacheRead != 40 || u.TotalTokens != 63 {
		t.Fatalf("after smaller snapshot: %+v", u)
	}
	u.MergeMax(nil) // must be a no-op, not a panic
	if u.Input != 100 {
		t.Fatalf("nil merge changed usage: %+v", u)
	}
}

// CacheRead stays an independent field: OpenAI's prompt_tokens already
// includes cached tokens, so folding them into Input would double-count.
func TestReadSSEUsageCacheReadIndependent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":200,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":150},"total_tokens":204}}`,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":200,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":150},"total_tokens":207}}`,
		"data: [DONE]",
		"",
	}, "\n")
	usage, err := readSSE(strings.NewReader(body), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if usage.Input != 200 || usage.CacheRead != 150 || usage.Output != 7 {
		t.Fatalf("usage = %+v, want input=200 cacheRead=150 output=7", usage)
	}
}
