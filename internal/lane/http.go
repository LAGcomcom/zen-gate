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
	neturl "net/url"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	regionRe  = regexp.MustCompile(`(?i)RegionError|not available in your country|region.?block`)
	quotaRe   = regexp.MustCompile(`(?i)FreeUsageLimitError|usage limit|rate limit`)
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
			e.RetryAfter = 60
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
}

var (
	rotMu    sync.RWMutex
	rotator  Rotator
	rotation atomic.Bool
)

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
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			rotMu.RLock()
			r := rotator
			rotMu.RUnlock()
			if r != nil {
				return r.Dial(ctx, network, addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	default:
		t.Proxy = http.ProxyFromEnvironment
	}
	laneHTTP.Transport = t
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
		return nil, ClassifyFailure(resp.StatusCode, string(head)+string(rest), retryAfter)
	}

	switch sniffBody(head) {
	case "empty":
		return nil, &UpstreamError{Code: CodeEmpty, Message: "empty response body"}
	case "json", "unknown":
		rest, _ := io.ReadAll(io.LimitReader(pump, 8<<20))
		full := string(head) + string(rest)
		var parsed any
		if jerr := json.Unmarshal([]byte(full), &parsed); jerr != nil {
			return nil, &UpstreamError{Code: CodeServer, Message: truncate(full, 300)}
		}
		if m, ok := parsed.(map[string]any); ok {
			if _, has := m["error"]; has {
				return nil, ClassifyFailure(resp.StatusCode, jsonString(m["error"]), retryAfter)
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
			return usage, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		if ue, ok := err.(*UpstreamError); ok {
			return usage, ue
		}
		return usage, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	return usage, nil
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
	mu      sync.Mutex
	pending []byte
	idle    time.Duration
}

func newStreamReader(ctx context.Context, src io.Reader, idle time.Duration) *streamReader {
	s := &streamReader{ch: make(chan streamChunk, 512), src: src, idle: idle}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			k, err := src.Read(buf)
			if k > 0 {
				cp := make([]byte, k)
				copy(cp, buf[:k])
				s.ch <- streamChunk{cp, nil}
			}
			if err != nil {
				if err == io.EOF {
					s.ch <- streamChunk{nil, io.EOF}
				} else {
					s.ch <- streamChunk{nil, err}
				}
				return
			}
			if s.closed.Load() {
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s
}

func (s *streamReader) Close() {
	s.once.Do(func() {
		s.closed.Store(true)
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
	case string:
		return t
	case nil:
		// An absent field is nothing, never the literal token a JSON
		// marshaller would invent for it.
		return ""
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
