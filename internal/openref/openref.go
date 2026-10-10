// Package openref fills capacity fields (context window / max output) for
// lane models the local table states nothing about, from a public model
// catalog (issue #28). The OpenCode Zen listing ships ids only — no
// capacity fields — so a model that joined yesterday gets the fallback
// guess (131072/32768) until someone hand-edits the table.
//
// The design is deliberately narrower than "import OpenRouter's metadata":
// the public listing describes the official model, while the free lane often
// serves a routed or trimmed variant, and an anonymous lane id can collide
// with a similarly-named third-party model. So this source is allowed to
// speak only where nothing else has: it NEVER touches a model the curated
// table matches, and NEVER touches a model that already has a tags.json row
// (probe, provider declaration, or AI verdict — 实测 > 上游声明 > AI标注 >
// 规则 stays enforced by not competing with the higher rungs). What it writes
// is a CapsOnly row: the capacities, and no modality claims. A first 能力实测
// or re-tag replaces the row wholesale, exactly like any other source.
package openref

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// Base is the catalog root; overridable for tests.
var Base = "https://openrouter.ai/api/v1"

// ModelMeta is one published capacity pair.
type ModelMeta struct {
	ContextWindow int
	MaxOutput     int
}

// Fetch pulls the public listing and returns capacities keyed by normalized
// base id. Only rows stating at least one capacity are kept.
func Fetch(ctx context.Context, client *http.Client) (map[string]ModelMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取公开模型目录失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("拉取公开模型目录失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			TopProvider   struct {
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析公开模型目录失败: %w", err)
	}
	out := map[string]ModelMeta{}
	tombstones := map[string]bool{}
	for _, row := range doc.Data {
		key := Normalize(row.ID)
		if key == "" || tombstones[key] {
			continue
		}
		m := ModelMeta{ContextWindow: row.ContextLength, MaxOutput: row.TopProvider.MaxCompletionTokens}
		if m.ContextWindow <= 0 && m.MaxOutput <= 0 {
			continue
		}
		// Two vendor rows collapsing onto one base id is exactly the
		// same-name-different-model hazard the design refuses to guess
		// about: bury the key for good rather than pick a winner — a third
		// row must not resurrect it by re-inserting over the deletion.
		if _, dup := out[key]; dup {
			delete(out, key)
			tombstones[key] = true
			continue
		}
		out[key] = m
	}
	return out, nil
}

// freeLaneSuffixRe strips the lane's own tier markers from a base id — and
// only the markers: "preview" is model-name content (longcat-2.5-preview),
// stripping it would collide a preview build with its stable sibling.
var freeLaneSuffixRe = regexp.MustCompile(`(?:-(?:free|contributor))+$`)
// versionSepRe unifies underscore spelling with the dominant dash form;
// dots stay — version numbers ("1.3") must survive to match on both sides.
var versionSepRe = regexp.MustCompile(`_+`)

// Normalize maps a catalog id to the comparable core: vendor prefix dropped,
// lane tier suffixes (free/preview/contributor/…) dropped, separators
// unified to '-', lower-cased. stepfun/step-5-preview-free and
// step-5-preview both land on "step-5-preview".
func Normalize(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(freeLaneSuffixRe.ReplaceAllString(s, ""))
	s = versionSepRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// mergeLog is one applied model for the summary line.
type mergeLog struct {
	ID    string
	Ctx   int
	Out   int
}

// Apply fills capacities into the tag store for lane models nothing else
// states. cat is the current lane catalog (free lane only); rows already in
// tags.json, ids the curated table matches, and ids with a collision in meta
// (already dropped by Fetch) are skipped. Returns what was applied.
func Apply(st *store.Store, cat []lane.ModelInfo, meta map[string]ModelMeta) []mergeLog {
	if len(meta) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	taken := st.SnapshotTags()
	var applied []mergeLog
	for _, m := range cat {
		if m.Channel != "" {
			continue // kilo rows carry the pool's own declared capacities
		}
		base := lane.BaseModelId(m.ID)
		if lane.CapabilityMatched(base) {
			continue // the curated table is curated on purpose
		}
		if _, has := taken[base]; has {
			continue // any existing verdict outranks a capacity hint
		}
		key := Normalize(base)
		mm, ok := meta[key]
		if !ok {
			continue
		}
		st.SetModelTag(base, store.ModelTag{
			CapsOnly:      true,
			ContextWindow: mm.ContextWindow,
			MaxOutput:     mm.MaxOutput,
			Source:        store.TagSourceListing,
			At:            now,
		})
		applied = append(applied, mergeLog{ID: base, Ctx: mm.ContextWindow, Out: mm.MaxOutput})
	}
	sort.Slice(applied, func(i, j int) bool { return applied[i].ID < applied[j].ID })
	return applied
}

// Summary renders the applied list for one log line.
func Summary(applied []mergeLog) string {
	parts := make([]string, 0, len(applied))
	for _, a := range applied {
		parts = append(parts, fmt.Sprintf("%s(ctx=%d,out=%d)", a.ID, a.Ctx, a.Out))
	}
	return strings.Join(parts, ", ")
}
