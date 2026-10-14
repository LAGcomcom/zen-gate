package lane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The account side of the WorkBuddy 账号渠道: login, refresh, the daily
// activity claim, and the credits readout. Everything here rides wbMu, which
// serializes token rotation — upstream swaps the refresh token on every
// refresh, so two overlapping refreshes void the second pair and log the
// account out.

// SetWorkBuddyAccount installs the account snapshot from config. Catalog
// refresh is the caller's job (main wires it at boot; admin refreshes after
// login/logout/toggles).
func (l *Lane) SetWorkBuddyAccount(acct WBAccount) {
	l.wbMu.Lock()
	l.wbAcct = acct
	l.wbMu.Unlock()
}

// SetWorkBuddyPersist wires the write-back path for rotated token pairs and
// activity dates. Called with wbMu held by the refresher — the callback must
// not call back into the account setters.
func (l *Lane) SetWorkBuddyPersist(fn func(WBAccount)) {
	l.wbMu.Lock()
	l.wbPersist = fn
	l.wbMu.Unlock()
}

// WorkBuddySnapshot returns a copy of the live account state.
func (l *Lane) WorkBuddySnapshot() WBAccount {
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	return l.wbAcct
}

// WorkBuddyLogout drops the account. The store copy is the caller's to clear;
// this only takes the live snapshot down so in-flight code stops using it.
func (l *Lane) WorkBuddyLogout() {
	l.wbMu.Lock()
	l.wbAcct = WBAccount{}
	l.wbLoginState = ""
	l.wbMu.Unlock()
	l.fireChange()
}

// workBuddyReady returns an enabled, token-bearing copy of the account.
func (l *Lane) workBuddyReady() (WBAccount, bool) {
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	if !l.wbAcct.Enabled || strings.TrimSpace(l.wbAcct.AccessToken) == "" {
		return WBAccount{}, false
	}
	return l.wbAcct, true
}

// wbPersist writes the account back under the lock that guarantees one writer.
func (l *Lane) wbPersistLocked() {
	if l.wbPersist == nil {
		return
	}
	l.wbPersist(l.wbAcct)
}

// wbPost is the one post entry every WorkBuddy turn goes through: the probe,
// the daily activity claim, and user traffic. It refreshes a token that is
// close to expiry before spending it, and on a credential refusal refreshes
// once and retries — a rotated-but-unsaved pair would otherwise fail the turn
// even though a valid refresh token is sitting right there.
func (l *Lane) wbPost(ctx context.Context, body map[string]any, onData func(payload []byte) error) (*Usage, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return nil, &UpstreamError{Code: CodeCredential, Message: "WorkBuddy 账号未登录或已停用"}
	}
	if wbTokenNearExpiry(acct) {
		if rerr := l.refreshWorkBuddy(ctx); rerr == nil {
			if fresh, ok := l.workBuddyReady(); ok {
				acct = fresh
			}
		}
		// A failed proactive refresh is not fatal: the old token may still
		// answer, and the 401 path below gets one more chance.
	}
	usage, err := PostWorkBuddyStreamed(ctx, body, acct, onData)
	if ue := asUpstream(err); ue != nil && ue.Code == CodeCredential {
		if rerr := l.refreshWorkBuddy(ctx); rerr == nil {
			if fresh, ok := l.workBuddyReady(); ok {
				return PostWorkBuddyStreamed(ctx, body, fresh, onData)
			}
		}
	}
	return usage, err
}

// refreshWorkBuddy exchanges the refresh token for a fresh pair. Single-flight
// by construction: the whole exchange holds wbMu, and a latecomer re-checks
// freshness before doing anything, so N concurrent 401s produce one refresh.
func (l *Lane) refreshWorkBuddy(ctx context.Context) error {
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	acct := l.wbAcct
	if acct.RefreshToken == "" {
		return errors.New("WorkBuddy 没有 refresh token")
	}
	if !wbTokenNearExpiry(acct) && time.Since(l.wbRefreshedAt) < time.Minute {
		return nil // someone just refreshed; the pair is current
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, WorkBuddyBase+wbAuthRefreshPath, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", wbDesktopUA)
	req.Header.Set("x-codebuddy-request", "1")
	req.Header.Set("x-refresh-token", acct.RefreshToken)
	req.Header.Set("x-auth-refresh-source", "plugin")
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("WorkBuddy 刷新返回 HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload.Code != 0 || payload.Data.AccessToken == "" {
		return fmt.Errorf("WorkBuddy 刷新被拒（code %d）", payload.Code)
	}
	acct.AccessToken = payload.Data.AccessToken
	if payload.Data.RefreshToken != "" {
		// The pair rotates: keep the new refresh token even if the answer
		// omits it (the old one may already be consumed).
		acct.RefreshToken = payload.Data.RefreshToken
	}
	if _, iss, expMS, ok := wbJWTClaims(acct.AccessToken); ok {
		if expMS > 0 {
			acct.ExpiresAtMS = expMS
		}
		_ = iss
	}
	l.wbAcct = acct
	l.wbRefreshedAt = time.Now()
	l.wbPersistLocked()
	return nil
}

// WorkBuddyLoginStart opens a login: the upstream mints a state and an
// authUrl, and the user finishes the login in their browser. There is no
// callback — completion is observed by polling with the state.
func (l *Lane) WorkBuddyLoginStart() (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		WorkBuddyBase+wbAuthStatePath+"?platform=CLI", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", wbDesktopUA)
	req.Header.Set("x-codebuddy-request", "1")
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("登录状态请求返回 HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	if payload.Code != 0 || payload.Data.State == "" || payload.Data.AuthURL == "" {
		return "", fmt.Errorf("登录状态请求被拒（code %d）", payload.Code)
	}
	l.wbMu.Lock()
	l.wbLoginState = payload.Data.State
	l.wbLoginAt = time.Now()
	l.wbMu.Unlock()
	return payload.Data.AuthURL, nil
}

// WorkBuddyLoginPoll checks one round of the login: "pending" while the
// browser side is unfinished, "ok" once the token pair landed (already
// persisted and folded into the lane). Poll errors name what went wrong.
func (l *Lane) WorkBuddyLoginPoll() (string, error) {
	l.wbMu.Lock()
	state := l.wbLoginState
	startedAt := l.wbLoginAt
	l.wbMu.Unlock()
	if state == "" {
		return "", errors.New("没有进行中的登录")
	}
	if time.Since(startedAt) > wbLoginTTL {
		l.wbMu.Lock()
		l.wbLoginState = ""
		l.wbMu.Unlock()
		return "", errors.New("登录已超时，请重新开始")
	}
	httpStatus, body, err := wbGet(wbAuthTokenPath+"?state="+state, WBAccount{})
	if err != nil {
		return "", err
	}
	if httpStatus != 200 {
		return "", fmt.Errorf("登录轮询返回 HTTP %d", httpStatus)
	}
	var payload struct {
		Code    int             `json:"code"`
		Msg     string          `json:"msg"`
		Data    wbTokenResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.Code == wbLoginPending {
		return "pending", nil
	}
	if payload.Code != 0 || payload.Data.AccessToken == "" {
		return "", fmt.Errorf("登录未完成（code %d）%s", payload.Code, strings.TrimSpace(payload.Msg))
	}
	acct, err := wbAccountFromToken(payload.Data)
	if err != nil {
		return "", err
	}
	l.wbMu.Lock()
	l.wbAcct = acct
	l.wbLoginState = ""
	l.wbMu.Unlock()
	l.wbPersist(acct)
	go l.RefreshCatalog(context.Background())
	l.fireChange()
	return "ok", nil
}

type wbTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    any    `json:"expiresAt"`
}

// wbAccountFromToken validates the minted pair: the JWT must name the intl
// realm (this build only speaks workbuddy.ai) and carries uid/expiry itself.
func wbAccountFromToken(t wbTokenResponse) (WBAccount, error) {
	sub, iss, expMS, ok := wbJWTClaims(t.AccessToken)
	if !ok {
		return WBAccount{}, errors.New("返回的令牌无法解析")
	}
	if !strings.Contains(iss, "workbuddy.ai") {
		return WBAccount{}, errors.New("令牌不属于国际版（workbuddy.ai），本渠道暂只支持国际版")
	}
	if sub == "" {
		sub = "anonymous"
	}
	acct := WBAccount{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		UID:          sub,
		ExpiresAtMS:  expMS,
		Enabled:      true,
	}
	if acct.ExpiresAtMS == 0 {
		acct.ExpiresAtMS = wbExpToMS(t.ExpiresAt)
	}
	// Best-effort nickname; its absence costs a display name only.
	if _, body, err := wbGet(wbLoginAccountPath+"?state=", acct); err == nil {
		var named struct {
			Data struct {
				Nickname string `json:"nickname"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &named) == nil {
			acct.Nickname = strings.TrimSpace(named.Data.Nickname)
		}
	}
	return acct, nil
}

// wbGet performs one identity-bearing GET against the WorkBuddy root and
// returns (upstream code, body) without treating non-2xx as a Go error — the
// callers read the envelope's own codes.
func wbGet(path string, acct WBAccount) (int, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, WorkBuddyBase+path, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range workBuddyHeaders(acct, wbRequestID()) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// WorkBuddyDailyActivity claims the day's activity reward with one tiny
// conversation on the free deepseek model — the intl pool's daily task, the
// same move the desktop client's 每日活跃 makes. Runs at most once per local
// calendar day; a spent free window (6004) also closes the day, since retrying
// inside a spent window buys nothing.
func (l *Lane) WorkBuddyDailyActivity(ctx context.Context) (bool, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return false, nil
	}
	today := time.Now().Format("2006-01-02")
	if acct.LastActivityDate == today {
		return false, nil
	}
	body := map[string]any{
		"model":            "deepseek-v4.1-flash",
		"stream":           true,
		"stream_options":   map[string]any{"include_usage": true},
		"messages":         []map[string]any{{"role": RoleUser, "content": "Hi"}},
		"max_tokens":       10,
		"reasoning_effort": "none",
	}
	_, err := l.wbPost(ctx, body, func([]byte) error { return nil })
	if ue := asUpstream(err); err != nil && (ue == nil || ue.Code != CodeQuota) {
		// Transient trouble (transport, 5xx, refresh failure): leave the day
		// open so the next hourly tick retries.
		return true, err
	}
	l.wbMarkActivity(today)
	return true, err
}

func (l *Lane) wbMarkActivity(date string) {
	l.wbMu.Lock()
	l.wbAcct.LastActivityDate = date
	acct := l.wbAcct
	fn := l.wbPersist
	l.wbMu.Unlock()
	if fn != nil {
		fn(acct)
	}
}

// WorkBuddyCredits reads the billing resource summary for the status card.
// The response's numeric layout is not contractual, so the reader accepts any
// numeric field named like a balance anywhere under data and reports the
// first one it finds; an empty string means "nothing recognizable".
//
// It rides the same refresh chain wbPost keeps: a near-expiry token is renewed
// before the GET, and a 401 refreshes and retries once. Without it the card
// reported "积分读不出" on a token the chat path still used fine (issue #46) —
// the chat went through wbPost's refresh, this raw GET did not.
func (l *Lane) WorkBuddyCredits(ctx context.Context) (string, error) {
	acct, ok := l.workBuddyReady()
	if !ok {
		return "", errors.New("WorkBuddy 账号未登录")
	}
	if wbTokenNearExpiry(acct) {
		if rerr := l.refreshWorkBuddy(ctx); rerr == nil {
			if fresh, ok := l.workBuddyReady(); ok {
				acct = fresh
			}
		}
	}
	outerCode, body, err := wbGetCancellable(ctx, wbUserResourcePath, acct)
	if err != nil {
		return "", err
	}
	if outerCode == 401 {
		// A credential refusal: refresh once and retry — same one chance wbPost
		// gives a rotated-but-unsaved pair.
		if rerr := l.refreshWorkBuddy(ctx); rerr != nil {
			return "", fmt.Errorf("WorkBuddy 登录态失效（刷新失败：%v）", rerr)
		}
		fresh, ok := l.workBuddyReady()
		if !ok {
			return "", errors.New("WorkBuddy 账号未登录")
		}
		if outerCode, body, err = wbGetCancellable(ctx, wbUserResourcePath, fresh); err != nil {
			return "", err
		}
	}
	if outerCode != 200 {
		return "", fmt.Errorf("积分查询返回 HTTP %d", outerCode)
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return "", errors.New("积分响应无法解析")
	}
	data, _ := payload["data"].(map[string]any)
	if v := wbFindBalance(data, 3); v != "" {
		return v, nil
	}
	return "", nil
}

// wbGetCancellable runs one wbGet but honours ctx cancellation instead of the
// bare 15s client timeout, so a status-card refresh cannot outlive its request.
func wbGetCancellable(ctx context.Context, path string, acct WBAccount) (int, []byte, error) {
	done := make(chan struct{})
	var (
		code int
		body []byte
		gerr error
	)
	go func() {
		code, body, gerr = wbGet(path, acct)
		close(done)
	}()
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-done:
		return code, body, gerr
	}
}

func wbFindBalance(m map[string]any, depth int) string {
	if m == nil || depth <= 0 {
		return ""
	}
	for _, key := range []string{"credit", "credits", "balance", "remainCredit", "totalCredit", "point", "points"} {
		if f, ok := toNum(m[key]); ok && f >= 0 {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	for _, v := range m {
		if child, ok := v.(map[string]any); ok {
			if s := wbFindBalance(child, depth-1); s != "" {
				return s
			}
		}
	}
	return ""
}

// probeWorkBuddy re-tests one WorkBuddy model with a minimal chat: the free
// window's cheapest possible proof of life. The lane probe machinery calls
// this through probeModelFor — ProbeModel itself is hardwired to the Zen
// endpoint.
func (l *Lane) probeWorkBuddy(ctx context.Context, m ModelInfo) ProbeResult {
	res := ProbeResult{Model: m.ID, State: StateUnknown, At: time.Now().UnixMilli()}
	if _, ok := l.workBuddyReady(); !ok {
		res.State = StateUnavailable
		res.Detail = "WorkBuddy 未登录或已停用"
		return res
	}
	body := map[string]any{
		"model":      m.ID,
		"stream":     true,
		"messages":   []map[string]any{{"role": RoleUser, "content": "ping"}},
		"max_tokens": 10,
	}
	// Same per-family quirks a real turn applies: deepseek needs the thinking
	// switch plus its effort spelling, the hy family only accepts "high". A
	// probe body without them is rejected by the upstream for reasons that
	// look like an outage while live chat to the same model answers fine
	// (issue #46: chat worked, manual probe said "探测不到上游").
	wbApplyModelQuirks(body, m.ID, "")
	start := time.Now()
	firstAt := int64(0)
	_, err := l.wbPost(ctx, body, func([]byte) error {
		if firstAt == 0 {
			firstAt = atLeastMS(start)
			res.TTFTMs = firstAt
		}
		return nil
	})
	res.LatencyMs = atLeastMS(start)
	if err != nil {
		ue := asUpstream(err)
		switch ue.Code {
		case CodeQuota:
			res.State = StateThrottled
			res.Detail = "免费窗口已用满" + wbThrottleDetail(ue)
		case CodeCredential:
			res.State = StateUnavailable
			res.Detail = "登录态失效（刷新失败）"
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

// wbThrottleDetail appends the reset wall clock to a throttle verdict when the
// failure carried one.
func wbThrottleDetail(ue *UpstreamError) string {
	if ue == nil || ue.RetryAfter <= 0 {
		return ""
	}
	until := time.Now().Add(time.Duration(ue.RetryAfter) * time.Second)
	return "（" + until.Format("15:04") + " 重置）"
}
