package lane

import (
	"context"
	"testing"
	"time"
)

// A whole 重新探测 round is many upstream calls and can run for minutes; the
// panel's only feedback used to be a spinning button, so a slow round and a
// stuck one looked the same (issue #117). ProbeProgress has to count the
// models the round will actually call, and go back to 0/0 when it ends.
func TestProbeProgressCountsTheRound(t *testing.T) {
	l := NewLane()
	l.mu.Lock()
	l.catalog = []ModelInfo{
		{ID: "a", Channel: ChannelZen},
		{ID: "b", Channel: ChannelKilo},
		{ID: "c", Channel: ChannelZen},
	}
	l.mu.Unlock()

	if done, total := l.ProbeProgress(); done != 0 || total != 0 {
		t.Fatalf("an idle lane reports %d/%d, want 0/0", done, total)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The denominator is the count of models that need a real upstream call:
	// the Kilo row resolves from the roster alone and must not inflate it.
	l.ProbeRound(ctx, true)

	// The round finished synchronously, so the counter must be back to idle.
	if done, total := l.ProbeProgress(); done != 0 || total != 0 {
		t.Fatalf("after the round the counter is %d/%d, want 0/0", done, total)
	}
}
