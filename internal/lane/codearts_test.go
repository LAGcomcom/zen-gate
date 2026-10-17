package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zen-gate/internal/codearts/upstream"
)

// caStub serves every CodeArts host (snap / manager / STS / benefit) from one
// httptest URL: the lane-level bases are pointed at it wholesale, so the tests
// drive the real request chain — signatures, DPoP, form encoding and all —
// against a recording stub.
type caStub struct {
	t        *testing.T
	mu         sync.Mutex
	chats      []http.Header // headers of every chat attempt
	chatsN     int
	tokens     int // oauth2/tokens hits
	refreshes  int
	expired    bool // hand out an expired STS first, a fresh one on refresh
	staleToken string // chat answers 401 while x-auth-token equals this
}

func newCAStub(t *testing.T) (*caStub, *httptest.Server) {
	st := &caStub{t: t}
	var srv *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, upstream.EpOAuthTokens):
			st.tokens++
			grant := r.FormValue("grant_type")
			if grant == "refresh_token" {
				st.refreshes++
			}
			fresh := st.expired // after the first refresh the token is fresh
			if grant == "refresh_token" {
				st.expired = false
				fresh = true
			}
			exp := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
			if !fresh {
				exp = time.Now().Add(-time.Hour).Format(time.RFC3339)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_id": "u-1", "user_name": "tester", "domain_id": "d-1",
				"refresh_token": "rt-2",
				"credentials": map[string]any{
					"access_key_id": "ak-" + fmt.Sprint(st.tokens),
					"secret_access_key": "sk-" + fmt.Sprint(st.tokens),
					"security_token":  fmt.Sprintf("tok-%d", st.tokens),
					"expiration":      exp,
				},
			})
		case strings.HasSuffix(r.URL.Path, upstream.EpLoginTicket):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_id": "u-1", "user_name": "tester", "domain_id": "d-1",
				"refresh_token": "rt-1",
				"credentials": map[string]any{
					"access_key_id": "ak-1", "secret_access_key": "sk-1",
					"security_token": "tok-1",
					"expiration":     time.Now().Add(24 * time.Hour).Format(time.RFC3339),
				},
			})
		case strings.HasSuffix(r.URL.Path, upstream.EpChatV2):
			st.chatsN++
			st.chats = append(st.chats, r.Header.Clone())
			if st.staleToken != "" && r.Header.Get("x-auth-token") == st.staleToken {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"token expired"}}`)
				return
			}
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`)
			fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			fmt.Fprintln(w, "data: [DONE]")
		case strings.HasSuffix(r.URL.Path, upstream.EpModelBuiltin):
			_ = json.NewEncoder(w).Encode(map[string]any{"builtinModels": []any{
				map[string]any{"model_id": "GLM-5.2", "context_window": 200000, "max_tokens": 131072},
			}})
		case strings.HasSuffix(r.URL.Path, upstream.EpBenefitClaim):
			_ = json.NewEncoder(w).Encode(map[string]any{"error_code": "0000"})
		case strings.HasSuffix(r.URL.Path, upstream.EpBenefitConfig):
			_ = json.NewEncoder(w).Encode(map[string]any{"error_code": "0000", "result": map[string]any{
				"models": []any{map[string]any{"model_id": "deepseek-v4-flash-0731", "context_window": 1000000, "max_tokens": 65536}},
			}})
		default: // agent-center and anything else: a clean empty answer
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
		}
	})
	srv = httptest.NewServer(handler)
	return st, srv
}

// pointCAHosts redirects every CodeArts base at srv and restores them on exit.
func pointCAHosts(t *testing.T, srv *httptest.Server) {
	t.Cleanup(func() {
		CodeArtsSnapBase, CodeArtsBenefitBase = upstream.SnapEngineApiHost, upstream.BenefitHost
		CodeArtsPortalBase, CodeArtsManagerBase, CodeArtsSTSBase = upstream.PortalHost, upstream.SnapManagerHost, upstream.STSHost
	})
	CodeArtsSnapBase = srv.URL
	CodeArtsBenefitBase = srv.URL
	CodeArtsPortalBase = srv.URL
	CodeArtsManagerBase = srv.URL
	CodeArtsSTSBase = srv.URL
}

func TestCodeArtsLoginPollCompletesViaTicket(t *testing.T) {
	_, srv := caStubServer(t)
	defer srv.Close()
	pointCAHosts(t, srv)

	l := NewLane()
	var saved []CAAccount
	l.SetCodeArtsPersist(func(a CAAccount) { saved = append(saved, a) })

	url, err := l.CodeArtsLoginStart()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "/authorize?") || !strings.Contains(url, "code_challenge=") {
		t.Fatalf("authorize url wrong: %s", url)
	}
	state, err := l.CodeArtsLoginPoll(context.Background())
	if err != nil || state != "ok" {
		t.Fatalf("poll = %q err %v, want ok", state, err)
	}
	a := l.CodeArtsSnapshot()
	if a.SecurityToken != "tok-1" || a.UserID != "u-1" || a.RefreshToken == "" || a.DPoPKey["d"] == "" {
		t.Fatalf("account not committed: %+v", a)
	}
	if len(saved) == 0 {
		t.Fatal("persist callback must have run")
	}
	// A second poll with no live session says so instead of pretending.
	l.CodeArtsLogout()
	if _, err := l.CodeArtsLoginPoll(context.Background()); err == nil {
		t.Fatal("polling without a started login must error")
	}
}

func caStubServer(t *testing.T) (*caStub, *httptest.Server) {
	st, srv := newCAStub(t)
	return st, srv
}

// A token past its expiry is renewed before the turn is spent: one refresh,
// one chat, and the chat carries the fresh STS token.
func TestCodeArtsPostRefreshesNearExpiry(t *testing.T) {
	stub, srv := caStubServer(t)
	defer srv.Close()
	pointCAHosts(t, srv)

	l := NewLane()
	l.SetCodeArtsAccount(CAAccount{
		Enabled: true, UserID: "u-1", SecurityToken: "tok-old",
		AccessKeyID: "ak", SecretAccessKey: "sk",
		RefreshToken: "rt", CodeVerifier: "cv", DPoPKey: mustDPoP(t),
		ExpirationMS: time.Now().UnixMilli(), // already expired
	})
	var got string
	usage, err := l.caPost(context.Background(),
		map[string]any{"model": "GLM-5.2", "stream": true, "messages": []any{}},
		"ca-chat-id", func(p []byte) error { got += string(p); return nil })
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if usage == nil || usage.Output != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if stub.refreshes != 1 || stub.chatsN != 1 {
		t.Fatalf("refreshes=%d chats=%d, want 1/1 (renewed before spending)", stub.refreshes, stub.chatsN)
	}
	if h := stub.chats[0].Get("x-auth-token"); h != "tok-1" {
		t.Fatalf("chat rode x-auth-token %q, want the refreshed tok-1", h)
	}
}

// A refusal mid-turn (the rotated-but-unsaved pair case) refreshes once and
// retries the chat; the retry must also carry the conversation's affinity ids.
func TestCodeArtsPostRefreshesOn401AndKeepsChatAffinity(t *testing.T) {
	stub, srv := caStubServer(t)
	defer srv.Close()
	pointCAHosts(t, srv)
	stub.staleToken = "tok-stale"

	l := NewLane()
	l.SetCodeArtsAccount(CAAccount{
		Enabled: true, UserID: "u-1", SecurityToken: "tok-stale",
		AccessKeyID: "ak", SecretAccessKey: "sk",
		RefreshToken: "rt", CodeVerifier: "cv", DPoPKey: mustDPoP(t),
		ExpirationMS: time.Now().Add(24 * time.Hour).UnixMilli(), // fresh on paper
	})
	var got string
	usage, err := l.caPost(context.Background(),
		map[string]any{"model": "GLM-5.2", "stream": true, "messages": []any{}},
		"ca-chat-id", func(p []byte) error { got += string(p); return nil })
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if usage == nil || usage.Output != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if stub.refreshes != 1 {
		t.Fatalf("refreshes = %d, want exactly one", stub.refreshes)
	}
	if stub.chatsN != 2 {
		t.Fatalf("chat attempts = %d, want 2 (401 then retry)", stub.chatsN)
	}
	if h := stub.chats[1].Get("x-auth-token"); h != "tok-1" {
		t.Fatalf("retry rode x-auth-token %q, want the refreshed one", h)
	}
	if stub.chats[1].Get("Chat-Id") != "ca-chat-id" {
		t.Fatal("chat affinity header must carry the conversation id")
	}
	if stub.chats[1].Get("Session-Id") == "" {
		t.Fatal("session id must derive from the chat id")
	}
	if !strings.Contains(got, "ok") {
		t.Fatalf("streamed payload = %q", got)
	}
}

func TestCodeArtsBenefitModelCarriesMaaSHeader(t *testing.T) {
	stub, srv := caStubServer(t)
	defer srv.Close()
	pointCAHosts(t, srv)

	l := NewLane()
	acct := CAAccount{Enabled: true, UserID: "u-1", SecurityToken: "tok", AccessKeyID: "ak", SecretAccessKey: "sk"}
	l.SetCodeArtsAccount(acct)

	// Discovery records which ids are benefit for this account; the post path
	// must then sign the request with maas_type: benefit (missing it, the
	// upstream answers 404 not-registered).
	if _, err := caFetchModels(acct); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"model": "GLM-5.2", "stream": true}
	if _, err := l.caPost(context.Background(), body, "c1", func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if h := stub.chats[0].Get("maas_type"); h != "" {
		t.Fatalf("built-in model rode maas_type %q, want none", h)
	}
	if _, err := l.caPost(context.Background(),
		map[string]any{"model": "deepseek-v4-flash-0731", "stream": true}, "c1",
		func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if h := stub.chats[1].Get("maas_type"); h != upstream.MaasBenefit {
		t.Fatalf("benefit model rode maas_type %q, want %q", h, upstream.MaasBenefit)
	}
}

func mustDPoP(t *testing.T) map[string]string {
	t.Helper()
	k, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestClassifyCodeArtsShapes(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, CodeCredential}, {403, CodeCredential},
		{429, CodeQuota}, {400, CodeClient}, {404, CodeClient}, {500, CodeServer},
	}
	for _, c := range cases {
		ue := classifyCodeArts(&upstream.ApiError{Status: c.status, Message: "x"})
		if ue.Code != c.want {
			t.Errorf("status %d → %s, want %s", c.status, ue.Code, c.want)
		}
	}
	if ue := classifyCodeArts(fmt.Errorf("dial tcp: refused")); ue.Code != CodeTransport {
		t.Errorf("non-ApiError → %s, want TRANSPORT", ue.Code)
	}
}

func TestCAChatIDIsStable32Hex(t *testing.T) {
	a, b := caChatID("conv-1"), caChatID("conv-1")
	if a != b || len(a) != 32 || caChatID("conv-2") == a {
		t.Fatalf("chat id shape/lifetime wrong: %s / %s", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("not lowercase hex: %s", a)
		}
	}
}

func TestCAAccountExpirationRoundtrip(t *testing.T) {
	// A login/refresh response with an RFC3339 expiry must land in ExpirationMS
	// so the proactive refresh can fire before the STS token dies.
	rfc := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)
	tok := &upstream.TokenResponse{UserID: "u", Credentials: upstream.Credentials{
		AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st", Expiration: rfc}}
	acct := caAccountFromToken(CAAccount{Enabled: true}, tok)
	if acct.ExpirationMS == 0 {
		t.Fatal("expiration not parsed")
	}
	if !caTokenExpiringSoon(CAAccount{ExpirationMS: time.Now().UnixMilli()}) {
		t.Fatal("a past expiry must count as expiring soon")
	}
	if caTokenExpiringSoon(CAAccount{ExpirationMS: time.Now().Add(3 * time.Hour).UnixMilli()}) {
		t.Fatal("3h out must not be expiring soon")
	}
}
