package lane

// Failover strategy selection. The requested model is always the first
// choice; a strategy only orders the candidate chain that takes over when
// that model refuses before any content was streamed.
//
//   - catalog  默认：探测可用优先，目录顺序保底（升级前的行为）
//   - latency  首字延迟优先：在可用且未冷却的候选里按探测/运行时首字延迟升序
//
// SmartRouting is the master switch: off restores the exact pre-routing
// behaviour (catalog order, no capability filtering).

const (
	StrategyCatalog = "catalog"
	StrategyLatency = "latency"
)

// NormalizeStrategy maps any stored value onto a known strategy spelling.
func NormalizeStrategy(s string) string {
	if s == StrategyLatency {
		return StrategyLatency
	}
	return StrategyCatalog
}

// StrategyNames lists the user-facing strategy ids in settings order.
func StrategyNames() []string { return []string{StrategyCatalog, StrategyLatency} }

// SetSmartRouting is the smart-routing master switch. When off, failover
// picks candidates exactly like before: probed-available first, catalog
// order, no modality filtering.
func (l *Lane) SetSmartRouting(on bool) {
	l.mu.Lock()
	l.smartRouting = on
	l.mu.Unlock()
}

// SetStrategy selects the failover ordering (effective while smart routing
// is on; hot-applied from the dashboard).
func (l *Lane) SetStrategy(s string) {
	l.mu.Lock()
	l.strategy = NormalizeStrategy(s)
	l.mu.Unlock()
}

// Strategy reports the active ordering.
func (l *Lane) Strategy() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.strategy
}

// TTFTSource returns the smoothed first-token latency (ms) of one model,
// 0 when unknown — wired from the persisted sample ring in main.
type TTFTSource func(model string) int64

// SetTTFTSource wires the runtime first-token sample source.
func (l *Lane) SetTTFTSource(fn TTFTSource) {
	l.mu.Lock()
	l.ttft = fn
	l.mu.Unlock()
}

// CapabilityTags carries third-party capability verdicts for one model (the
// AI tagger or a live probe). Nil pointers leave the catalog field untouched.
type CapabilityTags struct {
	Audio         *bool
	File          *bool
	Vision        *bool
	ContextWindow int
	MaxOutput     int
}

// ApplyCapabilityTags patches one catalog entry in place. Audio/File are
// always accepted (the curated table carries no evidence for them); vision
// is upgrade-only so a tag can never silence a probe-verified capability.
// Returns false when the model is not on the free lane.
func (l *Lane) ApplyCapabilityTags(model string, t CapabilityTags) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.catalog {
		if l.catalog[i].ID != model {
			continue
		}
		if t.Audio != nil {
			l.catalog[i].AudioInput = *t.Audio
		}
		if t.File != nil {
			l.catalog[i].FileInput = *t.File
		}
		if t.Vision != nil && *t.Vision && !l.catalog[i].Vision {
			l.catalog[i].Vision = true
		}
		if t.ContextWindow > 0 {
			l.catalog[i].ContextWindow = t.ContextWindow
		}
		if t.MaxOutput > 0 {
			l.catalog[i].MaxOutput = t.MaxOutput
		}
		return true
	}
	return false
}

// satisfiesNeeds reports whether a model can carry a request's modality
// footprint. The catalog carries a verdict for every lane model, so false is
// a hard filter — an image must never ride a text-only model.
func satisfiesNeeds(m ModelInfo, needs Needs) bool {
	if needs.Image && !m.Vision {
		return false
	}
	if needs.Audio && !m.AudioInput {
		return false
	}
	if needs.File && !m.FileInput {
		return false
	}
	return true
}

// candidateKey orders failover candidates; lower wins. Availability stays
// the dominant term in every strategy: a probed-available model always beats
// an unprobed one, and latency only ranks inside the same availability band.
type candidateKey struct {
	avail      int   // 0 probed-available, 1 unprobed
	known      int   // 0 has a TTFT measurement, 1 unmeasured (latency only)
	ttft       int64 // smoothed first-token ms (latency only)
	catalogIdx int   // catalog order tiebreak
}

func (l *Lane) candidateKey(m ModelInfo, idx int, av ProbeResult, strategy string, ttftFn TTFTSource) candidateKey {
	key := candidateKey{avail: 1, known: 1, catalogIdx: idx}
	if FreshState(av) == StateAvailable {
		key.avail = 0
	}
	if strategy == StrategyLatency {
		// The persisted ring is the estimate of what the model does; a single
		// probe is one draw from it, and one queued probe would otherwise rank
		// a fast model last until the next round.
		var ms int64
		if ttftFn != nil {
			ms = ttftFn(m.ID)
		}
		if ms <= 0 {
			ms = av.TTFTMs
		}
		if ms > 0 {
			key.known = 0
			key.ttft = ms
		}
	}
	return key
}

func lessCandidate(a, b candidateKey) bool {
	if a.avail != b.avail {
		return a.avail < b.avail
	}
	if a.known != b.known {
		return a.known < b.known
	}
	if a.ttft != b.ttft {
		return a.ttft < b.ttft
	}
	return a.catalogIdx < b.catalogIdx
}
