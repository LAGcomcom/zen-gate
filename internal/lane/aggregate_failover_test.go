package lane

import "testing"

// Failover prefers the same model on another channel over a different model.
//
// This is the behaviour the aggregation exists for: a throttled Zen twin is
// answered by the WorkBuddy pool, so the user keeps the model they asked for
// instead of silently getting a different one.
func TestNextSameModelPrefersChannelTwin(t *testing.T) {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "deepseek-v4.1-flash-free", Name: "DeepSeek V4.1 Flash", ContextWindow: 1048576},
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 1000000},
		{ID: "some-other-model", Name: "Other", ContextWindow: 128000},
	})
	l.SetSmartRouting(false)

	got := l.nextSameModel(Needs{}, "deepseek-v4.1-flash-free", map[string]bool{}, 1000)
	if got != "deepseek-v4.1-flash" {
		t.Fatalf("nextSameModel = %q, want deepseek-v4.1-flash", got)
	}

	// A model with no twin returns "": the caller falls back to nextCandidate.
	if got := l.nextSameModel(Needs{}, "some-other-model", map[string]bool{}, 1000); got != "" {
		t.Fatalf("nextSameModel(no twin) = %q, want empty", got)
	}
}

// A throttled twin is not a failover target — that is the whole reason we are
// failing over in the first place.
func TestNextSameModelSkipsThrottledTwin(t *testing.T) {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "deepseek-v4.1-flash-free", ContextWindow: 1048576},
		{ID: "deepseek-v4.1-flash", ContextWindow: 1000000},
	})
	l.SetSmartRouting(false)
	l.MarkThrottled("deepseek-v4.1-flash", 60)

	if got := l.nextSameModel(Needs{}, "deepseek-v4.1-flash-free", map[string]bool{}, 1000); got != "" {
		t.Fatalf("nextSameModel = %q, want empty (twin throttled)", got)
	}
}

// The twin's window is a hard limit: failing a 5M-token prompt into a 1M window
// makes the upstream refuse, which reads as a model failure and burns the turn.
func TestNextSameModelRespectsWindow(t *testing.T) {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "deepseek-v4.1-flash-free", ContextWindow: 1048576},
		{ID: "deepseek-v4.1-flash", ContextWindow: 1000000},
	})
	l.SetSmartRouting(false)

	if got := l.nextSameModel(Needs{}, "deepseek-v4.1-flash-free", map[string]bool{}, 5000000); got != "" {
		t.Fatalf("nextSameModel = %q, want empty (prompt over window)", got)
	}
}

// The pair that must never be treated as twins, end to end: 128K vs 1M.
func TestNextSameModelRefusesLightningPair(t *testing.T) {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "nemotron-3.5-lightning-free", ContextWindow: 128000},
		{ID: "nvidia/nemotron-3.5-lightning:free", ContextWindow: 1000000},
	})
	l.SetSmartRouting(false)

	if got := l.nextSameModel(Needs{}, "nemotron-3.5-lightning-free", map[string]bool{}, 1000); got != "" {
		t.Fatalf("nextSameModel = %q, want empty (different models, 7.8x window gap)", got)
	}
}

// Modality gating still applies to a twin: a text-only deployment must not
// receive an image request just because it shares the model name.
func TestNextSameModelHonoursModality(t *testing.T) {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "deepseek-v4.1-flash-free", ContextWindow: 1048576, Vision: true},
		{ID: "deepseek-v4.1-flash", ContextWindow: 1000000, Vision: false},
	})
	l.SetSmartRouting(true)

	if got := l.nextSameModel(Needs{Image: true}, "deepseek-v4.1-flash-free", map[string]bool{}, 1000); got != "" {
		t.Fatalf("nextSameModel = %q, want empty (twin has no vision)", got)
	}
	// Without the image need the twin is fine again.
	if got := l.nextSameModel(Needs{}, "deepseek-v4.1-flash-free", map[string]bool{}, 1000); got != "deepseek-v4.1-flash" {
		t.Fatalf("nextSameModel = %q, want deepseek-v4.1-flash", got)
	}
}
