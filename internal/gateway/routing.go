package gateway

import (
	"context"
	"strings"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/relay"
	"zen-gate/internal/store"
)

// Cross-lane routing: merged capability resolution plus the one-way fallback
// that offers a free-lane-exhausted turn to user-added providers.

// modalityCaps is one model's merged input-capability verdict. Trust order for
// the modality bools: live probe (实测) > the provider's own listing (上游声明)
// > AI tagger (AI标注) > name heuristics (规则) > unknown.
type modalityCaps struct {
	Vision        bool
	Audio         bool
	File          bool
	Reasoning     bool
	Known         bool // a modality source produced a verdict
	ContextWindow int
	MaxOutput     int
	Source        string
}

// modalityCapsOf resolves capabilities for one upstream model id (no
// namespace). Trust order for the modality bools: a live probe beats the
// provider's own listing, which beats the AI tagger, which beats the name
// heuristics. Token capacities merge per field — a source that measured the
// window but not the output cap must not discard the other's number.
func (s *Server) modalityCapsOf(upstreamModel string) modalityCaps {
	tag, hasTag := s.Store.ModelTagOf(upstreamModel)
	declared, hasDeclared := s.Store.DeclaredMeta(upstreamModel)
	var c modalityCaps
	switch {
	case hasTag && tag.Source == store.TagSourceProbe:
		c = modalityCaps{Vision: tag.Vision, Audio: tag.Audio, File: tag.File,
			Reasoning: tag.Reasoning, Known: true, Source: tag.Source}
	case hasDeclared && (declared.InputDeclared || declared.OutputDeclared):
		c = modalityCaps{Vision: declared.Vision, Audio: declared.Audio, File: declared.File,
			Reasoning: declared.Reasoning,
			// Inputs are what the router filters on; an output-only
			// declaration is not a modality verdict.
			Known: declared.InputDeclared, Source: store.TagSourceListing}
	case hasTag && !tag.CapsOnly:
		c = modalityCaps{Vision: tag.Vision, Audio: tag.Audio, File: tag.File,
			Reasoning: tag.Reasoning, Known: true, Source: tag.Source}
	default:
		if nt := lane.NameCapabilities(upstreamModel); nt.Matched {
			c = modalityCaps{Vision: nt.Vision, Audio: nt.Audio, File: nt.File,
				Reasoning: nt.Reasoning, Known: true, Source: store.TagSourceHeuris}
		}
	}
	// A CapsOnly row (the public capacity reference) states windows and
	// nothing else: modality answers come from the branches above exactly as
	// if the row did not exist, and only the source label is stamped — and
	// only where nothing stronger spoke.
	if hasTag && tag.CapsOnly && c.Source == "" {
		c.Source = tag.Source
	}
	if hasTag {
		c.ContextWindow, c.MaxOutput = tag.ContextWindow, tag.MaxOutput
	}
	if hasDeclared {
		if c.ContextWindow == 0 {
			c.ContextWindow = declared.ContextWindow
		}
		if c.MaxOutput == 0 {
			c.MaxOutput = declared.MaxOutput
		}
	}
	return c
}

// satisfiesCaps reports whether a model can carry a request's modality
// footprint. Unknown-capability models pass (soft) — the caller ranks them
// after known-capable ones; a known verdict is a hard filter.
func capsSatisfy(c modalityCaps, needs lane.Needs) bool {
	if !c.Known {
		return true
	}
	if needs.Image && !c.Vision {
		return false
	}
	if needs.Audio && !c.Audio {
		return false
	}
	if needs.File && !c.File {
		return false
	}
	return true
}

// fallbackPick is one candidate provider model for cross-lane fallback.
type fallbackPick struct {
	p     *store.Provider
	model string // upstream id
}

// providerFallbackPicks orders user-added provider models for a turn the
// free lane exhausted: the same upstream id as the requested lane model
// first, then known-capable models, then unknown-capability ones; provider
// catalog order breaks ties. With smart routing off no capability filter or
// ranking applies — plain provider order.
func (s *Server) providerFallbackPicks(needs lane.Needs, requestedBase string) []fallbackPick {
	cfg := s.Store.Config()
	smart := cfg.SmartRouting
	picks := make([]fallbackPick, 0, 8)
	late := make([]fallbackPick, 0, 8)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled || len(p.Models) == 0 {
			continue
		}
		for _, m := range p.Models {
			pick := fallbackPick{p: p, model: m}
			if !smart {
				picks = append(picks, pick)
				continue
			}
			caps := s.modalityCapsOf(m)
			if !capsSatisfy(caps, needs) {
				continue
			}
			if needs.PromptTokens > 0 && caps.ContextWindow > 0 && needs.PromptTokens > caps.ContextWindow {
				continue
			}
			if m == requestedBase {
				picks = append([]fallbackPick{pick}, picks...)
				continue
			}
			if caps.Known {
				picks = append(picks, pick)
			} else {
				late = append(late, pick)
			}
		}
	}
	return append(picks, late...)
}

// providerFallback offers a free-lane-exhausted turn to user-added providers
// in pick order, emitting lane chunks exactly like a relay turn. Returns the
// served namespaced id and outcome of the first provider that answered; ok is
// false when every provider refused (the caller then reports the original
// lane error).
func (s *Server) providerFallback(ctx context.Context, picks []fallbackPick, unified []lane.Message, tools []lane.ToolDef, maxTokens int, agent string, emit func(lane.Chunk)) (lane.Usage, string, string, *lane.UpstreamError, bool) {
	for _, pick := range picks {
		usage, finish, uerr := relay.Complete(ctx, relay.Turn{
			Provider: pick.p, Model: pick.model, Messages: unified,
			Tools: tools, MaxTokens: maxTokens, Agent: agent,
		}, emit)
		s.recordRelay(ctx, pick.p, pick.model, agent, finish != "", usage, 0, finish, uerr)
		if uerr == nil {
			s.logCat(logx.CatRouting, "info", "免费车道耗尽，已回落自定义 API: %s/%s", pick.p.ID, pick.model)
			return usage, finish, pick.p.ID + "/" + pick.model, nil, true
		}
		s.logCat(logx.CatRouting, "warn", "回落候选 %s/%s 失败: %s", pick.p.ID, pick.model, uerr.Message)
	}
	return lane.Usage{}, "", "", nil, false
}

// fallbackEnabled reports whether the one-way lane→provider fallback may run
// for this request.
func (s *Server) fallbackEnabled() bool {
	cfg := s.Store.Config()
	return cfg.LaneFallbackToProviders && len(cfg.Providers) > 0
}

// trimNamespaced splits a namespaced id for logging; lane ids pass through.
func trimNamespaced(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
