package lane

import (
	"testing"
	"time"
)

// A 429 on this lane normally arrives WITHOUT a Retry-After header, so the
// default is not a rare fallback — it is the value that actually parks the
// model. It has to stay short.
//
// The free pool is one globally shared credential ("Bearer public"), so a 429
// reports momentary crowding rather than a durable property of the model, and
// nextCandidate() skips a throttled model. An over-long park therefore shows
// the client "unavailable" for a model that answers again seconds later.
//
// Measured 2026-10-11: space-bunny-free and longcat-2.5-preview-free both
// answered 429, and both answered 200 again within two minutes; of 92 recorded
// throttle episodes the shortest was 0.6s.
func TestQuotaWithoutRetryAfterParksBriefly(t *testing.T) {
	e := ClassifyFailure(429, `{"error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}`, 0)
	if e.Code != CodeQuota {
		t.Fatalf("code = %q, want %q", e.Code, CodeQuota)
	}
	if e.RetryAfter != quotaCooldownNoHeaderSec {
		t.Errorf("RetryAfter = %d, want %d (the no-header default)", e.RetryAfter, quotaCooldownNoHeaderSec)
	}
	if quotaCooldownNoHeaderSec >= 60 {
		t.Errorf("quotaCooldownNoHeaderSec = %d: that is the old one-minute park again", quotaCooldownNoHeaderSec)
	}
}

// When upstream does name a wait, it knows more than we do — obey it.
func TestQuotaHonoursAnUpstreamRetryAfter(t *testing.T) {
	e := ClassifyFailure(429, `{"error":{"type":"FreeUsageLimitError"}}`, 120)
	if e.RetryAfter != 120 {
		t.Errorf("RetryAfter = %d, want the upstream value 120", e.RetryAfter)
	}
}

// Both writers must agree. Live traffic goes through ClassifyFailure, while a
// probe that finds a model throttled calls MarkThrottled(model, 0) and lands on
// the ledger's own fallback; if the two disagree, the same 429 parks a model for
// two different lengths depending on which path noticed it.
func TestThrottleFallbackMatchesTheQuotaDefault(t *testing.T) {
	const want = quotaCooldownNoHeaderSec * 1000
	if throttleCooldownDefault != want {
		t.Errorf("throttleCooldownDefault = %d, want %d ms", throttleCooldownDefault, want)
	}
}

// A park has to actually end, or the model stays unusable forever.
func TestThrottleExpiresAtItsDeadline(t *testing.T) {
	b := NewThrottleBook()
	b.MarkThrottled("m", 1)
	if !b.Throttled("m") {
		t.Fatal("not throttled right after MarkThrottled")
	}
	time.Sleep(1100 * time.Millisecond)
	if b.Throttled("m") {
		t.Error("still throttled after the cooldown elapsed")
	}
}

// A proven answer must clear the park immediately rather than leaving the model
// to wait out its cooldown — it just demonstrated that quota is back.
func TestMarkOKClearsAnOpenThrottle(t *testing.T) {
	b := NewThrottleBook()
	b.MarkThrottled("m", quotaCooldownNoHeaderSec)
	if !b.Throttled("m") {
		t.Fatal("not throttled right after MarkThrottled")
	}
	b.MarkOK("m")
	if b.Throttled("m") {
		t.Error("MarkOK left the model throttled; a proven answer should clear it")
	}
	if n := b.Note("m"); n.ThrottledAt != 0 || n.CooldownUntil != 0 {
		t.Errorf("note still open after MarkOK: %+v", n)
	}
}
