package gateway

import (
	"context"
	"net/http"
	"sort"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/openref"
	"zen-gate/internal/store"
)

// Capability audit desk. The 6-hourly public-catalog pull is intentionally
// automatic — forcing a human through a confirmation dialog every six hours
// would just train them to click through — but every external write must be
// visible, vetoable and reversible. These endpoints are that half of the
// design: the pull keeps running, and the user keeps the last word.
//
// Trust order stays exactly where it was (实测 > 上游声明 > AI标注 > 规则),
// with 人工校准 sharing the top rung: a hand-entered number is a decision, not
// a guess an external catalog is allowed to outrank.
//
// Why this exists rather than copying one upstream: a survey of the same
// class found no gateway that records an automatic catalog write as a
// reviewable, vetoable, reversible decision. LiteLLM's static
// model_prices_and_context_window.json and its Router fallbacks keep no change
// history; anomalyco/models.dev derives api.json from v2 TOML files whose
// history lives in git PRs, invisible to a running gateway's user;
// OpenRouter's /models is a current snapshot only; new-api ports channel
// priority but not provenance; claude-code-router routes by longContext and
// cc-switch rewrites only connection fields. Every one of them either trusts
// the catalog silently or makes the user hand-edit a file. Keeping the
// automation AND keeping the last word is the gap this desk fills.

// capDiffRow is one place where a catalog's current declaration disagrees
// with what the gateway is actually serving.
type capDiffRow struct {
	Model        string `json:"model"`
	Source       string `json:"source"`
	RefContext   int    `json:"refContext"`
	RefOutput    int    `json:"refOutput"`
	LocalContext int    `json:"localContext"`
	LocalOutput  int    `json:"localOutput"`
	TagSource    string `json:"tagSource"`
	Locked       bool   `json:"locked"`
}

// adminCapabilityAudit returns the decision log, the current vetoes, the last
// catalog snapshot, and the offline diff between the two.
func (s *Server) adminCapabilityAudit(w http.ResponseWriter, r *http.Request) {
	refs := s.Store.CapabilityRefs()
	locks := s.Store.CapabilityLocks()
	tags := s.Store.SnapshotTags()
	cat, _, _ := s.Lane.Snapshot()

	var diffs []capDiffRow
	for _, m := range cat {
		if m.Channel != "" {
			continue // the Kilo pool declares its own capacities
		}
		base := lane.BaseModelId(m.ID)
		src := store.TagSourceHeuris
		if t, ok := tags[base]; ok {
			src = t.Source
		} else if !lane.CapabilityMatched(base) {
			src = store.TagSourceUnknown
		}
		local := store.RefCap{ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput}
		// models.dev is keyed by the lane's exact ids; OpenRouter is keyed by
		// the normalized core. Two sources, two lookups, two rows if both
		// disagree — the user should see both opinions, not a merged winner.
		if ref, ok := refs.ModelsDev[base]; ok && ref != local {
			diffs = append(diffs, capDiffRow{
				Model: base, Source: store.CapOriginModelsDev,
				RefContext: ref.ContextWindow, RefOutput: ref.MaxOutput,
				LocalContext: m.ContextWindow, LocalOutput: m.MaxOutput,
				TagSource: src, Locked: locks[base],
			})
		}
		if ref, ok := refs.OpenRouter[openref.Normalize(base)]; ok && ref != local {
			diffs = append(diffs, capDiffRow{
				Model: base, Source: store.CapOriginOpenRouter,
				RefContext: ref.ContextWindow, RefOutput: ref.MaxOutput,
				LocalContext: m.ContextWindow, LocalOutput: m.MaxOutput,
				TagSource: src, Locked: locks[base],
			})
		}
	}
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].Model != diffs[j].Model {
			return diffs[i].Model < diffs[j].Model
		}
		return diffs[i].Source < diffs[j].Source
	})

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"entries": s.Store.CapabilityAudit(),
		"locks":   locks,
		"refs":    refs,
		"diff":    diffs,
	})
}

// adminCapabilityLock sets or clears a per-model veto. A locked model is
// skipped by both catalog passes, so a decision survives every future pull.
func (s *Server) adminCapabilityLock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model  string `json:"model"`
		Locked *bool  `json:"locked"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if in.Model == "" || in.Locked == nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "需要 model 与 locked"})
		return
	}
	s.Store.SetCapabilityLock(in.Model, *in.Locked)
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "model": in.Model, "locked": *in.Locked})
}

// adminCapabilityRollback restores one change's Before verdict. A rollback
// also locks the model: undoing a catalog's verdict is a veto on the next one,
// and without the lock the following 6-hourly pull would silently re-apply it.
func (s *Server) adminCapabilityRollback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Seq int64 `json:"seq"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	change, ok := s.Store.CapabilityChangeOf(in.Seq)
	if !ok {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "找不到这条校准记录"})
		return
	}
	cur, has := s.Store.ModelTagOf(change.Model)
	before := (*store.ModelTag)(nil)
	if has {
		cp := cur
		before = &cp
	}
	var after store.ModelTag
	if change.Before == nil {
		// There was no stored row: delete it so the curated table answers
		// again. Writing a zeroed row would publish a zero-size model.
		s.Store.DeleteModelTag(change.Model)
	} else {
		after = *change.Before
		s.Store.SetModelTagExact(change.Model, after)
	}
	s.Store.RecordManualChange(change.Model, before, after, store.CapOriginRollback)
	s.Store.SetCapabilityLock(change.Model, true)
	_ = s.Store.Save()
	s.applyStoredTags()
	if s.modelsSync != nil {
		s.modelsSync()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "model": change.Model, "locked": true})
}

// adminCapabilityCalibrate writes a hand-entered capacity verdict. It shares
// the top trust rung with a live probe and locks the model, so neither the
// next scheduled pull nor a re-tag can quietly overrule a human decision.
func (s *Server) adminCapabilityCalibrate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model         string `json:"model"`
		ContextWindow int    `json:"contextWindow"`
		MaxOutput     int    `json:"maxOutput"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if in.Model == "" || in.ContextWindow < 0 || in.MaxOutput < 0 ||
		(in.ContextWindow == 0 && in.MaxOutput == 0) {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "模型名不能为空，容量需为正数且至少填一项"})
		return
	}
	cur, has := s.Store.ModelTagOf(in.Model)
	before := (*store.ModelTag)(nil)
	if has {
		cp := cur
		before = &cp
	}
	next := cur
	next.ContextWindow = in.ContextWindow
	next.MaxOutput = in.MaxOutput
	next.Source = store.TagSourceManual
	next.CapsOnly = false
	next.At = time.Now().UnixMilli()
	// SetModelTagExact avoids field-wise inheritance: a deliberate zero must
	// stay a deliberate zero, not silently pick up the value being replaced.
	s.Store.SetModelTagExact(in.Model, next)
	s.Store.RecordManualChange(in.Model, before, next, store.CapOriginManual)
	s.Store.SetCapabilityLock(in.Model, true)
	_ = s.Store.Save()
	s.applyStoredTags()
	if s.modelsSync != nil {
		s.modelsSync()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "model": in.Model, "locked": true})
}

// adminCapabilityRefresh triggers one pull on demand. It runs detached for
// the same reason subs/probe does: r.Context() dies when the handler returns,
// which would kill a fetch that has barely started.
func (s *Server) adminCapabilityRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.Store.Config().OpenRefEnabled {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "公开容量参考未开启"})
		return
	}
	go s.OpenRefPull(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true, "running": true})
}
