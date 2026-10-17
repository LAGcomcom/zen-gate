package lane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/codearts/upstream"
)

// The account side of the CodeArts 账号渠道: the Huawei Cloud OAuth login,
// the STS refresh, the daily credit check-in and the availability probe. All of
// it rides caMu, which serializes token rotation.
//
// Why the refresh must be single-flight: the refresh token is bound (STS-side)
// to the DPoP keypair and the code_verifier that minted it, and refreshing
// rotates the token — two overlapping refreshes on the old pair lock the
// account out ("InvalidDPoPHeader" / "has been used"). The whole exchange
// therefore holds caMu, and a latecomer re-checks freshness first.

// SetCodeArtsAccount installs the account snapshot from config; the caller
// refreshes the catalog.
func (l *Lane) SetCodeArtsAccount(acct CAAccount) {
	l.caMu.Lock()
	l.caAcct = acct
	l.caMu.Unlock()
}

// SetCodeArtsPersist wires the write-back path for rotated STS credentials and
// check-in dates. Called with caMu held by the refresher.
func (l *Lane) SetCodeArtsPersist(fn func(CAAccount)) {
	l.caMu.Lock()
	l.caPersist = fn
	l.caMu.Unlock()
}

// CodeArtsSnapshot returns a copy of the live account state.
func (l *Lane) CodeArtsSnapshot() CAAccount {
	l.caMu.Lock()
	defer l.caMu.Unlock()
	return l.caAcct
}

// CodeArtsLogout drops the account (the store copy is the caller's to clear).
func (l *Lane) CodeArtsLogout() {
	l.caMu.Lock()
	l.caAcct = CAAccount{}
	if l.caLogin != nil {
		l.caLogin.shutdown()
		l.caLogin = nil
	}
	l.caMu.Unlock()
	l.fireChange()
}

// codeArtsReady returns an enabled, credential-bearing copy of the account.
func (l *Lane) codeArtsReady() (CAAccount, bool) {
	l.caMu.Lock()
	defer l.caMu.Unlock()
	a := l.caAcct
	if !a.Enabled || strings.TrimSpace(a.SecurityToken) == "" {
		return CAAccount{}, false
	}
	return a, true
}

// caPersistLocked writes the account back; caller holds caMu.
func (l *Lane) caPersistLocked() {
	if l.caPersist != nil {
		l.caPersist(l.caAcct)
	}
}

// caPendingLogin is one browser round-trip in progress: the loopback callback
// captures the authorization code (fast path), ticket polling is the fallback
// if the redirect can't reach 127.0.0.1. The portal hands back a secret on the
// first callback that ticket polling must then use, so the secret is guarded by
// its own mutex — the callback goroutine writes it, the poll reads it.
type caPendingLogin struct {
	ticketID  string
	verifier  string // PKCE code_verifier
	dpop      upstream.DPoPPrivateJWK
	port      int
	ln        net.Listener
	codeCh    chan string
	secMu     sync.Mutex
	secret    string
	closeOnce sync.Once
}

func (s *caPendingLogin) setSecret(v string) {
	if v == "" {
		return
	}
	s.secMu.Lock()
	s.secret = v
	s.secMu.Unlock()
}

func (s *caPendingLogin) getSecret(fallback string) string {
	s.secMu.Lock()
	defer s.secMu.Unlock()
	if s.secret != "" {
		return s.secret
	}
	return fallback
}

func (s *caPendingLogin) shutdown() {
	s.closeOnce.Do(func() {
		if s.ln != nil {
			_ = s.ln.Close()
		}
	})
}

// CodeArtsLoginStart opens a browser OAuth login and returns the URL. A
// loopback listener catches the redirect; completion is observed by
// CodeArtsLoginPoll, so this returns immediately.
func (l *Lane) CodeArtsLoginStart() (string, error) {
	ticketID, err := upstream.RandomHex(16)
	if err != nil {
		return "", err
	}
	secret, err := upstream.RandomHex(16)
	if err != nil {
		return "", err
	}
	verifier, challenge, err := upstream.PKCE()
	if err != nil {
		return "", err
	}
	dpop, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("loopback listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	l.caMu.Lock()
	if l.caLogin != nil {
		l.caLogin.shutdown()
	}
	sess := &caPendingLogin{
		ticketID: ticketID, verifier: verifier, dpop: dpop,
		port: port, ln: ln, codeCh: make(chan string, 1), secret: secret,
	}
	l.caLogin = sess
	cfg := caLoginConfig(CAAccount{})
	loginURL := caClient().BuildAuthorizeURL(cfg, ticketID, challenge, "SHA-256", port)
	l.caMu.Unlock()

	go serveCodeArtsCallback(ln, cfg.RedirectPath, sess)
	return loginURL, nil
}

// serveCodeArtsCallback mirrors cmd/login: the portal calls back twice — once
// with a secret + redirect (which must 307 or the chain breaks), once with the
// code. The callback's secret is the authoritative one for ticket polling.
func serveCodeArtsCallback(ln net.Listener, redirectPath string, sess *caPendingLogin) {
	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		sess.setSecret(q.Get("secret"))
		if code == "" && q.Get("redirect") != "" {
			http.Redirect(w, r, q.Get("redirect"), http.StatusTemporaryRedirect)
			return
		}
		if code == "" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			if vals, perr := url.ParseQuery(string(body)); perr == nil {
				code = vals.Get("code")
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<p>登录回调缺少 code，可改用轮询或重试。</p>"))
			return
		}
		_, _ = w.Write([]byte("<p>登录完成，可关闭此页面返回 zen-gate。</p>"))
		select {
		case sess.codeCh <- code:
		default:
		}
	})
	_ = http.Serve(ln, mux)
}

// CodeArtsLoginStatus is one poll of an in-progress login.
// "pending" while the browser round-trip is unfinished, "ok" once credentials
// landed (already persisted into the lane), an error otherwise.
func (l *Lane) CodeArtsLoginPoll(ctx context.Context) (string, error) {
	l.caMu.Lock()
	sess := l.caLogin
	if sess == nil {
		l.caMu.Unlock()
		return "", errors.New("没有进行中的登录")
	}
	l.caMu.Unlock()

	cfg := caLoginConfig(l.CodeArtsSnapshot())
	c := caClient()

	// Fast path: the callback delivered a code.
	var code string
	select {
	case code = <-sess.codeCh:
	default:
	}
	if code != "" {
		tok, err := c.ExchangeCode(ctx, cfg, code, sess.verifier, sess.port, sess.dpop)
		if err != nil {
			return "", err
		}
		return "ok", l.saveCodeArtsLogin(tok, sess)
	}

	// Fallback: ticket polling. snap-manager answers a real TokenResponse once
	// the browser has finished, even without the loopback callback reaching us.
	tok, terr := c.PollTicket(ctx, cfg, sess.ticketID, sess.getSecret(""))
	if terr == nil && tok != nil && tok.UserID != "" && tok.Credentials.SecurityToken != "" {
		return "ok", l.saveCodeArtsLogin(tok, sess)
	}
	return "pending", nil
}

// saveCodeArtsLogin commits a finished login: builds the account from the
// token pair (carrying the login-time bindings the response omits), persists,
// closes the listener and triggers a catalog refresh.
func (l *Lane) saveCodeArtsLogin(tok *upstream.TokenResponse, sess *caPendingLogin) error {
	if tok == nil {
		return errors.New("登录响应为空")
	}
	base := CAAccount{
		Enabled:      true,
		CodeVerifier: sess.verifier,
		DPoPKey:      sess.dpop,
		ClientID:     upstream.CLIENT_ID,
	}
	acct := caAccountFromToken(base, tok)
	if acct.SecurityToken == "" {
		return errors.New("登录未返回 STS 凭证")
	}
	l.caMu.Lock()
	l.caAcct = acct
	if l.caLogin == sess {
		sess.shutdown()
		l.caLogin = nil
	}
	l.caPersistLocked()
	l.caMu.Unlock()
	// A fresh login's first discovery also seeds the per-account benefit flags
	// the post path reads back, so the catalog rebuild is part of committing
	// the login — fireChange alone would only repaint the dashboard.
	go l.RefreshCatalog(context.Background())
	l.fireChange()
	return nil
}

// refreshCodeArts exchanges the refresh token for fresh STS credentials.
// Single-flight: holds caMu, and a latecomer re-checks freshness first, so N
// concurrent 401s produce one refresh.
func (l *Lane) refreshCodeArts(ctx context.Context) error {
	l.caMu.Lock()
	defer l.caMu.Unlock()
	acct := l.caAcct
	if acct.RefreshToken == "" || acct.CodeVerifier == "" {
		return errors.New("CodeArts 缺少 refresh 凭据")
	}
	if !caTokenExpiringSoon(acct) && time.Since(l.caRefreshedAt) < time.Minute {
		return nil // someone just refreshed; the pair is current
	}
	tok, err := caClient().RefreshToken(ctx, caLoginConfig(acct), acct.RefreshToken, acct.CodeVerifier, acct.DPoPKey)
	if err != nil {
		return err
	}
	if tok == nil || tok.Credentials.SecurityToken == "" {
		return errors.New("CodeArts 刷新未返回凭证")
	}
	l.caAcct = caAccountFromToken(acct, tok)
	l.caRefreshedAt = time.Now()
	l.caPersistLocked()
	return nil
}

// caPost is the one post entry every CodeArts turn goes through (probe, user
// traffic). It refreshes a token near expiry before spending it and on a
// credential refusal refreshes once and retries — a rotated-but-unsaved pair
// would otherwise fail the turn even though a valid refresh is sitting there.
func (l *Lane) caPost(ctx context.Context, body map[string]any, chatID string, onData func(payload []byte) error) (*Usage, error) {
	acct, ok := l.codeArtsReady()
	if !ok {
		return nil, &UpstreamError{Code: CodeCredential, Message: "CodeArts 账号未登录或已停用"}
	}
	if caTokenExpiringSoon(acct) {
		if rerr := l.refreshCodeArts(ctx); rerr == nil {
			if fresh, ok := l.codeArtsReady(); ok {
				acct = fresh
			}
		}
	}
	usage, err := caPostOnce(ctx, body, acct, chatID, caIsBenefit(acct, modelOfBody(body)), onData)
	if ue := asUpstream(err); ue != nil && ue.Code == CodeCredential {
		if rerr := l.refreshCodeArts(ctx); rerr == nil {
			if fresh, ok := l.codeArtsReady(); ok {
				return caPostOnce(ctx, body, fresh, chatID, caIsBenefit(fresh, modelOfBody(body)), onData)
			}
		}
	}
	return usage, err
}

// modelOfBody reads the requested model out of a lane chat body; "" if absent.
func modelOfBody(body map[string]any) string {
	if m, ok := body["model"].(string); ok {
		return m
	}
	return ""
}

// CodeArtsDailyCheckin claims the day's credit campaigns (the 华为云 daily
// check-in) once per local calendar day. Returns ran=false if today is already
// done. A spent eligibility is not an error.
func (l *Lane) CodeArtsDailyCheckin(ctx context.Context) (bool, error) {
	acct, ok := l.codeArtsReady()
	if !ok {
		return false, nil
	}
	today := time.Now().Format("2006-01-02")
	if acct.LastCheckinDate == today {
		return false, nil
	}
	if caTokenExpiringSoon(acct) {
		if rerr := l.refreshCodeArts(ctx); rerr == nil {
			if fresh, ok := l.codeArtsReady(); ok {
				acct = fresh
			}
		}
	}
	if _, err := caClient().Checkin(ctx, caCred(acct)); err != nil {
		return true, err // transient: leave the day open so the next tick retries
	}
	l.markCodeArtsCheckin(today)
	return true, nil
}

func (l *Lane) markCodeArtsCheckin(date string) {
	l.caMu.Lock()
	l.caAcct.LastCheckinDate = date
	l.caPersistLocked()
	l.caMu.Unlock()
}

// probeCodeArts re-tests one CodeArts model with a minimal chat — the cheapest
// proof of life. Routes through caPost so a near-expiry token refreshes first.
func (l *Lane) probeCodeArts(ctx context.Context, m ModelInfo) ProbeResult {
	res := ProbeResult{Model: m.ID, State: StateUnknown, At: time.Now().UnixMilli()}
	if _, ok := l.codeArtsReady(); !ok {
		res.State = StateUnavailable
		res.Detail = "CodeArts 未登录或已停用"
		return res
	}
	body := map[string]any{
		"model":      upstream.CanonicalModel(m.ID),
		"stream":     true,
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": RoleUser, "content": "ping"}},
	}
	start := time.Now()
	_, err := l.caPost(ctx, body, caChatID("probe:"+m.ID), func([]byte) error {
		if res.TTFTMs == 0 {
			res.TTFTMs = atLeastMS(start)
		}
		return nil
	})
	res.LatencyMs = atLeastMS(start)
	if err != nil {
		ue := asUpstream(err)
		switch ue.Code {
		case CodeQuota:
			res.State = StateThrottled
			res.Detail = "上游排队/限流"
		case CodeCredential:
			res.State = StateUnavailable
			res.Detail = "登录态失效（刷新失败）"
		case CodeClient:
			res.State = StateUnavailable
			res.Detail = truncate(ue.Message, 160)
		case CodeServer:
			if ue.Unavailable {
				res.State = StateUnavailable
			}
		}
		return res
	}
	res.State = StateAvailable
	return res
}
