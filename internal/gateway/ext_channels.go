package gateway

import (
	"net/http"
	"strings"

	"zen-gate/internal/lane"
)

// The preset 渠道 cards: six upstreams that all speak OpenAI's chat shape and
// differ only in URL, headers and one body quirk (see internal/lane/ext.go).
//
// These do not have a browser handoff: each vendor's login lives on its own IDE
// or console, and the user brings back the resulting token. So the card's job is
// "paste it, name it, prove it works" — and the proof is real: AddLogin lists the
// roster before storing, because a token that cannot list models cannot serve a
// turn either.

// adminExtStatus reports every preset channel with its accounts.
func (s *Server) adminExtStatus(w http.ResponseWriter) {
	out := make([]map[string]any, 0, len(lane.ExtKinds()))
	for _, kind := range lane.ExtKinds() {
		accts := s.Lane.ExtAccounts(kind)
		rows := make([]map[string]any, 0, len(accts))
		loggedIn := 0
		for _, a := range accts {
			if a.Enabled && strings.TrimSpace(a.Secret) != "" {
				loggedIn++
			}
			rows = append(rows, map[string]any{
				"id":      a.ID,
				"label":   a.Label,
				"enabled": a.Enabled,
				"hasSecret": strings.TrimSpace(a.Secret) != "",
				// The extra identity fields are not secret, but one of them
				// (officeIdentity, uuid) can identify a person — the card only
				// needs to know a value was supplied.
				"extraKeys": extExtraKeys(a),
				"addedAt":   a.AddedAt,
			})
		}
		out = append(out, map[string]any{
			"kind":     kind,
			"label":    lane.ExtPresetLabel(kind),
			"enabled":  s.Store.ChannelEnabled(kind),
			"loggedIn": loggedIn > 0,
			"accounts": rows,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "channels": out})
}

func extExtraKeys(a lane.ExtAccount) []string {
	if len(a.Extra) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.Extra))
	for k := range a.Extra {
		out = append(out, k)
	}
	return out
}

// adminExtAdd stores one pasted credential after proving it can list models.
func (s *Server) adminExtAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind   string            `json:"kind"`
		Label  string            `json:"label"`
		Secret string            `json:"secret"`
		Extra  map[string]string `json:"extra"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	acct, err := s.Lane.ExtAddLogin(in.Kind, in.Label, in.Secret, in.Extra)
	if err != nil {
		// A failed verification is the point of this endpoint: say exactly what
		// the upstream said instead of storing a token that will not work.
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.Store.UpsertExtAccount(acct)
	_ = s.Store.Save()
	// A newly added account must show up NOW, not at the next restart: the
	// catalog is rebuilt from live accounts, so without this the user adds a
	// key, sees "saved", and still has no models to pick. ExtAddLogin already
	// kicked a background refresh, but that one races the store write above —
	// run one here that is guaranteed to observe the saved account.
	s.Lane.RefreshCatalog(r.Context())
	s.refreshAfterAccountChange(acct.Kind + " 已接入")
	writeJSON(w, 200, map[string]any{"ok": true, "id": acct.ID, "label": acct.Label})
}

// refreshAfterAccountChange pushes an account-pool change out to everything that
// cached the model set: the admin state's model list, the agents' static
// configs (their pickers), and the wall.
//
// Adding an account needs this or the new models never appear; removing or
// disabling one needs it just as much, or the pickers keep offering models
// nothing can serve. Both directions go through here so neither is forgotten.
func (s *Server) refreshAfterAccountChange(what string) {
	if s.modelsSync != nil {
		s.modelsSync()
	}
	if s.registry != nil {
		if n := s.registry.ResyncEnabled(); n > 0 && s.logger != nil {
			s.logger.Infof("%s：已重新注入 %d 个已开启 Agent 的配置", what, n)
		}
	}
}

// adminExtRemove drops one account.
func (s *Server) adminExtRemove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.Lane.ExtRemove(in.Kind, in.ID)
	s.Store.RemoveExtAccount(in.Kind, in.ID)
	_ = s.Store.Save()
	// The catalog is rebuilt from live accounts, so without this refresh the
	// removed account's models stay listed: an agent picker keeps offering
	// them and every request behind one of them fails. Subtraction has to
	// reach the catalog as fast as addition does.
	s.Lane.RefreshCatalog(r.Context())
	s.refreshAfterAccountChange("ext/remove")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminExtToggle flips one account's enable flag.
func (s *Server) adminExtToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if !s.Lane.ExtSetEnabled(in.Kind, in.ID, in.Enabled) {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "没有这个账号"})
		return
	}
	s.Store.SetExtAccountEnabled(in.Kind, in.ID, in.Enabled)
	_ = s.Store.Save()
	// A disabled account must leave the pickers for the same reason a removed
	// one does: the pool no longer has anything to spend on those models.
	s.Lane.RefreshCatalog(r.Context())
	s.refreshAfterAccountChange("ext/toggle")
	writeJSON(w, 200, map[string]any{"ok": true})
}
