// models.dev source: the catalog OpenCode itself ships.
//
// The OpenRouter pull above can only speak where the curated table is silent,
// because its rows are name-normalized: "deepseek-v4.1-flash-free" has to be
// matched against a vendor's own "deepseek/deepseek-v4.1-flash" and two vendors
// collapsing onto one key have to be buried. That is the right discipline for a
// cross-vendor catalog, and it is also why the free lane kept publishing a
// stranger's window: the curated table's regex rungs (`^deepseek` → 1M) answer
// for every member of a family, so a trimmed free derivative inherits the
// flagship's capacity and nothing is allowed to correct it.
//
// sst/models.dev is a different kind of source: its `opencode` section is keyed
// by the very ids the free lane serves (deepseek-v4.1-flash-free, mimo-v2.6-
// flash-free, …) and carries the capacities OpenCode itself publishes for them.
// Ids are exact, so there is no normalization and no collision to guess about,
// which lets this source sit on its proper rung — 上游声明, above the binary's
// own 规则 rungs and below a live 实测 — instead of only filling blanks.
package openref

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// ModelsDevBase is the catalog root; overridable for tests.
var ModelsDevBase = "https://models.dev/api.json"

// modelsDevSections are the catalog sections whose ids mirror the free lane's.
// Only the Zen section qualifies: the Kilo pool declares its own capacities in
// its listing, and its ids are namespaced exactly as ours are.
var modelsDevSections = []string{"opencode"}

// modelsDevDoc is the part of the document this package reads: provider →
// models → id → declared limits.
type modelsDevDoc map[string]struct {
	Models map[string]struct {
		Limit struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		} `json:"limit"`
	} `json:"models"`
}

// FetchModelsDev pulls the catalog and returns capacities keyed by the exact
// model id. Only rows stating at least one capacity are kept.
func FetchModelsDev(ctx context.Context, client *http.Client) (map[string]ModelMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevBase, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 models.dev 目录失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("拉取 models.dev 目录失败: HTTP %d", resp.StatusCode)
	}
	// The live document is ~6 MB and grows with the catalog; 64 MB is a
	// generous ceiling that still bounds a hostile or broken response.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var doc modelsDevDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 models.dev 目录失败: %w", err)
	}
	out := map[string]ModelMeta{}
	for _, section := range modelsDevSections {
		provider, ok := doc[section]
		if !ok {
			continue
		}
		for id, row := range provider.Models {
			m := ModelMeta{ContextWindow: row.Limit.Context, MaxOutput: row.Limit.Output}
			if id == "" || (m.ContextWindow <= 0 && m.MaxOutput <= 0) {
				continue
			}
			out[id] = m
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models.dev 目录里没有 %v 段声明任何容量", modelsDevSections)
	}
	return out, nil
}

// ApplyExact writes the declared capacities for every free-lane model the
// catalog knows, on the trust rung this source has earned:
//
//   - a 实测 row is left alone: a measurement outranks a declaration;
//   - a 规则 row (the curated table's guess, which is what a model without a
//     tags row is served from) is corrected — this is the whole point of the
//     source;
//   - a lower-rung row that carries modality verdicts keeps them: this source
//     states capacities only, and replacing the row outright would silence a
//     modality some other source measured (see store.ModelTag.CapsOnly).
//
// Rows already stating exactly these capacities are not rewritten, so the six
// hourly pull does not churn tags.json.
func ApplyExact(st *store.Store, cat []lane.ModelInfo, meta map[string]ModelMeta) []mergeLog {
	if len(meta) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	taken := st.SnapshotTags()
	var applied []mergeLog
	for _, m := range cat {
		if m.Channel != "" {
			continue // the Kilo pool carries its own declared capacities
		}
		base := lane.BaseModelId(m.ID)
		row, has := taken[base]
		if has && row.Source == store.TagSourceProbe {
			continue // 实测 > 上游声明
		}
		mm, ok := meta[base]
		if !ok {
			continue // the catalog does not list this id: never guess
		}
		if mm.ContextWindow == 0 && mm.MaxOutput == 0 {
			continue
		}
		if has && row.Source == store.TagSourceListing &&
			row.ContextWindow == mm.ContextWindow && row.MaxOutput == mm.MaxOutput {
			continue // already stated, nothing to say
		}
		tag := store.ModelTag{
			CapsOnly:      true,
			ContextWindow: mm.ContextWindow,
			MaxOutput:     mm.MaxOutput,
			Source:        store.TagSourceListing,
			At:            now,
		}
		if has && !row.CapsOnly {
			// Per-field trust: the capacities come from the declaration, the
			// modality bools stay whatever the previous (lower-rung) verdict
			// said rather than being silently dropped.
			tag.Vision, tag.Audio, tag.File, tag.Reasoning = row.Vision, row.Audio, row.File, row.Reasoning
			tag.CapsOnly = false
		}
		st.SetModelTag(base, tag)
		applied = append(applied, mergeLog{ID: base, Ctx: mm.ContextWindow, Out: mm.MaxOutput})
	}
	sort.Slice(applied, func(i, j int) bool { return applied[i].ID < applied[j].ID })
	return applied
}
