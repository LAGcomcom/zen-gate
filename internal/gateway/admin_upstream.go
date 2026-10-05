package gateway

import (
	"net/http"
	"sort"
	"strings"

	"zen-gate/internal/store"
)

// Admin endpoints for user-configured upstreams.
//
// The whole list is written in one POST (replace semantics) rather than
// per-entry CRUD: the dashboard holds the full list in memory, and a single
// atomic write avoids a partially-applied config where one upstream validates
// and the next does not.

// adminUpstreamsSet replaces the configured upstream list.
//
// Validation is all-or-nothing on purpose. Half-applying a list would leave the
// gateway in a state the user cannot see from the dashboard (a model listed in
// one upstream but served by another), and the failure would only surface as a
// confusing 502 on the next request.
func (s *Server) adminUpstreamsSet(w http.ResponseWriter, r *http.Request) {	var in struct {
		Upstreams []store.Upstream `json:"upstreams"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	seenID := map[string]bool{}
	for i := range in.Upstreams {
		in.Upstreams[i].Trim()
		if err := in.Upstreams[i].Validate(); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error(),
				"index": i, "id": in.Upstreams[i].ID})
			return
		}
		if seenID[in.Upstreams[i].ID] {
			writeJSON(w, 400, map[string]any{"ok": false,
				"error": "duplicate upstream id: " + in.Upstreams[i].ID, "index": i})
			return
		}
		seenID[in.Upstreams[i].ID] = true
	}
	cfg := s.Store.Config()
	cfg.Upstreams = in.Upstreams
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.refreshAfterUpstreamChange()
	if s.logger != nil {
		s.logger.Infof("自定义上游已保存 · %d 个", len(cfg.Upstreams))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "upstreams": redactUpstreams(s.upstreams())})
}

// adminUpstreamDelete removes one upstream by id.
func (s *Server) adminUpstreamDelete(w http.ResponseWriter, id string) {
	id = strings.TrimSpace(id)
	cfg := s.Store.Config()
	kept := make([]store.Upstream, 0, len(cfg.Upstreams))
	found := false
	for _, u := range cfg.Upstreams {
		if u.ID == id {
			found = true
			continue
		}
		kept = append(kept, u)
	}
	if !found {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "no such upstream: " + id})
		return
	}
	cfg.Upstreams = kept
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.refreshAfterUpstreamChange()
	writeJSON(w, 200, map[string]any{"ok": true, "upstreams": redactUpstreams(s.upstreams())})
}

// refreshAfterUpstreamChange re-runs the agent adapters so a newly added
// upstream's models reach the Codex catalog file without a restart.
//
// The codex adapter rewrites its sidecar catalog from the gateway's model list
// on every Enable; toggling the adapter off and on is the existing mechanism
// for "the model set changed", so reuse it rather than inventing a second path.
// An adapter that fails to re-apply is logged, not fatal: the config is already
// saved and takes effect on the next app start.
func (s *Server) refreshAfterUpstreamChange() {
	if s.registry == nil {
		return
	}
	for _, v := range s.registry.Views() {
		if !v.Enabled {
			continue
		}
		if err := s.registry.Disable(v.ID); err != nil {
			if s.logger != nil {
				s.logger.Warnf("上游变更后重写 %s 配置失败（下次启动生效）: %v", v.ID, err)
			}
			continue
		}
		if err := s.registry.Enable(v.ID); err != nil {
			if s.logger != nil {
				s.logger.Warnf("上游变更后重写 %s 配置失败（下次启动生效）: %v", v.ID, err)
			}
		}
	}
}

// upstreamIDs is a small helper for the dashboard-facing listings.
func (s *Server) upstreamIDs() []string {
	out := []string{}
	for _, u := range s.upstreams() {
		out = append(out, u.ID)
	}
	sort.Strings(out)
	return out
}

// redactUpstreams renders the configured upstreams for the dashboard without
// their secrets: the browser edit form needs to know whether a key is set (so
// it can render a placeholder instead of silently blanking the field), but the
// key itself must never cross to the page. The state endpoint is loopback-
// guarded, yet a key sitting in the DOM is one XSS away from being exfiltrated.
func redactUpstreams(ups []store.Upstream) []map[string]any {
	out := []map[string]any{}
	for _, u := range ups {
		item := map[string]any{
			"id": u.ID, "name": u.Name, "baseUrl": u.BaseURL,
			"models": u.Models, "exposeRegion": u.ExposeRegion,
			"hasKey": u.APIKey != "",
		}
		out = append(out, item)
	}
	return out
}
