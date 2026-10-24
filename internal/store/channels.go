package store

import (
	"os"
	"time"
	"path/filepath"
	"sort"
)

// One channel, one file.
//
// Every free-lane channel keeps <home>/channels/<id>.json holding its enable
// flag and the model roster it currently serves. The point is attribution:
// when two channels carry the same model id (deepseek-v4.1-flash-free lives
// on the Zen lane and rots through the Kilo pool too), a merged view cannot
// say which source a request would ride — the per-channel file can, and the
// roster is rewritten on every catalog refresh so it never drifts.
//
// The files are gateway-maintained state: Enabled flips through the admin API
// (or by hand while the gateway is down), Models only ever by the lane. They
// are deliberately free of credentials — accounts and tokens stay in
// config.json, which already has the masking and 0600 treatment.

// ChannelIDs lists the free-lane channels in catalog order.
var ChannelIDs = []string{"zen", "kilo", "workbuddy", "codearts"}

// ChannelModel is one roster row as the channel file records it: the fields
// the panel shows, nothing secret.
type ChannelModel struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxOutput     int    `json:"maxOutput,omitempty"`
	Vision        bool   `json:"vision,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
}

// ChannelFile is one channel's on-disk state.
type ChannelFile struct {
	Enabled bool `json:"enabled"`
	Models  []ChannelModel `json:"models,omitempty"`
	// RefreshedAt is when the lane last rewrote this roster (unix seconds).
	// A disabled channel deliberately keeps its last roster and timestamp:
	// the pair reads as "this is what it served, as of then" — stale, not
	// wrong. Enable toggles do not touch it.
	RefreshedAt int64 `json:"refreshedAt,omitempty"`
}

// channelsDir is <home>/channels.
func (s *Store) channelsDir() string {
	return filepath.Join(s.Home, "channels")
}

// loadChannels reads every known channel file at boot; a missing or broken
// file means "enabled, empty roster" — a hand-edited file must never keep the
// gateway from starting. Missing files are materialized right away so the
// per-channel layout is visible from the first boot on.
func (s *Store) loadChannels() {
	s.channels = map[string]*ChannelFile{}
	_ = os.MkdirAll(s.channelsDir(), 0o700)
	for _, id := range ChannelIDs {
		cf := &ChannelFile{Enabled: true}
		if f, err := loadJSON[ChannelFile](s.channelPath(id)); err == nil && f != nil {
			cf = f
		} else {
			_ = writeJSON(s.channelPath(id), cf)
		}
		s.channels[id] = cf
	}
}

func (s *Store) channelPath(id string) string {
	return filepath.Join(s.channelsDir(), id+".json")
}

// ChannelEnabled reports whether the channel's file enables it. Unknown ids
// are enabled: an unknown channel has no file to disable it.
func (s *Store) ChannelEnabled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.channels[id]
	return cf == nil || cf.Enabled
}

// SetChannelEnabled flips one channel's enable flag and writes its file.
func (s *Store) SetChannelEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.channels[id]
	if cf == nil {
		cf = &ChannelFile{}
		s.channels[id] = cf
	}
	cf.Enabled = enabled
	return writeJSON(s.channelPath(id), cf)
}

// WriteChannelRoster replaces one channel's roster in memory and on disk. The
// enable flag is preserved — the roster sync must never resurrect a channel
// the user turned off.
func (s *Store) WriteChannelRoster(id string, models []ChannelModel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cf := s.channels[id]
	if cf == nil {
		cf = &ChannelFile{Enabled: true}
		s.channels[id] = cf
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	cf.Models = models
	cf.RefreshedAt = time.Now().Unix()
	_ = writeJSON(s.channelPath(id), cf)
}

// ChannelViews snapshots every channel for the admin state.
func (s *Store) ChannelViews() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(ChannelIDs))
	for _, id := range ChannelIDs {
		cf := s.channels[id]
		if cf == nil {
			cf = &ChannelFile{Enabled: true}
		}
		out = append(out, map[string]any{
			"id": id, "enabled": cf.Enabled, "models": len(cf.Models),
			"refreshedAt": cf.RefreshedAt,
		})
	}
	return out
}
