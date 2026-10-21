package lane

import "testing"

// Some upstreams repeat cumulative usage on every chunk: prompt_tokens stays
// put while completion_tokens walks up. Summing those frames (Merge) inflates
// the input side by orders of magnitude — a few hundred chunks turn 1.2k into
// six figures. Absorb takes the largest value per field, which is the real
// total for the single-terminal-frame shape as well as the repeated-cumulative
// one.
func TestAbsorbKeepsRepeatedCumulativeUsageHonest(t *testing.T) {
	frames := []Usage{
		{Input: 1200, Output: 1, TotalTokens: 1201},
		{Input: 1200, Output: 9, TotalTokens: 1209},
		{Input: 1200, Output: 42, TotalTokens: 1242},
	}
	var got Usage
	for i := range frames {
		got.Absorb(&frames[i])
	}
	if got.Input != 1200 || got.Output != 42 || got.TotalTokens != 1242 {
		t.Fatalf("absorb = %+v, want input 1200 / output 42 / total 1242", got)
	}
}

// Merge keeps its additive meaning: it is for genuinely separate requests
// (e.g. a truncated continuation that pays the prompt twice), not for frames.
func TestMergeStaysAdditive(t *testing.T) {
	u := Usage{Input: 1200, Output: 42}
	u.Merge(&Usage{Input: 1200, Output: 42})
	if u.Input != 2400 || u.Output != 84 {
		t.Fatalf("merge = %+v, want input 2400 / output 84", u)
	}
}
