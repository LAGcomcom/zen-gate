package lane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	io "io"
	"net"
	"net/http"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	regionRe   = regexp.MustCompile(`(?i)RegionError|not available in your country|region.?block`)
	quotaRe    = regexp.MustCompile(`(?i)FreeUsageLimitError|usage limit|rate limit`)
	modelErrRe = regexp.MustCompile(`(?i)ModelError|model is unavailable|model is not supported|not supported|Endpoint is unavailable`)
	htmlPageRe = regexp.MustCompile(`(?i)^\s*<(!doctype|html[\s>])`)
)

// isHTMLPage reports whether an upstream failure body is a front proxy's HTML
// error page. The status alone misfiles it — a WAF's 403 is not a bad
// credential, its 413 is not a request the caller can fix by re-sending — and
// the markup buries the one useful fact. It names the hop instead of the
// credential: if this hit a long conversation, the request size is the likely
// trigger.
func isHTMLPage(payload string) bool {
	return htmlPageRe.MatchString(payload) ||
		strings.Contains(payload, "with an HTML error page")
}

const htmlPageHint = "the gateway's front proxy answered with an HTML error page — usually a WAF or body-size limit in front of the gateway, not your credentials; if this hit a long conversation, its request size is the likely trigger"

// ClassifyFailure maps an upstream status/error payload onto a code.
// Payload patterns are checked before status codes: a reverse proxy's generic
// 503 "Service Unavailable" reason phrase must not be read as a per-model
// verdict, and an HTML page is the front proxy speaking, never the credential
// store.
func ClassifyFailure(status int, payload string, retryAfterSec int) *UpstreamError {
	e := &UpstreamError{Message: truncate(payload, 300), RetryAfter: retryAfterSec}
	switch {
	case regionRe.MatchString(payload):
		e.Code = CodeRegion
	case status == 429 || quotaRe.MatchString(payload):
		e.Code = CodeQuota
		if e.RetryAfter == 0 {
			// No Retry-After header is the normal case on this lane, so this
			// default is what a 429 actually parks the model for. See
			// quotaCooldownNoHeaderSec in throttle.go for why it is short.
			e.RetryAfter = quotaCooldownNoHeaderSec
		}
	case isHTMLPage(payload):
		// The front proxy refused before any model was consulted; replaying
		// the identical body draws the identical page, so this is outside the
		// switchable set regardless of status. The markup itself is noise —
		// the message names the hop and the status, never quotes the page.
		e.Code = CodeClient
		e.Message = htmlPageHint
		if status > 0 {
			e.Message += fmt.Sprintf(" (HTTP %d)", status)
		}
	case status == 401 || status == 403:
		e.Code = CodeCredential
	case status >= 500:
		e.Code = CodeServer
	case status == 404 || status == 400 || status == 422 || modelErrRe.MatchString(payload):
		e.Code = CodeServer
		e.Unavailable = true
	case status >= 400 && status != 408 && status != 425:
		// 4xx is the request's own fault: replaying the identical body
		// reproduces the identical refusal. 408 and 425 are the carve-out —
		// they name the gateway's own timing trouble, and a re-send can answer
		// differently.
		e.Code = CodeClient
	default:
		e.Code = CodeServer
	}
	return e
}

// laneHTTP is the shared client for all upstream traffic; zen-gate swaps its
// Transport when the proxy setting changes (hot, no restart).
var laneHTTP = &http.Client{}

// Rotator supplies the per-request egress for "rotate" proxy mode: each TCP
// dial is tunneled through one of the healthy subscription nodes so successive
// requests leave from different IPs. Implemented by the sing-box sidecar
// manager in internal/subs.
type Rotator interface {
	// Dial connects to addr (host:port) through the next healthy node.
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	// Healthy reports how many nodes are currently dialable.
	Healthy() int

	// Pick chooses an exit for one model. seed is the conversation's stable
	// identity: one conversation prefers the same exit, so the upstream has a
	// chance to reuse its prompt cache, while different conversations still
	// spread across the pool. Rotation is counted per **egress IP**, not per
	// node: quota is accounted per IP, and an airport routinely sells several
	// nodes that leave through one (measured: 55 live nodes, 21 IPs).
	Pick(seed, model string) string
	// DialThrough dials via one specific exit, so the session bound to that
	// exit is the session actually used.
	DialThrough(ctx context.Context, network, addr, nodeID string) (net.Conn, error)
	// ExitKey returns an exit's stable identity (its egress IP; the node id
	// when the IP is unknown). The upstream session is derived from it, so
	// nodes sharing one IP present one session instead of one each.
	ExitKey(nodeID string) string
	// Report books one failure against an (exit, model) pair. The ledger is
	// only ever used to skip a combination known to be bad.
	Report(nodeID, model, class string)
	// Revive clears the (exit, model) pair and the exit's transport cooldown
	// after a real request for that model succeeded through it — real traffic
	// is harder evidence than any probe sample. It never lifts bans recorded
	// against other models: one model working through an exit says nothing
	// about another the exit region-gates or quotas separately.
	Revive(nodeID, model string)
}

var (
	rotMu    sync.RWMutex
	rotator  Rotator
	rotation atomic.Bool
)

// ExitPlan is one attempt's egress decision: which exit serves which model.
// It rides the request context so the transport dials exactly the chosen
// exit — that is what binds the upstream session to a real exit.
type ExitPlan struct {
	NodeID  string
	ExitKey string
	Model   string
}

type exitPlanKey struct{}

// WithExitPlan stamps an egress decision onto a request context.
func WithExitPlan(ctx context.Context, p ExitPlan) context.Context {
	return context.WithValue(ctx, exitPlanKey{}, p)
}

// ExitPlanFrom reads the egress decision (zero value when unset).
func ExitPlanFrom(ctx context.Context) ExitPlan {
	if p, ok := ctx.Value(exitPlanKey{}).(ExitPlan); ok {
		return p
	}
	return ExitPlan{}
}

// CurrentRotator exposes the installed rotator so the lane can pick an exit
// before building the request, and bind the session to it.
func CurrentRotator() Rotator {
	rotMu.RLock()
	defer rotMu.RUnlock()
	return rotator
}

// SetRotator installs (or, with nil, removes) the per-request egress rotator.
func SetRotator(r Rotator) {
	rotMu.Lock()
	rotator = r
	rotMu.Unlock()
	rotation.Store(r != nil)
}

// RotationActive reports whether per-request egress rotation is live. The
// egress watcher uses it to skip the probe-round trigger on IP changes — with
// rotation the exit IP is expected to flap constantly.
func RotationActive() bool { return rotation.Load() }

// SetProxy reconfigures the shared client: "env" honors HTTP(S)_PROXY,
// "system" follows the Windows system proxy (WinINET settings), "direct"
// bypasses, "custom" uses the given URL, "rotate" tunnels every dial through
// the Rotator installed via SetRotator.
func SetProxy(mode, url string) {
	t := &http.Transport{}
	switch mode {
	case "direct":
		t.Proxy = nil
	case "custom":
		u, err := parseProxyURL(url)
		if err != nil {
			t.Proxy = http.ProxyFromEnvironment
		} else {
			t.Proxy = func(*http.Request) (*neturl.URL, error) { return u, nil }
		}
	case "system":
		t.Proxy = func(*http.Request) (*neturl.URL, error) { return SystemProxyURL(), nil }
	case "rotate":
		t.Proxy = nil
		// Free quota is counted per egress IP and DialContext only runs when a
		// connection is established: with keep-alives on, every request reuses
		// the first tunnel and rotation quietly pins the process to one IP.
		t.DisableKeepAlives = true
		t.DialContext = rotateDialContext
	default:
		t.Proxy = http.ProxyFromEnvironment
	}
	laneHTTP.Transport = t
}

// rotateDialContext sends one target out through the node pool, with two
// exceptions that both end in a plain dial:
//
//   - Loopback never reaches a node. A subscription served from this machine
//     (http://127.0.0.1:21000/sub) would otherwise be handed to a remote node
//     that dials its own loopback, so the pool could never fill and rotate mode
//     would lock itself out at every boot.
//   - A pool that cannot dial falls back to the system egress. An empty node
//     list is a reason to be slow, not a reason to answer 502 forever.
func rotateDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if isLoopbackAddr(addr) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	rotMu.RLock()
	r := rotator
	rotMu.RUnlock()
	if r != nil {
		// Prefer the exit the caller already chose: that keeps the upstream
		// session consistent with the exit actually used.
		if plan := ExitPlanFrom(ctx); plan.NodeID != "" {
			if c, derr := r.DialThrough(ctx, network, addr, plan.NodeID); derr == nil {
				return c, nil
			}
		}
		if c, err := r.Dial(ctx, network, addr); err == nil {
			return c, nil
		} else if ctx.Err() != nil {
			return nil, err
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// dialTimeout bounds one dial through a node.
//
// Without it a node that accepts the TCP connection but then stalls keeps a
// request alive far past any useful budget (observed: single requests taking
// 80s+, because the retry path re-dials and stalls the same way). Failing the
// dial lets the pool move to the next node instead.
const dialTimeout = 20 * time.Second

// isLoopbackAddr reports whether host:port names this machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Client returns the shared upstream HTTP client (proxy-aware).
func Client() *http.Client { return laneHTTP }

func parseProxyURL(raw string) (*neturl.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty proxy url")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return neturl.Parse(raw)
}

// PostStreamed posts one completion request and delivers decoded `data:`
// payloads to onData. The first ≤4 KiB are sniffed by body shape — the gateway
// sometimes answers a stream request with SSE under an application/json
// content-type — and the sniffed bytes are replayed ahead of the live reader.
// An idle deadline resets on every chunk; total duration is unbounded.
func PostStreamed(ctx context.Context, path string, body map[string]any, session, requestID string, onData func(payload []byte) error) (*Usage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, UpstreamBase+path, bytes.NewReader(payload))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	for k, v := range GatewayHeaders(session, requestID, true, "") {
		req.Header.Set(k, v)
	}
	resp, err := laneHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	defer resp.Body.Close()

	upstreamRID := headerRID(resp.Header)
	retryAfter := 0
	if ra := resp.Header.Get("retry-after"); ra != "" {
		if n, perr := strconv.Atoi(ra); perr == nil && n > 0 && n < 3600 {
			retryAfter = n
		}
	}

	// One pump owns resp.Body for the whole call; the head bytes are read
	// through it for shape inspection and then unread so decoding sees the
	// full stream.
	pump := newStreamReader(ctx, resp.Body, 5*time.Minute)
	defer pump.Close()
	head := readUpTo(pump, 4096, 20*time.Second)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rest, _ := io.ReadAll(io.LimitReader(pump, 1<<20))
		return nil, withRID(ClassifyFailure(resp.StatusCode, string(head)+string(rest), retryAfter), upstreamRID)
	}

	switch sniffBody(head) {
	case "empty":
		return nil, &UpstreamError{Code: CodeEmpty, Message: "empty response body", UpstreamRID: upstreamRID}
	case "json", "unknown":
		rest, _ := io.ReadAll(io.LimitReader(pump, 8<<20))
		full := string(head) + string(rest)
		var parsed any
		if jerr := json.Unmarshal([]byte(full), &parsed); jerr != nil {
			return nil, &UpstreamError{Code: CodeServer, Message: truncate(full, 300), UpstreamRID: upstreamRID}
		}
		if m, ok := parsed.(map[string]any); ok {
			if _, has := m["error"]; has {
				return nil, withRID(ClassifyFailure(resp.StatusCode, jsonString(m["error"]), retryAfter), upstreamRID)
			}
		}
		if uerr := onData([]byte(full)); uerr != nil {
			return nil, uerr
		}
		if m, ok := parsed.(map[string]any); ok {
			return usageFromBody(m), nil
		}
		return nil, nil
	}

	pump.Unread(head)
	// SSE body (whatever the header claimed).
	usage, err := readSSE(pump, onData)
	if err != nil {
		if ctx.Err() != nil {
			return usage, &UpstreamError{Code: CodeAborted, Message: "cancelled", UpstreamRID: upstreamRID}
		}
		if ue, ok := err.(*UpstreamError); ok {
			return usage, withRID(ue, upstreamRID)
		}
		return usage, &UpstreamError{Code: CodeTransport, Message: err.Error(), UpstreamRID: upstreamRID}
	}
	return usage, nil
}

// headerRID reads the provider's trace id under either spelling it uses.
func headerRID(h http.Header) string {
	for _, key := range []string{"X-Request-Id", "Request-Id"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// withRID stamps the provider trace id onto an error unless it already has one.
func withRID(e *UpstreamError, rid string) *UpstreamError {
	if e != nil && e.UpstreamRID == "" {
		e.UpstreamRID = rid
	}
	return e
}

// GetJSON performs a fingerprinted GET (model listing).
func GetJSON(ctx context.Context, path string, session, requestID string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UpstreamBase+path, nil)
	if err != nil {
		return err
	}
	for k, v := range GatewayHeaders(session, requestID, false, "application/json") {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ClassifyFailure(resp.StatusCode, string(data), 0)
	}
	return json.Unmarshal(data, out)
}

// --- body-shape sniffing -------------------------------------------------

// sniffBody classifies a response by body shape, not Content-Type:
// 'empty' | 'sse' | 'json' | 'unknown'.
func sniffBody(head []byte) string {
	trimmed := strings.TrimLeft(string(head), " \t\r\n")
	if trimmed == "" {
		return "empty"
	}
	if strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, "event:") ||
		strings.HasPrefix(trimmed, "retry:") || strings.HasPrefix(trimmed, "id:") ||
		strings.HasPrefix(trimmed, ":") {
		return "sse"
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "json"
	}
	return "unknown"
}

// readUpTo consumes up to n bytes from the pump within the deadline. The
// bytes are returned for shape inspection and must be Unread back before the
// stream is decoded, so nothing is lost.
func readUpTo(s *streamReader, n int, deadline time.Duration) []byte {
	buf := make([]byte, 0, n)
	tmp := make([]byte, 2048)
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for len(buf) < n {
		select {
		case <-timer.C:
			return buf
		default:
		}
		k, err := s.ReadTimeout(tmp, deadline)
		if k > 0 {
			buf = append(buf, tmp[:k]...)
		}
		if err != nil {
			return buf
		}
		switch sniffBody(buf) {
		case "sse", "json":
			return buf
		}
	}
	return buf
}

// --- stream pump with idle timeout ---------------------------------------

type streamChunk struct {
	data []byte
	err  error
}

// streamReader turns a blocking body into a channel-fed reader that errors
// when no data arrives within the idle timeout.
type streamReader struct {
	ch      chan streamChunk
	src     io.Reader
	closed  atomic.Bool
	once    sync.Once
	quit    chan struct{}
	mu      sync.Mutex
	pending []byte
	idle    time.Duration
}

func newStreamReader(ctx context.Context, src io.Reader, idle time.Duration) *streamReader {
	s := &streamReader{
		ch:   make(chan streamChunk, 512),
		src:  src,
		idle: idle,
		quit: make(chan struct{}),
	}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			k, err := src.Read(buf)
			if k > 0 {
				cp := make([]byte, k)
				copy(cp, buf[:k])
				if !s.emit(streamChunk{cp, nil}) {
					return
				}
			}
			if err != nil {
				s.emit(streamChunk{nil, err})
				return
			}
			if s.closed.Load() {
				return
			}
		}
	}()
	// The cancel watcher has to die with the stream: the probe path hands this
	// function context.Background(), whose Done() is a nil channel, so a watcher
	// waiting on it alone parked one goroutine — and the whole reader with its
	// chunk buffer — per probe, forever.
	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				s.Close()
			case <-s.quit:
			}
		}()
	}
	return s
}

// emit hands one chunk to the reader, and gives up once the stream is closed
// instead of blocking on a full buffer forever.
func (s *streamReader) emit(c streamChunk) bool {
	select {
	case s.ch <- c:
		return true
	case <-s.quit:
		return false
	}
}

func (s *streamReader) Close() {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.quit)
		if c, ok := s.src.(io.Closer); ok {
			_ = c.Close()
		}
	})
}

func (s *streamReader) Read(p []byte) (int, error) {
	return s.readWithTimeout(p, s.idle)
}

// ReadTimeout is Read with an explicit deadline for this call.
func (s *streamReader) ReadTimeout(p []byte, timeout time.Duration) (int, error) {
	return s.readWithTimeout(p, timeout)
}

// Unread pushes already-consumed bytes back to the front of the stream.
func (s *streamReader) Unread(data []byte) {
	s.mu.Lock()
	s.pending = append(append([]byte{}, data...), s.pending...)
	s.mu.Unlock()
}

func (s *streamReader) readWithTimeout(p []byte, timeout time.Duration) (int, error) {
	s.mu.Lock()
	if len(s.pending) > 0 {
		k := copy(p, s.pending)
		s.pending = s.pending[k:]
		s.mu.Unlock()
		return k, nil
	}
	s.mu.Unlock()

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	for {
		select {
		case <-timeoutCh:
			s.Close()
			return 0, errors.New("upstream idle timeout")
		case c := <-s.ch:
			if c.err != nil {
				if errors.Is(c.err, io.EOF) {
					return 0, io.EOF
				}
				return 0, c.err
			}
			s.mu.Lock()
			s.pending = append(s.pending, c.data...)
			k := copy(p, s.pending)
			s.pending = s.pending[k:]
			s.mu.Unlock()
			return k, nil
		}
	}
}

// readSSE parses `data:` frames, dropping comments and [DONE]. Usage-bearing
// frames are merged into the returned total (each field appears on at most a
// couple of terminal frames, so summation matches the reference behaviour).
func readSSE(r io.Reader, onData func([]byte) error) (*Usage, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var usage *Usage
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return usage, nil
			}
			return usage, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") || !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return usage, nil
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) == nil {
			if u := mapUsage(frame); u != nil {
				if usage == nil {
					usage = u
				} else {
					usage.Merge(u)
				}
			}
			if _, has := frame["error"]; has {
				return usage, ClassifyFailure(0, jsonString(frame["error"]), 0)
			}
		}
		if derr := onData([]byte(payload)); derr != nil {
			return usage, derr
		}
	}
}

// usageFromBody extracts usage from a non-streamed JSON answer.
func usageFromBody(m map[string]any) *Usage {
	return mapUsage(m)
}

// ParseUsage normalizes one wire frame's usage block; the relay package uses
// it so provider calls account tokens exactly like free-lane calls do.
func ParseUsage(frame map[string]any) *Usage { return mapUsage(frame) }

// mapUsage normalizes one usage frame; returns nil when nothing is present.
func mapUsage(frame map[string]any) *Usage {
	u := &Usage{}
	changed := false
	if m, ok := frame["usage"].(map[string]any); ok {
		changed = num(&u.Input, m, "prompt_tokens", "input_tokens") || changed
		changed = num(&u.Output, m, "completion_tokens", "output_tokens") || changed
		changed = num(&u.CacheRead, m, "cache_read_input_tokens", "cached_tokens", "cache_read_tokens") || changed
		changed = num(&u.CacheWrite, m, "cache_creation_input_tokens", "cache_write_tokens") || changed
		changed = num(&u.TotalTokens, m, "total_tokens") || changed
		if rd, ok := m["reasoning_tokens"]; ok {
			if f, ok := toNum(rd); ok && f >= 0 {
				u.Reasoning = int(f)
				changed = true
			}
		}
	}
	// Anthropic message_start carries usage at the top of the message object.
	if msg, ok := frame["message"].(map[string]any); ok {
		if mm, ok := msg["usage"].(map[string]any); ok {
			changed = num(&u.Input, mm, "input_tokens") || changed
			changed = num(&u.Output, mm, "output_tokens") || changed
			changed = num(&u.CacheRead, mm, "cache_read_input_tokens") || changed
			changed = num(&u.CacheWrite, mm, "cache_creation_input_tokens") || changed
		}
	}
	if !changed {
		return nil
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.Input + u.Output
	}
	return u
}

func num(dst *int, m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if f, ok := toNum(v); ok && f >= 0 {
				*dst += int(f)
				return true
			}
		}
	}
	return false
}

func toNum(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

// Merge folds one partial usage into another (fields present on either side).
func (u *Usage) Merge(o *Usage) {
	if o == nil {
		return
	}
	u.Input += o.Input
	u.Output += o.Output
	u.Reasoning += o.Reasoning
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.TotalTokens += o.TotalTokens
}

func jsonString(v any) string {
	switch t := v.(type) {
	case nil:
		// An absent or JSON-null field must stay empty. Marshaling nil would
		// return the four-character string "null", which downstream code then
		// ships to clients as a literal id/name (issue #7).
		return ""
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
