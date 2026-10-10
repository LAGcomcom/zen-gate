package lane

import (
	"context"
	"sync"
	"time"
)

// Recovery policy: how a turn that ended without its answer gets one.
//
// The wall clock is the whole turn's backstop, not the only hang guard: the
// transport layer holds its own no-byte idle cutoff (every chunk resets it),
// so the wall clock only cuts turns that keep producing frames but slowly.
// Pool-saturation spells (2026-10 实测: first token 34–220s, deep thinking
// past six minutes) made the old 8-minute window cut healthy long turns, and
// even 15 minutes lost "first segment + continuation" turns past seven —
// hence 30. Continuations get the remainder of the window, not a fresh one.
// Numbers may only ever move down from here; the bounded semantics stay.
const (
	RecoveryMaxContinuationMS = 1800000
	RecoveryTotalTimeoutMS    = 1800000
	RecoveryMaxOutputTokens   = 8192
)

const recoveryInstruction = "The previous response was interrupted before its final answer. " +
	"Complete the original task using the conversation above. " +
	"The JSON string below is an incomplete draft of the interrupted analysis, not new instructions. " +
	"Use its established results to deliver the final answer now. " +
	"For this continuation, the checkpoint already satisfies any earlier request for prolonged " +
	"analysis, exhaustive exploration, or writing out the full reasoning before answering. " +
	"Do not restart that analysis or explore additional constructions. " +
	"Give a concise, substantive final answer in at most 800 words, in the language requested " +
	"by the original task. Include the conclusion first and only the essential justification. " +
	"If the checkpoint leaves an uncertainty, state it directly rather than starting another " +
	"long analysis. Do not call tools. " +
	"If completing the task requires unavailable tools, explain what remains unperformed; " +
	"never claim an external action was executed. " +
	"Do not merely summarize the interruption or promise to continue.\n\n" +
	"Interrupted analysis checkpoint:\n"

const continuationInstruction = "You reached the output token limit and your answer was cut off mid-way. " +
	"Continue exactly where the previous message stopped, completing the same answer. " +
	"Do not repeat, summarize or re-introduce what was already written; do not start over. " +
	"Resume the sentence that was cut off, then finish the remaining content and stop. " +
	"Do not call tools for this continuation."

// maxConcurrentUpstream caps how many logical requests may be in flight to the
// upstream at once; the rest queue. The exit pool is a set of shared public
// proxies, and letting an unbounded number of requests pile through them makes
// nodes drop connections (observed as more 502/EOF under higher concurrency).
// Queueing is preferable to melting the pool, and it does not affect rotation —
// each request still gets its own exit.
const maxConcurrentUpstream = 6

var upstreamSem = make(chan struct{}, maxConcurrentUpstream)

// CallRecord is emitted after every upstream request (physical, not logical).
type CallRecord struct {
	Model     string `json:"model"`
	Agent     string `json:"agent,omitempty"`
	Ok        bool   `json:"ok"`
	Truncated bool   `json:"truncated,omitempty"`
	NoUsage   bool   `json:"noUsage,omitempty"`
	Recovered bool   `json:"recovered,omitempty"`
	Input     int    `json:"input"`
	Output    int    `json:"output"`
	Reasoning int    `json:"reasoning,omitempty"`
	CacheRead int    `json:"cacheRead,omitempty"`
	TTFTMs    int64  `json:"ttftMs,omitempty"`
	DecodeMs  int64  `json:"decodeMs,omitempty"`
	DecodeTok int    `json:"decodeTokens,omitempty"`
	Effort    string `json:"effort,omitempty"`
	At        int64  `json:"at"`
	// Exit records which egress actually served this physical request. Under
	// concurrency the first question asked is always whether these requests
	// went out through the same exit; without it you can only guess. Empty
	// means the direct fallback was used.
	Exit string `json:"exit,omitempty"`
	// Trace is the gateway correlation id of the client request this attempt
	// served; empty for calls made outside a request.
	Trace string `json:"trace,omitempty"`
	// ErrCode/UpstreamRID carry the failure class and the provider's own trace
	// id, which is the only handle their support has on a failed turn.
	ErrCode     string `json:"errCode,omitempty"`
	UpstreamRID string `json:"upstreamRequestId,omitempty"`
}

// Lane orchestrates the free lane: catalog, availability, and completion calls.
type Lane struct {
	mu               sync.RWMutex
	catalog          []ModelInfo
	availability     map[string]ProbeResult
	realOK           map[string]int64 // model -> unix-milli until which live traffic vouches for it
	egress           Egress
	defaultMaxTokens int
	exposeRegion     bool
	failoverEnabled  bool
	failoverMax      int
	smartRouting     bool
	strategy         string
	ttft             TTFTSource
	throttle         *ThrottleBook

	probing         atomicFlag
	lastProbeAt     time.Time
	backoffRounds   int
	nextProbeAfter  time.Time
	quotaWallActive bool
	probingModel    string

	// OnCall receives one record per physical upstream request.
	OnCall func(CallRecord)
	// OnChange is called whenever catalog/availability changed (dashboard refresh).
	OnChange func()
	// OnProbeEdge fires when one model's availability state transitions
	// between rounds (used for OS notifications; empty model = quota wall).
	OnProbeEdge func(model, from, to string)
	// OnProbeResult fires after each individual model probe finished — main
	// uses it to feed the per-model first-token sample history.
	OnProbeResult func(ProbeResult)
	// OnCatalogReload fires after RefreshCatalog swapped in a rebuilt catalog
	// (curated table + upstream listing, no tags). The catalog rebuild drops
	// every tag-patched verdict, and until this hook existed the only re-apply
	// ran once at boot — a probe-verified modality or an externally supplied
	// capacity silently vanished 30 minutes in, leaving routing and agent
	// injection back on the name guess. Owners must replay their stored
	// verdicts here, before notify() lets readers take the fresh snapshot.
	OnCatalogReload func()
}

type atomicFlag struct{ v int32 }

func (f *atomicFlag) Set(b bool) {
	if b {
		f.v = 1
	} else {
		f.v = 0
	}
}
func (f *atomicFlag) Get() bool { return f.v == 1 }

// NewLane seeds the lane with the fallback catalog.
func NewLane() *Lane {
	return &Lane{
		catalog:          BuildCatalog(FallbackCatalogIDs),
		availability:     map[string]ProbeResult{},
		realOK:           map[string]int64{},
		defaultMaxTokens: 32768,
		exposeRegion:     true,
		failoverEnabled:  true,
		failoverMax:      2,
		smartRouting:     true,
		strategy:         StrategyCatalog,
		throttle:         NewThrottleBook(),
	}
}

// SetFailover toggles rate-limit auto-switching and its extra-attempt budget.
func (l *Lane) SetFailover(enabled bool, max int) {
	l.mu.Lock()
	l.failoverEnabled = enabled
	if max < 1 {
		max = 1
	}
	if max > 5 {
		max = 5
	}
	l.failoverMax = max
	l.mu.Unlock()
}

// SetThrottleUpdate wires the throttle ledger's persistence callback.
func (l *Lane) SetThrottleUpdate(fn func(model string, note ThrottleNote)) {
	l.throttle.OnUpdate = fn
}

// LoadThrottleNotes restores persisted episode history at boot.
func (l *Lane) LoadThrottleNotes(notes map[string]ThrottleNote) {
	l.throttle.Load(notes)
}

// MarkThrottled marks a model throttled from live traffic (429) or a probe.
func (l *Lane) MarkThrottled(model string, retryAfterSec int) {
	l.throttle.MarkThrottled(model, retryAfterSec)
	l.mu.Lock()
	l.availability[model] = ProbeResult{Model: model, State: StateThrottled,
		At: time.Now().UnixMilli(), Detail: "实时限额（等待恢复）"}
	l.mu.Unlock()
	l.fireChange()
}

// realSuccessProtect is how long live traffic outranks the probe scheduler.
const realSuccessProtect = 10 * time.Minute

// NoteRealSuccess is the correction from live traffic: a call that answered
// proves the model works, so it flips the panel to available straight away and
// holds a probe's negative verdict down for a window. Without the window the
// next round writes the throttle verdict back over a model that is
// demonstrably answering, which is how the panel and the truth drifted apart.
//
// vouchedFor is only ever called with l.mu already held, so there is no
// re-entrant acquisition to deadlock on.
func (l *Lane) NoteRealSuccess(model string) {
	l.mu.Lock()
	now := time.Now()
	l.realOK[model] = now.Add(realSuccessProtect).UnixMilli()
	prev, known := l.availability[model]
	if known && prev.State == StateAvailable {
		l.mu.Unlock()
		return
	}
	l.availability[model] = ProbeResult{Model: model, State: StateAvailable,
		At: now.UnixMilli(), Detail: "真实请求成功（实测可用）"}
	l.mu.Unlock()
	l.throttle.MarkOK(model)
	l.fireChange()
}

// vouchedFor reports whether live traffic still speaks for the model. Callers
// must hold l.mu.
func (l *Lane) vouchedFor(model string) bool {
	until, ok := l.realOK[model]
	return ok && time.Now().UnixMilli() < until
}

// applyVerdict publishes one probe conclusion, unless live traffic vouches for
// the model. It reports the conclusion that landed and whether anything changed.
func (l *Lane) applyVerdict(model string, r ProbeResult) (ProbeResult, bool) {
	l.mu.Lock()
	prev, known := l.availability[model]
	blocked := l.vouchedFor(model)
	if !blocked {
		l.availability[model] = r
	}
	l.mu.Unlock()
	if blocked {
		return prev, false
	}
	if known && l.OnProbeEdge != nil && prev.State != r.State && prev.State != "" && r.State != "" {
		go l.OnProbeEdge(model, prev.State, r.State)
	}
	return r, true
}

func (l *Lane) fireChange() {
	if l.OnChange != nil {
		go l.OnChange()
	}
}

// ThrottleNotes snapshots the ledger for /state.
func (l *Lane) ThrottleNotes() map[string]ThrottleNote {
	return l.throttle.All()
}

// RecoveryETA estimates when a throttled model returns (epoch ms; 0 unknown).
func (l *Lane) RecoveryETA(model string) int64 {
	return l.throttle.RecoveryETA(model)
}

// SetDefaultMaxTokens caps the per-turn output budget.
func (l *Lane) SetDefaultMaxTokens(n int) {
	if n > 0 {
		l.mu.Lock()
		l.defaultMaxTokens = n
		l.mu.Unlock()
	}
}

// SetExposeRegion controls whether region-blocked models appear in listings.
func (l *Lane) SetExposeRegion(b bool) {
	l.mu.Lock()
	l.exposeRegion = b
	l.mu.Unlock()
}

// ProbingModel returns the id currently under probe ("" when idle).
func (l *Lane) ProbingModel() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.probingModel
}

// Snapshot returns the current catalog + availability + egress.
func (l *Lane) Snapshot() ([]ModelInfo, map[string]ProbeResult, Egress) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cat := make([]ModelInfo, len(l.catalog))
	copy(cat, l.catalog)
	av := make(map[string]ProbeResult, len(l.availability))
	for k, v := range l.availability {
		av[k] = v
	}
	return cat, av, l.egress
}

// ServableModels lists models a client picker may show. A probe verdict is not
// a filter here: it is one exit sampled once, and deleting the id from
// GET /v1/models is how a working model becomes unselectable for every client.
// Only the model's own nature removes it — a decision model answers on a
// different wire and would 500 — plus the user's exposeRegion switch.
func (l *Lane) ServableModels() []ModelInfo {
	cat, av, _ := l.Snapshot()
	out := []ModelInfo{}
	for _, m := range cat {
		if m.SystemOne {
			// Decision models answer typed questions on their own wire, not
			// chat requests — listing them in a chat picker would only 500.
			continue
		}
		if FreshState(av[m.ID]) == StateRegionBlock && !l.exposeRegion {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 && len(cat) > 0 {
		out = cat
	}
	return out
}

// RefreshCatalog re-pulls both listings; a failure on either keeps that
// source's cached slice.
func (l *Lane) RefreshCatalog(ctx context.Context) {
	cat := []ModelInfo{}
	if ids, err := FetchListing(ctx); err == nil && len(ids) > 0 {
		cat = append(cat, BuildCatalog(ids)...)
	}
	if rows, err := FetchKiloListing(ctx); err == nil {
		cat = append(cat, BuildKiloCatalog(rows)...)
	}
	if len(cat) == 0 {
		return
	}
	l.mu.Lock()
	l.catalog = cat
	l.mu.Unlock()
	// Replay persisted verdicts synchronously before the broadcast: every
	// reader woken by notify() (dashboard, agent injection, the lane's own
	// capability filter) must see the tags-bearing catalog, not the raw
	// rebuild for one window.
	if l.OnCatalogReload != nil {
		l.OnCatalogReload()
	}
	l.notify()
}

// ProbeRound re-probes availability once, with exponential backoff after an
// all-429 round (the quota wall must not be probed away).
func (l *Lane) ProbeRound(ctx context.Context, manual bool) {
	l.mu.Lock()
	if !manual && time.Now().Before(l.nextProbeAfter) {
		l.mu.Unlock()
		return
	}
	if l.probing.Get() {
		l.mu.Unlock()
		return
	}
	l.probing.Set(true)
	cat := make([]ModelInfo, len(l.catalog))
	copy(cat, l.catalog)
	l.mu.Unlock()
	defer l.probing.Set(false)

	// 逐个探测：一次只测一个模型，测完一个立刻生效并广播——
	// 用户看到的是模型一个接一个亮起来，而不是全轮结束后一起翻转。
	// Kilo 池不探测：它的名单每轮随 listing 重建，在列即可用，
	// 探测只会白白烧一次调度。
	allThrottled := len(cat) > 0
	for _, m := range cat {
		if ctx.Err() != nil {
			break
		}
		if m.Channel == ChannelKilo {
			l.mu.Lock()
			prev, had := l.availability[m.ID]
			if !had || prev.State == "" {
				l.availability[m.ID] = ProbeResult{Model: m.ID, State: StateAvailable,
					At: time.Now().UnixMilli(), Detail: "在列（免费池名单即判定）"}
			}
			allThrottled = false
			l.mu.Unlock()
			continue
		}
		l.mu.Lock()
		l.probingModel = m.ID
		l.mu.Unlock()
		l.notify()

		r := recheckNegative(ctx, m, ProbeModel(ctx, m))

		effective, _ := l.applyVerdict(m.ID, r)
		if effective.State != StateThrottled {
			allThrottled = false
		}
		// Probe verdicts feed the throttle ledger too: a throttled verdict
		// opens/extends an episode, a usable one closes it.
		switch effective.State {
		case StateThrottled:
			l.throttle.MarkThrottled(m.ID, 0)
		case StateAvailable:
			l.throttle.MarkOK(m.ID)
		}
		if l.OnProbeResult != nil && r.TTFTMs > 0 {
			l.OnProbeResult(r)
		}
		l.notify()
	}
	l.mu.Lock()
	l.probingModel = ""
	l.mu.Unlock()
	eg := DetectEgress(ctx)

	l.mu.Lock()
	if l.OnProbeEdge != nil && allThrottled && !l.quotaWallActive {
		go l.OnProbeEdge("", "", StateThrottled)
	}
	l.quotaWallActive = allThrottled
	if egressChanged(l.egress, eg) {
		l.egress = eg
	}
	l.lastProbeAt = time.Now()
	if allThrottled {
		l.backoffRounds++
		d := time.Duration(30*(1<<uint(min2(l.backoffRounds, 2)))) * time.Minute
		if d > 120*time.Minute {
			d = 120 * time.Minute
		}
		l.nextProbeAfter = time.Now().Add(d)
	} else {
		l.backoffRounds = 0
		l.nextProbeAfter = time.Time{}
	}
	l.mu.Unlock()
	l.notify()
}

// ProbeOne re-tests a single model on demand (the per-card 测试 button) and
// applies the same bookkeeping as a round: availability, throttle ledger,
// first-token sample, change notifications.
func (l *Lane) ProbeOne(model string) ProbeResult {
	return l.ProbeOneCtx(context.Background(), model)
}

// ProbeOneCtx is ProbeOne through a context that may carry a probe phase sink,
// so a caller watching one card sees the request go out and the answer start
// instead of waiting blind for a queued free lane.
func (l *Lane) ProbeOneCtx(ctx context.Context, model string) ProbeResult {
	var entry ModelInfo
	l.mu.RLock()
	for i := range l.catalog {
		if l.catalog[i].ID == model {
			entry = l.catalog[i]
			break
		}
	}
	l.mu.RUnlock()
	if entry.ID == "" {
		return ProbeResult{Model: model, State: StateUnknown, At: time.Now().UnixMilli()}
	}

	var r ProbeResult
	if entry.Channel == ChannelKilo {
		// Presence in the free-pool roster is the verdict; probing would only
		// spend a scheduling slot.
		r = ProbeResult{Model: entry.ID, State: StateAvailable, At: time.Now().UnixMilli(),
			Detail: "在列（免费池名单即判定）"}
		l.applyVerdict(model, r)
	} else {
		// The button asks "can this model answer right now", so the raw verdict
		// is what the caller sees; the stored state is still guarded by live
		// traffic.
		r = recheckNegative(ctx, entry, ProbeModel(ctx, entry))
		r, _ = l.applyVerdict(model, r)
	}
	switch r.State {
	case StateThrottled:
		l.throttle.MarkThrottled(model, 0)
	case StateAvailable:
		l.throttle.MarkOK(model)
	}
	if l.OnProbeResult != nil && r.TTFTMs > 0 {
		l.OnProbeResult(r)
	}
	l.notify()
	return r
}

// StartLoops runs the periodic catalog refresh + probe + egress watch until
// ctx is cancelled.
func (l *Lane) StartLoops(ctx context.Context, probeInterval time.Duration) {
	if probeInterval < time.Minute {
		probeInterval = time.Minute
	}
	go func() {
		l.RefreshCatalog(ctx)
		l.ProbeRound(ctx, false)
		t := time.NewTicker(probeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.RefreshCatalog(ctx)
				l.ProbeRound(ctx, false)
			}
		}
	}()
	go func() {
		t := time.NewTicker(120 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if RotationActive() {
					// With per-request egress rotation the exit IP is expected
					// to change constantly — treating each change as a network
					// event would fire a full probe round every two minutes.
					continue
				}
				eg := DetectEgress(ctx)
				l.mu.Lock()
				changed := egressChanged(l.egress, eg)
				if changed {
					l.egress = eg
				}
				l.mu.Unlock()
				if changed {
					l.ProbeRound(ctx, true)
				}
			}
		}
	}()
}

func egressChanged(a, b Egress) bool {
	if b.IP == "" {
		return false
	}
	return a.IP != b.IP || a.Country != b.Country
}

func (l *Lane) notify() {
	if l.OnChange != nil {
		go l.OnChange()
	}
}

// quotaEgressRetries caps the extra exits one request tries after a 429 while
// rotation is live. Each retry costs one upstream round-trip and no quota (a
// refused request never consumed any).
const quotaEgressRetries = 3

// The retry-on-another-exit loop needs a total-time bound as well as an attempt
// count: a per-attempt count alone lets a stalling exit burn a whole attempt
// each round, so 3 retries can still mean minutes of waiting.
//
// The bound depends on WHICH failure is being retried, because the two classes
// answer on completely different time scales:
//
//   - A 429 is a fast refusal (well under a second), so all three retries fit
//     comfortably in a few seconds.
//   - A transport failure or an upstream 5xx is a SLOW failure: measured here at
//     30-90s per attempt. A single budget sized for 429s therefore expires
//     before the first retry can even start, and the loop silently degrades to
//     attempts=1 — which is what it did on a real install, returning 502 after
//     93 seconds while healthy exits sat idle.
//
// So the budget is per class. Slow failures get enough room for the retries to
// actually happen, and the attempt count still caps how many.
const quotaEgressTotalMS = 40000

// slowEgressTotalMS is the budget for the exit-shaped slow failures. It has to
// clear at least one full attempt (the response-header timeout) plus room for a
// retry, or the retry never runs.
const slowEgressTotalMS = 210000

// Complete runs one logical turn: effort budgeting, wire encoding, the
// fingerprint gate, streaming decode, and — for a cut pure-reasoning turn —
// one bounded checkpoint recovery request.
// Complete runs one logical turn. When the requested model is rate-limited or
// otherwise refuses before anything was streamed, it switches to the next
// available model — a 429 must never be retried on the same model (quota is
// accounted per session; a retry is a second burn), so switching is the only
// honest lever.
func (l *Lane) Complete(ctx context.Context, req Request, emit func(Chunk)) (Outcome, *UpstreamError) {
	// Concurrency gate: wait here when saturated (the caller can still cancel).
	select {
	case upstreamSem <- struct{}{}:
		defer func() { <-upstreamSem }()
	case <-ctx.Done():
		return Outcome{}, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
	}

	effort := req.Effort
	if effort == "" {
		effort = EffortOf(req.Model)
	}
	requested := BaseModelId(req.Model)

	l.mu.RLock()
	failover := l.failoverEnabled
	failoverMax := l.failoverMax
	l.mu.RUnlock()

	model := requested
	failovers := 0
	egressTries := 0
	// The deadline is per attempt: each round gets the budget its own failure
	// class deserves, rather than one bound fixed before we knew what failed.
	egressDeadline := time.Now().Add(quotaEgressTotalMS * time.Millisecond)
	tried := map[string]bool{requested: true}
	for {
		out, uerr, sawAny := l.attemptModel(ctx, req, model, effort, emit)
		out.ServedModel = model
		out.Failovers = failovers
		if uerr == nil {
			return out, nil
		}
		// Under rotation a 429 is one exit's answer, not the model's: quota is
		// counted per egress IP and a funded exit may be two hops away, so ask
		// the pool a few times before believing it. Every retry is a fresh dial
		// (rotate disables keep-alives) and the node round-robin moves on.
		// Slow failures get their own budget: a retry that cannot start inside the
		// 429-sized window would otherwise be skipped, leaving attempts=1.
		if egressRetryable(uerr.Code) && !sawAny && RotationActive() && egressTries < quotaEgressRetries &&
			time.Now().Before(egressDeadline.Add(egressBudgetFor(uerr.Code))) {
			egressTries++
			continue
		}
		if uerr.Code == CodeQuota {
			l.MarkThrottled(model, uerr.RetryAfter)
		}
		// No switch after content reached the caller (it would duplicate
		// output), on egress-wide failures, or when the caller's own model
		// error can't plausibly be answered by another model.
		if !failover || sawAny || !switchableFailure(uerr) {
			return out, uerr
		}
		next := l.nextCandidate(req.Needs, tried)
		if next == "" {
			// The lane has nothing left to offer this request — tell the
			// gateway so it can (optionally) offer the turn to user-added
			// providers.
			uerr.Exhausted = true
			return out, uerr
		}
		if failovers >= failoverMax {
			// Attempt budget spent, but the lane still holds untried
			// candidates — not exhausted, just capped.
			return out, uerr
		}
		tried[next] = true
		model = next
		failovers++
	}
}

// switchableFailure reports whether this failure class can plausibly be
// answered by a different model. Transport/credential faults are egress-wide —
// switching models would just pay latency for the same refusal.
// egressRetryable reports whether the same model is worth asking again through
// a DIFFERENT exit. Quota is the original case (a 429 is one IP's answer, not
// the model's), but a transport failure or an upstream 5xx is just as
// exit-shaped: the request never got an answer, so asking another exit costs
// nothing and can turn a 502 into a 200. Before this, only CodeQuota retried,
// so a single stalling exit failed the turn while the rest of the pool idled.
// egressBudgetFor is the extra time this failure class may spend retrying
// before the loop gives up. 429s need almost none; slow failures need a full
// attempt's worth, because their per-attempt cost IS the reason they failed.
func egressBudgetFor(code string) time.Duration {
	switch code {
	case CodeQuota:
		return 0
	}
	return slowEgressTotalMS * time.Millisecond
}

func egressRetryable(code string) bool {
	switch code {
	case CodeQuota, CodeTransport, CodeServer, CodeTimeout:
		return true
	}
	return false
}

// A failure another model can plausibly answer. CodeTransport belongs here:
// the transport error is an egress-shaped failure, not a verdict about this
// model, and leaving it out meant a single stalled exit returned 502 to the
// user while a dozen other exits sat idle — observed at 217 seconds of
// waiting for one request, attempts=1.
func switchableFailure(uerr *UpstreamError) bool {
	switch uerr.Code {
	case CodeQuota, CodeRegion, CodeServer, CodeTimeout, CodeEmpty, CodeTransport:
		return true
	}
	return false
}

// Candidates lists up to n failover targets — the hint a 429 error body
// carries when the gateway itself ran out of models to try.
func (l *Lane) Candidates(needs Needs, exclude string, n int) []string {
	tried := map[string]bool{exclude: true}
	out := []string{}
	for len(out) < n {
		c := l.nextCandidate(needs, tried)
		if c == "" {
			break
		}
		tried[c] = true
		out = append(out, c)
	}
	return out
}

// nextCandidate picks the best failover target: probed-available models
// first, then unprobed ones; throttled, region-blocked, unavailable, decision
// models and already-tried ids are skipped. With smart routing on, candidates
// that cannot carry the request's modalities are hard-filtered and the
// selected strategy (latency) orders inside the availability band; with it
// off, or with the catalog strategy, ties keep catalog order.
func (l *Lane) nextCandidate(needs Needs, tried map[string]bool) string {
	l.mu.RLock()
	cat := make([]ModelInfo, len(l.catalog))
	copy(cat, l.catalog)
	av := make(map[string]ProbeResult, len(l.availability))
	for k, v := range l.availability {
		av[k] = v
	}
	smart := l.smartRouting
	strategy := l.strategy
	ttftFn := l.ttft
	l.mu.RUnlock()

	best, bestKey := "", candidateKey{avail: 99}
	for i, m := range cat {
		if m.SystemOne || m.Wire == "systemone" {
			continue
		}
		if tried[m.ID] || l.throttle.Throttled(m.ID) {
			continue
		}
		switch FreshState(av[m.ID]) {
		case StateUnavailable, StateRegionBlock:
			continue
		}
		if smart {
			if !satisfiesNeeds(m, needs) {
				continue
			}
			if needs.PromptTokens > 0 && m.ContextWindow > 0 && needs.PromptTokens > m.ContextWindow {
				continue
			}
		}
		key := l.candidateKey(m, i, av[m.ID], strategy, ttftFn)
		if !smart {
			key = candidateKey{avail: key.avail, catalogIdx: key.catalogIdx}
		}
		if best == "" || lessCandidate(key, bestKey) {
			best, bestKey = m.ID, key
		}
	}
	return best
}

func (l *Lane) attemptModel(ctx context.Context, req Request, model, effort string, emit func(Chunk)) (Outcome, *UpstreamError, bool) {
	start := time.Now()
	base := model

	l.mu.RLock()
	var entry *ModelInfo
	for i := range l.catalog {
		if l.catalog[i].ID == base {
			entry = &l.catalog[i]
			break
		}
	}
	defMax := l.defaultMaxTokens
	l.mu.RUnlock()
	if entry == nil {
		entry = &ModelInfo{ID: base, Name: base, Wire: WireFor(base),
			Reasoning: true, ContextWindow: 131072, MaxOutput: 8192, CanDisableThinking: true}
	}

	if entry.SystemOne || entry.Wire == "systemone" {
		return Outcome{}, &UpstreamError{Code: CodeServer,
			Message: base + " is a System One decision model: it answers typed questions on /zen/v1/systemone, not chat requests"}, false
	}

	budget := BudgetFor(effort, *entry, req.MaxTokens, defMax)
	messages := RepairToolPairing(req.Messages)
	wire := entry.Wire
	style := mapWireStyle(wire)

	// Choose the exit before the request is built, then derive the upstream
	// session from the exit's stable identity. Doing it in this order is what
	// makes one conversation one exit: the same prompt prefix keeps landing on
	// the same egress, so the upstream has a chance to reuse its prompt cache
	// instead of re-prefilling the whole conversation on every turn.
	plan := ExitPlan{Model: base}
	if r := CurrentRotator(); r != nil {
		plan.NodeID = r.Pick(req.SessionSeed, base)
		plan.ExitKey = r.ExitKey(plan.NodeID)
	}
	ctx = WithExitPlan(ctx, plan)

	// The Zen lane fingerprints tools and mints session/request ids; the Kilo
	// pool has no tool-name gate and no session concept — its models see the
	// caller's tools exactly as declared, and a plain POST per turn.
	kilo := entry.Channel == ChannelKilo
	buildBody := func(msgs []Message, tools []ToolDef, maxTokens int) map[string]any {
		body := map[string]any{"model": base, "stream": true}
		switch wire {
		case "responses":
			instructions, items := ToResponseInput(msgs)
			body["input"] = items
			body["max_output_tokens"] = maxTokens
			body["store"] = false
			if instructions != "" {
				body["instructions"] = instructions
			}
			if t := ToToolDefs(tools, style); t != nil {
				body["tools"] = t
			}
		case "messages":
			system, msgs := ToClaudeMessages(msgs)
			body["messages"] = msgs
			body["max_tokens"] = maxTokens
			body["anthropic_version"] = "2023-06-01"
			if system != "" {
				body["system"] = system
			}
			if t := ToToolDefs(tools, style); t != nil {
				body["tools"] = t
			}
		default:
			body["messages"] = ToChatMessages(msgs)
			body["max_tokens"] = maxTokens
			if t := ToToolDefs(tools, style); t != nil {
				body["tools"] = t
			}
		}
		// Kilo's thinking control is the effort field itself, not a token
		// ladder: the selected rung rides the request as the pool's own
		// reasoning effort (light→low, balanced→medium, deep→high), verified
		// against the families the pool honours. max_tokens still caps output.
		if kilo && entry.Reasoning {
			body["reasoning"] = map[string]any{"effort": kiloEffortRung(effort)}
		}
		return body
	}

	// The Zen lane fingerprints tools and mints session/request ids; the Kilo
	// pool has no tool-name gate and no session concept — its models see the
	// caller's tools exactly as declared, and a plain POST per turn.
	session := SessionForExit(req.SessionSeed, plan.ExitKey)
	requestID := RequestIdFor(session, req.TurnSeed)
	post := func(cctx context.Context, body map[string]any, onData func(payload []byte) error) (*Usage, error) {
		if kilo {
			return PostKiloStreamed(cctx, body, onData)
		}
		return PostStreamed(cctx, EndpointFor(base), body, session, requestID, onData)
	}

	body := buildBody(messages, req.Tools, budget)
	rename := map[string]string{}
	if !kilo {
		rename = ApplyFingerprint(body, style)
	}

	decoder := NewDecoder(wire, rename, emit)
	var firstAt int64
	usage, err := post(ctx, body, func(p []byte) error {
		if firstAt == 0 {
			firstAt = time.Since(start).Milliseconds()
		}
		decoder.Decode(p)
		return nil
	})
	result := decoder.Finish()
	if usage != nil && result.Usage.TotalTokens == 0 {
		result.Usage = *usage
	}
	sawAny := result.SawText || result.SawToolCall || result.SawReasoning

	if err != nil {
		l.record(ctx, CallRecord{Model: base, Ok: false, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, result, err, firstAt)
		// Book the failure against (exit, model) so later requests skip it. The
		// ledger only ever skips known-bad pairs — it is never used to prefer a
		// "known good" one, which would pin every concurrent request onto the
		// first success and burn that exit's allowance.
		switch asUpstream(err).Code {
		case CodeRegion:
			ReportExit(plan.NodeID, base, "region")
		case CodeQuota:
			ReportExit(plan.NodeID, base, "limited")
		case CodeTransport, CodeServer, CodeTimeout, CodeEmpty:
			ReportExit(plan.NodeID, base, "other")
		}
		// The caller decides what a 429 means: under rotation it re-asks the
		// pool before the model gets a throttle episode.
		return Outcome{Finish: result.Finish}, asUpstream(err), sawAny
	}
	if result.SawFinish {
		switch result.FinishToken {
		case "failed", "cancelled":
			// A terminal frame that names the turn a failure is not a clean
			// stop, even though the mapped reason collapses into stop.
			l.record(ctx, CallRecord{Model: base, Ok: false, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, result, nil, firstAt)
			return Outcome{Usage: result.Usage}, &UpstreamError{Code: CodeServer,
				Message: "upstream ended the turn with " + result.FinishToken}, sawAny
		}
		l.NoteRealSuccess(base)
		// A served request is proof: clear this exit's cooldown and the ban
		// booked against this very (exit, model) pair.
		if r := CurrentRotator(); r != nil && plan.NodeID != "" {
			r.Revive(plan.NodeID, base)
		}
		l.record(ctx, CallRecord{Model: base, Ok: true, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, result, nil, firstAt)
		return Outcome{Usage: result.Usage, Finish: result.Finish}, nil, true
	}

	// Ending without its answer. Three shapes get one continuation each —
	// a cut pure-reasoning turn (no finish), a clean stop that carried only
	// thinking, and an answer truncated at the output ceiling — everything
	// else reports honestly.
	elapsed := time.Since(start).Milliseconds()
	interrupted := canRecover(result, elapsed)
	silentStop := !interrupted && canRecoverSilentStop(result, elapsed)
	maxTokensCut := !interrupted && !silentStop && canContinueFromCut(result, elapsed)

	if interrupted || silentStop || maxTokensCut {
		remaining := budget - result.Usage.Output
		recovBudget := min2(RecoveryMaxOutputTokens, remaining)
		checkpoint := result.ReasoningText
		var recMsgs []Message
		if maxTokensCut {
			// The partial answer is already valid assistant output; the wire's
			// own history carries it, no checkpoint injection.
			recMsgs = append(append([]Message{}, messages...),
				Message{Role: RoleAssistant, Parts: []Part{TextPart{Text: result.AnswerText}}},
				Message{Role: RoleUser, Parts: []Part{TextPart{Text: continuationInstruction}}})
			checkpoint = result.AnswerText
		} else {
			recMsgs = append(append([]Message{}, messages...), Message{Role: RoleUser, Parts: []Part{
				TextPart{Text: recoveryInstruction + jsonString(result.ReasoningText)},
			}})
		}
		if recovBudget < 512 || !checkpointFits(recMsgs, *entry, checkpoint, recovBudget) {
			l.record(ctx, CallRecord{Model: base, Ok: false, Truncated: true, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, result, nil, firstAt)
			return Outcome{Usage: result.Usage}, &UpstreamError{Code: CodeTransport, Message: "upstream cut a turn short; the continuation does not fit the context"}, sawAny
		}
		recBody := buildBody(recMsgs, nil, recovBudget)
		if !kilo {
			ApplyFingerprint(recBody, style)
		}
		recDecoder := NewDecoder(wire, rename, emit)
		// The continuation deadline is the remainder of the turn's window —
		// whatever the first segment left, at most.
		remainingWall := RecoveryTotalTimeoutMS - elapsed
		if remainingWall > RecoveryMaxContinuationMS {
			remainingWall = RecoveryMaxContinuationMS
		}
		rctx, cancel := context.WithTimeout(ctx, time.Duration(remainingWall)*time.Millisecond)
		defer cancel()

		recUsage, rerr := post(rctx, recBody, func(p []byte) error {
			recDecoder.Decode(p)
			return nil
		})
		recResult := recDecoder.Finish()
		if recUsage != nil && recResult.Usage.TotalTokens == 0 {
			recResult.Usage = *recUsage
		}
		if rerr == nil && recResult.SawFinish && recResult.FinishToken != "failed" && recResult.FinishToken != "cancelled" && recResult.SawText {
			recResult.Usage.Merge(&result.Usage)
			l.NoteRealSuccess(base)
			// The continuation rode the same exit and served: same proof as a
			// clean turn, so lift this pair's cooldown/ban too.
			if r := CurrentRotator(); r != nil && plan.NodeID != "" {
				r.Revive(plan.NodeID, base)
			}
			l.record(ctx, CallRecord{Model: base, Ok: true, Recovered: true, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, recResult, nil, firstAt)
			return Outcome{Usage: recResult.Usage, Finish: recResult.Finish, Recovered: true}, nil, true
		}
		recResult.Usage.Merge(&result.Usage)
		l.record(ctx, CallRecord{Model: base, Ok: false, Truncated: true, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, recResult, rerr, firstAt)
		if rerr != nil {
			uerr := asUpstream(rerr)
			if uerr.Code == CodeQuota {
				l.MarkThrottled(base, uerr.RetryAfter)
			}
			return Outcome{Usage: recResult.Usage}, uerr, true
		}
		return Outcome{Usage: recResult.Usage}, &UpstreamError{Code: CodeTransport, Message: "upstream cut the stream; the continuation did not complete"}, true
	}

	// Not recoverable: a mid-content cut is a property of this turn's length,
	// and re-sending would pay the same five minutes again.
	l.record(ctx, CallRecord{Model: base, Ok: false, Truncated: true, Exit: plan.NodeID, Effort: effort, Agent: req.Agent, At: time.Now().UnixMilli()}, result, nil, firstAt)
	return Outcome{Usage: result.Usage}, &UpstreamError{Code: CodeTransport, Message: "upstream closed the stream without a finish token"}, sawAny
}
func (l *Lane) record(ctx context.Context, rec CallRecord, result StreamResult, err error, firstAt int64) {
	if l.OnCall == nil {
		return
	}
	rec.Trace = TraceFrom(ctx)
	if ue := asUpstream(err); ue != nil {
		rec.ErrCode = ue.Code
		rec.UpstreamRID = ue.UpstreamRID
	}
	if err != nil {
		rec.Ok = false
	} else {
		rec.Ok = result.SawFinish
	}
	rec.Input = result.Usage.Input
	rec.Output = result.Usage.Output
	rec.Reasoning = result.Usage.Reasoning
	rec.CacheRead = result.Usage.CacheRead
	rec.TTFTMs = firstAt
	if result.SawText {
		rec.DecodeTok = WindowTokens(result.Usage, result.SawReasoning)
	}
	if !rec.Ok && err == nil {
		rec.Truncated = true
	}
	if result.Usage.TotalTokens == 0 && err == nil && result.SawFinish {
		rec.NoUsage = true
	}
	l.OnCall(rec)
}

func canRecover(result StreamResult, elapsedMS int64) bool {
	return elapsedMS < RecoveryTotalTimeoutMS &&
		!result.SawFinish &&
		result.SawReasoning &&
		!result.SawText &&
		!result.SawToolCall &&
		!result.CheckpointTruncated &&
		nonWhitespace(result.ReasoningText)
}

// canRecoverSilentStop: the upstream closed cleanly but produced only
// thinking and no answer — most clients judge such a turn an empty response,
// so it gets the same one checkpoint continuation a cut pure-reasoning turn
// gets. The raw token decides "clean": the mapped Finish collapses unknown
// spellings into stop, so failed/cancelled must be excluded here by token.
func canRecoverSilentStop(result StreamResult, elapsedMS int64) bool {
	if !result.SawFinish || elapsedMS >= RecoveryTotalTimeoutMS {
		return false
	}
	switch result.FinishToken {
	case "", "stop", "end_turn", "stop_sequence":
	default:
		return false
	}
	return result.SawReasoning &&
		!result.SawText &&
		!result.SawToolCall &&
		!result.CheckpointTruncated &&
		nonWhitespace(result.ReasoningText)
}

// canContinueFromCut: the answer hit the output ceiling (finish length) and
// was cut mid-answer, not faulted. The partial answer feeds back as an
// assistant turn and exactly one continuation with the remaining budget
// replaces the client's manual "send 继续" relay. First segment only — a
// continuation that hits the wall again reports max-tokens honestly instead
// of continuing forever.
func canContinueFromCut(result StreamResult, elapsedMS int64) bool {
	return result.SawFinish &&
		result.Finish == FinishMaxTokens &&
		result.SawText &&
		!result.SawToolCall &&
		!result.BrokenToolCall &&
		elapsedMS < RecoveryTotalTimeoutMS &&
		nonWhitespace(result.AnswerText)
}

func nonWhitespace(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\n' && r != '\r' && r != '\t' {
			return true
		}
	}
	return false
}

// checkpointFits is a conservative byte-based margin check, not a tokenizer.
func checkpointFits(messages []Message, entry ModelInfo, checkpoint string, outputBudget int) bool {
	ctx := entry.ContextWindow
	if ctx <= 0 {
		return true
	}
	if len(checkpoint) > (ctx-outputBudget)/2 {
		return false
	}
	total := len(checkpoint) + outputBudget
	for _, m := range messages {
		total += len(m.TextOf())
	}
	return total < ctx
}

func mapWireStyle(wire string) string {
	switch wire {
	case "responses":
		return "flat"
	case "messages":
		return "claude"
	default:
		return "chat"
	}
}

func asUpstream(err error) *UpstreamError {
	if err == nil {
		return nil
	}
	if ue, ok := err.(*UpstreamError); ok {
		return ue
	}
	return &UpstreamError{Code: CodeTransport, Message: err.Error()}
}
