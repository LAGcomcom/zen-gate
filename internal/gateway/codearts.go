package gateway

import (
	"context"
	"net/http"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/store"
	"zen-gate/internal/tray"
)

// The CodeArts 账号渠道's dashboard surface: a status readout for the settings
// card plus the OAuth login endpoints. The heavy lifting — PKCE, the loopback
// callback / ticket race, STS refresh, the signed chat and benefit discovery —
// lives in the lane; these handlers only compose what the card shows and trigger
// the catalog resync, mirroring the WorkBuddy channel's handlers.

// adminCodeartsStatus answers the settings card: login state, expiry, the
// model count currently served and the last daily check-in day.
func (s *Server) adminCodeartsStatus(w http.ResponseWriter, r *http.Request) {
	acct := s.Lane.CodeArtsSnapshot()
	models := 0
	cat, _, _ := s.Lane.Snapshot()
	for _, m := range cat {
		if m.Channel == lane.ChannelCodeArts {
			models++
		}
	}
	writeJSON(w, 200, map[string]any{
		"loggedIn":          acct.SecurityToken != "",
		"enabled":           acct.Enabled,
		"uid":               acct.UserID,
		"nickname":          acct.UserName,
		"expiresAtMs":       acct.ExpirationMS,
		"lastCheckinDate":   acct.LastCheckinDate,
		"models":            models,
		"canRefresh":        acct.RefreshToken != "" && acct.CodeVerifier != "",
	})
}

// adminCodeartsLoginStart mints a PKCE login and hands the auth URL back; the
// card opens it in a new tab and starts polling.
func (s *Server) adminCodeartsLoginStart(w http.ResponseWriter) {
	authURL, err := s.Lane.CodeArtsLoginStart()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// Open in the DEFAULT browser, not a WebView2 popup: the embedded window's
	// profile is separate from the daily browser, so a popup would ask for a
	// Huawei Cloud login the browser session already has. Same as WorkBuddy.
	tray.Open(authURL)
	if s.logger != nil {
		s.logger.Infof("CodeArts 登录已开始，授权页已在默认浏览器打开")
	}
	writeJSON(w, 200, map[string]any{"ok": true, "authUrl": authURL})
}

// adminCodeartsLoginPoll advances the login once. "ok" means the STS pair is
// persisted (through the lane's persist callback) and the roster is rebuilding;
// the handler only re-syncs the agent injections.
func (s *Server) adminCodeartsLoginPoll(w http.ResponseWriter, r *http.Request) {
	state, err := s.Lane.CodeArtsLoginPoll(r.Context())
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error(), "state": "error"})
		return
	}
	if state == "ok" {
		acct := s.Lane.CodeArtsSnapshot()
		s.logCat(logx.CatAdmin, "info", "CodeArts 账号已登录：%s（免费模型进入目录中）", caAccountLabel(acct))
		if s.registry != nil {
			time.AfterFunc(1500*time.Millisecond, func() {
				if s.modelsSync != nil {
					s.modelsSync()
				}
				if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
					s.logger.Infof("CodeArts 渠道上线，已重新注入 %d 个已开启 Agent 的配置", n)
				}
			})
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": state})
}

// adminCodeartsLogout clears the account everywhere: store, lane snapshot and
// the catalog (the next rebuild drops the roster).
func (s *Server) adminCodeartsLogout(w http.ResponseWriter) {
	s.Store.Mutate(func(c *store.Config) { c.CodeArts = lane.CAAccount{} })
	_ = s.Store.Save()
	s.Lane.CodeArtsLogout()
	go s.Lane.RefreshCatalog(context.Background())
	if s.logger != nil {
		s.logger.Infof("CodeArts 账号已退出")
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func caAccountLabel(a lane.CAAccount) string {
	if a.UserName != "" {
		return a.UserName
	}
	return a.UserID
}
