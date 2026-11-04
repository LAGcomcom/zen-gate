package lane

import "testing"

// TestModelFallbackChainPicksUserRank verifies issue #25: the user-authored
// chain orders the catch, and the lane respects that ranking — skipping hidden
// and unknown ids, honouring tried/Throttled, and returning "" once exhausted.
func TestModelFallbackChainPicksUserRank(t *testing.T) {
	l := NewLane()
	l.smartRouting = false // isolate chain ranking from capability filtering
	l.catalog = []ModelInfo{
		{ID: "main"}, {ID: "alt-a"}, {ID: "alt-b"}, {ID: "hidden-m"},
	}
	l.availability = map[string]ProbeResult{
		"main": {}, "alt-a": {}, "alt-b": {}, "hidden-m": {},
	}
	l.hidden = map[string]bool{"hidden-m": true}
	l.SetModelFallbackChain([]string{"alt-a", "hidden-m", "alt-b", "ghost"})

	if got := l.nextChainModel(Needs{}, map[string]bool{}, nil, nil); got != "alt-a" {
		t.Fatalf("first pick should follow chain: want alt-a, got %q", got)
	}
	// alt-a already tried -> alt-b (hidden model + unknown ghost skipped)
	if got := l.nextChainModel(Needs{}, map[string]bool{"alt-a": true}, nil, nil); got != "alt-b" {
		t.Fatalf("after alt-a tried, want alt-b, got %q", got)
	}
	// everything tried/blocked -> empty
	if got := l.nextChainModel(Needs{}, map[string]bool{"alt-a": true, "alt-b": true}, nil, nil); got != "" {
		t.Fatalf("exhausted chain should return empty, got %q", got)
	}
	// a throttled chain member is skipped on a fresh pass
	l.MarkThrottled("alt-a", 0)
	if got := l.nextChainModel(Needs{}, map[string]bool{}, nil, nil); got != "alt-b" {
		t.Fatalf("throttled alt-a should be skipped, want alt-b, got %q", got)
	}
}

func TestModelFallbackChainBlankWhenUnset(t *testing.T) {
	l := NewLane()
	l.smartRouting = false
	l.catalog = []ModelInfo{{ID: "main"}, {ID: "alt-a"}}
	l.availability = map[string]ProbeResult{"main": {}, "alt-a": {}}
	if got := l.nextChainModel(Needs{}, map[string]bool{}, nil, nil); got != "" {
		t.Fatalf("unset chain must return empty so requestModel falls through to nextCandidate, got %q", got)
	}
}

func TestSetModelFallbackChainTrimsAndDropsBlank(t *testing.T) {
	l := NewLane()
	l.SetModelFallbackChain([]string{"  a ", "", "b", "  ", "c"})
	l.mu.RLock()
	got := l.modelFallbackChain
	l.mu.RUnlock()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("expected trimmed [a b c], got %#v", got)
	}
}
