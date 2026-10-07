package lane

import "testing"

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

// Fallback catalog order: mimo-v2.6, mimo-v2.5, ling-3.0, nemotron-3-ultra,
// nemotron-3.5-lightning, space-bunny, muse-1.3, muse-1.2. Vision carriers per
// the curated table: mimo*, muse*, space-bunny; text-only: ling, nemotron.

func TestNextCandidateFiltersByNeeds(t *testing.T) {
	l := NewLane()
	throttleAll(l, "ling-3.0-flash-fin-free", "space-bunny-free")

	// A vision request must skip the text-only ling even though it comes
	// first in catalog order.
	if got := l.nextCandidate(Needs{Image: true}, map[string]bool{}); got != "space-bunny-free" {
		t.Fatalf("vision request: next = %q, want space-bunny-free", got)
	}
	// Without a modality footprint the same pool keeps catalog order.
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "ling-3.0-flash-fin-free" {
		t.Fatalf("no needs: next = %q, want ling-3.0-flash-fin-free", got)
	}
}

func TestNextCandidateContextFit(t *testing.T) {
	l := NewLane()
	throttleAll(l, "nemotron-3-ultra-free") // 128k window, the smallest here
	if got := l.nextCandidate(Needs{PromptTokens: 200000}, map[string]bool{}); got != "" {
		t.Fatalf("oversized request: next = %q, want none", got)
	}
	if got := l.nextCandidate(Needs{PromptTokens: 1000}, map[string]bool{}); got != "nemotron-3-ultra-free" {
		t.Fatalf("small request: next = %q, want nemotron-3-ultra-free", got)
	}
}

func TestNextCandidateLatencyStrategy(t *testing.T) {
	l := NewLane()
	throttleAll(l, "ling-3.0-flash-fin-free", "nemotron-3-ultra-free")
	l.SetStrategy(StrategyLatency)
	l.SetTTFTSource(func(model string) int64 {
		if model == "nemotron-3-ultra-free" {
			return 120
		}
		return 900
	})
	// Both candidates are unprobed (same availability band); the measured-fast
	// one wins even though ling comes first in catalog order.
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "nemotron-3-ultra-free" {
		t.Fatalf("latency strategy: next = %q, want nemotron-3-ultra-free", got)
	}
	// Catalog strategy on the same pool: listing order.
	l.SetTTFTSource(nil)
	l.SetStrategy(StrategyCatalog)
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "ling-3.0-flash-fin-free" {
		t.Fatalf("catalog strategy: next = %q, want ling-3.0-flash-fin-free", got)
	}
}

func TestSmartRoutingOffRestoresLegacy(t *testing.T) {
	l := NewLane()
	l.SetSmartRouting(false)
	throttleAll(l, "ling-3.0-flash-fin-free", "space-bunny-free")
	// Legacy ignores the modality filter: a vision request lands on ling.
	if got := l.nextCandidate(Needs{Image: true}, map[string]bool{}); got != "ling-3.0-flash-fin-free" {
		t.Fatalf("legacy: next = %q, want ling-3.0-flash-fin-free", got)
	}
	// Legacy skips the context check too: a 10M-token prompt still finds ling.
	if got := l.nextCandidate(Needs{PromptTokens: 10 << 20}, map[string]bool{}); got != "ling-3.0-flash-fin-free" {
		t.Fatalf("legacy oversized: next = %q, want ling-3.0-flash-fin-free", got)
	}
}

func TestApplyCapabilityTags(t *testing.T) {
	l := NewLane()
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
	l := NewLane()
	throttleAll(l, "ling-3.0-flash-fin-free", "space-bunny-free")
	got := l.Candidates(Needs{Image: true}, "nobody", 5)
	if len(got) == 0 {
		t.Fatal("expected vision-capable suggestions")
	}
	for _, id := range got {
		if id == "ling-3.0-flash-fin-free" {
			t.Fatalf("suggestions include text-only model %q", id)
		}
	}
}
