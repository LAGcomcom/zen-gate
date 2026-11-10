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
	// Efforts are the thinking rungs the serving provider declared for this
	// model, in its own naming. Empty means nothing was declared, and the
	// gateway's own budget ladder is what the picker shows.
	Efforts       []string
	EffortDefault string
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
		// The rungs ride the declared row whatever else won the capability
		// branch: they are not a guess about the model, they are the names the
		// vendor's own endpoint answers to.
		c.Efforts = declared.Efforts
		c.EffortDefault = declared.EffortDefault
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

// fallbackPick is one step of the relay order: a free-lane model or a
// user-added provider's model.
type fallbackPick struct {
	source string // "lane" | "provider"
	p      *store.Provider
	model  string // upstream id
}

// chainMode reports whether the user-ordered relay chain replaces the built-in
// failover order. An enabled-but-empty chain counts as off so a half-configured
// switch cannot silently take the lane's own failover away.
func (s *Server) chainMode() bool {
	cfg := s.Store.Config()
	return cfg.RelayChainEnabled && len(cfg.RelayChain) > 0
}

// fallbackPicks orders the steps a failed turn walks. With the relay chain on,
// it is the user's order verbatim — lane and provider entries interleaved.
// Otherwise the legacy behaviour: provider catalog order, optionally ranked by
// capability for smart routing.
func (s *Server) fallbackPicks(needs lane.Needs, requestedBase string) []fallbackPick {
	cfg := s.Store.Config()
	if cfg.RelayChainEnabled && len(cfg.RelayChain) > 0 {
		return s.chainPicks(cfg)
	}
	smart := cfg.SmartRouting
	picks := make([]fallbackPick, 0, 8)
	late := make([]fallbackPick, 0, 8)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.Enabled || len(p.Models) == 0 {
			continue
		}
		for _, m := range p.Models {
			pick := fallbackPick{source: "provider", p: p, model: m}
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

// chainPicks resolves the configured chain into concrete steps, preserving the
// user's order. Invalid entries (blank model, unknown or disabled provider)
// are skipped with a log line; duplicates are dropped so one misconfiguration
// cannot cost the same upstream call twice.
func (s *Server) chainPicks(cfg *store.Config) []fallbackPick {
	picks := make([]fallbackPick, 0, len(cfg.RelayChain))
	seen := make(map[string]bool, len(cfg.RelayChain))
	for _, link := range cfg.RelayChain {
		model := strings.TrimSpace(link.Model)
		if model == "" {
			continue
		}
		var pick fallbackPick
		switch link.Source {
		case "lane":
			pick = fallbackPick{source: "lane", model: model}
		case "provider":
			var p *store.Provider
			for i := range cfg.Providers {
				if cfg.Providers[i].ID == link.Provider {
					p = &cfg.Providers[i]
					break
				}
			}
			if p == nil || !p.Enabled {
				s.logCat(logx.CatRouting, "warn", "接力链跳过不可用的供应商: %s", link.Provider)
				continue
			}
			pick = fallbackPick{source: "provider", p: p, model: model}
		default:
			continue
		}
		key := pick.source + "\x00" + pick.model
		if pick.p != nil {
			key = pick.source + "\x00" + pick.p.ID + "\x00" + pick.model
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		picks = append(picks, pick)
	}
	return picks
}

// chainTurn carries the request facts every relay step re-issues on a
// different model or provider.
type chainTurn struct {
	messages    []lane.Message
	tools       []lane.ToolDef
	maxTokens   int
	agent       string
	needs       lane.Needs
	sessionSeed string
	turnSeed    string
}

// chainResult is the outcome of walking the relay order.
type chainResult struct {
	Usage  lane.Usage
	Finish string
	Served string
	// Err is the last step's failure, so the caller can report why the chain
	// gave up instead of the original lane error.
	Err *lane.UpstreamError
	// Emitted means some step put output on the wire before failing: the turn
	// is committed and must not be replayed on the next step.
	Emitted bool
	OK      bool
}

// runChain walks the relay steps in order, emitting lane chunks exactly like a
// normal turn. It stops at the first step that answers, and also at the first
// step that starts emitting and then fails — replaying the request elsewhere
// would splice two answers together.
func (s *Server) runChain(ctx context.Context, picks []fallbackPick, t chainTurn, emit func(lane.Chunk)) chainResult {
	var res chainResult
	for i, pick := range picks {
		if ctx.Err() != nil {
			break
		}
		saw := false
		wrapped := func(c lane.Chunk) {
			saw = true
			emit(c)
		}
		if pick.source == "lane" {
			out, uerr := s.Lane.Complete(ctx, lane.Request{
				Model: pick.model, Messages: t.messages, Tools: t.tools,
				MaxTokens: t.maxTokens, SessionSeed: t.sessionSeed, TurnSeed: t.turnSeed,
				Agent: t.agent, Needs: t.needs, NoModelSwitch: true,
			}, wrapped)
			if uerr == nil {
				served := out.ServedModel
				if served == "" {
					served = pick.model
				}
				s.logCat(logx.CatRouting, "info", "接力链第 %d 步已接管: %s", i+1, served)
				return chainResult{Usage: out.Usage, Finish: out.Finish, Served: served, OK: true}
			}
			res.Err = uerr
			if saw {
				res.Emitted = true
				return res
			}
			s.logCat(logx.CatRouting, "warn", "接力链第 %d 步 %s 失败: %s", i+1, pick.model, uerr.Message)
			continue
		}
		usage, finish, uerr := relay.Complete(ctx, relay.Turn{
			Provider: pick.p, Model: pick.model, Messages: t.messages,
			Tools: t.tools, MaxTokens: t.maxTokens, Agent: t.agent,
			// No thinking rung is forwarded: the step serves a different
			// vendor's model than the caller picked, and a level chosen for
			// that model says nothing about this one.
		}, wrapped)
		s.recordRelay(ctx, pick.p, pick.model, t.agent, finish != "", usage, 0, finish, uerr)
		if uerr == nil {
			if s.chainMode() {
				s.logCat(logx.CatRouting, "info", "接力链第 %d 步已接管: %s/%s", i+1, pick.p.ID, pick.model)
			} else {
				// Keep the legacy wording for the one-way fallback: it is what
				// the dashboard and the log docs describe.
				s.logCat(logx.CatRouting, "info", "免费车道耗尽，已回落自定义 API: %s/%s", pick.p.ID, pick.model)
			}
			return chainResult{Usage: usage, Finish: finish, Served: pick.p.ID + "/" + pick.model, OK: true}
		}
		res.Err = uerr
		if saw {
			res.Emitted = true
			return res
		}
		if s.chainMode() {
			s.logCat(logx.CatRouting, "warn", "接力链第 %d 步 %s/%s 失败: %s", i+1, pick.p.ID, pick.model, uerr.Message)
		} else {
			s.logCat(logx.CatRouting, "warn", "回落候选 %s/%s 失败: %s", pick.p.ID, pick.model, uerr.Message)
		}
	}
	return res
}

// fallbackEnabled reports whether any cross-source rescue may run for this
// request: the user-ordered chain, or the legacy one-way lane→provider
// fallback.
func (s *Server) fallbackEnabled() bool {
	cfg := s.Store.Config()
	if cfg.RelayChainEnabled && len(cfg.RelayChain) > 0 {
		return true
	}
	return cfg.LaneFallbackToProviders && len(cfg.Providers) > 0
}

// trimNamespaced splits a namespaced id for logging; lane ids pass through.
func trimNamespaced(id string) string {
	if i := strings.Index(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
