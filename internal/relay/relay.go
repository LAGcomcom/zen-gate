// Package relay forwards requests to user-added upstream providers
// ("自定义 API"): standard OpenAI- or Anthropic-compatible endpoints with the
// user's own key. It reuses the lane's unified message/tool shapes and its SSE
// decoders, so a relayed turn produces the same chunk stream and usage
// accounting as a free-lane turn. No fingerprint gate, session minting or
// failover applies here — those are free-lane mechanics.
package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// providerStreamIdle bounds silence on a relayed stream — both the wait for
// the first frame after the 200 and the gap between frames. A user-added
// upstream can accept the request, send headers, then queue: observed on
// NVIDIA NIM's free tier with harness-sized prompts, where the body stayed
// silent for minutes while the client aborted on its own patience (access log
// read ABORTED at 20-44s with no in/out at all). Every received byte resets
// the clock, so a slow-but-live answer is never cut; 30s of true silence on a
// direct endpoint is a stuck request, not a queued one worth hanging for.
// A var so tests can shrink the bound.
var providerStreamIdle = 30 * time.Second

// providerHeaderBound caps the wait for response headers on a relayed turn.
// The lane's shared transport holds this at 90s — tuned for rotating egress —
// but a user-added upstream that opens the socket and then never answers is
// the exact shape observed on NVIDIA NIM's free queue: the client gave up at
// 20s (ABORTED, attempts=1, failover never saw it). Cutting the header wait
// at 30s turns that into a clean TRANSPORT error the gateway can report or
// retry. A var so tests can shrink it.
var providerHeaderBound = 30 * time.Second

// relayHTTP returns the relay's own client: the lane's shared transport
// cloned with the tighter header bound, and no end-to-end Timeout (a long
// answer is fine — what is bounded is silence, phase by phase). The clone is
// rebuilt when SetProxy swaps the base transport, so proxy mode stays honoured.
var (
	relayMu     sync.Mutex
	relayBase   http.RoundTripper
	relayClient *http.Client
)

func relayHTTP() *http.Client {
	relayMu.Lock()
	defer relayMu.Unlock()
	c := lane.Client()
	if relayClient != nil && c.Transport == relayBase {
		return relayClient
	}
	cl := *c
	if t, ok := c.Transport.(*http.Transport); ok {
		ct := t.Clone()
		ct.ResponseHeaderTimeout = providerHeaderBound
		cl.Transport = ct
		cl.Timeout = 0 // bounded per-phase, not end-to-end: a long answer is fine
	} else {
		cl.Timeout = providerHeaderBound
	}
	relayBase = c.Transport
	relayClient = &cl
	return relayClient
}

// NormalizeBaseURL trims and scheme-fills a provider base URL.
func NormalizeBaseURL(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u != "" && !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return u
}

// authHeaders writes the provider's credential headers. OpenAI-compatible
// endpoints take a Bearer key; Anthropic-compatible ones take x-api-key plus
// the version header.
func authHeaders(h http.Header, apiKey, protocol string) {
	if protocol == store.ProtocolAnthropic {
		h.Set("x-api-key", apiKey)
		h.Set("anthropic-version", "2023-06-01")
		return
	}
	h.Set("authorization", "Bearer "+apiKey)
}

// identityHeaders adds the session identity an opencode.ai endpoint demands.
// Its Go tier answers a session-less turn with 400 MissingSessionID ("cannot
// be routed efficiently") regardless of a valid key — the same session header
// the free lane already mints, so a user pointing a provider at OpenCode Go
// stops tripping that gate (issue #55). Every other provider gets nothing:
// unknown headers can upset a stricter upstream, and only this host asks.
func identityHeaders(h http.Header, p *store.Provider, session string) {
	if session == "" || !isOpenCodeHost(p.BaseURL) {
		return
	}
	h.Set("x-opencode-session", session)
}

// isOpenCodeHost reports whether a provider points at opencode.ai (the paid
// OpenCode Go tier, which gates on the session header).
func isOpenCodeHost(baseURL string) bool {
	host := strings.ToLower(NormalizeBaseURL(baseURL))
	return strings.Contains(host, "//opencode.ai") || strings.Contains(host, ".opencode.ai")
}

// relaySession derives the session identity a relayed turn presents to an
// opencode.ai endpoint: the lane's own minted id for a per-conversation seed,
// so it carries the timestamp-shaped prefix the gate expects (#47) and stays
// stable across the turns of one conversation. Mirrors gateway.SessionSeed
// without importing the gateway (which imports this package).
func relaySession(agent string, messages []lane.Message) string {
	first := ""
	for _, m := range messages {
		if m.Role == lane.RoleUser {
			first = m.TextOf()
			break
		}
	}
	if len(first) > 500 {
		first = first[:500]
	}
	return lane.SessionForConversation(agent + "\x00" + first)
}

// endpointOf returns the completion URL for one provider.
func endpointOf(p *store.Provider) string {
	base := NormalizeBaseURL(p.BaseURL)
	if p.Protocol == store.ProtocolAnthropic {
		return base + "/messages"
	}
	return base + "/chat/completions"
}

// FetchModelCatalog pulls the model listing from a provider endpoint and keeps
// the token capacities the provider declares alongside each id. Both protocol
// families answer GET {base}/models with {"data":[{"id":…}]}, so one parse path
// covers them; lane.ParseListingMeta also tolerates bare arrays.
func FetchModelCatalog(ctx context.Context, baseURL, apiKey, protocol string) ([]lane.ListingMeta, error) {
	base := NormalizeBaseURL(baseURL)
	if base == "" {
		return nil, errors.New("Base URL 不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	authHeaders(req.Header, apiKey, protocol)
	client := *lane.Client()
	client.Timeout = 20 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(string(data)))
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("响应不是 JSON 对象: %v", err)
	}
	rows := lane.ParseListingMeta(payload)
	if len(rows) == 0 {
		return nil, errors.New("模型列表为空（检查 Base URL 与 Key）")
	}
	return rows, nil
}

// FetchModels returns just the ids of a provider's listing.
func FetchModels(ctx context.Context, baseURL, apiKey, protocol string) ([]string, error) {
	rows, err := FetchModelCatalog(ctx, baseURL, apiKey, protocol)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// Turn is one relayed completion request.
type Turn struct {
	Provider  *store.Provider
	Model     string // upstream model id, namespace and effort suffix stripped
	Messages  []lane.Message
	Tools     []lane.ToolDef
	MaxTokens int    // ≤ 0 → provider default
	Agent     string // caller label for stats
	// Session is a stable per-conversation identity. Only some upstreams want
	// it: an opencode.ai endpoint rejects a turn that arrives without
	// x-opencode-session ("cannot be routed efficiently", issue #55), and the
	// value must repeat across a conversation for routing and prompt cache to
	// hold. Empty is fine for every other provider.
	Session string

	// ReasoningEffort is the thinking rung the caller asked for, already in the
	// naming this provider answers to. It rides the OpenAI body verbatim
	// because a provider that publishes rungs acts on them — dropping it is
	// what made the picker a no-op. Anthropic-protocol providers are left
	// alone: their thinking control is a budget object, not a level name, and
	// inventing one would be a claim about a field they may reject.
	ReasoningEffort string
}

// Complete streams one turn from the provider into lane chunks. Returns the
// usage, the finish reason ("" = upstream closed without one) and a classified
// error. The upstream is always asked for a stream; non-stream callers collect
// the chunks instead.
func Complete(ctx context.Context, t Turn, emit func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
	p := t.Provider
	session := t.Session
	if session == "" {
		session = relaySession(t.Agent, t.Messages)
	}
	protocol := p.Protocol
	wire := "chat"
	if protocol == store.ProtocolAnthropic {
		wire = "messages"
	}
	style := "chat"
	if protocol == store.ProtocolAnthropic {
		style = "claude"
	}

	msgs := lane.RepairToolPairing(t.Messages)
	maxTok := t.MaxTokens
	if maxTok <= 0 {
		maxTok = 8192
	}

	body := map[string]any{"model": t.Model, "stream": true}
	switch protocol {
	case store.ProtocolAnthropic:
		system, out := lane.ToClaudeMessages(msgs)
		body["messages"] = out
		body["max_tokens"] = maxTok
		if system != "" {
			body["system"] = system
		}
		if tl := lane.ToToolDefs(t.Tools, style); tl != nil {
			body["tools"] = tl
		}
	default:
		body["messages"] = lane.ToChatMessages(msgs)
		body["max_tokens"] = maxTok
		if tl := lane.ToToolDefs(t.Tools, style); tl != nil {
			body["tools"] = tl
		}
		if t.ReasoningEffort != "" {
			body["reasoning_effort"] = t.ReasoningEffort
		}
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	decoder := lane.NewDecoder(wire, nil, emit)
	usage, err := postStreamed(ctx, p, session, body, func(payload []byte) error {
		decoder.Decode(payload)
		return nil
	})
	result := decoder.Finish()
	if usage != nil && result.Usage.TotalTokens == 0 {
		result.Usage = *usage
	}
	if err != nil {
		return result.Usage, result.Finish, err
	}
	if !result.SawFinish && (result.SawText || result.SawReasoning || result.SawToolCall) {
		return result.Usage, "", &lane.UpstreamError{Code: lane.CodeTransport,
			Message: "上游流在结束前被关闭"}
	}
	return result.Usage, result.Finish, nil
}

// ProbeTiming reports how fast a provider answers; used by the dashboard's
// 探测 button. Kept trivial: one listing fetch, latency of that round trip.
func ProbeTiming(ctx context.Context, p *store.Provider) (int64, error) {
	t0 := time.Now()
	_, err := FetchModels(ctx, p.BaseURL, p.APIKey, p.Protocol)
	return time.Since(t0).Milliseconds(), err
}

// postStreamed POSTs one completion and pumps `data:` frames to onData,
// merging usage frames along the way. Mirrors lane.PostStreamed's contract
// (idle-tolerant read owned by the request context) against an arbitrary URL.
func postStreamed(ctx context.Context, p *store.Provider, session string, body map[string]any, onData func([]byte) error) (*lane.Usage, *lane.UpstreamError) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &lane.UpstreamError{Code: lane.CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointOf(p), bytes.NewReader(payload))
	if err != nil {
		return nil, &lane.UpstreamError{Code: lane.CodeTransport, Message: err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	authHeaders(req.Header, p.APIKey, p.Protocol)
	identityHeaders(req.Header, p, session)

	resp, err := relayHTTP().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &lane.UpstreamError{Code: lane.CodeAborted, Message: "cancelled"}
		}
		// A header-wait that hit ResponseHeaderTimeout reads as a net timeout
		// error with no ctx cancellation — that is the queued-forever shape,
		// classify it as a transport fault the caller can retry or report.
		return nil, &lane.UpstreamError{Code: lane.CodeTransport, Message: err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryAfter := 0
		if ra := resp.Header.Get("retry-after"); ra != "" {
			if n, perr := strconv.Atoi(ra); perr == nil && n > 0 && n < 3600 {
				retryAfter = n
			}
		}
		return nil, lane.ClassifyFailure(resp.StatusCode, string(data), retryAfter)
	}

	// Idle-bounded pump: an upstream that answers 200 and then queues in
	// silence gets cut at providerStreamIdle instead of hanging until the
	// client gives up (the ABORTED shape that skips every retry layer).
	pump := lane.NewStreamReader(ctx, resp.Body, providerStreamIdle)
	defer pump.Close()
	br := bufio.NewReaderSize(pump, 64*1024)
	var usage *lane.Usage
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil && line == "" {
			if rerr == io.EOF {
				return usage, nil
			}
			return usage, &lane.UpstreamError{Code: lane.CodeTransport, Message: rerr.Error()}
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		frameText := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if frameText == "" {
			continue
		}
		if frameText == "[DONE]" {
			return usage, nil
		}
		var frame map[string]any
		if json.Unmarshal([]byte(frameText), &frame) == nil {
			if u := lane.ParseUsage(frame); u != nil {
				if usage == nil {
					usage = u
				} else {
					usage.Merge(u)
				}
			}
			if _, has := frame["error"]; has {
				return usage, lane.ClassifyFailure(0, jsonString(frame["error"]), 0)
			}
		}
		if derr := onData([]byte(frameText)); derr != nil {
			return usage, &lane.UpstreamError{Code: lane.CodeTransport, Message: derr.Error()}
		}
	}
}

// probeHTTPTimeout bounds one probe: the free NVIDIA lane queues big models
// for minutes, and a verdict that late is worthless for daily use anyway.
const probeHTTPTimeout = 90 * time.Second

// elapsedMS is time.Since in whole milliseconds, rounded up to at least 1 —
// loopback probes can finish faster than one millisecond and a 0 would be
// dropped by the TTFT sample store.
func elapsedMS(t time.Time) int64 {
	if v := time.Since(t).Milliseconds(); v > 0 {
		return v
	}
	return 1
}

// ProbeModel pings one provider model with the smallest streaming completion
// and classifies the verdict like the free-lane prober does. It returns as
// soon as the first `data:` frame arrives — availability and first-token
// latency are the only things a probe needs. The three phases (request out,
// answer started, verdict) also go to the context sink, because a queued
// private endpoint can take tens of seconds and the dashboard has to say so.
func ProbeModel(ctx context.Context, p *store.Provider, model string) (res lane.ProbeResult) {
	res = lane.ProbeResult{Model: model, State: lane.StateUnknown, At: time.Now().UnixMilli()}
	lane.EmitProbeStep(ctx, lane.PhaseRequest, lane.StepRunning, 0, lane.SafeEndpoint(endpointOf(p))+" · "+model)
	defer func() {
		lane.EmitProbeStep(ctx, lane.PhaseVerdict, lane.StatusForProbeState(res.State), res.LatencyMs, res.Detail)
	}()
	// max_tokens + messages is the minimal shape both protocol families accept.
	body := map[string]any{"model": model, "stream": true, "max_tokens": 16,
		"messages": []map[string]any{{"role": "user", "content": "ping"}}}
	payload, err := json.Marshal(body)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointOf(p), bytes.NewReader(payload))
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	authHeaders(req.Header, p.APIKey, p.Protocol)

	client := *lane.Client()
	client.Timeout = probeHTTPTimeout
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			res.Detail = "已取消"
			return res
		}
		res.Detail = "请求失败: " + snippet(err.Error())
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ue := lane.ClassifyFailure(resp.StatusCode, string(data), 0)
		res.State, res.Detail = probeStateFromError(ue)
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}

	br := bufio.NewReaderSize(resp.Body, 16*1024)
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil && line == "" {
			res.Detail = "上游流在数据帧之前结束"
			break
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		if res.TTFTMs == 0 {
			res.TTFTMs = elapsedMS(start)
			lane.EmitProbeStep(ctx, lane.PhaseFirstByte, lane.StepOK, res.TTFTMs, "")
		}
		frameText := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if frameText == "" || frameText == "[DONE]" {
			break
		}
		var frame map[string]any
		if json.Unmarshal([]byte(frameText), &frame) == nil {
			if e, has := frame["error"]; has {
				ue := lane.ClassifyFailure(0, jsonString(e), 0)
				res.State, res.Detail = probeStateFromError(ue)
				res.LatencyMs = elapsedMS(start)
				return res
			}
		}
		res.State = lane.StateAvailable
		break
	}
	res.LatencyMs = elapsedMS(start)
	if res.TTFTMs == 0 {
		res.TTFTMs = res.LatencyMs
	}
	return res
}

// probeStateFromError maps a classified failure onto a probe verdict. Quota
// and region refusals name their states; transport/5xx stay unknown so a
// flaky network never marks a working model dead; a bad key or an unknown
// model is genuinely unavailable.
func probeStateFromError(ue *lane.UpstreamError) (state, detail string) {
	if ue == nil {
		return lane.StateUnknown, ""
	}
	switch ue.Code {
	case lane.CodeQuota:
		return lane.StateThrottled, ue.Message
	case lane.CodeRegion:
		return lane.StateRegionBlock, ue.Message
	case lane.CodeCredential:
		return lane.StateUnavailable, "Key 无效或无权限: " + ue.Message
	case lane.CodeServer:
		if ue.Unavailable {
			return lane.StateUnavailable, ue.Message
		}
		return lane.StateUnknown, ue.Message
	default:
		return lane.StateUnknown, ue.Message
	}
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// snippet condenses an error payload for surfacing in the dashboard.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
