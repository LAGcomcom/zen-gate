package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/logx"
)

// wireLog attaches a real logger to the test server so a test can read back
// exactly what the user would see in the 日志 panel.
func wireLog(t *testing.T, s *Server) *logx.Logger {
	t.Helper()
	l := logx.New(t.TempDir())
	s.SetLogger(l)
	t.Cleanup(func() { l.Close() })
	return l
}

func catEntries(l *logx.Logger, cat string) []logx.Entry {
	return l.Query(logx.Query{Cats: []string{cat}, Limit: 500})
}

// One client request must produce exactly one access line, carrying the id the
// client sees, the wire, the status, the model that answered and the token
// counts — that single line is both the audit record and the 排障 anchor.
func TestAccessLinePerClientRequest(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"你好"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		"[DONE]",
	})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	l.SetCategories(map[string]bool{"access": true, "upstream": true})

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	resp := postJSON(t, ts.URL+"/v1/chat/completions", s.Store.Config().MainKey,
		`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"stream":false}`, nil)
	rid := resp.Header.Get("x-zen-gate-request-id")

	got := catEntries(l, "access")
	if len(got) != 1 {
		t.Fatalf("access lines = %d, want 1: %+v", len(got), got)
	}
	for _, want := range []string{rid, "chat", "status=200", "model=mimo-v2.6-flash-free",
		"served=mimo-v2.6-flash-free", "in=7", "out=2", "attempts=1", "ms="} {
		if !strings.Contains(got[0].Msg, want) {
			t.Errorf("access line %q missing %q", got[0].Msg, want)
		}
	}
	if got[0].Level != "INFO" {
		t.Errorf("level = %q, want INFO for a served request", got[0].Level)
	}
}

// A rejected request is the one you most want to find by id, and it must not
// invent an upstream attempt that never happened.
func TestAccessLineOnRejectedRequest(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	l.SetCategories(map[string]bool{"access": true, "upstream": true})

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	resp := postJSON(t, ts.URL+"/v1/chat/completions", "wrong-key",
		`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	got := catEntries(l, "access")
	if len(got) != 1 {
		t.Fatalf("access lines = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Msg, "status=401") || !strings.Contains(got[0].Msg, "attempts=0") {
		t.Errorf("access line %q, want a 401 with no attempts", got[0].Msg)
	}
	if got[0].Level == "INFO" {
		t.Errorf("a rejected request logged at INFO; want a warning level")
	}
	if n := len(catEntries(l, "upstream")); n != 0 {
		t.Errorf("upstream lines = %d, want 0 (no attempt was made)", n)
	}
}

// Failovers are the case where per-attempt detail pays for itself: one access
// line, but one upstream line per model actually tried.
func TestUpstreamLinePerAttempt(t *testing.T) {
	up := laneUpstreamByModel(t, map[string]bool{"nemotron-3-ultra-free": true})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	l.SetCategories(map[string]bool{"access": true, "upstream": true})
	s.Lane.MarkThrottled("mimo-v2.6-flash-free", 120)
	s.Lane.MarkThrottled("mimo-v2.5-free", 120)

	resp := postChat(t, s, `{"model":"nemotron-3-ultra-free","stream":false,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	rid := resp.Header.Get("x-zen-gate-request-id")

	up1 := catEntries(l, "upstream")
	if len(up1) != 2 {
		t.Fatalf("upstream lines = %d, want 2 (refused model + failover): %+v", len(up1), up1)
	}
	if !strings.Contains(up1[0].Msg, "model=nemotron-3-ultra-free") ||
		!strings.Contains(up1[0].Msg, "RATE_LIMIT") {
		t.Errorf("first upstream line %q, want the refused model and its error code", up1[0].Msg)
	}
	if !strings.Contains(up1[1].Msg, "model=") || strings.Contains(up1[1].Msg, "nemotron") {
		t.Errorf("second upstream line %q, want the failover model", up1[1].Msg)
	}
	for i, e := range up1 {
		if !strings.Contains(e.Msg, rid) {
			t.Errorf("upstream line %d %q does not carry the request id %q", i, e.Msg, rid)
		}
	}
	acc := catEntries(l, "access")
	if len(acc) != 1 || !strings.Contains(acc[0].Msg, "attempts=2") {
		t.Fatalf("access lines = %+v, want one with attempts=2", acc)
	}
}

// A user-added provider attempt is a different egress than the free lane; the
// line has to say which one answered.
func TestUpstreamLineMarksProvider(t *testing.T) {
	provUp := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"from provider"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`,
		"[DONE]",
	})
	defer provUp.Close()
	laneUp := fakeUpstream(t, []string{"[DONE]"})
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	wireLog(t, s)
	l := s.logger.(*logx.Logger)
	l.SetCategories(map[string]bool{"access": true, "upstream": true})
	withProvider(t, s, provUp.URL, false)

	resp := postChat(t, s, `{"model":"pv1/gpt-4o-mini","stream":false,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	rid := resp.Header.Get("x-zen-gate-request-id")

	got := catEntries(l, "upstream")
	if len(got) != 1 {
		t.Fatalf("upstream lines = %d, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Msg, "provider") ||
		!strings.Contains(got[0].Msg, "model=pv1/gpt-4o-mini") {
		t.Errorf("upstream line %q, want the provider class and namespaced model", got[0].Msg)
	}
	acc := catEntries(l, "access")
	if len(acc) != 1 || !strings.Contains(acc[0].Msg, rid) ||
		!strings.Contains(acc[0].Msg, "served=pv1/gpt-4o-mini") {
		t.Fatalf("access lines = %+v, want one naming the provider as served-by", acc)
	}
}

// The routing decisions the client never sees — a turn handed over to a
// user-added provider — must land in the routing class, not in the catch-all,
// or the per-class switches cannot silence them.
func TestRoutingLineIsClassified(t *testing.T) {
	laneUp := laneUpstream429(t)
	defer laneUp.Close()
	provUp := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"from provider"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":3}}`,
		"[DONE]",
	})
	defer provUp.Close()
	s := newTestServer(t, laneUp)
	l := wireLog(t, s)
	l.SetCategories(logx.DefaultCategories())
	withProvider(t, s, provUp.URL, true)
	cat, _, _ := s.Lane.Snapshot()
	for _, m := range cat {
		if m.ID != "mimo-v2.6-flash-free" {
			s.Lane.MarkThrottled(m.ID, 120)
		}
	}

	resp := postChat(t, s, `{"model":"mimo-v2.6-flash-free","stream":false,`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the fallback to rescue the turn", resp.StatusCode)
	}
	got := catEntries(l, logx.CatRouting)
	if len(got) == 0 {
		t.Fatalf("no routing lines: %+v", catEntries(l, logx.CatApp))
	}
	if !strings.Contains(got[0].Msg, "免费车道耗尽") {
		t.Errorf("routing line %q, want the fallback decision", got[0].Msg)
	}
	for _, e := range catEntries(l, logx.CatApp) {
		if strings.Contains(e.Msg, "免费车道耗尽") {
			t.Errorf("fallback decision left in the unclassified app class: %q", e.Msg)
		}
	}
}

// must stay short and must never drag a base64 image through the log file.
func TestContentLineIsOffByDefaultAndTruncated(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"role":"assistant","content":"回答"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	l.SetCategories(logx.DefaultCategories())

	blob := strings.Repeat("A", 900)
	body := `{"model":"mimo-v2.6-flash-free","stream":false,"messages":[{"role":"user",` +
		`"content":[{"type":"text","text":"看看这只猫"},{"type":"image_url",` +
		`"image_url":{"url":"data:image/png;base64,` + blob + `"}}]}]}`

	resp := postChat(t, s, body)
	rid := resp.Header.Get("x-zen-gate-request-id")
	if n := len(catEntries(l, "content")); n != 0 {
		t.Fatalf("content lines = %d with the class switched off, want 0", n)
	}
	// The audit trail still has to be there with only the defaults on.
	if acc := catEntries(l, "access"); len(acc) != 1 || !strings.Contains(acc[0].Msg, rid) {
		t.Fatalf("access lines = %+v, want the request id %q", acc, rid)
	}

	l.SetCategories(map[string]bool{"content": true})
	postChat(t, s, body)
	got := catEntries(l, "content")
	if len(got) != 1 {
		t.Fatalf("content lines = %d, want 1: %+v", len(got), got)
	}
	if strings.Contains(got[0].Msg, blob) {
		t.Errorf("content line carried the whole base64 payload: %q", got[0].Msg)
	}
	if len([]rune(got[0].Msg)) > 400 {
		t.Errorf("content line is %d runes, want a bounded excerpt", len([]rune(got[0].Msg)))
	}
	if !strings.Contains(got[0].Msg, "看看这只猫") {
		t.Errorf("content line %q lost the prompt text", got[0].Msg)
	}
}
