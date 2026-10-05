package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	neturl "net/url"
	"net/http"
	"strings"
	"time"
	"zen-gate/internal/store"
)

// Custom-upstream passthrough.
//
// The free lane needs a minted session id, a fingerprint gate and a body
// rewrite (ApplyFingerprint) to get past opencode.ai. A user-declared endpoint
// — a local llama.cpp server, an Ollama box, anything speaking
// OpenAI-compatible HTTP — needs none of that and is best served verbatim: the
// request goes out with its body untouched and the answer streams back
// untouched. Re-encoding through the lane's own wire types would only lose
// fields the local server understands and zen-gate does not.

// passthrough forwards a request to a user-configured upstream when the
// requested model belongs to one. It reports whether it handled the request;
// when false the caller continues down the normal free-lane path.
//
// raw must be the exact request body — this path never re-encodes it.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, model, wire string, raw []byte) bool {
	u, ok := s.upstreamFor(model)
	if !ok {
		return false
	}
	start := time.Now()
	err := s.forwardToUpstream(w, r, u, model, raw)
	if err != nil {
		writeUpstreamError(w, err, wire)
		s.logUpstreamCall(u, model, http.StatusBadGateway, time.Since(start))
		return true
	}
	s.logUpstreamCall(u, model, http.StatusOK, time.Since(start))
	return true
}

// upstreamFor returns the configured upstream serving modelID, if any.
func (s *Server) upstreamFor(modelID string) (store.Upstream, bool) {
	if modelID == "" {
		return store.Upstream{}, false
	}
	for _, u := range s.upstreams() {
		for _, m := range u.Models {
			if strings.EqualFold(strings.TrimSpace(m.ID), modelID) {
				return u, true
			}
		}
	}
	return store.Upstream{}, false
}

// upstreams returns a snapshot of the configured upstreams with whitespace
// normalized, so a hand-edited config.json behaves like a dashboard-written one.
func (s *Server) upstreams() []store.Upstream {
	cfg := s.Store.Config()
	out := make([]store.Upstream, 0, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		u.Trim()
		if u.ID == "" || u.BaseURL == "" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// upstreamModelInfo describes one custom model in the same shape the free lane
// reports, so every catalog surface can merge both without special-casing.
type upstreamModelInfo struct {
	upstreamID string
	info       upstreamModelFields
}

type upstreamModelFields struct {
	ID            string
	Name          string
	Blurb         string
	Vision        bool
	Reasoning     bool
	ContextWindow int
	MaxOutput     int
	Wire          string
	RegionGated   bool
}

// upstreamModels flattens every configured upstream's models. It returns both
// the flattened list and the base id per entry so callers can group.
func (s *Server) upstreamModels() []upstreamModelInfo {
	ups := s.upstreams()
	out := make([]upstreamModelInfo, 0, len(ups))
	for _, u := range ups {
		for _, m := range u.Models {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = id
			}
			blurb := strings.TrimSpace(m.Blurb)
			if blurb == "" {
				blurb = "自定义上游 · " + u.Name
			}
			cw := m.ContextWindow
			if cw <= 0 {
				cw = store.DefaultContextWindow
			}
			mo := m.MaxOutput
			if mo <= 0 {
				mo = store.DefaultMaxOutput
			}
			out = append(out, upstreamModelInfo{
				upstreamID: u.ID,
				info: upstreamModelFields{
					ID: id, Name: name, Blurb: blurb,
					Vision: m.Vision, Reasoning: m.Reasoning,
					ContextWindow: cw, MaxOutput: mo,
					Wire:          m.Wire_(),
					RegionGated:   u.ExposeRegion,
				},
			})
		}
	}
	return out
}

// forwardToUpstream streams one request to a configured upstream verbatim and
// copies the response back to the client. It returns the upstream model id that
// was addressed, plus an error when the caller must emit a JSON error body.
//
// The body is passed through untouched in both directions: a local server's
// own quirks (reasoning_content, non-standard fields, SSE framing quirks) are
// none of the gateway's business on this path.
func (s *Server) forwardToUpstream(w http.ResponseWriter, r *http.Request, u store.Upstream, modelID string, body []byte) error {
	req, err := buildUpstreamRequest(r.Context(), u, r.URL.Path, body, r.Header)
	if err != nil {
		return err
	}
	resp, err := upstreamClient().Do(req)
	if err != nil {
		return &upstreamError{status: http.StatusBadGateway,
			msg: "上游 " + u.ID + " 无法连接: " + err.Error()}
	}
	defer resp.Body.Close()

	// Status first: an error body from the upstream must reach the client as
	// the upstream's own shape, not be re-wrapped.
	for k, vs := range resp.Header {
		if isHopByHop(k) || strings.EqualFold(k, "content-length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("x-zen-gate-served-by", modelID)
	w.Header().Set("x-zen-gate-upstream", u.ID)
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 300 || r.Method == http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
		return nil
	}
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// upstreamError is an error that already carries the HTTP status and the
// upstream's own error shape.
type upstreamError struct {
	status int
	msg    string
	// raw, when set, is written verbatim instead of a wrapped error body —
	// used to preserve an upstream's OpenAI-shaped error envelope.
	raw []byte
}

func (e *upstreamError) Error() string { return e.msg }

// buildUpstreamRequest maps one gateway path onto the upstream's own path.
//
// The gateway serves /v1/chat/completions, /v1/responses and /v1/messages;
// an OpenAI-compatible endpoint serves the same three. A base URL carrying a
// path prefix (http://host/llama) is honoured, so the upstream sees
// <prefix>/v1/chat/completions.
func buildUpstreamRequest(ctx context.Context, u store.Upstream, gatewayPath string, body []byte, header http.Header) (*http.Request, error) {
	base := strings.TrimSpace(u.BaseURL)
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	p, err := neturl.Parse(base)
	if err != nil {
		return nil, &upstreamError{status: http.StatusBadGateway, msg: "上游地址无效: " + err.Error()}
	}
	prefix := strings.TrimSuffix(p.Path, "/")
	suffix := gatewayPath
	if suffix == "" {
		suffix = "/"
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	p.Path = prefix + suffix
	p.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.String(), bytes.NewReader(body))
	if err != nil {
		return nil, &upstreamError{status: http.StatusBadGateway, msg: "请求构造失败: " + err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	if k := strings.TrimSpace(u.APIKey); k != "" {
		req.Header.Set("authorization", "Bearer "+k)
	}
	// Preserve the caller's streaming intent and any explicit accept header.
	if a := header.Get("accept"); a != "" {
		req.Header.Set("accept", a)
	}
	if strings.Contains(string(body), `"stream"`) && strings.Contains(string(body), "true") {
		req.Header.Set("accept", "text/event-stream")
	}
	req.Header.Set("user-agent", "zen-gate")
	return req, nil
}

// upstreamClient issues passthrough requests.
//
// ResponseHeaderTimeout bounds the wait for headers only: a streaming local
// model legitimately holds the connection open for minutes between frames, so
// an overall client timeout would sever healthy streams. The idle gap between
// frames is left to the caller's context (the agent's own request), matching
// how the free lane's stream pump behaves.
func upstreamClient() *http.Client {
	return &http.Client{
		Transport: upstreamTransport(),
		Timeout:   0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// upstreamTransport reuses the proxy-aware transport so a user-declared remote
// endpoint honours the same proxy setting as the free lane, while a loopback
// endpoint is never proxied — a proxy would break 127.0.0.1 by design.
func upstreamTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		c := t.Clone()
		c.ResponseHeaderTimeout = 0
		return c
	}
	return http.DefaultTransport
}

// isHopByHop reports headers that must not be copied to a proxied response.
func isHopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// writeUpstreamError emits an error in the shape the calling protocol expects,
// preserving an upstream's own OpenAI-style envelope when we captured one.
func writeUpstreamError(w http.ResponseWriter, err error, wire string) {
	ue, ok := err.(*upstreamError)
	if !ok {
		ue = &upstreamError{status: http.StatusBadGateway, msg: err.Error()}
	}
	status := ue.status
	if status == 0 {
		status = http.StatusBadGateway
	}
	if len(ue.raw) > 0 && json.Valid(ue.raw) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(ue.raw)
		return
	}
	switch wire {
	case "messages":
		writeJSON(w, status, map[string]any{"type": "error",
			"error": map[string]any{"type": "api_error", "message": ue.msg}})
	default:
		writeJSON(w, status, openaiError(ue.msg, "api_error"))
	}
}

// readBody buffers the request body for a passthrough, bounded the same way the
// decoding handlers bound it.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return nil, &upstreamError{status: http.StatusBadRequest, msg: "invalid request body: " + err.Error()}
	}
	return body, nil
}

// logUpstreamCall records one passthrough request so it shows in the log tail.
func (s *Server) logUpstreamCall(u store.Upstream, model string, status int, d time.Duration) {
	if s.logger == nil {
		return
	}
	s.logger.Infof("自定义上游 %s · %s · %d · %s", u.ID, model, status, d.Round(time.Millisecond))
}
