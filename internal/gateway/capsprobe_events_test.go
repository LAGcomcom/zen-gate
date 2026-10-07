package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// 能力实测 sends three separate turns (image, audio, PDF) and each one can take
// a while, so the drawer needs a row per modality — otherwise the card looks
// idle for a minute and then jumps straight to a verdict. The upstream's own
// rejection text is the evidence the user acts on, so it rides along.

func capsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		text := string(raw)
		switch {
		case strings.Contains(text, "image/png"):
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)
			fmt.Fprintln(w, `data: [DONE]`)
		case strings.Contains(text, "input_audio"), strings.Contains(text, `"wav"`):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"this model does not accept input_audio"}}`)
		case strings.Contains(text, "application/pdf"):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"document type application/pdf is unsupported"}}`)
		default:
			t.Errorf("unexpected probe body: %s", text)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"unrecognised modality"}}`)
		}
	}))
}

func TestCapabilityProbeReportsEachModality(t *testing.T) {
	gate := fakeUpstream(t, nil)
	defer gate.Close()
	s := newTestServer(t, gate)
	up := capsUpstream(t)
	defer up.Close()

	var steps []lane.ProbeStep
	ctx := lane.WithProbeSink(context.Background(), func(st lane.ProbeStep) { steps = append(steps, st) })
	p := &store.Provider{ID: "p1", Name: "P1", BaseURL: up.URL, APIKey: "sk", Protocol: store.ProtocolOpenAI, Enabled: true}
	verdicts := s.probeCapabilities(ctx, p, "m1")

	if verdicts["vision"] != verdictYes || verdicts["audio"] != verdictNo || verdicts["file"] != verdictNo {
		t.Fatalf("verdicts = %v", verdicts)
	}
	if len(steps) != 6 {
		t.Fatalf("steps = %d %+v, want start+verdict per modality", len(steps), stepNames(steps))
	}
	if got, want := strings.Join(stepNames(steps), ","), "图像,图像,音频,音频,文件,文件"; got != want {
		t.Fatalf("step names = %q, want %q", got, want)
	}
	if steps[0].Status != lane.StepRunning || steps[1].Status != lane.StepOK {
		t.Fatalf("vision steps = %+v %+v", steps[0], steps[1])
	}
	if !strings.Contains(steps[1].Detail, "OK") {
		t.Fatalf("a modality that answered should show the answer, got %q", steps[1].Detail)
	}
	if steps[3].Status != lane.StepFail {
		t.Fatalf("audio verdict = %+v, want fail for a rejection", steps[3])
	}
	if !strings.Contains(steps[3].Detail, "does not accept input_audio") {
		t.Fatalf("the drawer must keep the provider's own words, got %q", steps[3].Detail)
	}
	if steps[3].Ms <= 0 || steps[5].Ms <= 0 {
		t.Fatalf("each modality is timed: %+v %+v", steps[3], steps[5])
	}
	tag, ok := s.Store.ModelTagOf("m1")
	if !ok || !tag.Vision || tag.Audio || tag.File || tag.Source != store.TagSourceProbe {
		t.Fatalf("tag = %+v ok=%v", tag, ok)
	}
}

func TestCapabilityProbeKeepsAnInconclusiveModalityUnknown(t *testing.T) {
	gate := fakeUpstream(t, nil)
	defer gate.Close()
	s := newTestServer(t, gate)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limit exceeded"}}`)
	}))
	defer up.Close()

	var steps []lane.ProbeStep
	ctx := lane.WithProbeSink(context.Background(), func(st lane.ProbeStep) { steps = append(steps, st) })
	p := &store.Provider{ID: "p1", Name: "P1", BaseURL: up.URL, APIKey: "sk", Protocol: store.ProtocolOpenAI, Enabled: true}
	verdicts := s.probeCapabilities(ctx, p, "m2")
	if verdicts["vision"] != verdictUnknown {
		t.Fatalf("verdicts = %v, a quota fault is not a capability answer", verdicts)
	}
	for i := 1; i < len(steps); i += 2 {
		if steps[i].Status != lane.StepUnknown {
			t.Fatalf("step %+v, want unknown", steps[i])
		}
		if !strings.Contains(steps[i].Detail, "rate limit") {
			t.Fatalf("step %+v, want the upstream's words", steps[i])
		}
	}
	if _, ok := s.Store.ModelTagOf("m2"); ok {
		t.Fatal("an inconclusive probe must not write a tag")
	}
}

func stepNames(steps []lane.ProbeStep) []string {
	out := make([]string, len(steps))
	for i, st := range steps {
		out[i] = st.Name
	}
	return out
}
