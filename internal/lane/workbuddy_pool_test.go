package lane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wbPoolUpstream is a stub upstream that keys its answer on the bearer token,
// so one httptest server can play several accounts. The map is read/written
// from the handler goroutine only.
func wbPoolUpstream(t *testing.T, replies map[string]func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		tok := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
		if fn, ok := replies[tok]; ok {
			fn(w, r)
			return
		}
		w.WriteHeader(500)
		fmt.Fprintf(w, `{"code":0,"message":"no stub for %s"}`, tok)
	}))
	t.Cleanup(srv.Close)
	old := WorkBuddyBase
	WorkBuddyBase = srv.URL
	t.Cleanup(func() { WorkBuddyBase = old })
	return srv, &hits
}

func wbSSE(w http.ResponseWriter) {
	w.Header().Set("content-type", "text/event-stream")
	fmt.Fprint(w, wbChatSSE(
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
	))
}

func wbQuota6004(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(429)
	fmt.Fprint(w, `{"code":6004,"message":"free window exceeded, your usage will reset at 2099-01-01 00:00:00 UTC+8"}`)
}

func wbPoolAcct(token, uid string) WBAccount {
	return WBAccount{ID: uid, AccessToken: token, RefreshToken: "r-" + uid, UID: uid,
		Enabled: true, ExpiresAtMS: time.Now().Add(30 * 24 * time.Hour).UnixMilli()}
}

func wbPoolLane(accts ...WBAccount) *Lane {
	l := NewLane()
	l.SetWorkBuddyAccounts(accts)
	l.SetWorkBuddyPersist(func(WBAccount) {})
	return l
}

// The whole point of the pool: one account's spent window continues the same
// turn on the next account, and the client sees one answer.
func TestWBPoolQuotaFailover(t *testing.T) {
	_, hits := wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": wbQuota6004,
		"tok-b": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	var payloads []string
	usage, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1",
		func(p []byte) error { payloads = append(payloads, string(p)); return nil })
	if err != nil {
		t.Fatalf("turn failed despite a healthy second account: %v", err)
	}
	if usage == nil {
		t.Fatal("usage missing")
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("upstream hits = %d, want one refusal + one success", n)
	}
	if len(payloads) == 0 {
		t.Fatal("the answer never reached the client")
	}
	if got := l.WorkBuddyActiveID(); got != "b" {
		t.Fatalf("active account = %q, want the one that served (b)", got)
	}
}

// 6004 names ONE model's window. Benching the account outright would take its
// other models down for hours — the single most damaging thing a pool can do.
func TestWBPoolCooldownIsPerModel(t *testing.T) {
	var aHits int32
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			// Refuse the first model, serve any other one.
			if strings.Contains(readBodyString(r), "glm-5.3") {
				atomic.AddInt32(&aHits, 1)
				wbSSE(w)
				return
			}
			wbQuota6004(w, r)
		},
		"tok-b": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	// Spend account a's window on the deepseek model.
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil }); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// glm-5.3 must still be served by account a.
	l.wbSetActive("a")
	before := atomic.LoadInt32(&aHits)
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "glm-5.3"}, "c2", func([]byte) error { return nil }); err != nil {
		t.Fatalf("a healthy model on a cooling account must still work: %v", err)
	}
	if atomic.LoadInt32(&aHits) == before {
		t.Fatal("account a never served glm-5.3: the cooldown was applied to the whole account")
	}
}

// Sequential exhaustion: an account whose cooldown expired does not preempt
// the one now serving.
func TestWBPoolStickyDoesNotPreempt(t *testing.T) {
	var aHits int32
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&aHits, 1)
			wbSSE(w)
		},
		"tok-b": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	// Park a briefly, serve from b, then let a's cooldown lapse.
	slotA := l.wbSlotByID("a")
	l.wbCoolModel(slotA, "deepseek-v4.1-flash", 1, "test")
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil }); err != nil {
		t.Fatalf("turn while a is parked: %v", err)
	}
	if got := l.WorkBuddyActiveID(); got != "b" {
		t.Fatalf("active = %q, want b", got)
	}
	slotA.mu.Lock()
	slotA.cooled = map[string]int64{}
	slotA.mu.Unlock()

	before := atomic.LoadInt32(&aHits)
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c2", func([]byte) error { return nil }); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if atomic.LoadInt32(&aHits) != before {
		t.Fatal("the recovered earlier account preempted the one serving: prompt cache and stability both lose")
	}
}

// Once bytes reached the client the turn is committed: replaying on another
// account would duplicate the visible answer.
func TestWBPoolNoSwitchAfterOutput(t *testing.T) {
	var bHits int32
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			// One streamed delta, then the window refusal arrives as a frame —
			// the shape the upstream actually uses once a turn is in flight.
			// No [DONE] here: the refusal is what ends the stream.
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"error\":{\"code\":6004,\"message\":\"free window exceeded, reset at 2099-01-01 00:00:00 UTC+8\"}}\n\n")
			w.(http.Flusher).Flush()
		},
		"tok-b": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&bHits, 1)
			wbSSE(w)
		},
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil }); err == nil {
		t.Fatal("a turn that already streamed must surface the refusal, not be replayed")
	}
	if n := atomic.LoadInt32(&bHits); n != 0 {
		t.Fatalf("second account was hit %d times after output began", n)
	}
}

// With every account parked there is nothing to try: refuse immediately and
// say when it clears, instead of burning a round trip per account.
func TestWBPoolAllBusyShortCircuits(t *testing.T) {
	_, hits := wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": wbQuota6004,
		"tok-b": wbQuota6004,
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))
	l.wbCoolModel(l.wbSlotByID("a"), "deepseek-v4.1-flash", 300, "parked")
	l.wbCoolModel(l.wbSlotByID("b"), "deepseek-v4.1-flash", 600, "parked")

	_, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil })
	ue := asUpstream(err)
	if ue == nil || ue.Code != CodeQuota {
		t.Fatalf("err = %v, want a quota refusal", err)
	}
	if ue.RetryAfter <= 0 || ue.RetryAfter > 300 {
		t.Fatalf("RetryAfter = %d, want the earliest recovery (<=300)", ue.RetryAfter)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("upstream hits = %d, want none while every account is parked", n)
	}
}

// A dead credential moves the turn to the next account and takes that account
// out of rotation.
func TestWBPoolCredentialFailover(t *testing.T) {
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":401,"message":"unauthorized"}`)
		},
		"tok-b": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil }); err != nil {
		t.Fatalf("turn failed despite a healthy second account: %v", err)
	}
	slot := l.wbSlotByID("a")
	slot.mu.Lock()
	bad := slot.authBad
	slot.mu.Unlock()
	if !bad {
		t.Fatal("the 401 account must leave the pool until it is logged in again")
	}
	for _, v := range l.WorkBuddyViews() {
		if v.ID == "a" && v.State != "auth" {
			t.Fatalf("view state = %q, want auth", v.State)
		}
	}
}

// One 12153 is routinely a refresh race, not a revoked login: it must cost a
// short park, and only a run of them may bench the account.
func TestWBPoolSessionFailNeedsThreeStrikes(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	slot := l.wbSlotByID("a")
	for i := 1; i <= 2; i++ {
		l.wbMarkAuth(slot, true, "session")
		slot.mu.Lock()
		bad := slot.authBad
		slot.mu.Unlock()
		if bad {
			t.Fatalf("soft strike %d benched the account; one 12153 is usually transient", i)
		}
	}
	l.wbMarkAuth(slot, true, "session")
	slot.mu.Lock()
	bad := slot.authBad
	slot.mu.Unlock()
	if !bad {
		t.Fatalf("after %d soft strikes the account must be marked for re-login", wbAuthFailsMax)
	}
}

// A 5xx is not the account's fault: the turn surfaces it and the account stays
// usable for the retry the caller (or the probe) is about to make.
func TestWBPoolServerErrorDoesNotSwitch(t *testing.T) {
	var bHits int32
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"message":"upstream unavailable"}`)
		},
		"tok-b": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&bHits, 1)
			wbSSE(w)
		},
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))

	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil }); err == nil {
		t.Fatal("a 5xx must surface")
	}
	if n := atomic.LoadInt32(&bHits); n != 0 {
		t.Fatalf("second account was hit %d times on a server error", n)
	}
	// The very next attempt must still be able to use account a.
	l.wbSetActive("a")
	if _, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c2", func([]byte) error { return nil }); err == nil {
		t.Fatal("the stub still refuses with 503")
	}
}

// A busy account is skipped, not queued: the pool exists to spread load.
func TestWBPoolLeaseLimitSkipsBusyAccount(t *testing.T) {
	var bHits int32
	_, _ = wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": func(w http.ResponseWriter, r *http.Request) { wbSSE(w) },
		"tok-b": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&bHits, 1)
			wbSSE(w)
		},
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))
	slot := l.wbSlotByID("a")
	slot.mu.Lock()
	slot.inFlight = wbLeaseLimit
	slot.mu.Unlock()

	if got := l.wbAcquire("deepseek-v4.1-flash", map[string]bool{}); got == nil || got.acct.ID != "b" {
		t.Fatalf("acquired %v, want the account that still has a free lease", got)
	}
}

// One account, one refusal: exactly the old behaviour, so a single-account
// install cannot regress.
func TestWBPoolSingleAccountBehavesAsBefore(t *testing.T) {
	_, hits := wbPoolUpstream(t, map[string]func(http.ResponseWriter, *http.Request){
		"tok-a": wbQuota6004,
	})
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))

	_, err := l.wbPost(context.Background(), map[string]any{"model": "deepseek-v4.1-flash"}, "c1", func([]byte) error { return nil })
	ue := asUpstream(err)
	if ue == nil || ue.Code != CodeQuota {
		t.Fatalf("err = %v, want the upstream quota refusal", err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1", n)
	}
}

// Every account gets its daily claim — that is the whole reason the task moved
// from "the account" to "the pool".
func TestWBDailyCheckinCoversEveryAccount(t *testing.T) {
	var cnHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wbCheckinPath {
			atomic.AddInt32(&cnHits, 1)
		}
		fmt.Fprint(w, `{"code":0,"msg":"ok"}`)
	}))
	defer srv.Close()
	old := WorkBuddyCNBilling
	WorkBuddyCNBilling = srv.URL
	t.Cleanup(func() { WorkBuddyCNBilling = old })

	a := wbPoolAcct("tok-a", "a")
	a.Realm = WBRealmCN
	b := wbPoolAcct("tok-b", "b")
	b.Realm = WBRealmCN
	l := wbPoolLane(a, b)

	ran, err := l.WorkBuddyDailyCheckin(context.Background())
	if err != nil || !ran {
		t.Fatalf("run = (%v, %v)", ran, err)
	}
	if got := atomic.LoadInt32(&cnHits); got != 2 {
		t.Fatalf("meter hits = %d, want one per CN account", got)
	}
	for _, acct := range l.WorkBuddyAccounts() {
		if acct.LastCheckinDate != time.Now().Format("2006-01-02") {
			t.Errorf("account %s checkin date = %q", acct.ID, acct.LastCheckinDate)
		}
	}
}

// 停用 means "stop serving turns", not "stop recovering": a benched account
// still has to collect its daily, or it can never come back.
func TestWBDailyCheckinCoversDisabledAccount(t *testing.T) {
	var cnHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cnHits, 1)
		fmt.Fprint(w, `{"code":0,"msg":"ok"}`)
	}))
	defer srv.Close()
	old := WorkBuddyCNBilling
	WorkBuddyCNBilling = srv.URL
	t.Cleanup(func() { WorkBuddyCNBilling = old })

	a := wbPoolAcct("tok-a", "a")
	a.Realm = WBRealmCN
	a.Enabled = false
	l := wbPoolLane(a)

	if ran, err := l.WorkBuddyDailyCheckin(context.Background()); err != nil || !ran {
		t.Fatalf("run = (%v, %v), want the disabled account claimed", ran, err)
	}
	if got := atomic.LoadInt32(&cnHits); got != 1 {
		t.Fatalf("meter hits = %d, want 1", got)
	}
}

// An enterprise seat has no personal reward system: the meter answers 10001
// with a 企业 wording, and asking again every hour forever is pure noise.
func TestWBDailyCheckinSkipsEnterprise(t *testing.T) {
	var cnHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cnHits, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":10001,"msg":"企业账号不支持该操作"}`)
	}))
	defer srv.Close()
	old := WorkBuddyCNBilling
	WorkBuddyCNBilling = srv.URL
	t.Cleanup(func() { WorkBuddyCNBilling = old })

	a := wbPoolAcct("tok-a", "a")
	a.Realm = WBRealmCN
	l := wbPoolLane(a)

	ran, err := l.WorkBuddyDailyCheckin(context.Background())
	if err != nil {
		t.Fatalf("an enterprise seat is not an error: %v", err)
	}
	if !ran {
		t.Fatal("the account was attempted, so ran must be true")
	}
	if ran2, _ := l.WorkBuddyDailyCheckin(context.Background()); ran2 {
		t.Fatal("a flagged enterprise account must not be asked again")
	}
	if got := atomic.LoadInt32(&cnHits); got != 1 {
		t.Fatalf("meter hits = %d, want 1 (then never again)", got)
	}
}

// The hourly tick drains a large pool a few accounts at a time; the ones
// already done today drop out on their own.
func TestWBDailyCapsPerRunAndResumesNextRun(t *testing.T) {
	var cnHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cnHits, 1)
		fmt.Fprint(w, `{"code":0,"msg":"ok"}`)
	}))
	defer srv.Close()
	old := WorkBuddyCNBilling
	WorkBuddyCNBilling = srv.URL
	t.Cleanup(func() { WorkBuddyCNBilling = old })

	var accts []WBAccount
	for i := 0; i < 5; i++ {
		a := wbPoolAcct(fmt.Sprintf("tok-%d", i), fmt.Sprintf("u%d", i))
		a.Realm = WBRealmCN
		accts = append(accts, a)
	}
	l := wbPoolLane(accts...)

	if _, err := l.WorkBuddyDailyCheckin(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := atomic.LoadInt32(&cnHits); got != wbDailyMaxPerRun {
		t.Fatalf("first pass claimed %d accounts, want the cap of %d", got, wbDailyMaxPerRun)
	}
	if _, err := l.WorkBuddyDailyCheckin(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := atomic.LoadInt32(&cnHits); got != 5 {
		t.Fatalf("after two passes %d accounts were claimed, want all 5", got)
	}
	if ran, _ := l.WorkBuddyDailyCheckin(context.Background()); ran {
		t.Fatal("a third pass has nothing left to do")
	}
}

// One UID logging in twice updates the tokens in place. A second row for one
// account would advertise the same quota twice.
func TestWBPoolUpsertDedupesByUID(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"))
	l.WorkBuddyUpsert(WBAccount{ID: "a", AccessToken: "tok-a2", RefreshToken: "r2", UID: "a", Enabled: true})

	accts := l.WorkBuddyAccounts()
	if len(accts) != 1 {
		t.Fatalf("pool has %d accounts, want 1", len(accts))
	}
	if accts[0].AccessToken != "tok-a2" {
		t.Fatalf("token = %q, want the refreshed one", accts[0].AccessToken)
	}
}

func TestWBPoolRemoveOnlyRemovesOne(t *testing.T) {
	l := wbPoolLane(wbPoolAcct("tok-a", "a"), wbPoolAcct("tok-b", "b"))
	l.WorkBuddyRemove("a")
	accts := l.WorkBuddyAccounts()
	if len(accts) != 1 || accts[0].ID != "b" {
		t.Fatalf("after removing a: %+v", accts)
	}
}

// A phone number in the nickname is the user's own number on CN accounts: it
// must never reach a log line, an error message or a screenshot.
func TestWBPoolLabelMasksPhoneNumber(t *testing.T) {
	got := acctLabel(WBAccount{Nickname: "13812345678"})
	if strings.Contains(got, "13812345678") {
		t.Fatalf("label = %q, want the phone number masked", got)
	}
	if !strings.HasSuffix(got, "5678") {
		t.Fatalf("label = %q, want the last four digits kept", got)
	}
}

func readBodyString(r *http.Request) string {
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}
