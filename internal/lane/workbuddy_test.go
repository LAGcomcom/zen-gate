package lane

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wbJWT builds a token whose payload carries the given claims — the shape the
// fingerprint derivation and expiry logic read.
func wbJWT(sub, iss string, expSec int64) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "iss": iss, "exp": expSec})
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return head + "." + body + ".sig"
}

func TestWBDeriveIDIsStableMD5(t *testing.T) {
	sum := md5.Sum([]byte("machine:42"))
	want := hex.EncodeToString(sum[:])
	if got := wbDeriveID("42", "machine"); got != want {
		t.Fatalf("wbDeriveID = %q, want %q", got, want)
	}
	if wbDeriveID("42", "machine") != wbDeriveID("42", "machine") {
		t.Fatal("derivation must be idempotent: the account always presents one machine id")
	}
	if wbDeriveID("42", "machine") == wbDeriveID("42", "session") {
		t.Fatal("different salts must not collide: machine and session ids separate the risk surfaces")
	}
}

func TestWBJWTClaims(t *testing.T) {
	token := wbJWT("42", "https://www.workbuddy.ai", 1900000000)
	sub, iss, expMS, ok := wbJWTClaims(token)
	if !ok || sub != "42" || !strings.Contains(iss, "workbuddy.ai") {
		t.Fatalf("claims = (%q, %q, %v, %v)", sub, iss, expMS, ok)
	}
	if expMS != 1900000000000 {
		t.Fatalf("expMS = %d, want seconds scaled to ms", expMS)
	}
	if _, _, _, ok := wbJWTClaims("not-a-jwt"); ok {
		t.Fatal("a non-JWT must not parse")
	}
}

func wbChatSSE(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func wbAcct() WBAccount {
	return WBAccount{AccessToken: "tok-1", RefreshToken: "r1", UID: "42", Enabled: true,
		ExpiresAtMS: time.Now().Add(30 * 24 * time.Hour).UnixMilli()}
}

func TestPostWorkBuddyStreamedHappyPath(t *testing.T) {
	var gotUA, gotAuth, gotMachine, gotIDE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("user-agent")
		gotAuth = r.Header.Get("authorization")
		gotMachine = r.Header.Get("x-machine-id")
		gotIDE = r.Header.Get("x-ide-type")
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path = %q, want /v2/chat/completions", r.URL.Path)
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, wbChatSSE(
			`{"choices":[{"delta":{"role":"assistant","content":"he"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		))
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	var payloads []string
	usage, err := PostWorkBuddyStreamed(context.Background(), map[string]any{}, wbAcct(), "", func(p []byte) error {
		payloads = append(payloads, string(p))
		return nil
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if usage == nil || usage.Output != 2 || usage.Input != 5 {
		t.Fatalf("usage = %+v, want {5,2}", usage)
	}
	if gotAuth != "Bearer tok-1" {
		t.Errorf("authorization = %q, want the account's bearer token", gotAuth)
	}
	if gotIDE != "WorkBuddy" || gotUA != wbDesktopUA {
		t.Errorf("identity headers wrong: IDE=%q UA=%q", gotIDE, gotUA)
	}
	if gotMachine != wbDeriveID("42", "machine") {
		t.Errorf("x-machine-id = %q, want the uid derivation", gotMachine)
	}
	joined := strings.Join(payloads, "\n")
	if !strings.Contains(joined, "reasoning_content") {
		t.Error("the thinking delta must reach the decoder untouched (stream.go handles the field)")
	}
}

func TestWorkBuddy6004CarriesTheResetClock(t *testing.T) {
	body := `{"code":6004,"message":"free window exceeded, your usage will reset at 2099-01-01 00:00:00 UTC+8"}`
	ue := classifyWorkBuddyFailure(429, body, 0)
	if ue.Code != CodeQuota {
		t.Fatalf("code = %q, want %q", ue.Code, CodeQuota)
	}
	if ue.RetryAfter <= 0 || ue.RetryAfter > 48*3600 {
		t.Fatalf("RetryAfter = %d, want the parsed reset wall clock", ue.RetryAfter)
	}
	if got := classifyWorkBuddyFailure(429, `{"code":14003,"message":"too fast"}`, 0).RetryAfter; got != wbSoftCooldown {
		t.Fatalf("soft limit RetryAfter = %d, want %d", got, wbSoftCooldown)
	}
}

// The card's note line is prose, not a wire dump. A logged upstream refusal
// (2026-10-21, 14003) carried the Chinese sentence the vendor itself wrote in
// displayMsg.zh while the card printed the whole English JSON envelope.
func TestWorkBuddyRefusalReadsAsASentence(t *testing.T) {
	real := `{"code":14003,"msg":"too many requests","requestId":"6e879703292bdec799bdf9bc4bf19006",` +
		`"displayMsg":{"en":"Model busy. Please switch models or retry later","zh":"模型繁忙，请换模型或稍后再试"}}`
	ue := classifyWorkBuddyFailure(429, real, 0)
	if ue.Message != "模型繁忙，请换模型或稍后再试（上游 code 14003）" {
		t.Fatalf("message = %q, want the upstream's own Chinese line", ue.Message)
	}
	for _, leak := range []string{"requestId", "displayMsg", "too many requests", `{"code"`} {
		if strings.Contains(ue.Message, leak) {
			t.Errorf("message leaks envelope text %q: %q", leak, ue.Message)
		}
	}
	if ue.Code != CodeQuota || ue.RetryAfter != wbSoftCooldown {
		t.Fatalf("refusal filed as %+v, want a soft quota cooldown", ue)
	}

	// No Chinese: fall to the English sentence, still not the envelope.
	enOnly := classifyWorkBuddyFailure(429, `{"code":14003,"msg":"too many requests"}`, 0)
	if enOnly.Message != "too many requests（上游 code 14003）" {
		t.Fatalf("english-only fallback = %q", enOnly.Message)
	}

	// 6004's reset clock is the actionable half of the message: naming the
	// condition in Chinese must not throw the clock away.
	cap := classifyWorkBuddyFailure(429, `{"code":6004,"message":"free window exceeded, your usage will reset at 2099-01-01 00:00:00 UTC+8"}`, 0)
	if !strings.Contains(cap.Message, "2099-01-01 00:00:00") || !strings.Contains(cap.Message, "免费窗口已用满") {
		t.Fatalf("6004 message = %q, want the Chinese condition plus the reset clock", cap.Message)
	}

	// A non-JSON body (an openresty page, a plain-text verdict) is passed
	// through, shortened: inventing a sentence we cannot read is worse.
	html := classifyWorkBuddyFailure(503, "<html><body>503 Service Unavailable</body></html>", 0)
	if html == nil || !strings.Contains(html.Message, "503") {
		t.Fatalf("non-JSON refusal = %+v, want the body kept", html)
	}
}

func TestPostWorkBuddyStreamedSanitizesFingerprintsOn403(t *testing.T) {
	var calls atomic.Int64
	var firstBody, secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch calls.Add(1) {
		case 1:
			firstBody = string(body)
			w.WriteHeader(403)
			fmt.Fprint(w, `{"code":11128,"message":"Illegal API invocation from an unapproved channel"}`)
		default:
			secondBody = string(body)
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, wbChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
		}
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Claude Code. Main branch (you will usually use this for PRs). x-anthropic-billing-header: cc_entrypoint=cli"},
		{"role": "user", "content": "hi"},
	}}
	_, err := PostWorkBuddyStreamed(context.Background(), body, wbAcct(), "", func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("the sanitized retry must clear the WAF hit: %v", err)
	}
	if !strings.Contains(firstBody, "You are Claude Code") {
		t.Fatal("setup: the first request must carry the fingerprint the WAF matched")
	}
	if strings.Contains(secondBody, "You are Claude Code") || strings.Contains(secondBody, "cc_entrypoint=") {
		t.Errorf("the retry still carries marker strings: %q", secondBody)
	}
	if !strings.Contains(secondBody, "CLI tool for Claude") {
		t.Errorf("the retry lost its meaning: %q", secondBody)
	}
	// The caller's own body must survive the retry untouched.
	if !strings.Contains(firstBody, "You are Claude Code") {
		t.Fatal("the caller's body was mutated by the sanitize pass")
	}
}

// DeepSeek Harness opens every turn with a fixed self-introduction, and the
// upstream's WAF has learned it: the request is refused outright, and the
// sanitize pass has to take that one apart as well or the client never gets
// a turn at all.
func TestPostWorkBuddyStreamedSanitizesDeepSeekHarnessOpener(t *testing.T) {
	var calls atomic.Int64
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"code":11140,"message":"Illegal API invocation from an unapproved channel"}`)
			return
		}
		secondBody = string(body)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, wbChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	body := map[string]any{"messages": []map[string]any{
		{"role": "developer", "content": "You are an AI agent powered by DeepSeek Harness."},
		{"role": "user", "content": "hi"},
	}}
	if _, err := PostWorkBuddyStreamed(context.Background(), body, wbAcct(), "", func([]byte) error { return nil }); err != nil {
		t.Fatalf("the sanitized retry must clear the WAF hit: %v", err)
	}
	if strings.Contains(secondBody, "DeepSeek Harness") {
		t.Errorf("the retry still carries the Harness opener: %q", secondBody)
	}
	if !strings.Contains(secondBody, "You are an AI agent") {
		t.Errorf("the retry lost its meaning: %q", secondBody)
	}
}

func TestWBPostRefreshesOn401AndRetries(t *testing.T) {
	var chatCalls, refreshCalls atomic.Int64
	var secondAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, wbAuthRefreshPath):
			refreshCalls.Add(1)
			if r.Header.Get("x-refresh-token") != "r1" {
				t.Errorf("refresh rode the wrong token: %q", r.Header.Get("x-refresh-token"))
			}
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"tok-2","refreshToken":"r2"}}`)
		default:
			if chatCalls.Add(1) == 1 {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":"token expired"}`)
				return
			}
			secondAuth.Store(r.Header.Get("authorization"))
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, wbChatSSE(
				`{"choices":[{"delta":{"content":"ok"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
			))
		}
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	persisted := make(chan WBAccount, 4)
	l.SetWorkBuddyAccount(wbAcct())
	l.SetWorkBuddyPersist(func(a WBAccount) { persisted <- a })

	usage, err := l.wbPost(context.Background(), map[string]any{}, "", func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("wbPost: %v", err)
	}
	if usage == nil || usage.Output != 1 {
		t.Fatalf("usage = %+v, want the retried call's usage", usage)
	}
	if secondAuth.Load() != "Bearer tok-2" {
		t.Fatalf("retry rode %v, want the refreshed token", secondAuth.Load())
	}
	select {
	case a := <-persisted:
		if a.AccessToken != "tok-2" || a.RefreshToken != "r2" {
			t.Fatalf("persisted pair = %q/%q, want the rotated tok-2/r2", a.AccessToken, a.RefreshToken)
		}
	default:
		t.Fatal("the rotated pair must be written back")
	}
}

func TestRefreshCatalogGatesTheWorkBuddyBucketOnTheAccount(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fail.Close()
	oldKilo := KiloBase
	KiloBase = fail.URL
	t.Cleanup(func() { KiloBase = oldKilo })

	l := NewLane()
	// Logged in: the free models enter the catalog even though every
	// fetched source just failed. The count is the roster's own — step-5-preview
	// only rides along while its partnership window makes it free.
	l.SetWorkBuddyAccount(wbAcct())
	l.RefreshCatalog(context.Background())
	cat, _, _ := l.Snapshot()
	n := 0
	for _, m := range cat {
		if m.Channel == ChannelWorkBuddy {
			n++
		}
	}
	if want := len(buildWorkBuddyCatalog()); n != want {
		t.Fatalf("catalog holds %d WorkBuddy models, want %d", n, want)
	}

	// Logged out: the roster must leave the pickers — a subtract that is the
	// point, unlike the fetched sources' only-add rule.
	l.SetWorkBuddyAccount(WBAccount{})
	l.RefreshCatalog(context.Background())
	cat, _, _ = l.Snapshot()
	for _, m := range cat {
		if m.Channel == ChannelWorkBuddy {
			t.Fatalf("logged-out account must empty the WorkBuddy bucket, %s lingered", m.ID)
		}
	}
}

func TestWorkBuddyDailyActivityRunsOncePerDay(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, wbChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	persisted := make(chan WBAccount, 4)
	l.SetWorkBuddyAccount(wbAcct())
	l.SetWorkBuddyPersist(func(a WBAccount) { persisted <- a })

	ran, err := l.WorkBuddyDailyActivity(context.Background())
	if err != nil || !ran {
		t.Fatalf("first run = (%v, %v)", ran, err)
	}
	ran, err = l.WorkBuddyDailyActivity(context.Background())
	if ran {
		t.Fatal("the same local day must not claim activity twice")
	}
	if err != nil {
		t.Fatalf("second run error: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hit %d times, want exactly 1", hits.Load())
	}
	select {
	case a := <-persisted:
		if a.LastActivityDate != time.Now().Format("2006-01-02") {
			t.Fatalf("persisted activity date = %q", a.LastActivityDate)
		}
	default:
		t.Fatal("the activity date must be persisted")
	}
}

// The daily claim has to look like a chat turn on the wire: the gate's
// content checks never see an empty X-Conversation-ID or a hand-written
// reasoning_effort from real traffic, and answering those with CLIENT_ERROR
// every hour was issue #85.
func TestWorkBuddyDailyActivityUsesTheChatEnvelope(t *testing.T) {
	var convID, modelHeader string
	var payload struct {
		ReasoningEffort string         `json:"reasoning_effort"`
		Thinking        map[string]any `json:"thinking"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		convID = r.Header.Get("X-Conversation-ID")
		modelHeader = r.Header.Get("X-Model-ID")
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(data, &payload)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, wbChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct())
	l.SetWorkBuddyPersist(func(WBAccount) {})

	if ran, err := l.WorkBuddyDailyActivity(context.Background()); err != nil || !ran {
		t.Fatalf("run = (%v, %v)", ran, err)
	}
	if len(convID) != 32 {
		t.Fatalf("X-Conversation-ID = %q, want a 32-hex envelope like a chat turn sends", convID)
	}
	if _, err := hex.DecodeString(convID); err != nil {
		t.Fatalf("X-Conversation-ID = %q, not hex: %v", convID, err)
	}
	if modelHeader != "deepseek-v4.1-flash" {
		t.Fatalf("X-Model-ID = %q", modelHeader)
	}
	if payload.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %q, want the light rung mapped through the model quirks", payload.ReasoningEffort)
	}
	if payload.Thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v, want the deepseek thinking switch on", payload.Thinking)
	}
}

// The CN daily reward is a real endpoint rather than a conversation, and it
// lives on the meter host — asking the chat host for it is what answers 404.
func TestWorkBuddyDailyCheckinClaimsOnTheCNMeter(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody = strings.TrimSpace(string(data))
		fmt.Fprint(w, `{"code":0,"msg":"ok"}`)
	}))
	defer srv.Close()
	old := WorkBuddyCNBilling
	WorkBuddyCNBilling = srv.URL
	t.Cleanup(func() { WorkBuddyCNBilling = old })

	acct := wbAcct()
	acct.Realm = WBRealmCN
	l := NewLane()
	l.SetWorkBuddyAccount(acct)
	l.SetWorkBuddyPersist(func(WBAccount) {})

	ran, err := l.WorkBuddyDailyCheckin(context.Background())
	if err != nil || !ran {
		t.Fatalf("run = (%v, %v), want one claim", ran, err)
	}
	if gotPath != wbCheckinPath {
		t.Fatalf("path = %q, want %q", gotPath, wbCheckinPath)
	}
	if gotBody != "{}" {
		t.Fatalf("body = %q, want the empty object the meter expects", gotBody)
	}
	if got := l.WorkBuddySnapshot().LastCheckinDate; got != time.Now().Format("2006-01-02") {
		t.Fatalf("LastCheckinDate = %q, want today", got)
	}
}

// "Already claimed today" arrives as an HTTP 400 carrying code 10001. It is
// the same outcome as a fresh claim — reading the status line alone would
// leave the day open and retry hourly against a wall.
func TestWorkBuddyDailyCheckinAlreadyClaimedClosesTheDay(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":10001,"msg":"今天已签到"}`)
	}))
	defer srv.Close()
	old := WorkBuddyCNBilling
	WorkBuddyCNBilling = srv.URL
	t.Cleanup(func() { WorkBuddyCNBilling = old })

	acct := wbAcct()
	acct.Realm = WBRealmCN
	l := NewLane()
	l.SetWorkBuddyAccount(acct)
	l.SetWorkBuddyPersist(func(WBAccount) {})

	ran, err := l.WorkBuddyDailyCheckin(context.Background())
	if err != nil || !ran {
		t.Fatalf("run = (%v, %v), want the day closed, not an error", ran, err)
	}
	// The day is closed, so the hourly tick must not ask again.
	if ran2, err2 := l.WorkBuddyDailyCheckin(context.Background()); ran2 || err2 != nil {
		t.Fatalf("second run = (%v, %v), want a silent skip", ran2, err2)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("meter hits = %d, want the one claim", got)
	}
}

// intl has no checkin endpoint; its daily is the activity conversation. A CN
// checkin fired at an intl account would be a request to a path that does not
// exist there.
func TestWorkBuddyDailyCheckinSkipsIntlAccounts(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"code":0}`)
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct()) // no Realm: intl
	l.SetWorkBuddyPersist(func(WBAccount) {})

	ran, err := l.WorkBuddyDailyCheckin(context.Background())
	if ran || err != nil {
		t.Fatalf("run = (%v, %v), want a silent skip on intl", ran, err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("upstream hits = %d, want none", got)
	}
}

// A CN token must chat on the CN host. Sending it to workbuddy.ai is not a
// slower route, it is a refused one: the WAF reads the pair of token and
// headers as one fingerprint.
func TestWorkBuddyCNChatGoesToTheCNHost(t *testing.T) {
	var cnHits, intlHits int32
	sse := func(w http.ResponseWriter) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, wbChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	}
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cnHits, 1)
		sse(w)
	}))
	defer cn.Close()
	intl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&intlHits, 1)
		sse(w)
	}))
	defer intl.Close()
	oldCN, oldIntl := WorkBuddyCNBase, WorkBuddyBase
	WorkBuddyCNBase, WorkBuddyBase = cn.URL, intl.URL
	t.Cleanup(func() { WorkBuddyCNBase, WorkBuddyBase = oldCN, oldIntl })

	acct := wbAcct()
	acct.Realm = WBRealmCN
	l := NewLane()
	l.SetWorkBuddyAccount(acct)
	l.SetWorkBuddyPersist(func(WBAccount) {})

	_, _ = l.wbPost(context.Background(), map[string]any{
		"model": "deepseek-v4.1-flash", "stream": true,
		"messages": []map[string]any{{"role": RoleUser, "content": "hi"}},
	}, "conv-1", func([]byte) error { return nil })

	if got := atomic.LoadInt32(&intlHits); got != 0 {
		t.Fatalf("intl hits = %d, want 0 — a CN account must not land on workbuddy.ai", got)
	}
	if got := atomic.LoadInt32(&cnHits); got != 1 {
		t.Fatalf("CN hits = %d, want 1", got)
	}
}

// The realm comes from the issuer when it names one, and from the login the
// user opened when it does not — a silent issuer must not silently downgrade
// a CN account to intl.
func TestWBRealmFromIss(t *testing.T) {
	cases := []struct {
		iss  string
		want string
	}{
		{"https://www.workbuddy.ai", WBRealmIntl},
		{"codebuddy", WBRealmCN},
		{"https://copilot.tencent.com", WBRealmCN},
		{"internal-issuer", ""},
	}
	for _, c := range cases {
		if got := wbRealmFromIss(c.iss); got != c.want {
			t.Errorf("wbRealmFromIss(%q) = %q, want %q", c.iss, got, c.want)
		}
	}
}

func TestWorkBuddyDailyActivitySpentWindowClosesTheDay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"code":6004,"message":"free window exceeded, your usage will reset at 2099-01-01 00:00:00 UTC+8"}`)
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct())
	l.SetWorkBuddyPersist(func(WBAccount) {})

	ran, err := l.WorkBuddyDailyActivity(context.Background())
	if !ran {
		t.Fatal("a spent window must still consume the day's attempt")
	}
	if err == nil {
		t.Fatal("the caller should hear that the window was spent")
	}
	acct := l.WorkBuddySnapshot()
	if acct.LastActivityDate != time.Now().Format("2006-01-02") {
		t.Fatalf("a spent window must close the day, got %q", acct.LastActivityDate)
	}
}

// TestWBTurnTimeoutSharesTheStreamIdleDeadline keeps the WorkBuddy channel on
// the lane-wide silence bound: the stalled-exit arithmetic behind PR #40 (one
// response-header timeout + one idle timeout must fit the slow retry budget)
// applies to this source's streams exactly as it does to Zen and Kilo.
func TestWBTurnTimeoutSharesTheStreamIdleDeadline(t *testing.T) {
	if wbTurnTimeout != streamIdleTimeout {
		t.Errorf("wbTurnTimeout = %v, want it to share streamIdleTimeout (%v)", wbTurnTimeout, streamIdleTimeout)
	}
}

func TestWBApplyModelQuirks(t *testing.T) {
	body := map[string]any{}
	wbApplyModelQuirks(body, "hy4-preview-f", "light")
	if body["reasoning_effort"] != "high" {
		t.Fatalf("hy family effort = %v, want high (the only rung it accepts)", body["reasoning_effort"])
	}
	body = map[string]any{}
	wbApplyModelQuirks(body, "deepseek-v4.1-flash", "light")
	if body["reasoning_effort"] != "low" {
		t.Fatalf("deepseek light effort = %v, want low", body["reasoning_effort"])
	}
	if _, ok := body["thinking"].(map[string]any); !ok {
		t.Fatal("deepseek thinking must be enabled (forced reasoning)")
	}
	if _, ok := body["stream_options"].(map[string]any); !ok {
		t.Fatal("usage-bearing streams are mandatory on this lane")
	}
}

// The step family is a plain OpenAI-compatible vendor: its rung rides on
// reasoning_effort alone, with the three levels the upstream's own listing
// publishes (low / medium / high), and no deepseek-style thinking switch.
func TestWBStepFamilyRungRidesOnEffortAlone(t *testing.T) {
	for rung, want := range map[string]string{"light": "low", "balanced": "medium", "deep": "high"} {
		body := map[string]any{}
		wbApplyModelQuirks(body, "step-5-preview", rung)
		if body["reasoning_effort"] != want {
			t.Fatalf("step %s effort = %v, want %s", rung, body["reasoning_effort"], want)
		}
		if _, ok := body["thinking"]; ok {
			t.Fatalf("step %s must not carry a thinking switch: %v", rung, body["thinking"])
		}
	}
}

// step-5-preview is free while the official partnership window runs, and it
// joins the free roster with the capacities its own listing declares.
func TestWBCatalogCarriesStep5PreviewWhileItIsFree(t *testing.T) {
	old := wbStep5FreeUntil
	wbStep5FreeUntil = time.Now().Add(24 * time.Hour)
	t.Cleanup(func() { wbStep5FreeUntil = old })

	var got *ModelInfo
	for _, m := range buildWorkBuddyCatalog() {
		if m.ID == "step-5-preview" {
			row := m
			got = &row
		}
	}
	if got == nil {
		t.Fatal("step-5-preview must be in the free roster while the partnership window runs")
	}
	if got.Channel != ChannelWorkBuddy {
		t.Fatalf("channel = %q, want %q", got.Channel, ChannelWorkBuddy)
	}
	if !got.Vision || !got.Reasoning {
		t.Fatalf("step-5-preview capabilities = vision %v reasoning %v, want both true", got.Vision, got.Reasoning)
	}
	if got.ContextWindow != 1000000 || got.MaxOutput != 64000 {
		t.Fatalf("step-5-preview capacities = %d/%d, want 1000000/64000", got.ContextWindow, got.MaxOutput)
	}
	if got.CanDisableThinking {
		t.Fatal("step-5-preview is an always-reasoning model: thinking cannot be switched off")
	}
}

// After the window the model bills the account's credits, so the free roster
// must drop it rather than quietly spend a balance the user never agreed to.
func TestWBCatalogDropsStep5PreviewAfterTheFreeWindow(t *testing.T) {
	old := wbStep5FreeUntil
	wbStep5FreeUntil = time.Now().Add(-time.Hour)
	t.Cleanup(func() { wbStep5FreeUntil = old })

	for _, m := range buildWorkBuddyCatalog() {
		if m.ID == "step-5-preview" {
			t.Fatal("step-5-preview must leave the free roster once it bills credits")
		}
	}
}

// Issue #46: the manual probe hand-wrote a plain chat body and skipped the
// per-family quirks, so the upstream rejected it for thinking/effort reasons
// while live chat to the same model answered fine — the card read
// "探测不到上游" next to a working model. The probe must carry the same
// quirk fields a real turn does.
func TestProbeWorkBuddyCarriesModelQuirks(t *testing.T) {
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, wbChatPath) {
			raw, _ := io.ReadAll(r.Body)
			gotBody.Store(string(raw))
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, wbChatSSE(
				`{"choices":[{"delta":{"content":"ok"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct())
	res := l.probeWorkBuddy(context.Background(), ModelInfo{ID: "hy4-preview-f", Channel: ChannelWorkBuddy})
	if res.State != StateAvailable {
		t.Fatalf("probe state = %s (%s), want available", res.State, res.Detail)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody.Load().(string)), &body); err != nil {
		t.Fatalf("probe body is not JSON: %v", err)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("probe reasoning_effort = %v, want high — the hy family rejects anything else", body["reasoning_effort"])
	}
	if _, ok := body["stream_options"]; !ok {
		t.Fatal("probe body must carry usage-bearing stream_options like a real turn")
	}
}

// Issue #46 second half: the credits card called wbGet raw — no near-expiry
// renewal, no 401 refresh-and-retry — so a token the chat path still used
// (wbPost refreshes transparently) showed "积分读不出" on the card. Credits
// must ride the same refresh chain.
func TestWorkBuddyCreditsRefreshesOn401(t *testing.T) {
	var billingCalls, refreshCalls atomic.Int64
	var lastAuth, lastMethod atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, wbAuthRefreshPath):
			refreshCalls.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"tok-2","refreshToken":"r2"}}`)
		case strings.HasSuffix(r.URL.Path, wbUserResourcePath):
			n := billingCalls.Add(1)
			lastAuth.Store(r.Header.Get("authorization"))
			lastMethod.Store(r.Method)
			if n == 1 {
				w.WriteHeader(401) // openresty refuses the stale credential
				return
			}
			// The meter's own shape: two spendable packs, 249.04 + 100.
			fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[`+
				`{"PackageName":"Bonus Pack","CapacityUnit":"credit","Status":0,`+
				`"CycleCapacityRemain":249,"CycleCapacityRemainPrecise":"249.04","DeductionEndTime":9999999999999},`+
				`{"PackageName":"Free Plan Subscription","CapacityUnit":"credits","Status":0,`+
				`"CycleCapacityRemain":100,"CycleCapacityRemainPrecise":"100","DeductionEndTime":9999999999999}]}}}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })

	l := NewLane()
	l.SetWorkBuddyAccount(wbAcct())
	credits, err := l.WorkBuddyCredits(context.Background())
	if err != nil {
		t.Fatalf("credits read failed: %v", err)
	}
	if got := credits.String(); got != "349.04" {
		t.Fatalf("credits = %q, want the sum of the spendable packs (349.04)", got)
	}
	// The meter is POST-only: a GET is answered with a bare "404 page not
	// found" by the live router, which is exactly what made every card report
	// 积分查询返回 HTTP 404 while chat kept working (measured 2026-10-21).
	if lastMethod.Load() != "POST" {
		t.Fatalf("meter read rode %v, want POST", lastMethod.Load())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly one", refreshCalls.Load())
	}
	if lastAuth.Load() != "Bearer tok-2" {
		t.Fatalf("retry rode %v, want the refreshed token", lastAuth.Load())
	}
}

// The balance is the sum of the account's resource packs, not a field, and the
// packs the meter still lets it spend are not all the packs it returns. The
// sum is then split by when each pack stops being chargeable, because credit
// that dies this month and credit that does not are different things to a user
// deciding whether to burn an account now.
func TestWBMeterBalance(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	const noCutoff = time.Duration(-1)

	pack := func(name, unit string, status int, precise string, cutoff time.Duration, expired string) string {
		ms := int64(0)
		if cutoff != noCutoff {
			ms = now.Add(cutoff).UnixMilli()
		}
		return `{"PackageName":"` + name + `","CapacityUnit":"` + unit +
			`","Status":` + fmt.Sprint(status) +
			`,"CycleCapacityRemainPrecise":"` + precise +
			`","DeductionEndTime":` + fmt.Sprint(ms) +
			`,"ExpiredTime":"` + expired + `"}`
	}
	envelope := func(packs ...string) []byte {
		return []byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[` +
			strings.Join(packs, ",") + `]}}}}`)
	}
	in := func(d time.Duration) time.Duration { return d }

	t.Run("reads the precise value, not the truncated one", func(t *testing.T) {
		body := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[` +
			`{"CapacityUnit":"credit","Status":0,"CycleCapacityRemain":249,"CycleCapacityRemainPrecise":"249.04","DeductionEndTime":` +
			fmt.Sprint(now.Add(in(90*24*time.Hour)).UnixMilli()) + `}]}}}}`)
		got, err := wbMeterBalance(body, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != "249.04" {
			t.Fatalf("balance = %q, want 249.04 — the integer field drops the cents", got.String())
		}
	})
	t.Run("sums packs without floating-point tail noise", func(t *testing.T) {
		got, err := wbMeterBalance(envelope(
			pack("A", "credit", 0, "0.1", in(90*24*time.Hour), ""),
			pack("B", "credits", 0, "0.2", in(90*24*time.Hour), "")), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != "0.3" {
			t.Fatalf("balance = %q, want 0.3, not 0.30000000000000004", got.String())
		}
	})
	t.Run("splits by when the pack stops being chargeable", func(t *testing.T) {
		got, err := wbMeterBalance(envelope(
			pack("Dying", "credit", 0, "100", in(3*24*time.Hour), ""),
			pack("Long lived", "credit", 0, "250", in(90*24*time.Hour), "")), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Expiring != 100 || got.Permanent != 250 || got.Total != 350 {
			t.Fatalf("split = %+v, want expiring 100 / permanent 250 / total 350", got)
		}
		if got.Packs != 2 {
			t.Fatalf("packs = %d, want both live packs counted", got.Packs)
		}
	})
	t.Run("an unpublished cutoff is permanent, not expiring", func(t *testing.T) {
		got, err := wbMeterBalance(envelope(pack("No cutoff", "credit", 0, "40", noCutoff, "")), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Expiring != 0 || got.Permanent != 40 {
			t.Fatalf("split = %+v, want unknown-cutoff credit kept out of the dying bucket", got)
		}
	})
	t.Run("dead packs are reported apart, never folded into the total", func(t *testing.T) {
		got, err := wbMeterBalance(envelope(
			pack("Live", "credit", 0, "55.67", in(24*time.Hour), ""),
			pack("Dead status", "credit", wbMeterPackExpired, "400", in(24*time.Hour), ""),
			pack("Dead cutoff", "credit", 0, "100", in(-time.Hour), ""),
			pack("Dead date", "credit", 0, "50", in(24*time.Hour), "2026-10-01 00:00:00")), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 55.67 || got.Packs != 1 {
			t.Fatalf("total = %v over %d packs, want 55.67 over one live pack", got.Total, got.Packs)
		}
		if got.Dead != 550 {
			t.Fatalf("dead = %v, want the 550 the meter still ships for closed packs", got.Dead)
		}
	})
	t.Run("does not spend a token-metered pack as credit", func(t *testing.T) {
		got, err := wbMeterBalance(envelope(
			pack("Tokens", "token", 0, "5000", in(24*time.Hour), ""),
			pack("Credit", "credit", 0, "10", in(24*time.Hour), "")), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 10 {
			t.Fatalf("total = %v, want 10 — 5000 tokens is not 5000 积分", got.Total)
		}
	})
	t.Run("a spendable nothing is the answer 0, not an error", func(t *testing.T) {
		got, err := wbMeterBalance(envelope(pack("Used up", "credit", 0, "0", in(24*time.Hour), "")), now)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != "0" || got.Packs != 0 {
			t.Fatalf("balance = %q over %d packs, want 0 over none", got.String(), got.Packs)
		}
	})
	t.Run("a business refusal is an error with the upstream reason", func(t *testing.T) {
		_, err := wbMeterBalance([]byte(`{"code":10002,"msg":"no qualification"}`), now)
		if err == nil || !strings.Contains(err.Error(), "10002") || !strings.Contains(err.Error(), "no qualification") {
			t.Fatalf("err = %v, want the code and the upstream message", err)
		}
	})
	t.Run("a missing pack list is not a zero balance", func(t *testing.T) {
		if _, err := wbMeterBalance(envelope(), now); err == nil {
			t.Fatal("an empty Accounts list must read as unreadable, not as 0")
		}
	})
	t.Run("an unparsable body is an error", func(t *testing.T) {
		if _, err := wbMeterBalance([]byte("<html>404</html>"), now); err == nil {
			t.Fatal("non-JSON must not read as a balance")
		}
	})
}

// The window the buckets are cut with has to reach the card as data: a label
// that says one number next to a rule that used another tells the user a
// deadline we never applied.
func TestWBExpiringWindowIsPublished(t *testing.T) {
	if WBExpiringWindowDays() != 15 {
		t.Fatalf("window = %d days, want the 15 the split was measured against", WBExpiringWindowDays())
	}
}

// The v2 gate fingerprints the envelope, not the body: /v2/chat/completions
// answers 400 code 11128 "conversation id missing" unless X-Conversation-ID
// and the B3 trace quartet ride along (found in the community's CodeBuddy
// clients; measured against the live upstream 2026-10-16).
func TestWBChatHeadersCarryTheConversationProtocol(t *testing.T) {
	h := wbChatHeaders(wbAcct(), "req-1", "0123456789abcdef0123456789abcdef", "hy3")
	if h["X-Conversation-ID"] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("X-Conversation-ID = %q", h["X-Conversation-ID"])
	}
	if h["X-Request-ID"] != "req-1" || h["X-Conversation-Request-ID"] != "req-1" ||
		h["X-Conversation-Message-ID"] != "req-1" {
		t.Errorf("request ids must agree: %+v", h)
	}
	if h["X-Model-ID"] != "hy3" {
		t.Errorf("X-Model-ID = %q, want the model id", h["X-Model-ID"])
	}
	trace := h["X-Request-Trace-Id"]
	if len(trace) != 32 {
		t.Fatalf("trace id = %q, want 32 hex", trace)
	}
	if h["X-B3-TraceId"] != trace || h["X-B3-SpanId"] != trace[:16] ||
		h["X-B3-ParentSpanId"] != trace[16:32] || h["X-B3-Sampled"] != "1" {
		t.Errorf("B3 quartet inconsistent with the trace id: %+v", h)
	}
	if want := trace + "-" + trace[:16] + "-1-" + trace[16:32]; h["b3"] != want {
		t.Errorf("b3 = %q, want %q", h["b3"], want)
	}
	// The account identity still rides along: the gate reads both.
	if h["x-machine-id"] == "" || h["authorization"] != "Bearer tok-1" {
		t.Errorf("identity headers lost: %+v", h)
	}
}

// One conversation keeps one id (prompt cache); a fresh seed gets a fresh id,
// and a seedless turn (a probe) never reuses one.
func TestWBConversationIDIsStablePerConversation(t *testing.T) {
	a := wbConversationID("conv-1")
	if len(a) != 32 {
		t.Fatalf("id = %q, want 32 hex", a)
	}
	if wbConversationID("conv-1") != a {
		t.Error("the same conversation must keep one id for the upstream prompt cache")
	}
	if wbConversationID("conv-2") == a {
		t.Error("different conversations must not share an id")
	}
	if wbConversationID("") == wbConversationID("") {
		t.Error("a seedless turn must get a fresh id, not a shared one")
	}
}

// The gate also refuses a turn whose first message is not a system prompt
// (400 code 11128 "first message is not system prompt").
func TestWBEnsureSystemFirstLeadsWithASystemMessage(t *testing.T) {
	body := map[string]any{
		"model":    "hy3",
		"messages": []map[string]any{{"role": RoleUser, "content": "hi"}},
	}
	wbEnsureSystemFirst(body)
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) != 2 || msgs[0]["role"] != RoleSystem {
		t.Fatalf("messages = %+v, want a system message first", msgs)
	}
	if msgs[0]["content"] != wbSystemPreamble || msgs[1]["content"] != "hi" {
		t.Errorf("caller content must survive: %+v", msgs)
	}

	// A caller that already leads with its own system prompt keeps it.
	own := map[string]any{
		"model": "hy3",
		"messages": []map[string]any{
			{"role": RoleSystem, "content": "You are a careful assistant."},
			{"role": RoleUser, "content": "hi"},
		},
	}
	wbEnsureSystemFirst(own)
	got, _ := own["messages"].([]map[string]any)
	if len(got) != 2 || got[0]["content"] != "You are a careful assistant." {
		t.Errorf("an existing leading system prompt must not be replaced or duplicated: %+v", got)
	}
}
