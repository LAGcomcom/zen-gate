package lane

import (
	"context"
	"testing"
	"time"
)

// Proves the kilo stale-verdict bug: ProbeRound only (re)writes the Kilo
// verdict when the map has no entry or an empty State, so the At timestamp is
// never refreshed. After stateTTL (10m) FreshState flips to unknown and the
// card reads 未探测/灰 forever, while the pool is live.
func TestKiloVerdictAtNeverRefreshes(t *testing.T) {
	l := NewLane()
	l.catalog = []ModelInfo{
		{ID: "kilo-a:free", Channel: ChannelKilo, Wire: "chat"},
	}
	l.availability["kilo-a:free"] = ProbeResult{Model: "kilo-a:free", State: StateAvailable,
		At: time.Now().Add(-11 * time.Minute).UnixMilli()}

	l.ProbeRound(context.Background(), true)

	l.mu.RLock()
	got := l.availability["kilo-a:free"]
	l.mu.RUnlock()
	if fresh := FreshState(got); fresh != StateAvailable {
		t.Errorf("after a round the Kilo verdict is still stale: FreshState=%q age=%v",
			fresh, time.Since(time.UnixMilli(got.At)))
	}
}
