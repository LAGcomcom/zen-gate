package lane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The dashboard's 测试 button reports through these phases, so what a probe
// emits is part of the interface: without it the user cannot tell a queued
// model from a dead gateway.

func TestProbeModelReportsItsPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	var steps []ProbeStep
	ctx := WithProbeSink(context.Background(), func(s ProbeStep) { steps = append(steps, s) })
	res := ProbeModel(ctx, ModelInfo{ID: "space-bunny-free", Wire: "chat"})
	if res.State != StateAvailable {
		t.Fatalf("probe = %+v", res)
	}

	if len(steps) != 3 {
		t.Fatalf("steps = %d %+v, want 请求已发出 / 首字节到达 / 结论", len(steps), names(steps))
	}
	if steps[0].Name != PhaseRequest || steps[0].Status != StepRunning {
		t.Fatalf("step 0 = %+v, want the request phase marked running", steps[0])
	}
	if !strings.Contains(steps[0].Detail, "space-bunny-free") {
		t.Fatalf("step 0 must say which model was called, got %q", steps[0].Detail)
	}
	if strings.Contains(steps[0].Detail, "Bearer") {
		t.Fatalf("the phase detail must never carry credentials: %q", steps[0].Detail)
	}
	if steps[1].Name != PhaseFirstByte || steps[1].Status != StepOK || steps[1].Ms <= 0 {
		t.Fatalf("step 1 = %+v, want first byte ok with a measured ms", steps[1])
	}
	if steps[2].Name != PhaseVerdict || steps[2].Status != StepOK {
		t.Fatalf("step 2 = %+v, want an ok verdict", steps[2])
	}
	for i, s := range steps {
		if s.At <= 0 {
			t.Fatalf("step %d is unstamped: %+v", i, s)
		}
	}
}

func TestProbeModelReportsTheUpstreamFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded on free lane"}}`)
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	var steps []ProbeStep
	ctx := WithProbeSink(context.Background(), func(s ProbeStep) { steps = append(steps, s) })
	res := ProbeModel(ctx, ModelInfo{ID: "space-bunny-free", Wire: "chat"})
	if res.State != StateThrottled {
		t.Fatalf("probe = %+v", res)
	}
	last := steps[len(steps)-1]
	if last.Name != PhaseVerdict {
		t.Fatalf("last step = %+v, want the verdict", last)
	}
	// A throttle is not evidence about the model, so it must not read as a fail.
	if last.Status != StepUnknown {
		t.Fatalf("verdict status = %q, want unknown for a quota fault", last.Status)
	}
	if !strings.Contains(last.Detail, "rate limit exceeded") {
		t.Fatalf("the drawer has to show the upstream's own words, got %q", last.Detail)
	}
}

func names(steps []ProbeStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// The dashboard's free-lane 测试 button goes through Lane.ProbeOne, so the sink
// has to survive that hop — otherwise only custom providers get progress.
func TestProbeOneReportsItsPhasesToTheCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	l.catalog = []ModelInfo{{ID: "space-bunny-free", Wire: "chat"}}

	var steps []ProbeStep
	ctx := WithProbeSink(context.Background(), func(s ProbeStep) { steps = append(steps, s) })
	if res := l.ProbeOneCtx(ctx, "space-bunny-free"); res.State != StateAvailable {
		t.Fatalf("probe = %+v", res)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d %+v, want the three phases", len(steps), names(steps))
	}
	l.mu.RLock()
	got := l.availability["space-bunny-free"]
	l.mu.RUnlock()
	if got.State != StateAvailable {
		t.Fatalf("the round bookkeeping must still apply: %+v", got)
	}
}
