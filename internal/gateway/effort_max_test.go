package gateway

import (
	"testing"

	"zen-gate/internal/lane"
)

// Issue #97: clients that spell the deepest rung "max" (MyAgent profiles do)
// were silently downgraded to the configured default because the spelling was
// not recognised. "max" is the deepest rung, so it must land on deep.
func TestNormalizeEffortParamMax(t *testing.T) {
	cases := map[string]string{
		"max":     "deep",
		" MAX ":   "deep",
		"maximum": "deep",
		"highest": "deep",
		"xhigh":   "deep",
		"high":    "deep",
		"medium":  "balanced",
		"low":     "light",
		"":        "",
		"bogus":   "",
	}
	for in, want := range cases {
		if got := normalizeEffortParam(in); got != want {
			t.Errorf("normalizeEffortParam(%q) = %q, want %q", in, got, want)
		}
	}
}

// A client that states "max" and an explicit output budget must keep that
// budget: mapping the spelling onto deep must not clamp it back down.
func TestMaxEffortKeepsExplicitBudget(t *testing.T) {
	m := lane.ModelInfo{MaxOutput: 128000}
	got := lane.BudgetFor(normalizeEffortParam("max"), m, 20000, 32768)
	if got < 20000 {
		t.Fatalf("BudgetFor(max, requested 20000) = %d, want >= 20000", got)
	}
}
