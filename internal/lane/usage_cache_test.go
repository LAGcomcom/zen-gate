package lane

import (
	"strings"
	"testing"
)

// The two protocol families disagree about what "input" counts, and the
// gateway has to keep both honest without ever adding the fields up by hand.
//
// OpenAI's prompt_tokens ALREADY contains the cached tokens; Anthropic's
// input_tokens does not. Before CacheInclusive existed, every outbound usage
// block did Input + CacheRead, which double-counted the OpenAI half: a
// 166k-token prompt with a 165k cache hit was reported to clients as 332k
// input. That is the figure the panel was showing.
func TestUsageOpenAIWireInputIsInclusive(t *testing.T) {
	u := &Usage{Input: 200, Output: 7, CacheRead: 150, CacheInclusive: true}
	if got := u.InputTotal(); got != 200 {
		t.Errorf("InputTotal() = %d, want 200 (prompt_tokens already contains the hits)", got)
	}
	if got := u.InputFresh(); got != 50 {
		t.Errorf("InputFresh() = %d, want 50", got)
	}
}

func TestUsageAnthropicWireInputIsExclusive(t *testing.T) {
	u := &Usage{Input: 50, Output: 7, CacheRead: 150, CacheWrite: 20}
	if got := u.InputTotal(); got != 200 {
		t.Errorf("InputTotal() = %d, want 200 (input_tokens excludes the hits)", got)
	}
	if got := u.InputFresh(); got != 50 {
		t.Errorf("InputFresh() = %d, want 50", got)
	}
}

// An upstream that claims more cache hits than prompt tokens is stating
// something irreconcilable. Reporting a negative input would be worse than
// clamping: every consumer formats it directly into a usage block.
func TestUsageInputFreshNeverGoesNegative(t *testing.T) {
	u := &Usage{Input: 10, CacheRead: 99, CacheInclusive: true}
	if got := u.InputFresh(); got != 0 {
		t.Errorf("InputFresh() = %d, want 0 rather than a negative count", got)
	}
}

// The parser decides the family from the spelling the upstream used, and it
// must do so for the flat keys and the nested details block alike.
func TestReadSSEUsageMarksFamilyFromSpelling(t *testing.T) {
	openai := map[string]any{"usage": map[string]any{
		"prompt_tokens": 200.0, "completion_tokens": 7.0,
		"prompt_tokens_details": map[string]any{"cached_tokens": 150.0}}}
	u := mapUsage(openai)
	if u == nil || !u.CacheInclusive {
		t.Fatalf("openai frame: usage = %+v, want CacheInclusive", u)
	}
	if u.InputTotal() != 200 || u.CacheRead != 150 {
		t.Errorf("openai frame: total=%d cacheRead=%d, want 200/150", u.InputTotal(), u.CacheRead)
	}

	anthropic := map[string]any{"message": map[string]any{"usage": map[string]any{
		"input_tokens": 50.0, "output_tokens": 7.0,
		"cache_read_input_tokens": 150.0, "cache_creation_input_tokens": 20.0}}}
	au := mapUsage(anthropic)
	if au == nil || au.CacheInclusive {
		t.Fatalf("anthropic frame: usage = %+v, want exclusive input", au)
	}
	if au.InputTotal() != 200 || au.InputFresh() != 50 || au.CacheWrite != 20 {
		t.Errorf("anthropic frame: total=%d fresh=%d write=%d, want 200/50/20",
			au.InputTotal(), au.InputFresh(), au.CacheWrite)
	}
}

// Tencent / DeepSeek family gateways report the hit count under
// prompt_cache_hit_tokens instead of prompt_tokens_details. Missing that key
// is why a working prefix cache looked like no cache at all on the account
// channels: the hits were parsed as zero, so the panel had nothing to show.
func TestReadSSEUsageReadsPromptCacheHitTokens(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":8027,"completion_tokens":80,"prompt_cache_hit_tokens":7808,"prompt_cache_miss_tokens":219,"total_tokens":8107}}`,
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
	if usage.CacheRead != 7808 {
		t.Errorf("CacheRead = %d, want 7808 from prompt_cache_hit_tokens", usage.CacheRead)
	}
	if usage.InputTotal() != 8027 {
		t.Errorf("InputTotal() = %d, want 8027 (prompt_tokens is inclusive)", usage.InputTotal())
	}
	if usage.InputFresh() != 219 {
		t.Errorf("InputFresh() = %d, want 219", usage.InputFresh())
	}
}

// total_tokens is left to the upstream when it states one, and synthesized
// from the WHOLE prompt when it does not — Anthropic frames omit it, and
// summing the fresh part alone understated the turn by the hit size.
func TestReadSSEUsageTotalCountsTheCachedPart(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_read_input_tokens":150}}}`,
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
	if usage.TotalTokens != 201 {
		t.Errorf("TotalTokens = %d, want 201 (50 fresh + 150 cached + 1 out)", usage.TotalTokens)
	}
}

// A stream whose later frames carry only partial usage must not erase the
// family the first frame proved: the max-merge used to take the larger number
// per field and would have reset the inclusive flag to its zero value.
func TestMergeMaxKeepsFamilyAcrossPartialFrames(t *testing.T) {
	u := &Usage{Input: 200, CacheRead: 150, CacheInclusive: true}
	u.MergeMax(&Usage{Output: 9})
	if !u.CacheInclusive {
		t.Fatal("a partial frame cleared CacheInclusive; the next render would double the input")
	}
	if u.InputTotal() != 200 {
		t.Errorf("InputTotal() = %d, want 200", u.InputTotal())
	}
}
