package gateway

import (
	"context"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/openref"
)

// Public capacity reference (issue #28): a scheduled pull of two keyless
// public catalogs, merged into tags.json. main wires the loop; the pull itself
// lives here because merging needs the merged-capability view and the lane's
// catalog, both of which belong to the server.
//
// Two sources, two rungs:
//
//   - models.dev (the catalog OpenCode itself ships) is keyed by the lane's own
//     ids, so its declarations may correct the curated table's family-wide regex
//     guesses — that is where a trimmed free derivative stops inheriting its
//     flagship's 1M window.
//   - OpenRouter's listing is name-normalized, so it may only fill ids nothing
//     else states (see internal/openref for the trust-order argument).

// OpenRefPull runs one fetch-and-merge cycle and reports how many models were
// written. It is safe on a cold cache: a failed fetch keeps whatever tags the
// previous round wrote.
func (s *Server) OpenRefPull(ctx context.Context) int {
	cfg := s.Store.Config()
	if !cfg.OpenRefEnabled {
		return 0
	}
	client := lane.Client()
	cat, _, _ := s.Lane.Snapshot()

	total := 0
	// Declarations first: they correct the curated guesses, and the
	// normalized pass below must not re-decide a row they just settled.
	if dev, err := openref.FetchModelsDev(ctx, client); err != nil {
		s.logCat(logx.CatApp, "warn", "models.dev 目录拉取失败（保留上次结果）: %v", err)
	} else if applied := openref.ApplyExact(s.Store, cat, dev); len(applied) > 0 {
		s.logCat(logx.CatApp, "info", "上游声明已修正 %d 个模型的能力（来源「上游声明」，实测一次即夺回）: %s",
			len(applied), openref.Summary(applied))
		total += len(applied)
	}

	meta, err := openref.Fetch(ctx, client)
	if err != nil {
		s.logCat(logx.CatApp, "warn", "公开容量参考拉取失败（保留上次结果）: %v", err)
	} else if applied := openref.Apply(s.Store, cat, meta); len(applied) > 0 {
		s.logCat(logx.CatApp, "info", "公开容量参考已补全 %d 个模型（来源记为「上游声明」，实测一次即夺回）: %s",
			len(applied), openref.Summary(applied))
		total += len(applied)
	}
	if total == 0 {
		return 0
	}
	// The fresh rows must reach the live catalog (capacities ride ModelInfo
	// into agent injection and the picker) before the broadcast below tells
	// everyone to re-read it.
	s.applyStoredTags()
	if s.modelsSync != nil {
		s.modelsSync()
	}
	return total
}

// StartOpenRefLoop pulls once at boot and every 6 hours, mirroring the
// announcement/update-check cadence.
func (s *Server) StartOpenRefLoop(ctx context.Context) {
	go func() {
		// Boot catalogs land asynchronously (RefreshCatalog runs on its own
		// goroutine); waiting past the first listing keeps the opening pull
		// from matching against an empty catalog.
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		s.OpenRefPull(ctx)
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.OpenRefPull(ctx)
			}
		}
	}()
}
