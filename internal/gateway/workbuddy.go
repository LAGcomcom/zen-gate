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
	"zen-gate/internal/store"
	"zen-gate/internal/tray"
)

// The WorkBuddy 账号渠道's dashboard surface: a status readout for the
// settings card plus the three login-call endpoints. The heavy lifting —
// OAuth state polling, token rotation, persistence — lives in the lane; these
// handlers only compose what the card shows and trigger the catalog resync.

var (
	wbCreditsMu   sync.Mutex
	wbCreditsAt   time.Time
	wbCreditsLast string
)

// adminWorkBuddyStatus answers the settings card: login state, expiry,
// activity date, and a cached credits readout (the billing endpoint rides the
// upstream, so page refreshes share one answer per minute).
func (s *Server) adminWorkBuddyStatus(w http.ResponseWriter, r *http.Request) {
	acct := s.Store.Config().WorkBuddy
	credits := ""
	detail := ""
	if acct.AccessToken != "" {
		wbCreditsMu.Lock()
		if time.Since(wbCreditsAt) < time.Minute {
			credits = wbCreditsLast
			wbCreditsMu.Unlock()
		} else {
			wbCreditsMu.Unlock()
			if v, err := s.Lane.WorkBuddyCredits(r.Context()); err == nil {
				credits = v
			} else {
				detail = err.Error()
			}
			wbCreditsMu.Lock()
			wbCreditsAt = time.Now()
			wbCreditsLast = credits
			wbCreditsMu.Unlock()
		}
	}
	writeJSON(w, 200, map[string]any{
		"loggedIn":         acct.AccessToken != "",
		"enabled":          acct.Enabled,
		"uid":              acct.UID,
		"nickname":         acct.Nickname,
		"expiresAtMs":      acct.ExpiresAtMS,
		"lastActivityDate": acct.LastActivityDate,
		"lastCheckinDate":  acct.LastCheckinDate,
		"realm":            acct.Realm,
		// canCheckin is the card's cue to show the 签到 button: only the CN
		// publishment publishes the endpoint, and only for non-enterprise
		// accounts. False never hides a button that would have worked — the
		// lane re-checks on the click itself.
		"canCheckin": acct.Realm == lane.WBRealmCN && acct.AccessToken != "",
		"credits": credits,
		"detail":  detail,
	})
}

// adminWorkBuddyCheckin runs the CN daily checkin on demand. The scheduler
// claims it once a day on its own; this is the "did it actually land?" button.
func (s *Server) adminWorkBuddyCheckin(w http.ResponseWriter) {
	ran, err := s.Lane.WorkBuddyDailyCheckin(context.Background())
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !ran {
		// Either not logged in, not a CN account, or already claimed today.
		writeJSON(w, 200, map[string]any{"ok": true, "skipped": true})
		return
	}
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

// adminWorkBuddyLoginPoll advances the login once. "ok" means the pair is
// already persisted (through the lane's persist callback) and the roster is
// rebuilding; the handler only re-syncs the agent injections.
func (s *Server) adminWorkBuddyLoginPoll(w http.ResponseWriter) {
	state, err := s.Lane.WorkBuddyLoginPoll()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error(), "state": "error"})
		return
	}
	if state == "ok" {
		acct := s.Store.Config().WorkBuddy
		s.logCat(logx.CatAdmin, "info", "WorkBuddy 账号已登录：%s（免费模型已进入目录）", wbAccountLabel(acct))
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

// adminWorkBuddyLogout clears the account everywhere: store, lane snapshot,
// and the catalog (the next rebuild drops the roster).
func (s *Server) adminWorkBuddyLogout(w http.ResponseWriter) {
	s.Store.Mutate(func(c *store.Config) { c.WorkBuddy = lane.WBAccount{} })
	_ = s.Store.Save()
	s.Lane.WorkBuddyLogout()
	go s.Lane.RefreshCatalog(context.Background())
	if s.logger != nil {
		s.logger.Infof("WorkBuddy 账号已退出")
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func wbAccountLabel(a lane.WBAccount) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.UID
}
