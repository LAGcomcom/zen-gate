package gateway

import (
	"context"
	"net/http"
	"sync"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/relay"
	"zen-gate/internal/store"
)

// Seamless node switching for user-added suppliers.
//
// The free lane already has its own failover, egress-exit retries and
// same-model twins (lane.Complete). The gap was the custom-node half:
//   - a dead node stayed first in line, so EVERY request paid a timeout on it
//     before reaching a healthy one;
//   - a request that named a supplier by id (seed/gpt-x) failed straight back
//     to the caller even when another node served the same model;
//   - fallback tried every candidate with no attempt or time bound.
//
// This file is the shared decision layer. It never speaks HTTP itself: the
// caller supplies `run`, so the switching rules are testable without an
// upstream, and the three wire shapes (chat / responses / anthropic) all get
// the same behaviour.
//
// "Seamless" is deliberately narrow: a switch only happens BEFORE any content
// reached the caller. Once a stream has emitted real text, changing nodes
// would duplicate output, and the error is the honest answer — exactly the
// sawAny rule lane.Complete already uses.

const (
	// maxNodeSwitches caps extra attempts per request. A third healthy node is
	// enough to ride out one bad endpoint without turning a failure into a
	// minutes-long serial crawl.
	maxNodeSwitches = 2
	// nodeSwitchBudget bounds the time spent on FAILED attempts. It is checked
	// only when deciding whether to switch, so a successful long stream is
	// never cut short.
	nodeSwitchBudget = 90 * time.Second
	nodeCooldownBase = 20 * time.Second
	nodeCooldownMax  = 5 * time.Minute
)

type nodeCandidate struct {
	Provider *store.Provider
	Model    string
}

// nodeHealth is short-lived process state, not config: a reboot legitimately
// forgets a cooldown because the endpoint may well be back. Nothing here is
// persisted, and nothing here disables a provider permanently.
type nodeHealth struct {
	Fails         int    `json:"fails"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"` // unix milliseconds
	LastError     string `json:"lastError,omitempty"`
	LastFailAt    int64  `json:"lastFailAt,omitempty"`
	SwitchesAway  int    `json:"switchesAway,omitempty"`
	Recoveries    int    `json:"recoveries,omitempty"`
}

type nodeSwitchBook struct {
	mu    sync.Mutex
	nodes map[string]nodeHealth
}

func (b *nodeSwitchBook) remaining(id string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.nodes[id]
	if !ok || h.CooldownUntil == 0 {
		return 0
	}
	d := time.Until(time.UnixMilli(h.CooldownUntil))
	if d < 0 {
		return 0
	}
	return d
}

// order is a stable partition: healthy nodes first (catalog order kept),
// cooling nodes last (shortest remaining cooldown first). A cooling node is
// NEVER removed — if every node is cooling we still try them all rather than
// declaring an outage from a guess. This single sort is what makes the next
// request skip the dead node instead of paying its timeout again.
func (b *nodeSwitchBook) order(cands []nodeCandidate, now time.Time) []nodeCandidate {
	type ranked struct {
		cand nodeCandidate
		left time.Duration
		seq  int
	}
	rows := make([]ranked, 0, len(cands))
	for i, c := range cands {
		rows = append(rows, ranked{cand: c, left: b.remaining(c.Provider.ID, now), seq: i})
	}
	// Insertion sort is fine here (a handful of nodes) and keeps the stable
	// tie-break on catalog order obvious.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, z := rows[j-1], rows[j]
			swap := false
			if a.left == 0 && z.left > 0 {
				swap = false
			} else if a.left > 0 && z.left == 0 {
				swap = true
			} else if a.left > 0 && z.left > 0 && z.left < a.left {
				swap = true
			}
			if !swap {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
	out := make([]nodeCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.cand)
	}
	return out
}

func (b *nodeSwitchBook) fail(id, code string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes == nil {
		b.nodes = map[string]nodeHealth{}
	}
	h := b.nodes[id]
	h.Fails++
	h.LastError = code
	h.LastFailAt = now.UnixMilli()
	h.SwitchesAway++
	d := nodeCooldownBase << (h.Fails - 1)
	if h.Fails > 8 || d > nodeCooldownMax {
		d = nodeCooldownMax
	}
	h.CooldownUntil = now.Add(d).UnixMilli()
	b.nodes[id] = h
}

func (b *nodeSwitchBook) ok(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, exists := b.nodes[id]
	if !exists || (h.Fails == 0 && h.CooldownUntil == 0) {
		return
	}
	b.nodes[id] = nodeHealth{Recoveries: h.Recoveries + 1, SwitchesAway: h.SwitchesAway}
}

func (b *nodeSwitchBook) snapshot(now time.Time) map[string]nodeHealth {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]nodeHealth, len(b.nodes))
	for id, h := range b.nodes {
		if h.CooldownUntil > 0 && time.UnixMilli(h.CooldownUntil).Before(now) {
			h.CooldownUntil = 0
		}
		out[id] = h
	}
	return out
}

func (s *Server) book() *nodeSwitchBook {
	s.nodeOnce.Do(func() {
		s.nodeBook = &nodeSwitchBook{nodes: map[string]nodeHealth{}}
	})
	return s.nodeBook
}

// nodeCandidatesFor lists the nodes that can serve the SAME upstream model.
// It never changes the model to make a switch possible: a caller that named a
// model must not silently get a different one.
func (s *Server) nodeCandidatesFor(primary *store.Provider, upstream string) []nodeCandidate {
	cfg := s.Store.Config()
	out := []nodeCandidate{{Provider: primary, Model: upstream}}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.ID == primary.ID || !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if m == upstream {
				out = append(out, nodeCandidate{Provider: p, Model: m})
				break
			}
		}
	}
	return out
}

// nodeSwitchable says the failure is about the NODE, not the request. A 400 is
// our own body and every node will refuse it; a cancelled request must stop.
func nodeSwitchable(uerr *lane.UpstreamError) bool {
	if uerr == nil {
		return false
	}
	switch uerr.Code {
	case lane.CodeQuota, lane.CodeFreeTier, lane.CodeRegion, lane.CodeServer,
		lane.CodeTimeout, lane.CodeTransport, lane.CodeReset, lane.CodeEmpty,
		lane.CodeCredential:
		return true
	}
	return false
}

func chunkHasPayload(c lane.Chunk) bool {
	return c.Text != "" || c.Delta != "" || c.ID != "" || c.Name != ""
}

type nodeAttempt struct {
	Provider *store.Provider
	Model    string
	Usage    lane.Usage
	Finish   string
	Err      *lane.UpstreamError
	TTFMs    int64
}

type nodeRun struct {
	Attempts []nodeAttempt
	Served   nodeCandidate
	Switches int
	// Skipped names nodes that were moved behind a healthy one by cooldown.
	Skipped []string
}

// runNodeSwitch runs one logical turn across candidate nodes and returns every
// physical attempt so the caller can account for each one (usage, logs) rather
// than hiding the failed hops.
func (s *Server) runNodeSwitch(
	ctx context.Context,
	candidates []nodeCandidate,
	emit func(lane.Chunk),
	run func(nodeCandidate, func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError),
) nodeRun {
	out := nodeRun{}
	if len(candidates) == 0 {
		return out
	}
	now := time.Now()
	ordered := s.book().order(candidates, now)
	for _, c := range ordered {
		if d := s.book().remaining(c.Provider.ID, now); d > 0 && c.Provider.ID != candidates[0].Provider.ID {
			out.Skipped = append(out.Skipped, c.Provider.ID)
		}
	}
	start := now
	for i, c := range ordered {
		if ctx.Err() != nil {
			break
		}
		saw := false
		var firstAt int64
		attemptStart := time.Now()
		wrapped := func(ch lane.Chunk) {
			if firstAt == 0 {
				firstAt = time.Since(attemptStart).Milliseconds()
			}
			if chunkHasPayload(ch) {
				saw = true
			}
			if emit != nil {
				emit(ch)
			}
		}
		usage, finish, uerr := run(c, wrapped)
		out.Attempts = append(out.Attempts, nodeAttempt{
			Provider: c.Provider, Model: c.Model, Usage: usage,
			Finish: finish, Err: uerr, TTFMs: firstAt,
		})
		out.Served = c
		if uerr == nil {
			s.book().ok(c.Provider.ID)
			if out.Switches > 0 {
				s.logCat(logx.CatRouting, "info", "节点无感切换完成：%s/%s 接住了请求（切换 %d 次）",
					c.Provider.ID, c.Model, out.Switches)
			}
			return out
		}
		if nodeSwitchable(uerr) {
			s.book().fail(c.Provider.ID, uerr.Code, time.Now())
		}
		if i == len(ordered)-1 || saw || !nodeSwitchable(uerr) {
			break
		}
		if out.Switches >= maxNodeSwitches || time.Since(start) > nodeSwitchBudget {
			break
		}
		out.Switches++
		s.logCat(logx.CatRouting, "warn", "节点 %s/%s 失败（%s），请求内无感切换到下一个节点",
			c.Provider.ID, c.Model, uerr.Code)
	}
	return out
}

// completeRelayWithNodeSwitch is the one entry point for the three direct
// provider wire shapes. The caller keeps its own response encoding; it only
// has to account for every attempt in the returned nodeRun.
func (s *Server) completeRelayWithNodeSwitch(
	ctx context.Context,
	primary *store.Provider,
	upstream string,
	turn relay.Turn,
	emit func(lane.Chunk),
) nodeRun {
	return s.runNodeSwitch(ctx, s.nodeCandidatesFor(primary, upstream), emit,
		func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
			t := turn
			t.Provider = c.Provider
			t.Model = c.Model
			if c.Provider.ID != primary.ID {
				t.ReasoningEffort = ""
			}
			return relay.Complete(ctx, t, e)
		})
}

// accountNodeRun books every physical hop (including the failed ones) and
// returns the last attempt, which is the outcome the caller encodes. The
// response still names the model the caller asked for — a switch is invisible
// to the client, but visible in the channel ledger and the routing log.
func (s *Server) accountNodeRun(ctx context.Context, res nodeRun, agent string) (lane.Usage, string, *lane.UpstreamError, int64) {
	for _, a := range res.Attempts {
		s.recordRelay(ctx, a.Provider, a.Model, agent, a.Finish != "", a.Usage, a.TTFMs, a.Finish, a.Err)
	}
	if len(res.Attempts) == 0 {
		return lane.Usage{}, "", nil, 0
	}
	last := res.Attempts[len(res.Attempts)-1]
	return last.Usage, last.Finish, last.Err, last.TTFMs
}

// adminNodesHealth exposes the short-lived switching state so the node list
// card can show which supplier is cooling, how many times it was switched
// away from, and when it recovered. Nothing here is persisted or editable.
func (s *Server) adminNodesHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	health := s.book().snapshot(now)
	cfg := s.Store.Config()
	rows := make([]map[string]any, 0, len(cfg.Providers))
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		h := health[p.ID]
		coolingMS := int64(0)
		if h.CooldownUntil > now.UnixMilli() {
			coolingMS = h.CooldownUntil - now.UnixMilli()
		}
		rows = append(rows, map[string]any{
			"id": p.ID, "name": p.Name, "enabled": p.Enabled,
			"healthy":   coolingMS == 0 && h.Fails == 0,
			"coolingMs": coolingMS, "fails": h.Fails,
			"lastError": h.LastError, "lastFailAt": h.LastFailAt,
			"switchesAway": h.SwitchesAway, "recoveries": h.Recoveries,
		})
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "rows": rows,
		"maxSwitches":        maxNodeSwitches,
		"budgetSeconds":      int(nodeSwitchBudget / time.Second),
		"cooldownMaxSeconds": int(nodeCooldownMax / time.Second),
	})
}
