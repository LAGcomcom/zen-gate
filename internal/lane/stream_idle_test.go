package lane

import (
	"testing"
	"time"
)

// TestStreamIdleTimeoutFitsInsideTheRetryBudget pins the interaction the
// per-class budget is documented around. slowEgressTotalMS has to clear one
// full attempt plus room for a retry "or the retry never runs"; with the stream
// idle deadline left at five minutes, a stalled stream spent longer than the
// entire budget before it was even noticed, so no retry could ever happen and
// the caller saw only its own cancellation.
func TestStreamIdleTimeoutFitsInsideTheRetryBudget(t *testing.T) {
	if streamIdleTimeout <= 0 {
		t.Fatal("streamIdleTimeout must be positive or a stalled stream never ends")
	}
	if streamIdleTimeout > 2*time.Minute {
		t.Errorf("streamIdleTimeout = %v, too long: the user cancels first and the retry never runs", streamIdleTimeout)
	}
	if streamIdleTimeout < 30*time.Second {
		t.Errorf("streamIdleTimeout = %v, too short: a slow first token could be cut off", streamIdleTimeout)
	}
	// A retry only happens if the budget outlives one whole attempt, and an
	// attempt can spend a response-header timeout and then an idle timeout.
	if responseHeaderTimeout+streamIdleTimeout > slowEgressTotalMS*time.Millisecond {
		t.Errorf("response-header %v + idle %v exceeds the slow retry budget %v: the retry never runs",
			responseHeaderTimeout, streamIdleTimeout, slowEgressTotalMS*time.Millisecond)
	}
}

// TestKiloTurnTimeoutSharesTheStreamIdleDeadline keeps the two lanes on one
// value: both bound the same silence, and a drifting copy is how one lane ends
// up outside the budget again.
func TestKiloTurnTimeoutSharesTheStreamIdleDeadline(t *testing.T) {
	if kiloTurnTimeout != streamIdleTimeout {
		t.Errorf("kiloTurnTimeout = %v, want it to share streamIdleTimeout (%v)", kiloTurnTimeout, streamIdleTimeout)
	}
}
