package lane

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The content gate is not a rate limiter. A 429 answers "try later"; this gate
// answers "this traffic was flagged", and the vendor escalates on repeat
// offenders — the reference implementation of this same backend records that its
// risk-control code (3012) carries an account penalty that must never be probed
// by retrying. So the pool has to stop feeding a flagged account on its own,
// without waiting for a ban to make the decision for it.

// wbContentGate is the double refusal PostWorkBuddyStreamed produces: the first
// 11140 triggers the marker rewrite, so a gate-hit account must refuse twice.
func wbContentGate(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(400)
	fmt.Fprint(w, `{"code":11140,"msg":"invalid request"}`)
}

// A single gate hit is an accident — one marker string happened to appear in a
// prompt. The account must stay in rotation; parking it here would let one
// unlucky turn take a healthy account offline for a quarter of an hour.
func TestWBSanitizeSingleHitDoesNotPark(t *testing.T) {
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": wbContentGate,
		"tok-b": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	if _, err := l.wbPost(context.Background(), map[string]any{"model": "m"}, "c1",
		func([]byte) error { return nil }); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	for _, v := range l.WorkBuddyViews() {
		if v.ID == "a" && v.SanitizeUntil != 0 {
			t.Errorf("account a was parked after ONE gate hit (until %d)", v.SanitizeUntil)
		}
	}
	// Still selectable: the next turn must be free to start on it again.
	if l.wbAcquire("m", map[string]bool{}) == nil {
		t.Error("no account is selectable after a single gate hit")
	}
}

// Two hits inside the window is the traffic itself being flagged: park the
// account, and say so on the card under its own field — a quota window is
// something the user waits out, a flagged account is something they need to
// know about.
//
// One account on purpose: the pool sticks to the account that last answered, so
// with two accounts the healthy one absorbs every turn and the flagged one is
// never asked a second time — which is good behaviour, but it means the streak
// only accumulates on an account that keeps getting tried.
func TestWBSanitizeSecondHitParksTheAccount(t *testing.T) {
	_, hits := wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": wbContentGate,
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))

	// Turn 1: the gate refuses twice (original + sanitized retry) and the turn
	// fails; the account is not parked yet.
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "m"}, "c1",
		func([]byte) error { return nil }); err == nil {
		t.Fatal("the gate refused every attempt but the turn reported success")
	}
	for _, v := range l.WorkBuddyViews() {
		if v.SanitizeUntil != 0 {
			t.Fatalf("account parked after ONE turn (until %d)", v.SanitizeUntil)
		}
	}
	firstTurnHits := atomic.LoadInt32(hits)
	if firstTurnHits != 2 {
		t.Fatalf("upstream hits for the first turn = %d, want 2 (gate + rewritten retry)", firstTurnHits)
	}

	// Turn 2: the second hit parks the account.
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "m"}, "c1",
		func([]byte) error { return nil }); err == nil {
		t.Fatal("the gate refused every attempt but the turn reported success")
	}

	var parked int64
	for _, v := range l.WorkBuddyViews() {
		if v.ID == "a" {
			parked = v.SanitizeUntil
			if v.State != "cooldown" {
				t.Errorf("account a state = %q, want cooldown", v.State)
			}
		}
	}
	if parked <= time.Now().UnixMilli() {
		t.Fatalf("account a was not parked after repeated gate hits (until %d)", parked)
	}

	// And the pool must actually skip it: the next turn reaches no upstream at
	// all, because the only account is parked.
	before := atomic.LoadInt32(hits)
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "m"}, "c1",
		func([]byte) error { return nil }); err == nil {
		t.Fatal("a turn succeeded while the only account was parked")
	}
	if after := atomic.LoadInt32(hits) - before; after != 0 {
		t.Errorf("upstream hits while parked = %d, want 0 (probing a park is how the ban hardens)", after)
	}
}

// The park is account-wide. A gate verdict is about the account that sent the
// traffic, so every model on it would draw the same answer, and probing each
// model once is exactly the escalation the park exists to avoid.
func TestWBSanitizeParkCoversEveryModel(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	slot := l.wbAcquire("model-one", map[string]bool{})
	if slot == nil {
		t.Fatal("no slot")
	}
	l.wbRelease(slot)
	for i := 0; i < wbSanitizeStrikes; i++ {
		l.wbMarkSanitize(slot, "content gate")
	}
	if got := l.wbAcquire("model-two", map[string]bool{}); got != nil {
		t.Errorf("a different model still got the parked account %q", got.snapshot().ID)
	}
}

// The two parks must not share a deadline. A quota cooldown is normally much
// SHORTER than a gate park (a 600-second window vs 15 minutes), so storing the
// gate park under the same account-wide key would let the next 429 overwrite it
// and hand a flagged account straight back to rotation.
func TestWBSanitizeParkIsNotShortenedByAQuotaWindow(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	slot := l.wbAcquire("m", map[string]bool{})
	if slot == nil {
		t.Fatal("no slot")
	}
	l.wbRelease(slot)
	for i := 0; i < wbSanitizeStrikes; i++ {
		l.wbMarkSanitize(slot, "content gate")
	}
	parkUntil := slot.wbSanitizeUntil()
	if parkUntil == 0 {
		t.Fatal("setup: the account is not parked")
	}

	// A short quota window lands on top. It must not touch the gate park.
	l.wbCoolAccount(slot, 60, "free window exceeded")
	if got := slot.wbSanitizeUntil(); got != parkUntil {
		t.Errorf("a 60s quota window changed the gate park: %d → %d", parkUntil, got)
	}
	if got := l.wbAcquire("m", map[string]bool{}); got != nil {
		t.Errorf("the account came back into rotation while still gate-parked (%q)", got.snapshot().ID)
	}
}

// A checkin may legitimately lift a spent quota window — that is what wbThaw is
// for. It must NOT lift a content-gate park: the user topped up their balance,
// not their standing with the WAF.
func TestWBSanitizeParkSurvivesACheckinThaw(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	slot := l.wbAcquire("m", map[string]bool{})
	if slot == nil {
		t.Fatal("no slot")
	}
	l.wbRelease(slot)
	l.wbCoolAccount(slot, 600, "free window exceeded")
	for i := 0; i < wbSanitizeStrikes; i++ {
		l.wbMarkSanitize(slot, "content gate")
	}

	l.wbThaw(slot)

	// The quota park is gone…
	slot.mu.Lock()
	quota := slot.cooled[wbAllCool]
	slot.mu.Unlock()
	if quota > nowMS() {
		t.Errorf("wbThaw left the quota cooldown in place (%d)", quota)
	}
	// …the gate park is not.
	if slot.wbSanitizeUntil() == 0 {
		t.Error("wbThaw cleared the content-gate park; a checkin is not a reprieve from the WAF")
	}
	if got := l.wbAcquire("m", map[string]bool{}); got != nil {
		t.Errorf("a thawed but gate-parked account went back into rotation (%q)", got.snapshot().ID)
	}
}

// A streak that aged out of the window must not count: one hit last week plus
// one hit today is two accidents, not evasion.
func TestWBSanitizeStreakExpires(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	slot := l.wbAcquire("m", map[string]bool{})
	if slot == nil {
		t.Fatal("no slot")
	}
	l.wbRelease(slot)

	l.wbMarkSanitize(slot, "content gate")
	// Age the streak past the window, as if a day had passed.
	slot.mu.Lock()
	slot.sanitizeFirstAt -= int64(wbSanitizeWindow) / int64(time.Millisecond) * 2
	slot.mu.Unlock()

	l.wbMarkSanitize(slot, "content gate") // restarts the streak, must not park
	if got := l.wbAcquire("m", map[string]bool{}); got == nil {
		t.Error("a streak that aged out still parked the account")
	}
	for _, v := range l.WorkBuddyViews() {
		if v.SanitizeUntil != 0 {
			t.Errorf("sanitize park booked from a stale streak: %d", v.SanitizeUntil)
		}
	}
}

// A plain quota cooldown also lives under the same account-wide key. It must
// not be relabelled as a content-gate park — the two tell the user different
// things and only one of them is about their prompt.
func TestWBSanitizeLabelOnlyForGateHits(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	slot := l.wbAcquire("m", map[string]bool{})
	if slot == nil {
		t.Fatal("no slot")
	}
	l.wbRelease(slot)
	l.wbCoolAccount(slot, 600, "free window exceeded")

	views := l.WorkBuddyViews()
	if len(views) != 1 || views[0].State != "cooldown" {
		t.Fatalf("views = %+v, want one account in cooldown", views)
	}
	if views[0].SanitizeUntil != 0 {
		t.Errorf("a quota cooldown was labelled a content-gate park (%d)", views[0].SanitizeUntil)
	}
	if views[0].CooldownUntil == 0 {
		t.Error("the quota cooldown lost its own field")
	}
}

// A request-shaped fault that is NOT the content gate must keep its old
// behaviour: no account penalty, no switch, fail fast.
//
// The payload is 11101 ("unmarshal chat params failed"), which is the code the
// classifier files as CodeClient — the SAME code the content gate ends up with.
// That is what makes this a real distinction test rather than a test of the
// CodeServer branch: 11101 reaches wbPost's default arm, where the gate mark is
// what decides between "this account is flagged" and "this body is malformed".
func TestWBUnrelatedClientErrorDoesNotPark(t *testing.T) {
	_, hits := wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":11101,"msg":"unmarshal chat params failed"}`)
		},
		"tok-b": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	_, err := l.wbPost(context.Background(), map[string]any{"model": "m"}, "c1",
		func([]byte) error { return nil })
	if err == nil {
		t.Fatal("a malformed body must fail, not fail over")
	}
	if ue := asUpstream(err); ue == nil || ue.Code != CodeClient {
		t.Fatalf("error class = %v, want CodeClient (the fixture must reach the default arm)", ue)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (a request fault is not an account fault)", n)
	}
	for _, v := range l.WorkBuddyViews() {
		if v.SanitizeUntil != 0 {
			t.Errorf("account %s was parked for a request-shaped 400", v.ID)
		}
	}
}

// The mark the pool keys off must be the same string the gate path writes.
// Two spellings of one sentence is the classic way to make a guard silently
// stop firing.
func TestWBContentGateMarkMatchesTheEmittedMessage(t *testing.T) {
	if !strings.Contains(wbContentGateMark, "内容门") {
		t.Fatalf("wbContentGateMark = %q, want the phrase the gate path emits", wbContentGateMark)
	}
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": wbContentGate,
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	_, err := l.wbPost(context.Background(), map[string]any{"model": "m"}, "c1",
		func([]byte) error { return nil })
	ue := asUpstream(err)
	if ue == nil {
		t.Fatalf("expected a classified error, got %v", err)
	}
	if !strings.Contains(ue.Message, wbContentGateMark) {
		t.Errorf("gate message %q does not contain the mark %q — the pool would never book the hit",
			ue.Message, wbContentGateMark)
	}
}
