package lane

import (
	"net/http"
	"testing"
	"time"
)

// A transport failure is exit-shaped, not model-shaped: the request never got an
// answer, so the same model asked through another exit costs nothing and can
// turn a 502 into a 200. It used to retry on neither axis — attempts=1, 502
// returned to the caller — while a dozen healthy exits sat idle. Measured on a
// real install: one stalled exit produced a 217-second request ending in
// TRANSPORT, for a model that answered in ~15s through other exits.
func TestEgressRetryableCoversExitShapedFailures(t *testing.T) {
	for _, code := range []string{CodeQuota, CodeTransport, CodeServer, CodeTimeout} {
		if !egressRetryable(code) {
			t.Errorf("egressRetryable(%s) = false, want true — this failure is about the exit", code)
		}
	}
	// Not exit-shaped: a region block is deterministic, an aborted request is the
	// caller's own cancellation, and empty is a content verdict.
	for _, code := range []string{CodeRegion, CodeAborted, CodeEmpty, ""} {
		if egressRetryable(code) {
			t.Errorf("egressRetryable(%s) = true, want false", code)
		}
	}
}

// switchableFailure decides whether another MODEL can answer. CodeTransport was
// missing, which meant a stalled exit ended the turn instead of moving on.
func TestSwitchableFailureIncludesTransport(t *testing.T) {
	if !switchableFailure(&UpstreamError{Code: CodeTransport}) {
		t.Error("a transport failure must be switchable: another model plausibly answers it")
	}
	for _, code := range []string{CodeQuota, CodeRegion, CodeServer, CodeTimeout, CodeEmpty} {
		if !switchableFailure(&UpstreamError{Code: code}) {
			t.Errorf("switchableFailure(%s) = false, want true", code)
		}
	}
	if switchableFailure(&UpstreamError{Code: CodeAborted}) {
		t.Error("an aborted request is the caller's cancellation, not a model failure")
	}
}

// The dial timeout does not bound the wait for a response. Without a header
// timeout an exit that accepts the connection and never answers pins a request
// forever, which is exactly what produced that 217-second call.
func TestSetProxyInstallsAResponseHeaderTimeout(t *testing.T) {
	old := laneHTTP.Transport
	t.Cleanup(func() { laneHTTP.Transport = old })

	SetProxy("rotate", "")
	tr, ok := laneHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("SetProxy did not install an *http.Transport")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("rotate transport has no ResponseHeaderTimeout: a silent exit would hang the request")
	}
	// It has to clear the slowest cold start measured on a real install, or it
	// would cut off a model that is merely warming up.
	if tr.ResponseHeaderTimeout < 60*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, too short for a cold start (a first-call step-5 took 62s)", tr.ResponseHeaderTimeout)
	}
}

// The retry loop needs a total-time bound, but one bound cannot fit both failure
// classes: a 429 refuses in well under a second, while a transport failure or an
// upstream 5xx costs 30-90s per attempt. With a single 429-sized budget the slow
// class expired before its first retry could start, so the loop silently
// degraded to attempts=1 -- observed returning 502 after 93 seconds while
// healthy exits sat idle.
func TestEgressBudgetIsPerFailureClass(t *testing.T) {
	if got := egressBudgetFor(CodeQuota); got != 0 {
		t.Errorf("egressBudgetFor(quota) = %v, want 0: a 429 answers fast, the base window already fits its retries", got)
	}
	// The slow class needs room for at least one full attempt to answer, or the
	// retry it is meant to enable cannot begin.
	for _, code := range []string{CodeTransport, CodeServer, CodeTimeout} {
		got := egressBudgetFor(code)
		if got < responseHeaderTimeout {
			t.Errorf("egressBudgetFor(%s) = %v, need at least one full attempt (%v) or the retry can never start", code, got, responseHeaderTimeout)
		}
	}
}
