package lane

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
)

// The WAF gate can refuse in-band after the upstream already streamed part of
// the answer. Replaying the turn then sends the prefix twice, interleaves two
// delta streams and bills the request twice. Only an empty stream may retry.
func TestWBGateMidStreamDoesNotReplay(t *testing.T) {
	_, hits := wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"error\":{\"code\":11140,\"message\":\"invalid request\"}}\n\n")
		},
	})
	acct := wbPoolAcct("tok-a", "a")

	calls := 0
	_, err := PostWorkBuddyStreamed(context.Background(), map[string]any{"model": "m"}, acct, "c1",
		func([]byte) error { calls++; return nil })
	if err == nil {
		t.Fatal("mid-stream gate hit should fail the turn")
	}
	if ue := asUpstream(err); ue == nil || ue.Code != wbCodeSanitize {
		t.Fatalf("error = %#v, want sanitize gate", ue)
	}
	if calls != 1 {
		t.Fatalf("onData called %d times, want 1 (no replay after output)", calls)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hit %d times, want 1", got)
	}
}
