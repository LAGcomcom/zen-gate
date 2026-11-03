package lane

import "testing"

// TestNextCandidateSkipsHiddenModels covers issue #103: a model the user
// unchecked on the 模型 page must never be auto-picked as a failover /
// rate-limit-switch target. Without the hidden filter, nextCandidate would
// return space-bunny-free (the first catalog row) here.
func TestNextCandidateSkipsHiddenModels(t *testing.T) {
	l := strategyLane()
	l.SetHidden([]string{"space-bunny-free"})

	got := l.nextCandidate(Needs{}, map[string]bool{})
	if got == "space-bunny-free" {
		t.Fatalf("nextCandidate picked a hidden model: %s", got)
	}
	if got == "" {
		t.Fatalf("nextCandidate returned nothing although unhidden models exist")
	}
}

// TestNextCandidateStillServesAfterUnhide confirms clearing the hidden set
// restores the model to the candidate pool.
func TestNextCandidateStillServesAfterUnhide(t *testing.T) {
	l := strategyLane()
	l.SetHidden([]string{"space-bunny-free"})
	l.SetHidden(nil) // unhide everything

	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("after unhide, expected space-bunny-free, got %s", got)
	}
}
