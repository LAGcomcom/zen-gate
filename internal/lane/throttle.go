package lane

import (
	"sort"
	"sync"
	"time"
)

// ThrottleBook tracks per-model quota exhaustion as observed from live 429s
// and probe verdicts. The free lane exposes no balance API — quota is accounted
// per session upstream and only signals itself by refusing — so this is the
// closest honest substitute: instant throttle marking (no waiting for the next
// probe round), a cooldown that auto-expires (LiteLLM-style, no blacklisting),
// and a history of throttle episodes for recovery estimation.

// ThrottleEpisode is one observed quota-exhaustion window, in epoch ms.
type ThrottleEpisode struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // 0 while the episode is still open
}

// ThrottleNote is one model's throttle state and episode history.
type ThrottleNote struct {
	ThrottledAt   int64             `json:"throttledAt,omitempty"`
	CooldownUntil int64             `json:"cooldownUntil,omitempty"`
	LastOK        int64             `json:"lastOk,omitempty"`
	Episodes      []ThrottleEpisode `json:"episodes,omitempty"`
}

const (
	throttleCooldownDefault = 60 * 1000       // 1 min when upstream gave no Retry-After
	throttleCooldownMax     = 2 * 3600 * 1000 // 2 h cap — probes will re-verify
	throttleEpisodeTTL      = 14 * 24 * 3600 * 1000
	throttleMaxEpisodes     = 12
)

// ThrottleBook is the per-model throttle ledger. All methods are safe for
// concurrent use; OnUpdate fires after every mutation for persistence.
type ThrottleBook struct {
	mu    sync.Mutex
	notes map[string]*ThrottleNote
	// OnUpdate fires after a model's note changed (persist + UI refresh).
	OnUpdate func(model string, note ThrottleNote)
}

func NewThrottleBook() *ThrottleBook {
	return &ThrottleBook{notes: map[string]*ThrottleNote{}}
}

// Load seeds persisted notes at boot (keeps episode history across restarts).
func (b *ThrottleBook) Load(notes map[string]ThrottleNote) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for m, n := range notes {
		cp := n
		b.notes[m] = &cp
	}
}

func (b *ThrottleBook) noteLocked(model string) *ThrottleNote {
	n, ok := b.notes[model]
	if !ok {
		n = &ThrottleNote{}
		b.notes[model] = n
	}
	return n
}

func (b *ThrottleBook) fire(model string, n ThrottleNote) {
	if b.OnUpdate != nil {
		b.OnUpdate(model, n)
	}
}

// MarkThrottled records a live quota refusal. The first call of an episode
// opens it; repeats only extend the cooldown window.
func (b *ThrottleBook) MarkThrottled(model string, retryAfterSec int) {
	now := time.Now().UnixMilli()
	cd := int64(retryAfterSec) * 1000
	if cd <= 0 {
		cd = throttleCooldownDefault
	}
	if cd > throttleCooldownMax {
		cd = throttleCooldownMax
	}
	b.mu.Lock()
	n := b.noteLocked(model)
	if n.ThrottledAt == 0 {
		n.ThrottledAt = now
		n.Episodes = append(n.Episodes, ThrottleEpisode{Start: now})
	}
	n.CooldownUntil = now + cd
	b.pruneLocked(n)
	note := *n
	b.mu.Unlock()
	b.fire(model, note)
}

// MarkOK records a successful traffic request or probe, closing any open
// episode and clearing the throttle.
func (b *ThrottleBook) MarkOK(model string) {
	now := time.Now().UnixMilli()
	b.mu.Lock()
	n, ok := b.notes[model]
	if !ok {
		b.mu.Unlock()
		return
	}
	changed := n.ThrottledAt != 0
	n.LastOK = now
	if changed {
		for i := range n.Episodes {
			if n.Episodes[i].End == 0 {
				n.Episodes[i].End = now
			}
		}
		n.ThrottledAt = 0
		n.CooldownUntil = 0
		b.pruneLocked(n)
	}
	note := *n
	b.mu.Unlock()
	if changed {
		b.fire(model, note)
	}
}

// Throttled reports the live throttle state, lazily expiring cooldowns that
// have run out (the next probe will confirm whether quota actually returned).
func (b *ThrottleBook) Throttled(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.notes[model]
	if !ok {
		return false
	}
	b.expireLocked(n)
	return n.ThrottledAt != 0
}

// Note returns a copy of one model's note (expired cooldowns applied).
func (b *ThrottleBook) Note(model string) ThrottleNote {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.notes[model]
	if !ok {
		return ThrottleNote{}
	}
	b.expireLocked(n)
	return *n
}

// All snapshots every note with expiry applied.
func (b *ThrottleBook) All() map[string]ThrottleNote {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]ThrottleNote{}
	for m, n := range b.notes {
		b.expireLocked(n)
		out[m] = *n
	}
	return out
}

// RecoveryETA estimates when a currently-throttled model comes back: the
// median of closed episode durations (needs ≥2 samples) projected from the
// throttle onset; otherwise the cooldown boundary. 0 when not throttled.
func (b *ThrottleBook) RecoveryETA(model string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.notes[model]
	if !ok || n.ThrottledAt == 0 {
		return 0
	}
	var durs []int64
	for _, e := range n.Episodes {
		if e.End > e.Start {
			durs = append(durs, e.End-e.Start)
		}
	}
	if len(durs) >= 2 {
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		median := durs[len(durs)/2]
		eta := n.ThrottledAt + median
		if now := time.Now().UnixMilli(); eta < now {
			eta = now + 60*1000
		}
		return eta
	}
	return n.CooldownUntil
}

func (b *ThrottleBook) expireLocked(n *ThrottleNote) {
	if n.ThrottledAt != 0 && time.Now().UnixMilli() >= n.CooldownUntil {
		for i := range n.Episodes {
			if n.Episodes[i].End == 0 {
				n.Episodes[i].End = n.CooldownUntil
			}
		}
		n.ThrottledAt = 0
		n.CooldownUntil = 0
	}
}

func (b *ThrottleBook) pruneLocked(n *ThrottleNote) {
	cutoff := time.Now().UnixMilli() - throttleEpisodeTTL
	kept := n.Episodes[:0]
	for _, e := range n.Episodes {
		if e.End == 0 || e.End >= cutoff {
			kept = append(kept, e)
		}
	}
	n.Episodes = kept
	if len(n.Episodes) > throttleMaxEpisodes {
		n.Episodes = n.Episodes[len(n.Episodes)-throttleMaxEpisodes:]
	}
}
