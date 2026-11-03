package lane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// One client request must cost exactly one stats record per attempt: the day
// counters (Requests/Input/Output) are what the dashboard's 用量 numbers are
// built from, and a second record for the same attempt doubles them.
func TestOneRecordPerSuccessfulAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, chatSSE(
			`{"choices":[{"delta":{"role":"assistant","content":"hi"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	l.SetFailover(false, 0) // one model, one attempt: exactly one record expected
	var recs []CallRecord
	l.OnCall = func(r CallRecord) { recs = append(recs, r) }

	_, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})
	if uerr != nil {
		t.Fatalf("complete: %v", uerr)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want exactly 1 per attempt: %+v", len(recs), recs)
	}
	if !recs[0].Ok || recs[0].Input != 10 || recs[0].Output != 2 {
		t.Errorf("record = %+v, want a successful 10/2 attempt", recs[0])
	}
}

func TestOneRecordPerRefusedAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-request-id", "req_quota_1")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"FreeUsageLimitError"}}`)
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	l := NewLane()
	l.SetFailover(false, 1)
	var recs []CallRecord
	l.OnCall = func(r CallRecord) { recs = append(recs, r) }

	_, uerr := l.Complete(context.Background(), Request{
		Model:    "mimo-v2.6-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}}},
	}, func(Chunk) {})
	if uerr == nil {
		t.Fatal("a 429 lane must refuse")
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want exactly 1: %+v", len(recs), recs)
	}
	if recs[0].Ok || recs[0].ErrCode != CodeQuota || recs[0].UpstreamRID != "req_quota_1" {
		t.Errorf("record = %+v, want the failure class and the provider trace id", recs[0])
	}
}
