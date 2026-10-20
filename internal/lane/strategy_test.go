package lane

import "testing"

// strategyLane is the roster the routing rules rank over: a vision carrier
// first, then a text-only row with the smallest window, then two more vision
// carriers. The assertions below depend on these traits and this order only —
// the retired Zen roster is gone, so the names are local to the test.
func strategyLane() *Lane {
	l := NewLane()
	l.SetCatalog([]ModelInfo{
		{ID: "space-bunny-free", Name: "Space Bunny", Wire: "chat", Vision: true,
			ContextWindow: 1048576, MaxOutput: 65536},
		{ID: "nemotron-3.5-lightning-free", Name: "Nemotron 3.5 Lightning", Wire: "chat",
			ContextWindow: 128000, MaxOutput: 32768},
		{ID: "ll-alpha-free", Name: "LL Alpha", Wire: "chat", Vision: true,
			ContextWindow: 1048576, MaxOutput: 131072},
		{ID: "mimo-v2.6-flash-free", Name: "MiMo V2.6 Flash", Wire: "chat", Vision: true,
			ContextWindow: 1048576, MaxOutput: 131072},
	})
	return l
}

// throttleAll parks every catalog model except the named keeps, so
// nextCandidate picks from a tiny deterministic pool.
func throttleAll(l *Lane, keep ...string) {
	kept := map[string]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	for _, m := range l.catalog {
		if !kept[m.ID] {
			l.MarkThrottled(m.ID, 120)
		}
	}
}

// Fallback catalog order: mimo-v2.6, deepseek-v4.1, space-bunny,
// nemotron-3.5-lightning, nemotron-3.5-super-vl, ll-alpha, ...
// Vision carriers per the measured table: mimo*, muse*, space-bunny,
// deepseek-v4.1, ll-alpha, nemotron-super-vl; text-only: nemotron-lightning.
// The strategy tests need samples with distinct traits, not specific
// models: they were rewritten when ling-3.0 / nemotron-3-ultra left the
// upstream roster, and keep using whichever roster members carry the
// trait each assertion is about.

func TestNextCandidateFiltersByNeeds(t *testing.T) {
	l := strategyLane()
	// nemotron-lightning is text-only and sorts before the vision carriers.
	throttleAll(l, "nemotron-3.5-lightning-free", "space-bunny-free")

	// A vision request must skip the text-only nemotron even though it comes
	// first in catalog order.
	if got := l.nextCandidate(Needs{Image: true}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("vision request: next = %q, want space-bunny-free", got)
	}
	// Without a modality footprint the same pool keeps catalog order —
	// space-bunny sits ahead of nemotron-lightning in the roster.
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("no needs: next = %q, want space-bunny-free", got)
	}
}

func TestNextCandidateContextFit(t *testing.T) {
	l := strategyLane()
	throttleAll(l, "nemotron-3.5-lightning-free") // 128k window, the smallest here
	if got := l.nextCandidate(Needs{PromptTokens: 200000}, map[string]bool{}); got != "" {
		t.Fatalf("oversized request: next = %q, want none", got)
	}
	if got := l.nextCandidate(Needs{PromptTokens: 1000}, map[string]bool{}); got != "nemotron-3.5-lightning-free" {
		t.Fatalf("small request: next = %q, want nemotron-3.5-lightning-free", got)
	}
}

func TestNextCandidateLatencyStrategy(t *testing.T) {
	l := strategyLane()
	throttleAll(l, "nemotron-3.5-lightning-free", "space-bunny-free")
	l.SetStrategy(StrategyLatency)
	l.SetTTFTSource(func(model string) int64 {
		if model == "space-bunny-free" {
			return 120
		}
		return 900
	})
	// Both candidates are unprobed (same availability band); the measured-fast
	// one wins even though nemotron-lightning comes first in catalog order.
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("latency strategy: next = %q, want space-bunny-free", got)
	}
	// Catalog strategy on the same pool: listing order.
	l.SetTTFTSource(nil)
	l.SetStrategy(StrategyCatalog)
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("catalog strategy: next = %q, want space-bunny-free", got)
	}
}

func TestSmartRoutingOffRestoresLegacy(t *testing.T) {
	l := strategyLane()
	l.SetSmartRouting(false)
	throttleAll(l, "ll-alpha-free", "space-bunny-free")
	// Legacy ignores the modality filter: a vision request still lands on
	// space-bunny purely by catalog position, which is exactly what "off" means.
	if got := l.nextCandidate(Needs{Image: true}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("legacy: next = %q, want space-bunny-free", got)
	}
	// Legacy skips the context check too: a 10M-token prompt still finds one.
	if got := l.nextCandidate(Needs{PromptTokens: 10 << 20}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("legacy oversized: next = %q, want space-bunny-free", got)
	}
}

func TestApplyCapabilityTags(t *testing.T) {
	l := strategyLane()
	yes, no := true, false
	if !l.ApplyCapabilityTags("mimo-v2.6-flash-free", CapabilityTags{Audio: &yes, File: &yes}) {
		t.Fatal("lane model not found")
	}
	cat, _, _ := l.Snapshot()
	for _, m := range cat {
		if m.ID == "mimo-v2.6-flash-free" {
			if !m.AudioInput || !m.FileInput {
				t.Fatalf("audio/file tags not applied: %+v", m)
			}
		}
	}
	// Vision is upgrade-only: a false verdict must not silence a
	// probe-verified capability.
	if !l.ApplyCapabilityTags("mimo-v2.6-flash-free", CapabilityTags{Vision: &no}) {
		t.Fatal("lane model not found on second apply")
	}
	cat, _, _ = l.Snapshot()
	for _, m := range cat {
		if m.ID == "mimo-v2.6-flash-free" && !m.Vision {
			t.Fatal("vision must be upgrade-only")
		}
	}
	// Unknown (non-lane) ids are rejected.
	if l.ApplyCapabilityTags("gpt-4o", CapabilityTags{Audio: &yes}) {
		t.Fatal("provider id must not match the lane catalog")
	}
}

func TestNormalizeStrategy(t *testing.T) {
	for _, in := range []string{"", "catalog", "bogus"} {
		if NormalizeStrategy(in) != StrategyCatalog {
			t.Fatalf("NormalizeStrategy(%q) = %s", in, NormalizeStrategy(in))
		}
	}
	if NormalizeStrategy("latency") != StrategyLatency {
		t.Fatal("latency must survive normalization")
	}
}

func TestSummarizeNeeds(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Parts: []Part{TextPart{Text: "sys"}}},
		{Role: RoleUser, Parts: []Part{
			TextPart{Text: "hello world"},                                       // 11 chars → 2 tok
			ImagePart{DataURL: "data:image/png;base64,AAAA"},                    // +1024
			AudioPart{Data: "BBBB", Format: "wav"},                              // +1
			FilePart{Name: "a.pdf", MediaType: "application/pdf", Data: "CCCC"}, // +1
		}},
	}
	n := SummarizeNeeds(msgs, 2)
	if !n.Image || !n.Audio || !n.File || !n.Tools {
		t.Fatalf("needs flags = %+v", n)
	}
	// text total = 11 ("hello world") + 3 ("sys") = 14 chars → 3 tok
	if n.PromptTokens != 1024+1+1+3 {
		t.Fatalf("prompt tokens = %d, want %d", n.PromptTokens, 1024+1+1+3)
	}
	if SummarizeNeeds(nil, 0).AnyModality() {
		t.Fatal("plain text must not set modality flags")
	}
}

func TestCandidatesHonorsNeeds(t *testing.T) {
	l := strategyLane()
	throttleAll(l, "nemotron-3.5-lightning-free", "space-bunny-free")
	got := l.Candidates(Needs{Image: true}, "nobody", 5)
	if len(got) == 0 {
		t.Fatal("expected vision-capable suggestions")
	}
	for _, id := range got {
		if id == "nemotron-3.5-lightning-free" {
			t.Fatalf("suggestions include text-only model %q", id)
		}
	}
}

// One queued probe must not demote a fast model for the rest of the probe
// interval: the persisted sample ring is the honest estimate of what the model
// does, and it is what routing should rank on.
func TestLatencyStrategyPrefersTheSampleRingOverTheLastProbe(t *testing.T) {
	l := strategyLane()
	throttleAll(l, "nemotron-3.5-lightning-free", "space-bunny-free")
	l.SetStrategy(StrategyLatency)
	l.availability["nemotron-3.5-lightning-free"] = ProbeResult{
		Model: "nemotron-3.5-lightning-free", State: StateAvailable, TTFTMs: 1500}
	l.availability["space-bunny-free"] = ProbeResult{
		Model: "space-bunny-free", State: StateAvailable, TTFTMs: 169766}
	l.SetTTFTSource(func(model string) int64 {
		if model == "space-bunny-free" {
			return 1305 // the median of its samples; the probe caught a queue
		}
		return 0
	})
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("next = %q, want the model the ring measured fast", got)
	}
}
