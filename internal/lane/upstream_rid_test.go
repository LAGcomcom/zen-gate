package lane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// When a user asks the provider's support team about a failed turn, the only
// thing that identifies it on their side is the trace id they return. It used
// to be dropped on the floor.
func TestUpstreamErrorKeepsProviderRequestID(t *testing.T) {
	old := UpstreamBase
	t.Cleanup(func() { UpstreamBase = old })

	for _, tc := range []struct{ header, want string }{
		{"x-request-id", "req_openai_42"},
		{"request-id", "req_anthropic_7"},
	} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(tc.header, tc.want)
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"too fast"}}`)
		}))
		UpstreamBase = up.URL
		_, err := PostStreamed(context.Background(), "/v1/chat/completions",
			map[string]any{"model": "m-one"}, "", "", func([]byte) error { return nil })
		up.Close()

		ue, ok := err.(*UpstreamError)
		if !ok {
			t.Fatalf("%s: err = %T %v, want *UpstreamError", tc.header, err, err)
		}
		if ue.Code != CodeQuota {
			t.Errorf("%s: code = %q, want %q", tc.header, ue.Code, CodeQuota)
		}
		if ue.UpstreamRID != tc.want {
			t.Errorf("%s: UpstreamRID = %q, want %q", tc.header, ue.UpstreamRID, tc.want)
		}
	}
}

// A success must not invent one, and a provider that sends no such header must
// not produce a bogus empty-but-set value.
func TestUpstreamRIDStaysEmptyWhenAbsent(t *testing.T) {
	old := UpstreamBase
	t.Cleanup(func() { UpstreamBase = old })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	UpstreamBase = up.URL

	_, err := PostStreamed(context.Background(), "/v1/chat/completions",
		map[string]any{"model": "m-one"}, "", "", func([]byte) error { return nil })
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("err = %T %v", err, err)
	}
	if ue.UpstreamRID != "" {
		t.Errorf("UpstreamRID = %q, want empty", ue.UpstreamRID)
	}
}
