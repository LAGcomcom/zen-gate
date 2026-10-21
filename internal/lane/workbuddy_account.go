package lane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The account side of the WorkBuddy 账号渠道: login, refresh, the daily
// activity claim, and the credits readout. The pool itself (membership,
// selection, cooldowns) lives in workbuddy_pool.go; this file is about one
// account's credential lifecycle. Token rotation is single-flight PER ACCOUNT
// (each slot holds its own lock) — upstream swaps the refresh token on every
// refresh, so two overlapping refreshes of one account void the second pair
// and log it out, while two different accounts must not wait on each other.

// SetWorkBuddyAccount installs a single account snapshot from config. A zero
// account clears the pool, which is what the old single-account call sites
// (logout, tests) mean. Catalog refresh is the caller's job.
func (l *Lane) SetWorkBuddyAccount(acct WBAccount) {
	if strings.TrimSpace(acct.AccessToken) == "" && strings.TrimSpace(acct.RefreshToken) == "" &&
		strings.TrimSpace(acct.UID) == "" {
		l.SetWorkBuddyAccounts(nil)
		return
	}
	l.SetWorkBuddyAccounts([]WBAccount{acct})
}

// SetWorkBuddyPersist wires the write-back path for rotated token pairs and
// activity dates. The callback must not call back into the account setters.
func (l *Lane) SetWorkBuddyPersist(fn func(WBAccount)) {
	l.wbMu.Lock()
	l.wbPersist = fn
	l.wbMu.Unlock()
}

// WorkBuddySnapshot returns a copy of the first account's live state — the
// single-account view the older callers and tests expect.
func (l *Lane) WorkBuddySnapshot() WBAccount {
	slots := l.wbSlotsSnapshot()
	if len(slots) == 0 {
		return WBAccount{}
	}
	return slots[0].snapshot()
}

// WorkBuddyLogout drops every account. The store copy is the caller's to
// clear; this only takes the live pool down so in-flight code stops using it.
func (l *Lane) WorkBuddyLogout() {
	l.wbMu.Lock()
	l.wbSlots = nil
	l.wbActiveID = ""
	l.wbLoginState = ""
	l.wbMu.Unlock()
	l.fireChange()
}

// wbPost is the one post entry every WorkBuddy turn goes through: the probe,
// the daily activity claim, and user traffic. It walks the account pool: a
// quota refusal on one account continues the same turn on the next, as long as
// nothing has been streamed to the client yet.
func (l *Lane) wbPost(ctx context.Context, body map[string]any, conversationID string, onData func(payload []byte) error) (*Usage, error) {
	model, _ := body["model"].(string)

	// Once a byte reaches the client the turn is committed: replaying it on
	// another account would duplicate the visible answer.
	emitted := false
	tracked := func(payload []byte) error {
		emitted = true
		return onData(payload)
	}

	if len(l.wbSlotsSnapshot()) == 0 {
		return nil, l.wbNoAccountError()
	}

	tried := map[string]bool{}
	var (
		sawQuota bool
		sawCred  bool
		lastErr  error
	)
	for i := 0; i < wbMaxAccountTries; i++ {
		slot := l.wbAcquire(model, tried)
		if slot == nil {
			break
		}
		usage, err := func() (*Usage, error) {
			defer l.wbRelease(slot)
			acct := slot.snapshot()
			if wbTokenNearExpiry(acct) {
				if rerr := l.refreshWBSlot(ctx, slot); rerr == nil {
					acct = slot.snapshot()
				}
				// A failed proactive refresh is not fatal: the old token may
				// still answer, and the 401 path below gets one more chance.
			}
			u, err := PostWorkBuddyStreamed(ctx, body, acct, conversationID, tracked)
			if ue := asUpstream(err); ue != nil && ue.Code == CodeCredential && !ue.Soft {
				// A rotated-but-unsaved pair answers 401 even though a valid
				// refresh token is sitting right there: refresh once, retry
				// once, on this same account.
				if rerr := l.refreshWBSlot(ctx, slot); rerr == nil {
					acct = slot.snapshot()
					u, err = PostWorkBuddyStreamed(ctx, body, acct, conversationID, tracked)
				}
			}
			if err == nil {
				l.wbMarkOK(slot)
				l.wbSetActive(acct.ID)
			}
			return u, err
		}()
		tried[slot.acct.ID] = true
		if err == nil {
			return usage, nil
		}
		lastErr = err
		ue := asUpstream(err)
		if ue == nil {
			return usage, err
		}
		switch ue.Code {
		case CodeQuota:
			sawQuota = true
			// The classifier says how far the refusal reaches; without a scope
			// the safe reading is "this model only", which never over-benches.
			if ue.Scope == ScopeAccount {
				l.wbCoolAccount(slot, ue.RetryAfter, ue.Message)
			} else {
				l.wbCoolModel(slot, model, ue.RetryAfter, ue.Message)
			}
			l.logf("WorkBuddy 账号 %s 限流（%s，%ds）：%s",
				acctLabel(slot.snapshot()), model, ue.RetryAfter, truncate(ue.Message, 120))
		case CodeCredential:
			sawCred = true
			l.wbMarkAuth(slot, ue.Soft, ue.Message)
			l.logf("WorkBuddy 账号 %s 登录态问题：%s",
				acctLabel(slot.snapshot()), truncate(ue.Message, 120))
		case CodeServer, CodeTransport, CodeTimeout, CodeReset:
			// Not the account's fault in a way another account could fix, but
			// worth counting so a genuinely broken one backs off.
			l.wbMarkServer(slot, ue.Message)
			return usage, err
		default:
			// The request's own fault (malformed body, WAF verdict, content
			// block): every account answers identically.
			return usage, err
		}
		if emitted {
			return usage, err
		}
		next := l.wbAcquire(model, tried)
		if next != nil {
			l.wbRelease(next) // peeking only; wbAcquire at the loop head re-takes it
			l.logf("WorkBuddy 切换账号：%s → %s", acctLabel(slot.snapshot()), acctLabel(next.snapshot()))
		}
	}

	if sawQuota {
		// Prefer the upstream's own refusal: it carries the reset wall clock and
		// the caller's classification (the daily-activity path tells a spent
		// window from transient trouble by exactly that code). The synthesized
		// "everyone is busy" answer is the fallback for the case where no
		// account was even reachable.
		if lastErr != nil {
			if ue := asUpstream(lastErr); ue != nil && ue.Code == CodeQuota {
				return nil, lastErr
			}
		}
		return nil, l.wbAllBusyError(model)
	}
	if sawCred {
		msg := "所有 WorkBuddy 账号登录态失效，请到设置页重新登录"
		if d := l.wbAccountDigest(); d != "" {
			msg += "：" + d
		}
		return nil, &UpstreamError{Code: CodeCredential, Message: msg}
	}
	if lastErr == nil {
		if l.wbHasToken() {
			return nil, l.wbAllBusyError(model)
		}
		return nil, l.wbNoAccountError()
	}
	return nil, lastErr
}

// refreshWBSlot exchanges one account's refresh token for a fresh pair.
// Single-flight per account: the whole exchange holds that slot's lock, and a
// latecomer re-checks freshness before doing anything, so N concurrent 401s on
// one account produce one refresh — while other accounts stay unblocked.
func (l *Lane) refreshWBSlot(ctx context.Context, s *wbSlot) error {
	if s == nil {
		return errors.New("WorkBuddy 账号不存在")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := s.acct
	if acct.RefreshToken == "" {
		return errors.New("WorkBuddy 没有 refresh token")
	}
	if !wbTokenNearExpiry(acct) && time.Since(s.refreshedAt) < time.Minute {
		return nil // someone just refreshed; the pair is current
	}
	rc := wbRealmConfig(wbRealmOf(acct))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rc.ChatBase+wbAuthRefreshPath, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", rc.DesktopUA)
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
	if _, _, expMS, ok := wbJWTClaims(acct.AccessToken); ok && expMS > 0 {
		acct.ExpiresAtMS = expMS
	}
	s.acct = acct
	s.refreshedAt = time.Now()
	s.authBad = false
	s.authFails = 0
	persist := l.wbPersist
	go func() {
		if persist != nil {
			persist(acct)
		}
	}()
	return nil
}

// WorkBuddyLoginStart opens a login: the upstream mints a state and an
// authUrl, and the user finishes the login in their browser. There is no
// callback — completion is observed by polling with the state.
//
// realm picks the publishment: "cn" for codebuddy (copilot.tencent.com),
// anything else for the intl workbuddy.ai flow. The choice is remembered for
// the poll, because the two publishments do not share a login state.
func (l *Lane) WorkBuddyLoginStart(realm string) (string, error) {
	rc := wbRealmConfig(realm)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		rc.ChatBase+wbAuthStatePath+"?platform=CLI", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", rc.DesktopUA)
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
	l.wbLoginRealm = realm
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
	realm := l.wbLoginRealm
	startedAt := l.wbLoginAt
	l.wbMu.Unlock()
	if state == "" {
		return "", errors.New("没有进行中的登录")
	}
	if time.Since(startedAt) > wbLoginTTL {
		l.wbMu.Lock()
		l.wbLoginState = ""
		l.wbLoginRealm = ""
		l.wbMu.Unlock()
		return "", errors.New("登录已超时，请重新开始")
	}
	httpStatus, body, err := wbGet(wbAuthTokenPath+"?state="+state, WBAccount{Realm: realm})
	if err != nil {
		return "", err
	}
	if httpStatus != 200 {
		return "", fmt.Errorf("登录轮询返回 HTTP %d", httpStatus)
	}
	var payload struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data wbTokenResponse `json:"data"`
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
	acct, err := wbAccountFromToken(payload.Data, realm)
	if err != nil {
		return "", err
	}
	acct.ID = wbStableID(acct.UID)
	l.wbUpsertSlot(acct)
	l.wbMu.Lock()
	l.wbLoginState = ""
	l.wbLoginRealm = ""
	l.wbMu.Unlock()
	l.wbPersistAcct(acct)
	go l.RefreshCatalog(context.Background())
	l.fireChange()
	// A fresh account must collect today's reward now, not on the next hourly
	// tick: the user just logged in to spend it.
	go func() {
		ctx := context.Background()
		if acct.Realm == WBRealmCN {
			_, _ = l.WorkBuddyDailyCheckin(ctx)
		} else {
			_, _ = l.WorkBuddyDailyActivity(ctx)
		}
	}()
	return "ok", nil
}

type wbTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    any    `json:"expiresAt"`
}

// wbRealmFromIss reads the publishment out of a token's issuer. Empty when
// the issuer names neither realm — plenty of mints carry a bare or internal
// issuer, and in that case the caller's own knowledge (which login did we
// open?) is the better witness.
func wbRealmFromIss(iss string) string {
	i := strings.ToLower(iss)
	switch {
	case strings.Contains(i, "workbuddy.ai"):
		return WBRealmIntl
	case strings.Contains(i, "codebuddy"), strings.Contains(i, "copilot.tencent.com"), strings.Contains(i, "tencent"):
		return WBRealmCN
	}
	return ""
}

// wbAccountFromToken validates the minted pair and stamps the account with
// the realm it belongs to. hint is the region whose login produced it; an
// issuer that names a realm outranks the hint, and a silent issuer defers to
// it. The intl path keeps its assertion — a token that is neither clearly CN
// nor issued by workbuddy.ai is not an intl account.
func wbAccountFromToken(t wbTokenResponse, hint string) (WBAccount, error) {
	sub, iss, expMS, ok := wbJWTClaims(t.AccessToken)
	if !ok {
		return WBAccount{}, errors.New("返回的令牌无法解析")
	}
	realm := hint
	if ev := wbRealmFromIss(iss); ev != "" {
		realm = ev
	}
	if realm != WBRealmCN && !strings.Contains(iss, "workbuddy.ai") {
		return WBAccount{}, errors.New("令牌不属于国际版（workbuddy.ai）；国内版请在登录时选择「国内版」")
	}
	if realm == "" {
		realm = WBRealmIntl
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
		Realm:        realm,
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

// wbGet performs one identity-bearing GET against the account's publishment
// root and returns (upstream code, body) without treating non-2xx as a Go
// error — the callers read the envelope's own codes. The billing meter is not
// in this family: it answers GET with a bare "404 page not found" (measured
// 2026-10-21, both publishments, both hosts) because those routes are
// POST-only, so every meter read goes through wbBillingPost.
func wbGet(path string, acct WBAccount) (int, []byte, error) {
	return wbGetOn(wbRealmConfig(wbRealmOf(acct)).ChatBase, path, acct)
}

func wbGetOn(base, path string, acct WBAccount) (int, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, nil)
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
// same move the desktop client's 每日活跃 makes. Runs at most once per account
// per local calendar day; a spent free window (6004) also closes that
// account's day, since retrying inside a spent window buys nothing.
//
// Every account is covered, including the disabled ones: 停用 only means "stop
// serving turns". Skipping them here would cut the one automatic way a benched
// account ever recovers.
func (l *Lane) WorkBuddyDailyActivity(ctx context.Context) (bool, error) {
	today := time.Now().Format("2006-01-02")
	ran := false
	processed := 0
	var firstErr error
	for _, slot := range l.wbOrderedSlots() {
		if processed >= wbDailyMaxPerRun {
			break
		}
		acct := slot.snapshot()
		if wbRealmOf(acct) != WBRealmIntl || strings.TrimSpace(acct.AccessToken) == "" {
			continue
		}
		if acct.LastActivityDate == today {
			continue
		}
		ran = true
		processed++
		body := map[string]any{
			"model":      "deepseek-v4.1-flash",
			"stream":     true,
			"messages":   []map[string]any{{"role": RoleUser, "content": "Hi"}},
			"max_tokens": 10,
		}
		// Ride the same envelope and model quirks as a chat turn (issue #85): a
		// bare conversation id of "" and a hand-written reasoning_effort are
		// both shapes the gate never sees from real traffic, and every hourly
		// retry answered CLIENT_ERROR because of it. "light" keeps the forced
		// deepseek thinking at its cheapest rung.
		wbApplyModelQuirks(body, "deepseek-v4.1-flash", "light")
		_, err := l.wbPost(ctx, body, wbConversationID("wb-daily-activity"), func([]byte) error { return nil })
		if ue := asUpstream(err); err != nil && (ue == nil || ue.Code != CodeQuota) {
			// Transient trouble (transport, 5xx, refresh failure): leave this
			// account's day open so the next hourly tick retries it.
			if firstErr == nil {
				firstErr = err
			}
			time.Sleep(wbDailyGap)
			continue
		}
		// A spent free window (6004) closes the day — retrying inside a spent
		// window buys nothing — but the caller still hears about it.
		l.wbMarkActivity(slot, today)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		time.Sleep(wbDailyGap)
	}
	return ran, firstErr
}

// wbMarkActivity stamps one account's activity day and persists it.
func (l *Lane) wbMarkActivity(s *wbSlot, date string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.acct.LastActivityDate = date
	acct := s.acct
	s.mu.Unlock()
	l.wbPersistAcct(acct)
}

// WorkBuddyDailyCheckin claims the CN publishment's daily reward on its own
// endpoint, once per account per day. intl has no such endpoint — it earns its
// daily by conversing (WorkBuddyDailyActivity) — so a CN account left on the
// activity chat would collect nothing all week while still looking busy.
//
// The meter answers code 0 on a fresh claim and 10001 (or any 已签到 wording,
// sometimes behind an HTTP 400) once the day is claimed. Both mean the day is
// finished, so both mark it; only a genuinely different code leaves the day
// open for the next tick.
//
// An enterprise seat answers 10001「企业账号不支持该操作」: it has no personal
// reward system at all, so that account is flagged and never asked again
// instead of failing the pass every hour.
func (l *Lane) WorkBuddyDailyCheckin(ctx context.Context) (bool, error) {
	today := time.Now().Format("2006-01-02")
	ran := false
	processed := 0
	var firstErr error
	for _, slot := range l.wbOrderedSlots() {
		if processed >= wbDailyMaxPerRun {
			break
		}
		slot.mu.Lock()
		acct := slot.acct
		skip := slot.noCheckin
		slot.mu.Unlock()
		if skip || wbRealmOf(acct) != WBRealmCN || strings.TrimSpace(acct.AccessToken) == "" {
			continue
		}
		if acct.LastCheckinDate == today {
			continue
		}
		ran = true
		processed++
		if wbTokenNearExpiry(acct) {
			if rerr := l.refreshWBSlot(ctx, slot); rerr == nil {
				acct = slot.snapshot()
			}
		}
		status, body, err := wbBillingPost(ctx, wbCheckinPath, acct, "{}")
		if err != nil {
			// Transport trouble only; this account's day stays open.
			if firstErr == nil {
				firstErr = err
			}
			time.Sleep(wbDailyGap)
			continue
		}
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(body, &resp)
		if wbCheckinIsEnterprise(resp.Code, resp.Msg) {
			l.WorkBuddyMarkNoCheckin(acct.ID, true)
			l.logf("WorkBuddy 账号 %s 是企业账号，没有每日签到，已跳过", acctLabel(acct))
			time.Sleep(wbDailyGap)
			continue
		}
		if resp.Code != 0 && resp.Code != wbCheckinAlready && !strings.Contains(resp.Msg, "已签到") {
			if firstErr == nil {
				firstErr = fmt.Errorf("国内版签到未成功（HTTP %d，code %d）%s",
					status, resp.Code, strings.TrimSpace(resp.Msg))
			}
			time.Sleep(wbDailyGap)
			continue
		}
		l.wbMarkCheckin(slot, today)
		// A balance that just came back is the one upstream fact that can end
		// a 402 park early.
		l.wbThaw(slot)
		time.Sleep(wbDailyGap)
	}
	return ran, firstErr
}

// wbCheckinIsEnterprise spots the meter's "this seat has no personal rewards"
// answer. It arrives as a 400 with code 10001, the same code the meter uses for
// "already claimed today", so the wording is what separates them.
func wbCheckinIsEnterprise(code int, msg string) bool {
	if code != wbCheckinAlready {
		return false
	}
	if strings.Contains(msg, "已签到") {
		return false
	}
	return strings.Contains(msg, "企业") || strings.Contains(msg, "enterprise")
}

// wbMarkCheckin stamps one account's checkin day and persists it.
func (l *Lane) wbMarkCheckin(s *wbSlot, date string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.acct.LastCheckinDate = date
	acct := s.acct
	s.mu.Unlock()
	l.wbPersistAcct(acct)
}

// wbBillingPost is the POST twin of wbBillingGet: same meter host, same
// desktop identity headers the chat path presents.
func wbBillingPost(ctx context.Context, path string, acct WBAccount, payload string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		wbRealmConfig(wbRealmOf(acct)).BillingBase+path, strings.NewReader(payload))
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

// WorkBuddyCredits reads the billing resource summary for the status card,
// using the account currently serving. See WorkBuddyCreditsFor for the pool.
func (l *Lane) WorkBuddyCredits(ctx context.Context) (WBCredits, error) {
	for _, slot := range l.wbOrderedSlots() {
		acct := slot.snapshot()
		if strings.TrimSpace(acct.AccessToken) == "" {
			continue
		}
		if v, err := l.creditsForSlot(ctx, slot); err == nil {
			return v, nil
		}
	}
	return WBCredits{}, errors.New("没有可读积分的 WorkBuddy 账号")
}

// WorkBuddyCreditsFor reads one account's balance by id.
func (l *Lane) WorkBuddyCreditsFor(ctx context.Context, id string) (WBCredits, error) {
	slot := l.wbSlotByID(id)
	if slot == nil {
		return WBCredits{}, errors.New("WorkBuddy 账号不存在")
	}
	return l.creditsForSlot(ctx, slot)
}

// creditsForSlot reads one account's remaining credit from the billing meter.
//
// Two facts about that endpoint, both measured against the live meter
// (2026-10-21, both publishments, both candidate hosts), and both contradicting
// what this file used to assume:
//
//   - It is POST-only. A GET on the same URL answers a bare
//     "404 page not found" from the router, which is why the card said
//     积分查询返回 HTTP 404 on accounts whose chat path worked fine. The host
//     was never the problem: copilot.tencent.com, www.codebuddy.cn and
//     www.workbuddy.ai all answer the POST.
//   - The balance is not a field. It is the sum of the account's still-spendable
//     resource packs under data.Response.Data.Accounts[], each with an integer
//     value and a *Precise string twin (CycleCapacityRemain 249 /
//     "249.04"). Reporting any one field shows one pack, not the account.
//
// It rides the same refresh chain wbPost keeps: a near-expiry token is renewed
// before the request, and a 401 refreshes and retries once. Without it the card
// reported "积分读不出" on a token the chat path still used fine (issue #46).
func (l *Lane) creditsForSlot(ctx context.Context, slot *wbSlot) (WBCredits, error) {
	acct := slot.snapshot()
	if strings.TrimSpace(acct.AccessToken) == "" {
		return WBCredits{}, errors.New("WorkBuddy 账号未登录")
	}
	if wbTokenNearExpiry(acct) {
		if rerr := l.refreshWBSlot(ctx, slot); rerr == nil {
			acct = slot.snapshot()
		}
	}
	code, body, err := wbBillingPost(ctx, wbUserResourcePath, acct, "{}")
	if err != nil {
		return WBCredits{}, err
	}
	if code == 401 {
		// A credential refusal: refresh once and retry — same one chance wbPost
		// gives a rotated-but-unsaved pair.
		if rerr := l.refreshWBSlot(ctx, slot); rerr != nil {
			return WBCredits{}, fmt.Errorf("WorkBuddy 登录态失效（刷新失败：%v）", rerr)
		}
		acct = slot.snapshot()
		if code, body, err = wbBillingPost(ctx, wbUserResourcePath, acct, "{}"); err != nil {
			return WBCredits{}, err
		}
	}
	if code != 200 {
		return WBCredits{}, fmt.Errorf("积分查询返回 HTTP %d", code)
	}
	return wbMeterBalance(body, time.Now())
}

// wbMeterPack is one resource pack as the meter publishes it. Only the fields
// this reader needs are decoded; the envelope carries ~50 more per pack.
type wbMeterPack struct {
	PackageName                string  `json:"PackageName"`
	SubProductName             string  `json:"SubProductName"`
	PackageCode                string  `json:"PackageCode"`
	CapacityUnit               string  `json:"CapacityUnit"`
	Status                     int     `json:"Status"`
	ExpiredTime                string  `json:"ExpiredTime"`
	DeductionEndTime           int64   `json:"DeductionEndTime"`
	CycleCapacityRemain        float64 `json:"CycleCapacityRemain"`
	CycleCapacityRemainPrecise string  `json:"CycleCapacityRemainPrecise"`
}

// wbMeterPackExpired is the Status value the meter puts on a dead pack. It
// still ships the pack with its numbers filled in, so a sum that ignores this
// counts credit the account cannot spend.
const wbMeterPackExpired = 3

// remaining prefers the precise string the desktop client prints: the integer
// field is truncated (249 vs "249.04"), and rounding a balance down by a
// fraction is the difference between "still usable" and "spent".
func (p wbMeterPack) remaining() float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(p.CycleCapacityRemainPrecise), 64); err == nil {
		return f
	}
	return p.CycleCapacityRemain
}

// spendable reports whether the account can still be charged against this pack.
// Three independent signals say a pack is dead — Status, an ExpiredTime already
// in the past, and a DeductionEndTime (the cut-off for charging) already in the
// past — and the meter can send any one of them, so all three are checked. An
// unparsable date is read as live: hiding spendable credit is worse than
// showing a stale pack.
func (p wbMeterPack) spendable(now time.Time) bool {
	if p.Status == wbMeterPackExpired {
		return false
	}
	if s := strings.TrimSpace(p.ExpiredTime); s != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, now.Location()); err == nil && !t.After(now) {
			return false
		}
	}
	if p.DeductionEndTime > 0 && !time.UnixMilli(p.DeductionEndTime).After(now) {
		return false
	}
	return true
}

// diesWithin says the pack stops being chargeable within window of now. A pack
// whose cut-off the meter never published is read as living longer: calling
// unknown permanent is the conservative half of the split, since a wrong
// "expiring" tag would spend long-lived credit first for no reason.
func (p wbMeterPack) diesWithin(now time.Time, window time.Duration) bool {
	if p.DeductionEndTime <= 0 {
		return false
	}
	return time.UnixMilli(p.DeductionEndTime).Before(now.Add(window))
}

// isCredit says whether a pack is metered in credit at all. The unit string
// arrives as both "credit" and "credits" for the same thing; a pack metered in
// tokens is a different currency and summing it into 积分 would print a number
// the account cannot spend as credit.
func (p wbMeterPack) isCredit() bool {
	switch strings.ToLower(strings.TrimSpace(p.CapacityUnit)) {
	case "", "credit", "credits", "积分":
		return true
	default:
		return false
	}
}

// WBCredits is one account's spendable credit, split by when it dies.
//
// The split is not decoration: credit that is about to expire is only spendable
// until it is not, and one blended total hides which half evaporates first.
// Expiring means the pack's charging cut-off (DeductionEndTime) falls inside
// WBExpiringWindowDays; every other pack — including one whose cut-off the
// meter never published — lands in Permanent, which is the conservative
// direction (see the reference client's same rule).
type WBCredits struct {
	Total     float64 `json:"total"`
	Expiring  float64 `json:"expiring"`
	Permanent float64 `json:"permanent"`
	// Dead is the balance still attached to packs the meter has already closed.
	// It is reported apart and never folded into Total: the meter keeps shipping
	// those numbers, and summing them inflated a real account from 155.67 to
	// 655.67 in the community's measurement.
	Packs int     `json:"packs"`
	Dead  float64 `json:"dead"`
}

// String is the display total: two decimals, no trailing zeros. Summing the
// meter's float values surfaces their tail noise (55.67000031 + … =
// 668.0400002), and the desktop client itself only ever shows cents.
func (c WBCredits) String() string { return wbFormatCredits(c.Total) }

func wbFormatCredits(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// wbExpiringWindowDays is the horizon under which credit counts as dying soon.
// It leaves the package as a function because the dashboard prints the number
// in a label: a hard-coded label beside a rule that later drifts tells the user
// a deadline we never applied.
const wbExpiringWindowDays = 15

// WBExpiringWindowDays publishes the window the split was made with.
func WBExpiringWindowDays() int { return wbExpiringWindowDays }

// wbMeterBalance turns one get-user-resource envelope into the account's
// credit, bucketed by expiry. now is a parameter so the rules are testable
// without waiting on a calendar.
//
// An error means "we cannot tell", which the card must not render as 0; a clean
// envelope with nothing spendable is the real answer 0.
func wbMeterBalance(body []byte, now time.Time) (WBCredits, error) {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []wbMeterPack `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	var c WBCredits
	if json.Unmarshal(body, &env) != nil {
		return c, errors.New("积分响应无法解析")
	}
	if env.Code != 0 {
		return c, fmt.Errorf("积分查询未成功（code %d）%s", env.Code, strings.TrimSpace(env.Msg))
	}
	if len(env.Data.Response.Data.Accounts) == 0 {
		return c, errors.New("积分响应没有资源包列表")
	}
	window := time.Duration(wbExpiringWindowDays) * 24 * time.Hour
	for _, p := range env.Data.Response.Data.Accounts {
		if !p.isCredit() {
			continue
		}
		remain := p.remaining()
		if !p.spendable(now) {
			c.Dead += remain
			continue
		}
		if remain <= 0 {
			// A closed-out pack is not a credit source; counting it as a pack
			// would tell the user two pools are live when one is empty.
			continue
		}
		c.Packs++
		c.Total += remain
		if p.diesWithin(now, window) {
			c.Expiring += remain
		} else {
			c.Permanent += remain
		}
	}
	c.Dead = round2(c.Dead)
	c.Expiring = round2(c.Expiring)
	c.Permanent = round2(c.Permanent)
	c.Total = round2(c.Total)
	return c, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// probeWorkBuddy re-tests one WorkBuddy model with a minimal chat: the free
// window's cheapest possible proof of life. The lane probe machinery calls
// this through probeModelFor — ProbeModel itself is hardwired to the Zen
// endpoint.
func (l *Lane) probeWorkBuddy(ctx context.Context, m ModelInfo) ProbeResult {
	res := ProbeResult{Model: m.ID, State: StateUnknown, At: time.Now().UnixMilli()}
	if !l.wbServeable() {
		res.State = StateUnavailable
		res.Detail = "WorkBuddy 未登录或全部账号已停用"
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
	_, err := l.wbPost(ctx, body, "", func([]byte) error {
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
