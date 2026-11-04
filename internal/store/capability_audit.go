package store

import (
	"fmt"
	"path/filepath"
	"time"
)

// Capability origins name who wrote a tags.json row. They land in JSON and are
// shown verbatim in the audit desk, so keep them short and stable.
const (
	CapOriginModelsDev  = "models.dev"
	CapOriginOpenRouter = "OpenRouter"
	CapOriginManual     = "人工校准"
	CapOriginRollback   = "回滚"
)

// capabilityAuditKeep bounds the on-disk history. The audit trail is a
// decision log, not a metric stream: 200 entries is many months of 6-hourly
// pulls, and trimming never rewrites the surviving entries' sequence numbers.
const capabilityAuditKeep = 200

// RefCap is one public catalog's declared capacity pair for a model id.
type RefCap struct {
	ContextWindow int `json:"contextWindow,omitempty"`
	MaxOutput     int `json:"maxOutput,omitempty"`
}

// CapabilityChange is one recorded write to tags.json. Before is nil when the
// model had no stored verdict at all — in that case the curated table (规则)
// was answering, and rolling back means deleting the row rather than writing
// a zeroed one.
type CapabilityChange struct {
	Seq    int64     `json:"seq"`
	Model  string    `json:"model"`
	Origin string    `json:"origin"`
	At     int64     `json:"at"`
	Before *ModelTag `json:"before,omitempty"`
	After  ModelTag  `json:"after"`
	Fields []string  `json:"fields,omitempty"`
	// Removed marks a change that deleted the stored verdict altogether. The
	// curated table (规则) answers again; there is no "after" row to display.
	Removed bool `json:"removed,omitempty"`
}

type capabilityAuditFile struct {
	Version int                `json:"version"`
	Entries []CapabilityChange `json:"entries"`
}

// CapabilityRefs is the last successful public-catalog snapshot. Keeping it
// lets the audit desk show "what the catalog says" against "what we believe"
// without a live network call, and without depending on the pull succeeding
// at the moment the user opens the page.
type CapabilityRefs struct {
	At         int64             `json:"at"`
	ModelsDev  map[string]RefCap `json:"modelsDev,omitempty"`
	OpenRouter map[string]RefCap `json:"openRouter,omitempty"`
}

func (s *Store) capabilityAuditPath() string {
	return filepath.Join(s.Home, "capability_audit.json")
}

func (s *Store) capabilityRefsPath() string {
	return filepath.Join(s.Home, "capability_refs.json")
}

// loadCapabilityAudit is called by New, before the store is published.
func (s *Store) loadCapabilityAudit() {
	f, err := loadJSON[capabilityAuditFile](s.capabilityAuditPath())
	if err != nil || f == nil {
		s.audit = []CapabilityChange{}
		return
	}
	s.audit = f.Entries
	if s.audit == nil {
		s.audit = []CapabilityChange{}
	}
}

func (s *Store) loadCapabilityRefs() {
	f, err := loadJSON[CapabilityRefs](s.capabilityRefsPath())
	if err != nil || f == nil {
		s.refs = CapabilityRefs{}
		return
	}
	s.refs = *f
}

// CapabilityAudit returns the decision log, newest first.
func (s *Store) CapabilityAudit() []CapabilityChange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CapabilityChange, len(s.audit))
	for i, e := range s.audit {
		out[len(s.audit)-1-i] = e
	}
	return out
}

// CapabilityChangeOf finds one entry by sequence number.
func (s *Store) CapabilityChangeOf(seq int64) (CapabilityChange, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.audit {
		if e.Seq == seq {
			return e, true
		}
	}
	return CapabilityChange{}, false
}

func (s *Store) nextCapabilitySeq() int64 {
	if n := len(s.audit); n > 0 {
		return s.audit[n-1].Seq + 1
	}
	return 1
}

func (s *Store) appendCapabilityChangeLocked(e CapabilityChange) {
	s.audit = append(s.audit, e)
	if len(s.audit) > capabilityAuditKeep {
		s.audit = append([]CapabilityChange{}, s.audit[len(s.audit)-capabilityAuditKeep:]...)
	}
	_ = writeJSON(s.capabilityAuditPath(), capabilityAuditFile{Version: 1, Entries: s.audit})
}

// capabilityDiff names the fields a write actually changed. It is the narrow
// answer to "what did the catalog do to this model" — a source-only re-stamp
// with identical capacities produces no entry at all.
func capabilityDiff(prev ModelTag, has bool, next ModelTag) []string {
	var out []string
	if !has {
		return []string{"contextWindow", "maxOutput"}
	}
	if prev.ContextWindow != next.ContextWindow {
		out = append(out, "contextWindow")
	}
	if prev.MaxOutput != next.MaxOutput {
		out = append(out, "maxOutput")
	}
	if prev.Vision != next.Vision {
		out = append(out, "vision")
	}
	if prev.Audio != next.Audio {
		out = append(out, "audio")
	}
	if prev.File != next.File {
		out = append(out, "file")
	}
	if prev.Reasoning != next.Reasoning {
		out = append(out, "reasoning")
	}
	if prev.Source != next.Source {
		out = append(out, "source")
	}
	return out
}

func sameCapability(a, b ModelTag) bool {
	return a.ContextWindow == b.ContextWindow && a.MaxOutput == b.MaxOutput &&
		a.Vision == b.Vision && a.Audio == b.Audio && a.File == b.File &&
		a.Reasoning == b.Reasoning && a.Source == b.Source && a.CapsOnly == b.CapsOnly
}

// setModelTagLocked is the shared write path. Callers hold s.mu. A zero
// capacity on the incoming row means "this source has nothing to say about
// it", not "set it to zero" — a 能力实测 run answers modalities only and must
// not erase the window a provider's listing declared.
func (s *Store) setModelTagLocked(model string, t ModelTag) {
	if prev, ok := s.tags[model]; ok {
		if t.ContextWindow == 0 {
			t.ContextWindow = prev.ContextWindow
		}
		if t.MaxOutput == 0 {
			t.MaxOutput = prev.MaxOutput
		}
	}
	s.tags[model] = t
	_ = writeJSON(filepath.Join(s.Home, "tags.json"), s.tags)
}

// SetModelTagTracked writes a row and records a before→after entry when the
// verdict actually changed. Public catalog pulls use it so every external
// correction is auditable and reversible instead of silently overwriting
// tags.json. Returns false when nothing changed.
func (s *Store) SetModelTagTracked(model string, t ModelTag, origin string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, has := s.tags[model]
	final := t
	if has {
		if final.ContextWindow == 0 {
			final.ContextWindow = prev.ContextWindow
		}
		if final.MaxOutput == 0 {
			final.MaxOutput = prev.MaxOutput
		}
	}
	if has && sameCapability(prev, final) {
		return false
	}
	before := (*ModelTag)(nil)
	if has {
		cp := prev
		before = &cp
	}
	e := CapabilityChange{
		Seq:    s.nextCapabilitySeq(),
		Model:  model,
		Origin: origin,
		At:     time.Now().UnixMilli(),
		Before: before,
		After:  final,
		Fields: capabilityDiff(prev, has, final),
	}
	s.setModelTagLocked(model, final)
	s.appendCapabilityChangeLocked(e)
	return true
}

// SetModelTagExact replaces a row without field-wise inheritance. Rollback
// needs this: restoring a verdict whose window was zero must not inherit the
// very value being undone.
func (s *Store) SetModelTagExact(model string, t ModelTag) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags[model] = t
	_ = writeJSON(filepath.Join(s.Home, "tags.json"), s.tags)
}

// DeleteModelTag removes a stored verdict so the curated table answers again.
// It is the rollback form for a change whose Before was nil.
func (s *Store) DeleteModelTag(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tags[model]; !ok {
		return
	}
	delete(s.tags, model)
	_ = writeJSON(filepath.Join(s.Home, "tags.json"), s.tags)
}

// RecordManualChange writes an audit entry for a user-driven verdict change
// (rollback or hand calibration) so the trail stays complete in both
// directions. The caller has already written the tag.
func (s *Store) RecordManualChange(model string, before *ModelTag, after ModelTag, origin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := CapabilityChange{
		Seq:    s.nextCapabilitySeq(),
		Model:  model,
		Origin: origin,
		At:     time.Now().UnixMilli(),
		Before: before,
		After:  after,
	}
	if before != nil {
		e.Fields = capabilityDiff(*before, true, after)
	}
	if after == (ModelTag{}) {
		e.Removed = true
	}
	s.appendCapabilityChangeLocked(e)
}

// CapabilityLocks returns the models the user has shielded from automatic
// catalog corrections.
func (s *Store) CapabilityLocks() map[string]bool {
	return cloneBoolMap(s.Config().CapabilityLocks)
}

// SetCapabilityLock shields (or releases) one model. Mutate takes s.mu itself,
// so this method must not hold it.
func (s *Store) SetCapabilityLock(model string, on bool) {
	s.Mutate(func(cfg *Config) {
		if on {
			if cfg.CapabilityLocks == nil {
				cfg.CapabilityLocks = map[string]bool{}
			}
			cfg.CapabilityLocks[model] = true
		} else {
			delete(cfg.CapabilityLocks, model)
			if len(cfg.CapabilityLocks) == 0 {
				cfg.CapabilityLocks = nil
			}
		}
	})
}

// CapabilityRefs returns the last successful catalog snapshot.
func (s *Store) CapabilityRefs() CapabilityRefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.refs
	out.ModelsDev = cloneRefCaps(s.refs.ModelsDev)
	out.OpenRouter = cloneRefCaps(s.refs.OpenRouter)
	return out
}

func cloneRefCaps(in map[string]RefCap) map[string]RefCap {
	if in == nil {
		return nil
	}
	out := make(map[string]RefCap, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SetCapabilityRefs stores one source's freshly fetched declarations.
func (s *Store) SetCapabilityRefs(origin string, caps map[string]RefCap) error {
	if origin != CapOriginModelsDev && origin != CapOriginOpenRouter {
		return fmt.Errorf("未知的容量参考来源: %s", origin)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch origin {
	case CapOriginModelsDev:
		s.refs.ModelsDev = cloneRefCaps(caps)
	case CapOriginOpenRouter:
		s.refs.OpenRouter = cloneRefCaps(caps)
	}
	s.refs.At = time.Now().UnixMilli()
	return writeJSON(s.capabilityRefsPath(), s.refs)
}
