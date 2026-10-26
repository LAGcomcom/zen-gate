package lane

import "testing"

// The rows below are copied from this gateway's live channel files
// (%APPDATA%/zen-gate/channels/*.json), so the tests assert against the catalog
// that actually ships rather than invented fixtures.
var aggregateTestCatalog = []ModelInfo{
	// zen lane (blank channel)
	{ID: "beam-free", Name: "Beam", ContextWindow: 131072},
	{ID: "deepseek-v4.1-flash-free", Name: "DeepSeek V4.1 Flash", ContextWindow: 1048576},
	{ID: "nemotron-3.5-lightning-free", Name: "Nemotron 3.5 Lightning", ContextWindow: 128000},
	{ID: "nemotron-3.5-super-vl-free", Name: "Nemotron 3.5 Super VL", ContextWindow: 1048576},
	{ID: "glm-5.3-flash-free", Name: "GLM-5.3-Flash", ContextWindow: 128000},
	// kilo free pool
	{ID: "reflection/beam-free", Name: "Beam 1.0", ContextWindow: 1048576},
	{ID: "nvidia/nemotron-3.5-lightning:free", Name: "Nemotron 3.5 Lightning", ContextWindow: 1000000},
	{ID: "nvidia/nemotron-3.5-super-vl:free", Name: "Nemotron 3.5 Super VL", ContextWindow: 1000000},
	// workbuddy account pool
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 1000000},
	{ID: "hy3", Name: "Hy3", ContextWindow: 128000},
}

func TestAggregateKey(t *testing.T) {
	tests := []struct{ in, want string }{
		// Namespace prefix is the vendor, not the model.
		{"nvidia/nemotron-3.5-super-vl:free", "nemotron-3-5-super-vl"},
		{"reflection/beam-free", "beam"},
		{"deepseek-v4.1-flash-free", "deepseek-v4-1-flash"},
		{"deepseek-v4.1-flash", "deepseek-v4-1-flash"},
		// Effort suffix is the client's knob, not the identity.
		{"glm-5.3-flash-free (deep)", "glm-5-3-flash"},
		// A key that is only a marker must not group anything.
		{"free", ""},
		{"auto", ""},
		{"kilo-auto/free", "kilo-auto"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := AggregateKey(tt.in); got != tt.want {
			t.Errorf("AggregateKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The pair a naive id match would destroy: same key, 7.8x apart in context.
// Grouping them would fail a 128K prompt over to the 1M deployment — a
// different model with different quality, silently.
func TestAggregateRefusesContextMismatch(t *testing.T) {
	groups := AggregateGroups(aggregateTestCatalog)
	if g, ok := groups["nemotron-3.5-lightning-free"]; ok {
		t.Fatalf("nemotron-3.5-lightning-free must not group (found %v)", g.Members)
	}
	if g, ok := groups["nvidia/nemotron-3.5-lightning:free"]; ok {
		t.Fatalf("nvidia/nemotron-3.5-lightning:free must not group (found %v)", g.Members)
	}
}

// The pairs this feature exists for: same model, windows published at
// different precision by two channels (10^6 vs 1024*1024 = 1.0486x).
func TestAggregateGroupsRealTwins(t *testing.T) {
	groups := AggregateGroups(aggregateTestCatalog)

	for _, want := range []struct {
		keys    []string
		members []string
	}{
		{
			keys:    []string{"deepseek-v4.1-flash-free", "deepseek-v4.1-flash"},
			members: []string{"deepseek-v4.1-flash-free", "deepseek-v4.1-flash"},
		},
		{
			keys:    []string{"nemotron-3.5-super-vl-free", "nvidia/nemotron-3.5-super-vl:free"},
			members: []string{"nemotron-3.5-super-vl-free", "nvidia/nemotron-3.5-super-vl:free"},
		},
	} {
		for _, key := range want.keys {
			g, ok := groups[key]
			if !ok {
				t.Fatalf("%q should be in a group", key)
			}
			if len(g.Members) != 2 {
				t.Fatalf("group[%q].Members = %v, want 2 members", key, g.Members)
			}
			for i, m := range want.members {
				if g.Members[i] != m {
					t.Fatalf("group[%q].Members = %v, want %v", key, g.Members, want.members)
				}
			}
		}
	}
	// A single-channel model is just its catalog row: no group.
	if _, ok := groups["hy3"]; ok {
		t.Fatal("hy3 must not be grouped (single channel)")
	}
}

// beam-free and reflection/beam-free share a key but are NOT the same model:
// 131072 vs 1048576 (8x). This is the case that proves the tolerance is doing
// real work rather than being decoration.
func TestAggregateBeamIsSplitByWindow(t *testing.T) {
	groups := AggregateGroups(aggregateTestCatalog)
	if g, ok := groups["beam-free"]; ok {
		t.Fatalf("beam-free (131072) and reflection/beam-free (1048576) must not group, got %v", g.Members)
	}
}

func TestAggregateCompatible(t *testing.T) {
	tests := []struct {
		a, b int
		want bool
	}{
		{1000000, 1048576, true},  // the real twin pair
		{128000, 1000000, false},  // lightning
		{131072, 1048576, false},  // beam
		{128000, 128000, true},    // exact
		{0, 0, true},              // both unknown
		{0, 128000, false},        // unknown never proves sameness
		{128000, 131072, true},    // 1.024x
	}
	for _, tt := range tests {
		if got := aggregateCompatible(tt.a, tt.b); got != tt.want {
			t.Errorf("aggregateCompatible(%d, %d) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// A prompt that does not fit the alternate's window must not be failed over
// into it: the upstream refusal would look like a model failure and burn the
// turn.
func TestAggregateAlternatesRespectWindow(t *testing.T) {
	groups := AggregateGroups(aggregateTestCatalog)
	g, ok := groups["deepseek-v4.1-flash-free"]
	if !ok {
		t.Fatal("deepseek group missing")
	}

	// Small prompt: the workbuddy twin is a valid target.
	got := aggregateAlternates(g, "deepseek-v4.1-flash-free", 1000, map[string]bool{})
	if len(got) != 1 || got[0] != "deepseek-v4.1-flash" {
		t.Fatalf("alternates = %v, want [deepseek-v4.1-flash]", got)
	}

	// Already tried: nothing left.
	if got := aggregateAlternates(g, "deepseek-v4.1-flash-free", 1000,
		map[string]bool{"deepseek-v4.1-flash": true}); len(got) != 0 {
		t.Fatalf("alternates = %v, want none (already tried)", got)
	}

	// Prompt far past the alternate's window: excluded.
	if got := aggregateAlternates(g, "deepseek-v4.1-flash-free", 5000000,
		map[string]bool{}); len(got) != 0 {
		t.Fatalf("alternates = %v, want none (window too small)", got)
	}

	// A nil group is the "not grouped" case and must be safe.
	if got := aggregateAlternates(nil, "x", 0, nil); got != nil {
		t.Fatalf("alternates(nil) = %v, want nil", got)
	}
}
