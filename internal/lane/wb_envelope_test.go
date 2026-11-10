package lane

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A 11128 refuses the envelope, and the envelope is byte-identical on a second
// POST: replaying it only burns another account turn. Before the split every
// 11128 went through the sanitize path, which is why a missing conversation id
// showed up as "内容门拦截，改写后仍被拒绝" and cost two upstream calls
// (issue #128-3).
func TestWBEnvelopeHitIsNotReplayed(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":11128,"message":"conversation id missing"}`)
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are a helpful assistant."},
		{"role": "user", "content": "hi"},
	}}
	_, err := PostWorkBuddyStreamed(context.Background(), body, wbAcct(), wbConversationID("conv"), func([]byte) error { return nil })
	if err == nil {
		t.Fatal("11128 应当作为错误返回，而不是被当成可重试的内容门")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("信封拒绝被重打了 %d 次；它不会因为改写文本而变化，重试只是白烧一次账号额度", got)
	}
}

// 11140 still gets exactly one replay: that is what the rewrite is for.
func TestWBTextHitIsReplayedOnce(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		if n == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"code":11140,"message":"blocked"}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, wbChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Claude Code."},
		{"role": "user", "content": "hi"},
	}}
	if _, err := PostWorkBuddyStreamed(context.Background(), body, wbAcct(), wbConversationID("conv"), func([]byte) error { return nil }); err != nil {
		t.Fatalf("改写重试应清掉文本指纹: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("文本指纹应恰好重试一次，实际 %d 次", got)
	}
}

// The v2 gate refuses a turn whose X-Conversation-ID is empty, and the request
// is not retried away: the whole call fails with 400 / 11128. The probe body is
// built by hand and used to pass "", which is why the WorkBuddy card read
// "内容门拦截" while manual chat answered normally (issue #128-3).
//
// The header builder is the last place that can catch it, so an empty seed must
// still yield a usable 32-hex id.
func TestWBConversationHeaderNeverEmpty(t *testing.T) {
	h := wbConversationProtocol("req-1", "", "hy4-preview-f")
	conv := h["X-Conversation-ID"]
	if conv == "" {
		t.Fatal("空的会话种子仍必须产出非空 X-Conversation-ID，否则上游 400/11128")
	}
	if len(conv) != 32 {
		t.Fatalf("会话 ID 应为 32 位十六进制，得到 %q（%d 位）", conv, len(conv))
	}
	for _, c := range conv {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("会话 ID 含非十六进制字符: %q", conv)
		}
	}
}

// A derived seed must stay stable: the upstream prompt cache keys on the
// conversation, so two turns of one conversation have to produce one id, and
// two conversations must not collide.
func TestWBConversationHeaderStablePerSeed(t *testing.T) {
	a := wbConversationProtocol("req-1", wbConversationID("wb-probe:hy4"), "hy4")["X-Conversation-ID"]
	b := wbConversationProtocol("req-2", wbConversationID("wb-probe:hy4"), "hy4")["X-Conversation-ID"]
	if a != b {
		t.Fatalf("同一会话两轮得到不同 ID：%q vs %q", a, b)
	}
	c := wbConversationProtocol("req-3", wbConversationID("wb-probe:other"), "other")["X-Conversation-ID"]
	if a == c {
		t.Fatal("不同会话不应共用同一个 ID")
	}
}

// 11140 is the WAF reading the request text: rewriting the body and replaying
// once is the cure. 11128 is the envelope (missing conversation id, or a turn
// that does not start with a system message) — no amount of text rewriting can
// satisfy it, so it must not enter the sanitize-and-retry path.
func TestWBGateCodesAreNotOneBucket(t *testing.T) {
	textHit := classifyWorkBuddyFailure(403, `{"code":11140,"msg":"blocked"}`, 0)
	if textHit.Code != wbCodeSanitize {
		t.Fatalf("11140 应归内容门（可改写重试），得到 %q", textHit.Code)
	}

	envelope := classifyWorkBuddyFailure(400, `{"code":11128,"msg":"conversation id missing"}`, 0)
	if envelope.Code != CodeClient {
		t.Fatalf("11128 是信封问题，应归客户端错误而不是内容门，得到 %q", envelope.Code)
	}
	if envelope.Code == wbCodeSanitize {
		t.Fatal("11128 走内容门会白打一次上游请求，并给出错误的卡片文案")
	}
}
