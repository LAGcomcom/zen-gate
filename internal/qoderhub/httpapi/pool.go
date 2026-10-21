package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/qoderhub/remote"
)

// The account-pool surface. It exists so the settings card can show and steer
// the pool without reading the on-disk index itself: the hub owns the pool, so
// the hub answers for it.
//
// Nothing here returns credential material. A status response that carried a
// token would make the whole point of keeping secrets in per-account files
// (a response safe to show) disappear.

type poolAccountView struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	UID           string `json:"uid"`
	Enabled       bool   `json:"enabled"`
	State         string `json:"state"`
	CooldownUntil string `json:"cooldownUntil,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
	// ExpiresAt/DaysLeft describe a device-authorized account's token. An
	// imported IDE snapshot has neither, and the card then says nothing.
	ExpiresAt string `json:"expiresAt,omitempty"`
	DaysLeft  int    `json:"daysLeft,omitempty"`
}

// handlePool answers GET /pool: the roster plus which account serves first.
func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	pool := s.svc.Pool()
	if pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"enabled":  false,
			"accounts": []poolAccountView{},
			"detail":   "账号池未启用：当前只用 Qoder IDE 的登录",
		})
		return
	}
	now := time.Now()
	rows := pool.Snapshot()
	views := make([]poolAccountView, 0, len(rows))
	for _, a := range rows {
		v := poolAccountView{
			ID: a.ID, Label: a.Label, UID: a.UID, Enabled: a.Enabled,
			State:     a.State(now),
			LastError: a.LastError(),
		}
		if until := a.CooldownUntil(); !until.IsZero() {
			v.CooldownUntil = until.Format(time.RFC3339)
		}
		if !a.CreatedAt.IsZero() {
			v.CreatedAt = a.CreatedAt.Format(time.RFC3339)
		}
		if !a.ExpiresAt.IsZero() {
			v.ExpiresAt = a.ExpiresAt.Format(time.RFC3339)
			v.DaysLeft = int(time.Until(a.ExpiresAt).Hours() / 24)
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"enabled":  true,
		"dir":      pool.Dir(),
		"ideLogin": remote.IDELoggedIn(),
		"accounts": views,
	})
}

// handlePoolAccount answers POST /pool/account with one action:
//
//	{"action":"import"}                  snapshot the IDE's live login
//	{"action":"toggle","id":…,"enabled":…}
//	{"action":"primary","id":…}
//	{"action":"delete","id":…}
//	{"action":"login-start","realm":…}   open a device-authorization window
//	{"action":"login-poll","state":…}    ask whether the browser finished
//	{"action":"login-cancel","state":…}  close a window
//
// The actions mirror the WorkBuddy card deliberately: two channels with two
// different account models would make the same page teach two mental models.
func (s *Server) handlePoolAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	pool := s.svc.Pool()
	if pool == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"账号池未启用：当前只用 Qoder IDE 的登录，没有可管理的账号")
		return
	}
	var in struct {
		Action  string `json:"action"`
		ID      string `json:"id"`
		Label   string `json:"label"`
		Enabled *bool  `json:"enabled"`
		State   string `json:"state"`
		Realm   string `json:"realm"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败")
		return
	}
	if err := json.Unmarshal(data, &in); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON")
		return
	}
	id := strings.TrimSpace(in.ID)

	switch strings.TrimSpace(in.Action) {
	case "import":
		acct, err := pool.ImportCurrent(strings.TrimSpace(in.Label))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if err := pool.SaveIndex(); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": acct.ID})
	case "login-start":
		start, err := s.logins.Start(strings.TrimSpace(in.Realm))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "state": start.State, "authUrl": start.AuthURL, "realm": start.Realm,
		})
	case "login-poll":
		result, err := s.logins.Poll(in.State)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "server_error", err.Error())
			return
		}
		out := map[string]any{
			"ok": true, "status": result.Status,
			"message": result.Message,
		}
		if result.Cred != nil {
			// The label the user typed wins, but a login usually has no reason
			// to type one: the account name the grant reported is what the
			// roster should show, so it has to be stored, not just echoed.
			label := strings.TrimSpace(in.Label)
			if label == "" {
				label = result.Label
			}
			acct, err := pool.Import(*result.Cred, label)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if err := pool.SaveIndex(); err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
				return
			}
			out["account"] = map[string]any{
				"id": acct.ID, "label": acct.Label, "uid": acct.UID, "expiresAt": result.ExpiresAt,
			}
		}
		writeJSON(w, http.StatusOK, out)
	case "login-cancel":
		s.logins.Cancel(in.State)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "toggle":
		if id == "" || in.Enabled == nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 id 或 enabled")
			return
		}
		if err := pool.SetEnabled(id, *in.Enabled); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		_ = pool.SaveIndex()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "primary":
		if id == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 id")
			return
		}
		if err := pool.MoveFirst(id); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		_ = pool.SaveIndex()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "delete":
		if id == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "缺少 id")
			return
		}
		if err := pool.Delete(id); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		_ = pool.SaveIndex()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
			"未知操作：可用 import / toggle / primary / delete / login-start / login-poll / login-cancel")
	}
}
