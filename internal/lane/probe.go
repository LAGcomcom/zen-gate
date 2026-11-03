package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Availability states for one model.
const (
	StateAvailable   = "available"
	StateRegionBlock = "region-blocked"
	StateUnavailable = "unavailable"
	StateThrottled   = "throttled"
	StateUnknown     = "unknown"
)

// ProbeResult is one model's verdict.
type ProbeResult struct {
	Model     string `json:"model"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
	TTFTMs    int64  `json:"ttftMs,omitempty"`
	LatencyMs int64  `json:"latencyMs"`
	At        int64  `json:"at"`
}

// stateTTL bounds how long one sampled verdict speaks for the model. Free
// quota is counted per egress IP and moves on a seconds scale, so a refusal is
// a snapshot of one exit at one moment — not a property of the model.
const stateTTL = 10 * time.Minute

// FreshState is the only way to act on a verdict. Past the TTL it reads
// "unknown": the model stays listed and routable until a probe renews the
// claim, instead of one bad sample hiding it for the whole
// probeIntervalMinutes cycle.
func FreshState(r ProbeResult) string {
	if r.State == "" {
		return StateUnknown
	}
	if r.At > 0 && time.Since(time.UnixMilli(r.At)) > stateTTL {
		return StateUnknown
	}
	return r.State
}

// probeRechecks is how many extra exits a refusal is re-asked on before it
// becomes a verdict. Under egress rotation each retry leaves from a different
// IP, which is the only way to tell "this exit has no quota" from "the model
// is down".
const probeRechecks = 2

// recheckNegative re-asks a refusal elsewhere. An available answer anywhere
// wins immediately; otherwise the last refusal is what we publish. probe is
// the caller's channel-aware prober — re-checks must ride the same source as
// the first verdict, never the Zen endpoint a WorkBuddy model cannot answer.
func recheckNegative(ctx context.Context, m ModelInfo, r ProbeResult, probe func(context.Context, ModelInfo) ProbeResult) ProbeResult {
	if r.State == StateAvailable || r.State == StateUnknown {
		return r
	}
	for i := 0; i < probeRechecks; i++ {
		if ctx.Err() != nil {
			break
		}
		EmitProbeStep(ctx, PhaseRequest, StepRunning, 0,
			fmt.Sprintf("换个出口复核 %d/%d", i+1, probeRechecks))
		rr := probe(ctx, m)
		if rr.State == StateAvailable {
			return rr
		}
		r = rr
	}
	return r
}

// ProbeModel sends the smallest streaming request per wire and classifies the
// answer. Only a gateway refusal that names the model counts against it:
// 5xx, quota and transport faults stay "unknown" and never remove a model
// from a picker. Each phase is reported through the context sink so a watcher
// sees the request go out and the answer start before the verdict lands.
func ProbeModel(ctx context.Context, m ModelInfo) ProbeResult {
	EmitProbeStep(ctx, PhaseRequest, StepRunning, 0, EndpointFor(m.ID)+" · "+m.ID)
	res := probeModelInner(ctx, m)
	EmitProbeStep(ctx, PhaseVerdict, StatusForProbeState(res.State), res.LatencyMs, res.Detail)
	return res
}

func probeModelInner(ctx context.Context, m ModelInfo) ProbeResult {
	res := ProbeResult{Model: m.ID, State: StateUnknown, At: time.Now().UnixMilli()}
	if m.Wire == "systemone" || IsSystemOneModel(m.ID) {
		return probeSystemOne(ctx, m, res)
	}
	body := map[string]any{"model": m.ID, "stream": true, "max_tokens": 16}
	var wire string
	switch m.Wire {
	case "responses":
		wire = "flat"
		body["input"] = "ping"
		body["max_output_tokens"] = 16
		body["store"] = false
		delete(body, "max_tokens")
	case "messages":
		wire = "claude"
		body["messages"] = []map[string]any{{"role": "user", "content": "ping"}}
	case "chat":
		wire = "chat"
		body["messages"] = []map[string]any{{"role": "user", "content": "ping"}}
	}
	ApplyFingerprint(body, wire)

	start := time.Now()
	session := SessionForConversation("probe:" + m.ID)
	firstAt := int64(0)
	var lastPayload atomic.Value
	_, err := PostStreamed(ctx, EndpointFor(m.ID), body, session, MintRequestId(0), func(p []byte) error {
		if firstAt == 0 {
			// Round up to 1 ms: a loopback answer is faster than a millisecond,
			// and a 0 TTFT both drops the sample and hides the phase.
			firstAt = atLeastMS(start)
			res.TTFTMs = firstAt
			EmitProbeStep(ctx, PhaseFirstByte, StepOK, firstAt, "")
		}
		lastPayload.Store(string(p))
		return nil
	})
	res.LatencyMs = atLeastMS(start)
	if err != nil {
		ue, _ := err.(*UpstreamError)
		if ue == nil {
			ue = &UpstreamError{Code: CodeTransport, Message: err.Error()}
		}
		switch ue.Code {
		case CodeRegion, CodeFreeTier:
			res.State = StateRegionBlock
		case CodeQuota:
			res.State = StateThrottled
		case CodeServer:
			if ue.Unavailable {
				res.State = StateUnavailable
			}
		}
		res.Detail = ue.Message
		return res
	}
	// A 200 stream can still carry an in-band refusal frame.
	if payload, ok := lastPayload.Load().(string); ok && payload != "" {
		if st, detail := inBandRefusal(payload); st != "" {
			res.State = st
			res.Detail = detail
			return res
		}
	}
	res.State = StateAvailable
	return res
}

var (
	inBandUnavailableRe = regexp.MustCompile(`(?i)model is unavailable|not supported|does not exist|invalid model`)
	inBandRegionRe      = regexp.MustCompile(`(?i)not available in your country|region`)
)

func inBandRefusal(payload string) (state, detail string) {
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil {
		return "", ""
	}
	if e, has := m["error"]; has {
		s := jsonString(e)
		switch {
		case inBandRegionRe.MatchString(s):
			return StateRegionBlock, s
		case inBandUnavailableRe.MatchString(s):
			return StateUnavailable, s
		default:
			return StateUnknown, s
		}
	}
	return "", ""
}

// probeSystemOne pings a decision model on its own wire: a typed question
// with a one-word state — no stream, no tools, no chat fields. A 200 with an
// answers object is the same "alive" signal a chat ping is for chat wires.
func probeSystemOne(ctx context.Context, m ModelInfo, res ProbeResult) ProbeResult {
	body := map[string]any{
		"model": m.ID,
		"state": "probe ping",
		"questions": map[string]any{
			"probe": map[string]any{
				"type":         "noul",
				"instructions": "Return the answer that means yes.",
			},
		},
	}
	start := time.Now()
	session := SessionForConversation("probe:" + m.ID)
	sawAnswers := false
	firstSeen := false
	_, err := PostStreamed(ctx, EndpointFor(m.ID), body, session, MintRequestId(0), func(p []byte) error {
		if !firstSeen {
			firstSeen = true
			ms := atLeastMS(start)
			res.TTFTMs = ms
			EmitProbeStep(ctx, PhaseFirstByte, StepOK, ms, "")
		}
		var frame map[string]any
		if json.Unmarshal(p, &frame) == nil {
			if _, has := frame["answers"]; has {
				sawAnswers = true
			}
		}
		return nil
	})
	res.LatencyMs = atLeastMS(start)
	if res.TTFTMs == 0 {
		res.TTFTMs = res.LatencyMs
	}
	if err != nil {
		ue, _ := err.(*UpstreamError)
		if ue == nil {
			ue = &UpstreamError{Code: CodeTransport, Message: err.Error()}
		}
		switch ue.Code {
		case CodeRegion, CodeFreeTier:
			res.State = StateRegionBlock
		case CodeQuota:
			res.State = StateThrottled
		case CodeServer:
			if ue.Unavailable {
				res.State = StateUnavailable
			}
		}
		res.Detail = ue.Message
		return res
	}
	if !sawAnswers {
		res.State = StateUnknown
		res.Detail = "response carried no answers object"
		return res
	}
	res.State = StateAvailable
	return res
}

// ProbeCatalog probes models with bounded concurrency. A strictly serial probe
// means the last model in a large catalog is only verified after the whole
// round finished, so its "available" verdict is already up to stateTTL old by
// the time routing reads it. A small worker pool keeps the round fast while
// still delivering results progressively. onResult is serialized through a
// mutex so callers need not be concurrency-safe.
//
// The bound is small on purpose and must stay small. Unlike the node egress
// probe (subs.probeMaxParallel), every call here is a real request to a real
// account: it spends quota and it is attributable. A wave of N would show the
// upstream one account calling from N exits at the same instant — the exact
// signature of a shared account, which is answered with a ban rather than an
// error. 8 is already the ceiling of what a free tier tolerates; widening it
// does not make the round faster, it makes the account disposable.
func ProbeCatalog(ctx context.Context, models []ModelInfo, onResult func(ProbeResult)) {
	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	emit := func(r ProbeResult) {
		if onResult == nil {
			return
		}
		mu.Lock()
		onResult(r)
		mu.Unlock()
	}
	for _, m := range models {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m ModelInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			emit(ProbeModel(ctx, m))
		}(m)
	}
	wg.Wait()
}

// Egress reports the machine's public exit IP and country.
type Egress struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
}

// DetectEgress asks three IP echo services in order; failure is tolerated
// (empty result). It rides the shared upstream client so the reported exit
// matches what the gateway actually sees when a proxy is configured.
func DetectEgress(ctx context.Context) Egress {
	return DetectEgressVia(ctx, laneHTTP)
}

// DetectEgressVia is DetectEgress through an explicit client (proxy testing).
func DetectEgressVia(ctx context.Context, client *http.Client) Egress {
	e, _ := DetectEgressViaErr(ctx, client)
	return e
}

// egressSource is one IP echo endpoint plus how to read its answer.
type egressSource struct {
	url   string
	parse func([]byte) Egress
}

// egressSources are raced, not tried in order.
//
// They answer the same question about the same exit, so trying them one after
// another only bought the sum of three timeouts: at the old 12s budget a single
// node could spend 36s failing to learn its own IP, and a probe round over a
// 64-node pool spent that per node. Racing them costs a few extra KB of
// request and collapses the whole question to one round-trip.
var egressSources = []egressSource{
	{"https://ipinfo.io/json", func(b []byte) Egress {
		var m struct {
			IP      string `json:"ip"`
			Country string `json:"country"`
		}
		_ = json.Unmarshal(b, &m)
		return Egress{IP: m.IP, Country: strings.ToUpper(m.Country)}
	}},
	{"https://api.ipify.org?format=json", func(b []byte) Egress {
		var m struct {
			IP string `json:"ip"`
		}
		_ = json.Unmarshal(b, &m)
		return Egress{IP: m.IP}
	}},
	{"https://ipapi.co/json/", func(b []byte) Egress {
		var m struct {
			IP          string `json:"ip"`
			CountryCode string `json:"country_code"`
		}
		_ = json.Unmarshal(b, &m)
		return Egress{IP: m.IP, Country: strings.ToUpper(m.CountryCode)}
	}},
}

// countryGrace is how long an answer with an IP but no country waits for one
// that also names the country. ipify is usually first but never says where;
// ipinfo and ipapi do. The IP is the identity that matters for rotation and
// drift, so a country-less answer is still an answer — it just gets a short
// grace period, not the whole probe budget.
const countryGrace = 300 * time.Millisecond

// DetectEgressViaErr keeps the failure reason alongside the (empty) answer.
//
// The caller needs the reason, not just the absence of a result: "no echo
// service could be read" and "the transport refused every request" lead to
// opposite conclusions about a node — unknown versus dead — and an operator
// cannot act on either without knowing which one happened.
func DetectEgressViaErr(ctx context.Context, client *http.Client) (Egress, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type answer struct {
		eg  Egress
		err error
	}
	// Buffered to len(sources): the losers of the race must be able to finish
	// writing after the winner returned, or they leak a goroutine each.
	ch := make(chan answer, len(egressSources))
	for _, src := range egressSources {
		go func(src egressSource) {
			eg, err := fetchEgress(ctx, client, src)
			ch <- answer{eg: eg, err: err}
		}(src)
	}

	var ipOnly Egress // first answer that had an IP but no country
	var firstErr error
	grace := time.NewTimer(countryGrace)
	defer grace.Stop()
	graceC := grace.C

	for i := 0; i < len(egressSources); i++ {
		select {
		case a := <-ch:
			switch {
			case a.err != nil:
				if firstErr == nil {
					firstErr = a.err
				}
			case a.eg.Country != "":
				// Complete answer: nothing left worth waiting for.
				return a.eg, nil
			case ipOnly.IP == "":
				ipOnly = a.eg
			}
		case <-graceC:
			// A country-less answer is already in hand; do not hold the round
			// hostage to the slower services just for a display field.
			graceC = nil
			if ipOnly.IP != "" {
				return ipOnly, nil
			}
		}
	}
	if ipOnly.IP != "" {
		return ipOnly, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("三个出口回显服务都没有给出 IP")
	}
	return Egress{}, firstErr
}

// fetchEgress asks one echo service through the given client.
func fetchEgress(ctx context.Context, client *http.Client, src egressSource) (Egress, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url, nil)
	if err != nil {
		return Egress{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Egress{}, fmt.Errorf("%s: %w", src.url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Egress{}, fmt.Errorf("%s: 读取响应失败: %w", src.url, err)
	}
	e := src.parse(data)
	if e.IP == "" {
		return Egress{}, fmt.Errorf("%s: HTTP %d，响应里没有 IP", src.url, resp.StatusCode)
	}
	return e, nil
}
