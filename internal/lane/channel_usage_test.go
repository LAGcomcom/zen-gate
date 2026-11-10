package lane

import (
	"context"
	"testing"
)

// The channel is stamped on the request context alongside the egress choice;
// record() must pick it up without a second catalog lookup (which would need
// the lane lock while the stream is finishing).
func TestRecordStampsChannelFromExitPlan(t *testing.T) {
	var got CallRecord
	l := &Lane{OnCall: func(r CallRecord) { got = r }}
	ctx := WithExitPlan(context.Background(), ExitPlan{Model: "m", Channel: ChannelKilo})
	l.record(ctx, CallRecord{Model: "m"}, StreamResult{}, nil, 0)
	if got.Channel != ChannelKilo {
		t.Fatalf("channel = %q, want %q", got.Channel, ChannelKilo)
	}

	// An explicit channel on the record (probe / relay attempts) wins when the
	// context carries no plan, so attribution never silently disappears.
	got = CallRecord{}
	l.record(context.Background(), CallRecord{Model: "seed/m", Channel: "seed"}, StreamResult{}, nil, 0)
	if got.Channel != "seed" {
		t.Fatalf("channel = %q, want seed", got.Channel)
	}
}
