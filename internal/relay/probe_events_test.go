package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// A user-added provider is where a probe can take tens of seconds (a queued
// NIM lane), so its phases have to reach the dashboard the same way the free
// lane's do.

func TestProbeModelReportsItsPhases(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer up.Close()

	var steps []lane.ProbeStep
	ctx := lane.WithProbeSink(context.Background(), func(s lane.ProbeStep) { steps = append(steps, s) })
	p := &store.Provider{ID: "t", BaseURL: up.URL, APIKey: "sk-test-secret", Protocol: store.ProtocolOpenAI}
	if r := ProbeModel(ctx, p, "m1"); r.State != lane.StateAvailable {
		t.Fatalf("probe = %+v", r)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 请求已发出 / 首字节到达 / 结论 (%+v)", len(steps), steps)
	}
	if steps[0].Name != lane.PhaseRequest || steps[0].Status != lane.StepRunning {
		t.Fatalf("step 0 = %+v", steps[0])
	}
	if !strings.Contains(steps[0].Detail, "m1") {
		t.Fatalf("step 0 should name the model, got %q", steps[0].Detail)
	}
	if strings.Contains(steps[0].Detail, "sk-test-secret") {
		t.Fatalf("the phase detail must never carry the provider key: %q", steps[0].Detail)
	}
	if steps[1].Name != lane.PhaseFirstByte || steps[1].Ms <= 0 {
		t.Fatalf("step 1 = %+v, want first byte with a measured ms", steps[1])
	}
	if steps[2].Name != lane.PhaseVerdict || steps[2].Status != lane.StepOK {
		t.Fatalf("step 2 = %+v", steps[2])
	}
}

func TestProbeModelReportsTheProviderRejection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer up.Close()

	var steps []lane.ProbeStep
	ctx := lane.WithProbeSink(context.Background(), func(s lane.ProbeStep) { steps = append(steps, s) })
	p := &store.Provider{ID: "t", BaseURL: up.URL, APIKey: "nope", Protocol: store.ProtocolOpenAI}
	if r := ProbeModel(ctx, p, "m1"); r.State != lane.StateUnavailable {
		t.Fatalf("probe = %+v", r)
	}
	last := steps[len(steps)-1]
	if last.Status != lane.StepFail {
		t.Fatalf("verdict status = %q, want fail for a rejected key", last.Status)
	}
	if !strings.Contains(last.Detail, "invalid api key") {
		t.Fatalf("the drawer must show the provider's own words, got %q", last.Detail)
	}
}
