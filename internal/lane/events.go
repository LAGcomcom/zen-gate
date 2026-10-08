package lane

// Probe progress events. A probe — availability or capability — is the one
// admin action a user watches, and the answer can take minutes on a queued
// free lane, so the probe functions report each phase through the context
// instead of only returning a verdict at the end. This mirrors WithTrace: no
// call site has to grow a parameter it does not use, and a probe nobody is
// watching simply has no sink.

import (
	"context"
	"net/url"
	"time"
)

// ProbeStep is one reported phase of a probe run.
type ProbeStep struct {
	At     int64  `json:"at"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Ms     int64  `json:"ms,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Probe phases. A run always opens with the request and closes with the
// verdict; the first-byte phase only appears once the upstream actually
// started answering, so its absence is itself the signal that a model is
// still queued rather than dead.
const (
	PhaseRequest   = "请求已发出"
	PhaseFirstByte = "首字节到达"
	PhaseVerdict   = "结论"
)

// Step statuses. StepUnknown is for a phase that finished without a verdict —
// a quota fault or a transport error is not evidence about the model.
const (
	StepRunning = "running"
	StepOK      = "ok"
	StepFail    = "fail"
	StepUnknown = "unknown"
)

// StatusForProbeState maps a verdict onto a step status: only a refusal that
// names the model, or a blocked region, counts as a failure; throttling and
// transport faults stay unknown so the drawer never blames the model.
func StatusForProbeState(state string) string {
	switch state {
	case StateAvailable:
		return StepOK
	case StateUnavailable, StateRegionBlock:
		return StepFail
	default:
		return StepUnknown
	}
}

// ProbeEventSink receives one run's phases in order.
type ProbeEventSink func(ProbeStep)

type probeSinkKey struct{}

// WithProbeSink returns ctx reporting its probe phases to sink.
func WithProbeSink(ctx context.Context, sink ProbeEventSink) context.Context {
	return context.WithValue(ctx, probeSinkKey{}, sink)
}

// ProbeSinkFrom reads the sink, nil when nobody is watching.
func ProbeSinkFrom(ctx context.Context) ProbeEventSink {
	if v, ok := ctx.Value(probeSinkKey{}).(ProbeEventSink); ok {
		return v
	}
	return nil
}

// atLeastMS rounds elapsed to whole milliseconds, minimum 1: a loopback answer
// can finish inside a millisecond, and a 0 both drops the TTFT sample and makes
// the drawer hide a phase that did happen.
func atLeastMS(t time.Time) int64 {
	if ms := time.Since(t).Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

// SafeEndpoint strips anything credential-shaped from a URL before it is
// reported into a drawer: a base URL can carry userinfo or a key in its query,
// and phase details are shown verbatim to whoever opens the dashboard.
func SafeEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if len(raw) > 120 {
			return raw[:120]
		}
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// EmitProbeStep reports one phase. Callers pass ms only for a measured phase;
// the timestamp is stamped here so a drawer can always show when it happened.
func EmitProbeStep(ctx context.Context, name, status string, ms int64, detail string) {
	sink := ProbeSinkFrom(ctx)
	if sink == nil {
		return
	}
	sink(ProbeStep{At: time.Now().UnixMilli(), Name: name, Status: status, Ms: ms, Detail: detail})
}
