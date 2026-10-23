package lane

import "testing"

// TestUsageMergeMaxRepeatedBlockDoesNotInflate covers issue #105: a provider
// that repeats the same complete usage block across streaming frames must not
// inflate the totals the way the additive Merge would.
func TestUsageMergeMaxRepeatedBlockDoesNotInflate(t *testing.T) {
	u := &Usage{Input: 100, Output: 50, TotalTokens: 150}
	// The same full block arrives three more times (OpenAI include_usage with a
	// chatty upstream, or a relay that re-emits it on every chunk).
	for i := 0; i < 3; i++ {
		u.MergeMax(&Usage{Input: 100, Output: 50, TotalTokens: 150})
	}
	if u.Input != 100 || u.Output != 50 || u.TotalTokens != 150 {
		t.Fatalf("repeated identical block inflated usage: %+v", u)
	}
}

// TestUsageMergeMaxSplitsAcrossFrames covers the Anthropic shape: input arrives
// in message_start, output in message_delta. Max-per-field must keep both
// instead of dropping the earlier one.
func TestUsageMergeMaxSplitsAcrossFrames(t *testing.T) {
	u := &Usage{Input: 120, Output: 0, TotalTokens: 0}
	u.MergeMax(&Usage{Input: 0, Output: 80, TotalTokens: 200})
	if u.Input != 120 || u.Output != 80 || u.TotalTokens != 200 {
		t.Fatalf("split usage dropped a field: %+v", u)
	}
}

// TestUsageMergeMaxTakesLatestCumulative covers a provider that grows the
// cumulative count across frames: the larger value wins, not a sum.
func TestUsageMergeMaxTakesLatestCumulative(t *testing.T) {
	u := &Usage{Input: 100}
	u.MergeMax(&Usage{Input: 250})
	u.MergeMax(&Usage{Input: 180})
	if u.Input != 250 {
		t.Fatalf("latest cumulative not taken: %+v", u)
	}
}
