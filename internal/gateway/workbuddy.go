package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/tray"
)

// The WorkBuddy 账号渠道's dashboard surface: a status readout for the
// settings card plus the login-call and pool-management endpoints. The heavy
// lifting — OAuth state polling, token rotation, account selection, cooldowns —
// lives in the lane; these handlers only compose what the card shows and
// trigger the catalog resync.

// wbCreditCache memoizes one account's balance readout. The billing endpoint
// rides the upstream, so page refreshes share one answer per account per
// minute instead of firing a request per row per repaint.
//
// gen is the pool generation the entry belongs to: a read that started before
// the pool changed must not land afterwards and paint a stale number over the
// new state (a deleted account's balance appearing under a fresh row was a
// real bug class in this shape of card).
type wbCreditCache struct {
	gen    int64
	at     time.Time
	value  lane.WBCredits
	ok     bool
	detail string
}

var (
	wbCreditsMu    sync.Mutex
	wbCreditsCache = map[string]wbCreditCache{}
	wbPoolGen      int64
)

// wbBumpPoolGen invalidates every cached balance: the pool changed, so no
// in-flight read is about the pool we now have.
func wbBumpPoolGen() {
	wbCreditsMu.Lock()
	wbPoolGen++
	wbCreditsCache = map[string]wbCreditCache{}
	wbCreditsMu.Unlock()
}

// wbCreditsFor reads one account's cached balance. force skips the cache: the
// card's 刷新余额 button must actually retry a failed read, which a cached
// failure would otherwise hide for a whole minute.
//
// The three outcomes are kept apart on purpose — a zero balance, a balance we
// could not read, and "read fine but the response carried no balance field"
// are different facts, and rendering any of them as 0 would be a lie.
func (s *Server) wbCreditsFor(ctx context.Context, id string, force bool) (lane.WBCredits, bool, string) {
	wbCreditsMu.Lock()
	gen := wbPoolGen
	if !force {
		if c, ok := wbCreditsCache[id]; ok && c.gen == gen && time.Since(c.at) < time.Minute {
			wbCreditsMu.Unlock()
			return c.value, c.ok, c.detail
		}
	}
	wbCreditsMu.Unlock()

	value, ok, detail := lane.WBCredits{}, false, ""
	if v, err := s.Lane.WorkBuddyCreditsFor(ctx, id); err == nil {
		value, ok = v, true
	} else {
		detail = err.Error()
	}
	wbCreditsMu.Lock()
	if wbPoolGen == gen {
		wbCreditsCache[id] = wbCreditCache{gen: gen, at: time.Now(), value: value, ok: ok, detail: detail}
	}
	wbCreditsMu.Unlock()
	return value, ok, detail
}

// wbCreditsJSON paints one balance read the way the card reads it: the total
// for the metric row, the two expiry buckets under it, and the window the
// buckets were cut with — so the label cannot drift from the rule that produced
// the numbers. A failed read carries nothing: the card shows — and the detail.
func wbCreditsJSON(credits lane.WBCredits, ok bool, detail string) map[string]any {
	if !ok {
		return map[string]any{"credits": "", "creditsKnown": false, "detail": detail}
	}
	return map[string]any{
		"credits":          credits.String(),
		"creditsKnown":     true,
		"detail":           "",
		"creditsExpiring":  credits.Expiring,
		"creditsPermanent": credits.Permanent,
		"creditsDead":      credits.Dead,
		"creditsPacks":     credits.Packs,
		"creditsWindow":    lane.WBExpiringWindowDays(),
	}
}

// wbDropCredits forgets one account's cached balance, so a toggle or a fresh
// login cannot show the previous account's number.
func wbDropCredits(id string) {
	wbCreditsMu.Lock()
	delete(wbCreditsCache, id)
	wbCreditsMu.Unlock()
}

// adminWorkBuddyStatus answers the settings card: one row per account with its
// durable state plus just enough runtime state to explain why it is (not)
// serving. The top-level fields describe the account currently serving, so an
// older card that only knows the single-account shape still renders.
//
// ?force=1 re-reads every balance: the card's 刷新余额 button must be able to
// retry a failed read instead of showing the cached failure for a minute.
func (s *Server) adminWorkBuddyStatus(w http.ResponseWriter, r *http.Request) {
	views := s.Lane.WorkBuddyViews()
	active := s.Lane.WorkBuddyActiveID()
	force := r.URL.Query().Get("force") == "1"
	if active == "" && len(views) > 0 {
		// Nothing has served yet: name the account that WILL serve first, so
		// the card can mark a row instead of showing an empty selection.
		active = views[0].ID
	}

	accounts := make([]map[string]any, 0, len(views))
	for _, v := range views {
		credits, ok, detail := s.wbCreditsFor(r.Context(), v.ID, force)
		row := map[string]any{
			"id":               v.ID,
			"uid":              v.UID,
			"nickname":         v.Nickname,
			"realm":            v.Realm,
			"enabled":          v.Enabled,
			"expiresAtMs":      v.ExpiresAtMS,
			"lastActivityDate": v.LastActivityDate,
			"lastCheckinDate":  v.LastCheckinDate,
			"canCheckin":       v.Realm == lane.WBRealmCN && v.AccessToken != "" && !v.NoCheckin,
			"state":            v.State,
			"cooldownUntil":    v.CooldownUntil,
			"cooldownModel":    v.CooldownModel,
			"inFlight":         v.InFlight,
			"lastError":        v.LastError,
			"noCheckin":        v.NoCheckin,
		}
		for k, val := range wbCreditsJSON(credits, ok, detail) {
			row[k] = val
		}
		accounts = append(accounts, row)
	}
	// A partial failure is its own fact: some rows read, some did not. The card
	// says so instead of leaving the user to notice one odd row.
	failedCredits := 0
	for _, a := range accounts {
		if a["creditsKnown"] != true {
			failedCredits++
		}
	}

	// The legacy top-level block mirrors the serving account (or the first one
	// when nothing has served yet).
	head := map[string]any{
		"loggedIn": false, "enabled": false, "canCheckin": false,
		"credits": "", "detail": "",
	}
	if len(views) > 0 {
		pick := views[0]
		for _, v := range views {
			if v.ID == active {
				pick = v
				break
			}
		}
		credits, ok, detail := s.wbCreditsFor(r.Context(), pick.ID, force)
		head = map[string]any{
			"loggedIn":         pick.AccessToken != "",
			"enabled":          pick.Enabled,
			"uid":              pick.UID,
			"nickname":         pick.Nickname,
			"expiresAtMs":      pick.ExpiresAtMS,
			"lastActivityDate": pick.LastActivityDate,
			"lastCheckinDate":  pick.LastCheckinDate,
			"realm":            pick.Realm,
			"canCheckin":       pick.Realm == lane.WBRealmCN && pick.AccessToken != "" && !pick.NoCheckin,
		}
		for k, val := range wbCreditsJSON(credits, ok, detail) {
			head[k] = val
		}
	}
	head["activeId"] = active
	head["accounts"] = accounts
	head["creditsFailed"] = failedCredits
	writeJSON(w, 200, head)
}

// adminWorkBuddyCheckin runs the CN daily checkin on demand. The scheduler
// claims it once a day per account on its own; this is the "did it actually
// land?" button.
func (s *Server) adminWorkBuddyCheckin(w http.ResponseWriter) {
	ran, err := s.Lane.WorkBuddyDailyCheckin(context.Background())
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !ran {
		// Either no CN account, or every one of them already claimed today.
		writeJSON(w, 200, map[string]any{"ok": true, "skipped": true})
		return
	}
	wbBumpPoolGen()
	if s.logger != nil {
		s.logger.Infof("WorkBuddy 国内版每日签到已完成")
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminWorkBuddyLoginStart mints a login state and hands the auth URL back;
// the card opens it in a new tab and starts polling. ?realm=cn opens the
// login against the CN publishment instead — the two do not share a login
// state, so the choice has to be made before the browser is sent anywhere.
// the card posts it (api() sends a JSON body); both accepted so the endpoint
// does not care which way the caller talks.
func (s *Server) adminWorkBuddyLoginStart(w http.ResponseWriter, r *http.Request) {
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	if realm == "" {
		var in struct {
			Realm string `json:"realm"`
		}
		if data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
			_ = json.Unmarshal(data, &in)
		}
		realm = strings.TrimSpace(in.Realm)
	}
	if realm != lane.WBRealmCN {
		realm = lane.WBRealmIntl
	}
	authURL, err := s.Lane.WorkBuddyLoginStart(realm)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Open the auth page in the user's DEFAULT browser, not a WebView2 popup:
	// the embedded window's profile is separate from their daily browser, so
	// a popup would ask for a login the browser session already has. Same
	// mechanism as the tray's 打开管理页.
	tray.Open(authURL)
	if s.logger != nil {
		if realm == lane.WBRealmCN {
			s.logger.Infof("WorkBuddy 国内版登录已开始，授权页已在默认浏览器打开")
		} else {
			s.logger.Infof("WorkBuddy 登录已开始，授权页已在默认浏览器打开")
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "authUrl": authURL})
}

// adminWorkBuddyLoginPoll advances the login once. "ok" means the account is
// already merged into the pool and persisted (through the lane's persist
// callback) and the roster is rebuilding; the handler only re-syncs the agent
// injections.
func (s *Server) adminWorkBuddyLoginPoll(w http.ResponseWriter) {
	state, err := s.Lane.WorkBuddyLoginPoll()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error(), "state": "error"})
		return
	}
	if state == "ok" {
		wbBumpPoolGen()
		accts := s.Lane.WorkBuddyAccounts()
		label := "未命名账号"
		if len(accts) > 0 {
			label = wbAccountLabel(accts[len(accts)-1])
		}
		s.logCat(logx.CatAdmin, "info", "WorkBuddy 账号已加入账号池：%s（当前共 %d 个账号）", label, len(accts))
		if s.registry != nil {
			time.AfterFunc(1500*time.Millisecond, func() {
				if s.modelsSync != nil {
					s.modelsSync()
				}
				if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
					s.logger.Infof("WorkBuddy 渠道上线，已重新注入 %d 个已开启 Agent 的配置", n)
				}
			})
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": state})
}

// adminWorkBuddyLogout drops one account, or every account when the body
// carries no id (which is what the pre-pool card sent).
func (s *Server) adminWorkBuddyLogout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if r != nil {
		if data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
			_ = json.Unmarshal(data, &in)
		}
	}
	id := strings.TrimSpace(in.ID)

	s.Store.RemoveWorkBuddyAccount(id)
	_ = s.Store.Save()
	s.Lane.WorkBuddyRemove(id)
	wbDropCredits(id)
	go s.Lane.RefreshCatalog(context.Background())
	if s.logger != nil {
		if id == "" {
			s.logger.Infof("WorkBuddy 账号已全部退出")
		} else {
			s.logger.Infof("WorkBuddy 账号已退出（id=%s）", id)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminWorkBuddyAccountToggle flips one account's participation. The store is
// the authority, so it is written first and the lane follows.
func (s *Server) adminWorkBuddyAccountToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" || in.Enabled == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "缺少 id 或 enabled"})
		return
	}
	s.Store.SetWorkBuddyAccountEnabled(id, *in.Enabled)
	_ = s.Store.Save()
	s.Lane.WorkBuddySetEnabled(id, *in.Enabled)
	go s.Lane.RefreshCatalog(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminWorkBuddyAccountPrimary moves one account to the front of the pool and
// makes it the serving one, without waiting for a cooldown to expire.
func (s *Server) adminWorkBuddyAccountPrimary(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "缺少 id"})
		return
	}
	s.Store.MoveWorkBuddyAccountFirst(id)
	_ = s.Store.Save()
	s.Lane.WorkBuddySetPrimary(id)
	go s.Lane.RefreshCatalog(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true})
}

func wbAccountLabel(a lane.WBAccount) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	if a.UID != "" {
		return a.UID
	}
	return a.ID
}
