package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postJSON(t *testing.T, url, key, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+key)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The correlation id is what ties one dashboard log line to the client's view
// of a failed turn, so every model endpoint must stamp it before writing.
func TestChatCompletionsStampsACorrelationID(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		"[DONE]",
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/chat/completions", s.Store.Config().MainKey,
		`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rid := resp.Header.Get("x-zen-gate-request-id")
	if !strings.HasPrefix(rid, "zreq-") {
		t.Fatalf("x-zen-gate-request-id = %q, want a generated zreq- id", rid)
	}
}

func TestAnthropicMessagesStampsACorrelationID(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		"[DONE]",
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/messages", s.Store.Config().MainKey,
		`{"model":"mimo-v2.6-flash-free","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	rid := resp.Header.Get("x-zen-gate-request-id")
	if rid == "" {
		t.Fatalf("no correlation id on /v1/messages (status %d)", resp.StatusCode)
	}
	if !strings.HasPrefix(rid, "zreq-") {
		t.Fatalf("rid = %q", rid)
	}
}

// A client that already has a trace id wins — that is the only way its own
// logs and ours line up. Both spellings are seen in the wild.
func TestCorrelationIDReusesTheClientHeader(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	for _, hdr := range []map[string]string{
		{"x-request-id": "client-trace-1"},
		{"x-client-request-id": "client-trace-2"},
	} {
		resp := postJSON(t, ts.URL+"/v1/chat/completions", s.Store.Config().MainKey,
			`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":false}`, hdr)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		want := hdr["x-request-id"]
		if want == "" {
			want = hdr["x-client-request-id"]
		}
		if got := resp.Header.Get("x-zen-gate-request-id"); got != want {
			t.Errorf("inbound %q → echoed %q, want the client's own id", want, got)
		}
	}
}

// Rejections are exactly the responses you want to grep by id, so the header
// must survive the early-return error paths.
func TestCorrelationIDPresentOnRejectedRequest(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/chat/completions", s.Store.Config().MainKey,
		`{"model":"mimo-v2.6-flash-free","messages":[`, nil)
	if resp.StatusCode == 200 {
		t.Fatal("a truncated body should not succeed")
	}
	if rid := resp.Header.Get("x-zen-gate-request-id"); !strings.HasPrefix(rid, "zreq-") {
		t.Errorf("status %d carried no correlation id (got %q)", resp.StatusCode, rid)
	}
}
