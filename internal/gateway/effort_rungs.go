package gateway

import (
	"strings"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// Vendor-declared thinking rungs.
//
// A provider that states its own levels owns the picker: the gateway advertises
// those names, because they are the strings its endpoint answers to. The
// light/balanced/deep ladder is the gateway's invention — a token-budget
// policy for sources that state nothing — and presenting it as a vendor's
// vocabulary would send "balanced" to an upstream whose rungs are
// "low/medium/xhigh".

// rungLevelFor maps a vendor rung onto the gateway's budget level so the rung
// still carries a real output ceiling in its badge.
func rungLevelFor(rung string) string {
	switch strings.ToLower(strings.TrimSpace(rung)) {
	case "none", "off", "disabled", "minimal":
		return "light"
	case "low", "light":
		return "light"
	case "medium", "balanced":
		return "balanced"
	default: // high, xhigh, max and anything heavier
		return "deep"
	}
}

// rungZH names one vendor rung for the picker. An unrecognised name keeps
// itself rather than being dressed in someone else's Chinese label.
var rungZH = map[string]string{
	"minimal":  "最简",
	"low":      "精简",
	"light":    "精简",
	"medium":   "均衡",
	"balanced": "均衡",
	"high":     "深思",
	"xhigh":    "极深",
	"max":      "极限",
}

// effortLadder builds the picker list for one model. Declared rungs win; the
// gateway's own budget ladder is the fallback for every source that states
// nothing, and the returned default is the configured one in that case.
func effortLadder(floor lane.ModelInfo, caps modalityCaps, cfg *store.Config) ([]lane.LevelBudget, string) {
	if len(caps.Efforts) == 0 {
		return lane.EffortsFor(floor, 0, cfg.DefaultMaxTokens, cfg.DefaultEffort), cfg.DefaultEffort
	}
	rungs := caps.Efforts
	def := caps.EffortDefault
	if def == "" {
		// Nothing starred: the configured default, if this vendor happens to
		// use that name, otherwise the middle of what it declared.
		def = rungIdx(rungs, cfg.DefaultEffort)
		if def == "" {
			def = rungs[(len(rungs)-1)/2]
		}
	}
	// A model whose thinking cannot be switched off shares one ceiling between
	// reasoning and the answer, which is what BudgetFor's factor encodes.
	info := floor
	info.Reasoning = true
	out := make([]lane.LevelBudget, 0, len(rungs))
	for _, rung := range rungs {
		out = append(out, lane.LevelBudget{
			ID:        rung,
			Name:      rung,
			ZH:        rungZH[strings.ToLower(rung)],
			Tokens:    lane.BudgetFor(rungLevelFor(rung), info, 0, cfg.DefaultMaxTokens),
			IsDefault: rung == def,
		})
	}
	return out, def
}

// rungIdx returns the declared rung named want, or "" when the vendor has no
// such rung.
func rungIdx(rungs []string, want string) string {
	for _, r := range rungs {
		if strings.EqualFold(r, want) {
			return r
		}
	}
	return ""
}

// forwardEffort decides what reasoning_effort the provider actually receives.
// The client's own word wins when the vendor knows it; the gateway's budget
// names map onto the vendor's nearest rung so a picker set to "deep" still
// reaches an upstream that only speaks "xhigh"; anything the vendor does not
// declare is passed through untouched, because a name the gateway has never
// seen is still the caller's claim about a source it may know better.
func forwardEffort(caps modalityCaps, requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ""
	}
	// An explicit "think not at all" must never be softened into the lightest
	// rung: a rung still thinks, and this vendor can switch it off.
	if isEffortOff(requested) {
		return requested
	}
	if len(caps.Efforts) == 0 {
		return requested
	}
	if r := rungIdx(caps.Efforts, requested); r != "" {
		return r
	}
	// light/balanced/deep are the gateway's own vocabulary; map it.
	level := rungLevelFor(requested)
	want := map[string]string{"light": "low", "balanced": "medium", "deep": "high"}[level]
	if r := rungIdx(caps.Efforts, want); r != "" {
		return r
	}
	// The vendor has rungs but none matches: take the heaviest for "deep", the
	// lightest for "light", and the declared default for anything in between,
	// rather than sending a word that will be ignored.
	switch level {
	case "deep":
		return rungAt(caps.Efforts, false)
	case "light":
		return rungAt(caps.Efforts, true)
	}
	if d := caps.EffortDefault; d != "" {
		return d
	}
	return rungAt(caps.Efforts, false)
}

// rungRank is how heavy one vendor rung is. An unrecognised name sorts after
// every known one, so a private ladder still keeps its order.
func rungRank(name string) int {
	if r, ok := effortRungRank[strings.ToLower(strings.TrimSpace(name))]; ok {
		return r
	}
	return 100 + len(name)
}

// rungAt picks the lightest (light) or heaviest rung by rank, not by position:
// the listing parser's order is a convenience, and a hand-edited config may
// state rungs any which way.
func rungAt(rungs []string, light bool) string {
	if len(rungs) == 0 {
		return ""
	}
	best, bestRank := rungs[0], rungRank(rungs[0])
	for _, r := range rungs[1:] {
		rk := rungRank(r)
		if (light && rk < bestRank) || (!light && rk > bestRank) {
			best, bestRank = r, rk
		}
	}
	return best
}

// isEffortOff recognises the caller asking for no thinking at all.
func isEffortOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "off", "disabled", "false", "no", "not", "stop":
		return true
	}
	return false
}

// relayEffort is the reasoning_effort one relayed turn sends upstream.
//
// The caller's own word comes first and is never collapsed: "xhigh" that
// reaches a vendor with high and xhigh rungs must stay xhigh. With nothing
// declared, only a provider that publishes rungs gets the configured default —
// a level silently added to a request for a source that states nothing would
// be an unclaimed change to someone else's working call.
func (s *Server) relayEffort(gatewayID, upstream, declared string) string {
	caps := s.modalityCapsOf(upstream)
	if d := strings.TrimSpace(declared); d != "" {
		return forwardEffort(caps, d)
	}
	if len(caps.Efforts) == 0 {
		return ""
	}
	return forwardEffort(caps, s.resolveEffort(gatewayID))
}
