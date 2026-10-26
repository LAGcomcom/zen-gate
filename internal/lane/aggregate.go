package lane

import (
	"regexp"
	"strings"
)

// Cross-channel aggregation: one logical model, several channels.
//
// The lane's catalog already lists every channel's roster in one slice, so the
// same model can appear twice under two ids: `deepseek-v4.1-flash` is served by
// the WorkBuddy account pool, while OpenCode Zen serves the same model as
// `deepseek-v4.1-flash-free`. Today a request for one of them can only fail
// over to a *different model* (nextCandidate walks the whole catalog by
// availability + latency), which changes what the user asked for. Grouping the
// twins lets failover keep the model and switch the channel instead.
//
// ## Conservative by construction: id shape alone never merges
//
// The source plugin's aggregation learns this the hard way and writes it down:
// merging two different models is unrecoverable, because the request silently
// lands on a model the user did not pick. Splitting one model into two rows is
// visible and harmless — the user just sees both.
//
// So a group here needs BOTH:
//
//  1. the same normalized id (namespace prefix, `:free` suffix and `-free`
//     suffix stripped), AND
//  2. a compatible context window — within aggregateContextTolerance of each
//     other, smallest against largest.
//
// (2) is what stops the one pair this catalog really has that a naive id match
// would destroy: `nemotron-3.5-lightning-free` (zen, 128000) and
// `nvidia/nemotron-3.5-lightning:free` (kilo, 1000000) normalize to the same
// key but are not the same product — 7.8x apart. Failing a 128K prompt over to
// the 1M variant is not "the same model on another channel"; it is a different
// deployment with different quality and price.
//
// The remaining tolerance is not sloppiness: `deepseek-v4.1-flash` is 1000000
// on workbuddy and 1048576 on zen (1024*1024 vs 10^6) and
// `nemotron-3.5-super-vl` is 1000000 vs 1048576 — the same model, published at
// slightly different precision by two channels. A strict equality test would
// refuse to group the very pairs this feature exists for.

// aggregateContextTolerance is the largest ratio (bigger/smaller) between two
// candidates' context windows that still counts as the same model.
//
// 1.15 accepts 1000000 vs 1048576 (1.0486) and rejects 128000 vs 1000000
// (7.8125). Chosen from the catalog's actual rows, not invented: every real
// twin measured either matches to within 5% or is off by more than 7x, so the
// gap between those two clusters is wide enough that the exact number is not
// delicate.
const aggregateContextTolerance = 1.15

var (
	// A namespace prefix (`nvidia/`, `reflection/`) names the upstream vendor,
	// not the model, and is not stable across channels.
	aggregateNamespaceRe = regexp.MustCompile(`^.*/`)
	// `:free` and `-free` are this gateway's free-lane markers, added by
	// whichever channel listed the row. They are not part of the model name.
	//
	// ⚠️ Go's regexp has no lookbehind, and the naive `[-_:]free$` swallows the
	// separator with the word: `beam-free` -> `beam`, which looks right until
	// the word ends in the same letter as the separator rule touches —
	// `deepseek-v4.1-flash-free` lost its `h` and `kilo-auto/free` collapsed to
	// an empty key. The capture group puts the separator back.
	aggregateSuffixRe = regexp.MustCompile(`([-_:])(?:free)+$`)
	// Effort suffixes are the client's own knob, already handled separately.
	aggregateEffortRe = regexp.MustCompile(`\s*\((?:light|balanced|deep)\)$`)
)

// AggregateKey normalizes a model id to the key that groups cross-channel
// twins. It returns "" for ids that must never join a group.
func AggregateKey(modelID string) string {
	s := strings.TrimSpace(strings.ToLower(modelID))
	s = aggregateEffortRe.ReplaceAllString(s, "")
	// Strip the free markers from the end, repeatedly, and keep the separator
	// that preceded them. Done as a loop rather than one regex because Go's
	// regexp has no lookbehind: any single-pattern form either swallows the
	// separator with the word (`beam-free` -> `bea`) or leaves the separator
	// in the key (`…-vl:free` -> `…-vl:`).
	//
	// ⚠️ This runs BEFORE the namespace strip. `kilo-auto/free` carries a
	// marker on an unprefixed id, so stripping namespaces first would delete
	// `kilo-auto/` as if it were a vendor prefix and leave a bare `free`.
	for {
		trimmed := strings.TrimSuffix(s, "free")
		if trimmed == s {
			break
		}
		// A marker only counts when a separator precedes it, otherwise a model
		// whose name happens to end in `free` would lose real letters.
		if len(trimmed) == 0 {
			s = ""
			break
		}
		if sep := trimmed[len(trimmed)-1]; sep != '-' && sep != '_' && sep != ':' && sep != '/' {
			break
		}
		s = trimmed[:len(trimmed)-1]
		// The separator is consumed with the marker; a namespace form leaves a
		// trailing `/` that must not survive into the key either.
		s = strings.TrimSuffix(s, "/")
	}
	s = aggregateNamespaceRe.ReplaceAllString(s, "")
	// Dots carry a version (`v4.1`); normalize them so `v4.1` and `v4-1` meet.
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return ""
	}
	// A key that is nothing but a marker (`free`, `auto`) says nothing about
	// which model it is; grouping on it would merge unrelated rows.
	switch s {
	case "free", "auto":
		return ""
	}
	return s
}

// aggregateCompatible reports whether two context windows belong to the same
// model. A zero window means "unknown" — unknown never proves sameness, so it
// only groups with another unknown of the same key.
func aggregateCompatible(a, b int) bool {
	if a <= 0 || b <= 0 {
		return a == b
	}
	lo, hi := a, b
	if lo > hi {
		lo, hi = hi, lo
	}
	return float64(hi)/float64(lo) <= aggregateContextTolerance
}

// AggregateGroup is one logical model and the channels that serve it.
type AggregateGroup struct {
	// Key is the normalized id shared by every member.
	Key string
	// Members are the catalog ids that serve this model, in catalog order.
	// The first member is the one the client already asked for, when the
	// caller built the group around a request.
	Members []string
	// Windows maps member id -> its declared context window, so a failover
	// target can be checked against the request size before it is used.
	Windows map[string]int
}

// AggregateGroups builds the cross-channel groups for one catalog snapshot.
//
// Only groups with at least two members are returned: a group of one is just
// the catalog row itself and needs no aggregation. The blank channel is the zen
// lane, so members are ordered by catalog index — deterministic across calls.
func AggregateGroups(catalog []ModelInfo) map[string]*AggregateGroup {
	type bucket struct {
		key     string
		members []ModelInfo
	}
	byKey := make(map[string]*bucket, len(catalog))
	order := make([]string, 0, len(catalog))
	for _, m := range catalog {
		if m.SystemOne || m.Wire == "systemone" || m.ID == "" {
			continue
		}
		key := AggregateKey(m.ID)
		if key == "" {
			continue
		}
		b, ok := byKey[key]
		if !ok {
			b = &bucket{key: key}
			byKey[key] = b
			order = append(order, key)
		}
		b.members = append(b.members, m)
	}

	out := make(map[string]*AggregateGroup, len(byKey))
	for _, key := range order {
		b := byKey[key]
		if len(b.members) < 2 {
			continue
		}
		// Split the bucket into compatible sub-groups. A bucket normally holds
		// one pair; when it holds two incompatible windows, each window shape
		// becomes its own group rather than one merged group.
		groups := make([]*AggregateGroup, 0, 1)
		for _, m := range b.members {
			placed := false
			for _, g := range groups {
				if aggregateCompatible(g.Windows[g.Members[0]], m.ContextWindow) {
					g.Members = append(g.Members, m.ID)
					g.Windows[m.ID] = m.ContextWindow
					placed = true
					break
				}
			}
			if !placed {
				groups = append(groups, &AggregateGroup{
					Key:     key,
					Members: []string{m.ID},
					Windows: map[string]int{m.ID: m.ContextWindow},
				})
			}
		}
		for _, g := range groups {
			if len(g.Members) < 2 {
				continue
			}
			out[g.Members[0]] = g
			for _, id := range g.Members[1:] {
				out[id] = g
			}
		}
	}
	return out
}

// aggregateAlternates returns the other members of id's group, in catalog
// order, skipping ids the caller already tried and windows too small for the
// request.
//
// The window check is the reason a group stores windows at all: a failover that
// silently overflows upstream looks like a model failure and burns the turn.
func aggregateAlternates(group *AggregateGroup, id string, promptTokens int, tried map[string]bool) []string {
	if group == nil {
		return nil
	}
	out := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		if member == id || tried[member] {
			continue
		}
		if w := group.Windows[member]; promptTokens > 0 && w > 0 && promptTokens > applyHeadroom(w) {
			continue
		}
		out = append(out, member)
	}
	return out
}
