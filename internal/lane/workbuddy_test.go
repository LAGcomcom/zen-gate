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
	// Logged in: the three free models enter the catalog even though every
	// fetched source just failed.
	l.SetWorkBuddyAccount(wbAcct())
	l.RefreshCatalog(context.Background())
	cat, _, _ := l.Snapshot()
	n := 0
	for _, m := range cat {
		if m.Channel == ChannelWorkBuddy {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("catalog holds %d WorkBuddy models, want 3", n)
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
	var lastAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, wbAuthRefreshPath):
			refreshCalls.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"tok-2","refreshToken":"r2"}}`)
		case strings.HasSuffix(r.URL.Path, wbUserResourcePath):
			n := billingCalls.Add(1)
			lastAuth.Store(r.Header.Get("authorization"))
			if n == 1 {
				w.WriteHeader(401) // openresty refuses the stale credential
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{"credit":123}}`)
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
	if credits != "123" {
		t.Fatalf("credits = %q, want 123", credits)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want exactly one", refreshCalls.Load())
	}
	if lastAuth.Load() != "Bearer tok-2" {
		t.Fatalf("retry rode %v, want the refreshed token", lastAuth.Load())
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
