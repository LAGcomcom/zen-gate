package lane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
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

// ProbeModel sends the smallest streaming request per wire and classifies the
// answer. Only a gateway refusal that names the model counts against it:
// 5xx, quota and transport faults stay "unknown" and never remove a model
// from a picker.
func ProbeModel(ctx context.Context, m ModelInfo) ProbeResult {
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
			firstAt = time.Since(start).Milliseconds()
			res.TTFTMs = firstAt
		}
		lastPayload.Store(string(p))
		return nil
	})
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		ue, _ := err.(*UpstreamError)
		if ue == nil {
			ue = &UpstreamError{Code: CodeTransport, Message: err.Error()}
		}
		switch ue.Code {
		case CodeRegion:
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
	_, err := PostStreamed(ctx, EndpointFor(m.ID), body, session, MintRequestId(0), func(p []byte) error {
		var frame map[string]any
		if json.Unmarshal(p, &frame) == nil {
			if _, has := frame["answers"]; has {
				sawAnswers = true
			}
		}
		return nil
	})
	res.LatencyMs = time.Since(start).Milliseconds()
	if res.LatencyMs > 0 && res.TTFTMs == 0 {
		res.TTFTMs = res.LatencyMs
	}
	if err != nil {
		ue, _ := err.(*UpstreamError)
		if ue == nil {
			ue = &UpstreamError{Code: CodeTransport, Message: err.Error()}
		}
		switch ue.Code {
		case CodeRegion:
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

// ProbeCatalog probes models strictly one at a time (the free lane and its
// dashboard both prefer sequential, progressive results over parallel bursts).
func ProbeCatalog(ctx context.Context, models []ModelInfo, onResult func(ProbeResult)) {
	for _, m := range models {
		if ctx.Err() != nil {
			return
		}
		if onResult != nil {
			onResult(ProbeModel(ctx, m))
		}
	}
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
	try := func(url string, parse func([]byte) Egress) (Egress, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return Egress{}, false
		}
		resp, err := client.Do(req)
		if err != nil {
			return Egress{}, false
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err != nil {
			return Egress{}, false
		}
		e := parse(data)
		if e.IP == "" {
			return Egress{}, false
		}
		return e, true
	}
	if e, ok := try("https://ipinfo.io/json", func(b []byte) Egress {
		var m struct {
			IP      string `json:"ip"`
			Country string `json:"country"`
		}
		_ = json.Unmarshal(b, &m)
		return Egress{IP: m.IP, Country: strings.ToUpper(m.Country)}
	}); ok {
		return e
	}
	if e, ok := try("https://api.ipify.org?format=json", func(b []byte) Egress {
		var m struct {
			IP string `json:"ip"`
		}
		_ = json.Unmarshal(b, &m)
		return Egress{IP: m.IP}
	}); ok {
		return e
	}
	if e, ok := try("https://ipapi.co/json/", func(b []byte) Egress {
		var m struct {
			IP          string `json:"ip"`
			CountryCode string `json:"country_code"`
		}
		_ = json.Unmarshal(b, &m)
		return Egress{IP: m.IP, Country: strings.ToUpper(m.CountryCode)}
	}); ok {
		return e
	}
	return Egress{}
}
