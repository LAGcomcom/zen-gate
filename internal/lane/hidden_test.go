package lane

import (
	"context"
	"testing"
	"time"
)

// A hidden model is one the user unchecked on the 模型 page. It stays servable
// when a request names it, but it must not be probed and must not come back as
// a failover substitute: a probe is a real upstream request, and spending one
// on a model the user deliberately turned off is both surprising and, on a
// metered free lane, a waste of the quota that was left unconsumed on purpose.
func TestHiddenModelsLeaveTheProbeRoundAndTheCandidateList(t *testing.T) {
	const keep = "poolside/laguna-s-2.1:free"
	const hidden = "mimo-v2.6-flash-free"

	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: keep, Wire: "chat", Channel: ChannelKilo},
		{ID: hidden, Wire: "chat", Channel: ChannelKilo},
	})
	l.SetHidden([]string{hidden})

	targets := l.probeTargets()
	if len(targets) != 1 || targets[0].ID != keep {
		t.Fatalf("probe targets = %+v, want only %s", targets, keep)
	}

	got := l.Candidates(Needs{}, "", 4)
	for _, id := range got {
		if id == hidden {
			t.Fatalf("hidden model came back as a candidate: %v", got)
		}
	}
	if len(got) == 0 || got[0] != keep {
		t.Fatalf("candidates = %v, want %s first", got, keep)
	}

	// Un-hiding puts the model back: the switch is not a one-way door.
	l.SetHidden(nil)
	if targets := l.probeTargets(); len(targets) != 2 {
		t.Fatalf("after unhide = %+v, want both models", targets)
	}
}

// ProbeRound collects its targets while still holding the write lock, so any
// helper it calls there must not take a read lock: sync.RWMutex is not
// reentrant and the round would never return. Hiding every model makes the
// round a no-op, which is enough to prove it finishes at all.
func TestProbeRoundFinishesWithEveryModelHidden(t *testing.T) {
	const id = "poolside/laguna-s-2.1:free"
	l := NewLane()
	l.SetCatalog([]ModelInfo{{ID: id, Wire: "chat", Channel: ChannelKilo}})
	l.SetHidden([]string{id})

	done := make(chan struct{})
	go func() {
		l.ProbeRound(context.Background(), true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ProbeRound never returned: the target list deadlocks against the round's write lock")
	}
}
