package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
