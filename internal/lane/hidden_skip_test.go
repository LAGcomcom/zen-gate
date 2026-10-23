package lane

import (
	"context"
	"testing"
	"time"
)

// Issue #103: models the user unchecked on the 模型 page must stop being
// probed (each probe is a real upstream call that burns quota) and must stop
// acting as failover candidates — but a client that names one explicitly must
// still reach it, which is why the filter lives in nextCandidate/ProbeRound
// and not in attemptModel.

func TestNextCandidateSkipsHidden(t *testing.T) {
	l := strategyLane()
	// Unchecked models stay probed-available here: the filter under test is
	// the hidden set, not the probe verdict.
	l.SetHidden(map[string]bool{"mimo-v2.6-flash-free": true})
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("next = %q, want space-bunny-free (hidden mimo must be skipped)", got)
	}

	l.SetHidden(map[string]bool{
		"space-bunny-free":            true,
		"mimo-v2.6-flash-free":        true,
		"nemotron-3.5-lightning-free": true,
		"ll-alpha-free":               true,
	})
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "" {
		t.Fatalf("next = %q, want empty when every model is hidden", got)
	}
	// The 429 hint body lists candidates through Candidates(); it must not
	// surface hidden ids either.
	if cands := l.Candidates(Needs{}, "", 3); len(cands) != 0 {
		t.Fatalf("Candidates = %v, want none while all are hidden", cands)
	}

	// Re-showing a model restores it immediately — the set swap is atomic.
	l.SetHidden(nil)
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("next = %q after clearing the hidden set, want space-bunny-free", got)
	}
}

func TestProbeRoundSkipsHiddenModels(t *testing.T) {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "space-bunny-free", Wire: "chat", ContextWindow: 1024, MaxOutput: 16},
		{ID: "ll-alpha-free", Wire: "chat", ContextWindow: 1024, MaxOutput: 16},
	})
	l.SetHidden(map[string]bool{"space-bunny-free": true, "ll-alpha-free": true})

	l.ProbeRound(context.Background(), true)

	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.availability) != 0 {
		t.Fatalf("hidden models were probed anyway: %+v", l.availability)
	}
	if l.quotaWallActive {
		t.Fatal("an all-hidden round must not count as a quota wall")
	}
	if !l.nextProbeAfter.IsZero() {
		t.Fatalf("an all-hidden round scheduled a backoff until %v", l.nextProbeAfter)
	}
}

func TestProbeRoundQuotaWallStillNeedsProbes(t *testing.T) {
	// With nothing hidden the quota-wall bookkeeping must keep working: an
	// empty catalog was never a wall, and the guard must not turn one into a
	// permanent no-backoff state.
	l := NewLane()
	l.SetCatalog(nil)
	l.ProbeRound(context.Background(), true)
	l.mu.RLock()
	wall := l.quotaWallActive
	l.mu.RUnlock()
	if wall {
		t.Fatal("an empty catalog must not count as a quota wall")
	}
	_ = time.Now() // keep the time import if the assertion above ever grows
}
