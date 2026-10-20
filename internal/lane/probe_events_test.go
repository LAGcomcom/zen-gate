package lane

import (
	"context"
	"testing"
)

func names(steps []ProbeStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// The dashboard's 测试 button goes through Lane.ProbeOne, so the sink has to
// survive that hop — otherwise a click reports nothing while it waits.
//
// A Kilo row is verified by its roster entry rather than by a request, so the
// run carries the request phase and the verdict, never a first byte.
func TestProbeOneReportsItsPhasesToTheCaller(t *testing.T) {
	const id = "poolside/laguna-s-2.1:free"
	l := NewLane()
	l.SetCatalog([]ModelInfo{{ID: id, Wire: "chat", Channel: ChannelKilo}})

	var steps []ProbeStep
	ctx := WithProbeSink(context.Background(), func(s ProbeStep) { steps = append(steps, s) })
	if res := l.ProbeOneCtx(ctx, id); res.State != StateAvailable {
		t.Fatalf("probe = %+v", res)
	}
	if got, want := names(steps), []string{PhaseRequest, PhaseVerdict}; len(got) != 2 ||
		got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if steps[0].Status != StepOK || steps[1].Status != StepOK {
		t.Fatalf("steps = %+v, want both phases reported ok", steps)
	}
	for i, s := range steps {
		if s.At <= 0 {
			t.Fatalf("step %d is unstamped: %+v", i, s)
		}
	}
	l.mu.RLock()
	got := l.availability[id]
	l.mu.RUnlock()
	if got.State != StateAvailable {
		t.Fatalf("the round bookkeeping must still apply: %+v", got)
	}
}
