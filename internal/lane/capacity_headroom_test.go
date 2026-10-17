package lane

import "testing"

// The user-visible complaint this pins: a model measured at 256K answered with
// 8K on the default rung and 32K on deep, so it "hit the output limit
// immediately" while its real ceiling was far higher. Measured capacities are
// FLOORS — the probe stops at the first size it did not try — so the table
// value must be widened, and a configured fallback must not cap a model whose
// capacity is actually known.

func TestMeasuredCapacityGetsHeadroom(t *testing.T) {
	// deep on a measured 256K model: the ceiling is the model's own capacity
	// with headroom, never the raw probe value.
	m := ModelInfo{MaxOutput: 262144, Reasoning: true, CanDisableThinking: true}
	got := BudgetFor("deep", m, 0, 0)
	if got <= 262144 {
		t.Fatalf("deep = %d, must exceed the measured floor %d", got, 262144)
	}
	if got != 262144*3/2 {
		t.Fatalf("deep = %d, want measured × 1.5", got)
	}
}

func TestKnownCapacityIgnoresTheConfiguredFallback(t *testing.T) {
	// The old rule capped from above ("只压低不抬高"); on a big model that read
	// as an artificial output ceiling. A known capacity now speaks for itself.
	big := ModelInfo{MaxOutput: 262144, Reasoning: true, CanDisableThinking: true}
	if got := BudgetFor("deep", big, 0, 32768); got != 262144*3/2 {
		t.Fatalf("known capacity capped by the fallback: %d", got)
	}
	// An unknown capacity still relies on the fallback to stay sane.
	unknown := ModelInfo{Reasoning: true, CanDisableThinking: true}
	if got := BudgetFor("deep", unknown, 0, 32768); got != 32768 {
		t.Fatalf("unknown capacity = %d, want the fallback 32768", got)
	}
}

func TestBoundedRungsStillControl(t *testing.T) {
	// Widening capacity must not turn the small rungs into unlimited ones: a
	// user picking 精简/均衡 still gets a short answer even on a huge model.
	big := ModelInfo{MaxOutput: 262144, Reasoning: true, CanDisableThinking: true}
	if got := BudgetFor("light", big, 0, 0); got != 2048 {
		t.Fatalf("light on a big model = %d, want 2048", got)
	}
	if got := BudgetFor("balanced", big, 0, 0); got != 8192 {
		t.Fatalf("balanced on a big model = %d, want 8192", got)
	}
}

func TestHeadroomNeverInventsACapacity(t *testing.T) {
	// 0 means "nothing measured". Widening it would fabricate a ceiling; the
	// unknown-model default must stay in charge instead.
	if got := applyHeadroom(0); got != 0 {
		t.Fatalf("applyHeadroom(0) = %d, want 0", got)
	}
	if got := applyHeadroom(-5); got != 0 {
		t.Fatalf("applyHeadroom(-5) = %d, want 0", got)
	}
}

func TestContextWindowHeadroomKeepsCandidates(t *testing.T) {
	// The routing filter uses the same measured floor; a prompt slightly over
	// the table value must still reach the model (the upstream is the judge),
	// because dropping the candidate silently costs the whole request.
	l := NewLane()
	throttleAll(l, "nemotron-3.5-lightning-free")
	m := ModelInfo{ID: "nemotron-3.5-lightning-free", ContextWindow: 128000,
		MaxOutput: 32768, Reasoning: true, CanDisableThinking: true}
	// 1.4× the table value is inside the headroom, so the model stays eligible.
	if needs := (Needs{PromptTokens: 128000 * 14 / 10}); !fitsWindow(m, needs) {
		t.Fatalf("a prompt within the headroom was excluded")
	}
	// Far beyond it is still excluded — the filter must keep working.
	if needs := (Needs{PromptTokens: 128000 * 3}); fitsWindow(m, needs) {
		t.Fatalf("a prompt well past the headroom must still be excluded")
	}
}

// fitsWindow mirrors the routing filter's window test so the headroom rule can
// be asserted without standing up a whole catalog.
func fitsWindow(m ModelInfo, needs Needs) bool {
	window := applyHeadroom(m.ContextWindow)
	return !(needs.PromptTokens > 0 && window > 0 && needs.PromptTokens > window)
}