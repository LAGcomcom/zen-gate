package gateway

import (
	"net/http"
	"strings"

	"zen-gate/internal/lane"
)

// The ZCode 渠道 card: login, roster, per-account state.
//
// The login is a device-authorization flow this gateway runs itself, so the
// card's job is narrow: hand out the authorize URL, then poll once per second
// until the account lands. There is no callback port to manage and no second
// app to read from.

// adminZCodeStatus reports the pool and, when a login is in flight, its state.
func (s *Server) adminZCodeStatus(w http.ResponseWriter) {
	accts := s.Lane.ZCodeAccounts()
	rows := make([]map[string]any, 0, len(accts))
	loggedIn := 0
	for _, a := range accts {
		if a.Enabled && strings.TrimSpace(a.JWT) != "" {
			loggedIn++
		}
		rows = append(rows, map[string]any{
			"id":       a.ID,
			"userId":   a.UserID,
			"label":    a.Label,
			"enabled":  a.Enabled,
			// Never echo the JWT itself: the card only needs to know it exists.
			"hasToken": strings.TrimSpace(a.JWT) != "",
			"hasZai":   strings.TrimSpace(a.ZaiToken) != "",
			"parked":   lane.ZCodeAccountParked(a.ID),
		})
	}
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"loggedIn": loggedIn > 0,
		"accounts": rows,
		"channel":  s.Store.ChannelEnabled(lane.ChannelZCode),
	})
}

// adminZCodeLoginStart opens a flow and returns the URL the user must open.
func (s *Server) adminZCodeLoginStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
	}
	_ = decodeBody(r, &in)
	url, err := s.Lane.ZCodeLoginStart(in.Provider)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "authUrl": url})
}

// adminZCodeLoginPoll checks one round of the in-flight login.
func (s *Server) adminZCodeLoginPoll(w http.ResponseWriter) {
	state, err := s.Lane.ZCodeLoginPoll()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": state})
}

// adminZCodeLogout drops one account.
func (s *Server) adminZCodeLogout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.Lane.ZCodeLogout(in.ID)
	s.Store.RemoveZCodeAccount(in.ID)
	_ = s.Store.Save()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminZCodeAccountToggle flips one account's enable flag.
func (s *Server) adminZCodeAccountToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !s.Lane.ZCodeSetEnabled(in.ID, in.Enabled) {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "没有这个账号"})
		return
	}
	s.Store.SetZCodeAccountEnabled(in.ID, in.Enabled)
	_ = s.Store.Save()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminTraeStatus reports the TRAE pool.
func (s *Server) adminTraeStatus(w http.ResponseWriter) {
	accts := s.Lane.TraeAccounts()
	rows := make([]map[string]any, 0, len(accts))
	loggedIn := 0
	for _, a := range accts {
		if a.Enabled && strings.TrimSpace(a.AccessToken) != "" {
			loggedIn++
		}
		rows = append(rows, map[string]any{
			"id":       a.ID,
			"uid":      a.UID,
			"nickname": a.Nickname,
			"enabled":  a.Enabled,
			"hasToken": strings.TrimSpace(a.AccessToken) != "",
			"hasRefresh": strings.TrimSpace(a.RefreshToken) != "",
			"expiresAtMs": a.ExpiresAtMS,
		})
	}
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"loggedIn": loggedIn > 0,
		"accounts": rows,
		"channel":  s.Store.ChannelEnabled(lane.ChannelTrae),
	})
}

// adminTraeLoginStart opens the browser handoff.
func (s *Server) adminTraeLoginStart(w http.ResponseWriter) {
	url, err := s.Lane.TraeLoginStart()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "authUrl": url})
}

// adminTraeLoginPoll checks one round of the browser handoff.
func (s *Server) adminTraeLoginPoll(w http.ResponseWriter) {
	state, err := s.Lane.TraeLoginPoll()
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": state})
}

// adminTraeLogout drops one account.
func (s *Server) adminTraeLogout(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.Lane.TraeLogout(in.ID)
	s.Store.RemoveTraeAccount(in.ID)
	_ = s.Store.Save()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminTraeAccountToggle flips one account's enable flag.
func (s *Server) adminTraeAccountToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !s.Lane.TraeSetEnabled(in.ID, in.Enabled) {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "没有这个账号"})
		return
	}
	s.Store.SetTraeAccountEnabled(in.ID, in.Enabled)
	_ = s.Store.Save()
	writeJSON(w, 200, map[string]any{"ok": true})
}
