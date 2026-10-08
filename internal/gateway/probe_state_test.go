package gateway

import (
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// The 模型 page showed 不可用 for an hour after one exit answered 404 once, and
// the same verdict deleted the id from GET /v1/models. The row's state is the
// panel's only window on a probe result, so the TTL has to be applied here too
// — an expired claim reads as 未探测, which is what it actually is.
func TestStateRowExpiresAStaleVerdict(t *testing.T) {
	stale := lane.ProbeResult{State: lane.StateUnavailable, At: time.Now().Add(-31 * time.Minute).UnixMilli()}
	if got := stateOrDefault(stale); got != lane.StateUnknown {
		t.Errorf("a 31-minute-old refusal renders as %q, want %q", got, lane.StateUnknown)
	}
	fresh := lane.ProbeResult{State: lane.StateUnavailable, At: time.Now().UnixMilli()}
	if got := stateOrDefault(fresh); got != lane.StateUnavailable {
		t.Errorf("a fresh refusal renders as %q, want it still visible", got)
	}
}
