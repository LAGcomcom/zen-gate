package lane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The WorkBuddy 账号池: several logins behind one channel. A turn spends the
// account that is currently serving; when that account's free window for *that
// model* is spent (upstream 429 + code 6004, which carries a reset wall clock)
// the same turn continues on the next account, and the client never sees the
// swap.
//
// Three state axes, deliberately separate — collapsing them is what makes a
// pool either bench healthy accounts or keep spending dead ones:
//
//   - authBad: the credential is gone. The account leaves the pool until the
//     user logs in again.
//   - cooldown: the upstream said "not now". Keyed by (account, model) for
//     quota refusals, because 6004 names one model's window — benching the
//     whole account would take its other models down for hours. Account-wide
//     for paid-balance exhaustion (402), which really is account-level.
//   - breaker: our own count of consecutive hard failures (5xx, transport).
//     Nothing upstream asked for; we back off so a broken account cannot eat
//     every turn.
//
// Scheduling is "sequential exhaustion": keep using the active account until
// it cannot serve, then move forward. An account whose cooldown expires does
// NOT preempt the one now serving — one account serving consecutive turns is
// what lets the upstream reuse its prompt cache, and hopping accounts every
// few seconds is also the pattern that looks like abuse.

const (
	// wbMaxAccountTries bounds how many accounts one turn may spend. Two or
	// three cover the common case (one window spent); a larger bound mostly
	// buys latency, since every miss is a full upstream round trip.
	wbMaxAccountTries = 4
	// wbLeaseLimit is how many turns one account may have in flight. Beyond
	// it the account is skipped rather than queued: the point of a pool is to
	// spread load, and a single account answering many concurrent turns is the
	// shape the upstream throttles first.
	wbLeaseLimit = 2
	// wbPoolSoftCooldown is the base park for a quota refusal that carries no
	// reset clock at all. It grows exponentially with consecutive hits and is
	// capped by wbSoftMaxCooldown. A refusal that DID state a clock (6004) is
	// honoured as given and never capped here.
	wbPoolSoftCooldown = 600
	wbSoftMaxCooldown  = 7200
	// wbServerCooldown used to park an account for a moment after a 5xx. It is
	// gone: the probe path re-asks the same model immediately after a refusal,
	// and any park at that point reads as "no account available". A run of hard
	// failures is what wbBreakerFail/wbBreakerBase below are for.
	wbAuthCooldown = 600
	wbAuthFailsMax = 3
	// wbBreakerFail is how many consecutive hard failures trip the breaker;
	// it then doubles from wbBreakerBase up to wbBreakerMax.
	wbBreakerFail = 3
	wbBreakerBase = 1800
	wbBreakerMax  = 21600
	// wbDailyMaxPerRun bounds one daily-task pass. The scheduler ticks hourly,
	// and an account that is done today is skipped on the next tick, so a
	// large pool drains over a few hours instead of firing ten upstream
	// requests back to back.
	wbDailyMaxPerRun = 3
	// wbDailyGap is the pause between two accounts in one daily pass.
	wbDailyGap = 800 * time.Millisecond
	// wbAllCool marks an account-wide cooldown in the per-model map, so one
	// map covers both granularities.
	wbAllCool = "*"
)

// wbSlot is one account's runtime state. acct is the durable snapshot (what
// gets persisted); everything else is memory-only and resets on restart, which
// is correct: a cooldown is a fact about right now, not about the account.
type wbSlot struct {
	mu   sync.Mutex
	acct WBAccount

	authBad   bool
	authFails int
	// cooled maps wbAllCool or a model id to the unix-milli instant it may be
	// used again.
	cooled map[string]int64

	softStreak   int
	fails        int
	breakerUntil int64
	inFlight     int

	lastErr     string
	refreshedAt time.Time
	// noCheckin records an enterprise account: the meter answers 10001
	// "企业账号不支持该操作", and retrying it every hour forever is noise.
	noCheckin bool
}

// WBAccountView is one account as the settings card shows it: the durable
// fields plus just enough runtime state to explain why it is (not) serving.
type WBAccountView struct {
	WBAccount
	State         string `json:"state"` // ok | cooldown | auth | unready | disabled
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
	CooldownModel string `json:"cooldownModel,omitempty"`
	InFlight      int    `json:"inFlight"`
	LastError     string `json:"lastError,omitempty"`
	NoCheckin     bool   `json:"noCheckin,omitempty"`
}

// snapshot copies the durable half under the slot lock.
func (s *wbSlot) snapshot() WBAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acct
}

// nowMS is the clock the cooldown bookkeeping uses.
func nowMS() int64 { return time.Now().UnixMilli() }

// wbStableID derives the pool id from a token subject, falling back to a
// random hex id when the mint carries no subject at all.
func wbStableID(uid string) string {
	if s := strings.TrimSpace(uid); s != "" {
		return s
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("wb-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// WBAccountID is the exported form of the pool id derivation: the store uses
// it to backfill an id for an account that arrived without one (an imported
// config, a hand edit) so every admin route has a handle to address.
func WBAccountID(a WBAccount) string { return wbStableID(a.UID) }

// wbPhoneRe spots a phone number in a display name. WorkBuddy CN accounts are
// phone-based, so the nickname is routinely the user's own number — it must
// not reach a log line or a screenshot.
var wbPhoneRe = regexp.MustCompile(`\d{7,15}`)

// acctLabel is the only way an account is named in a log line, an error
// message or the UI. It never includes a token, and it masks a phone number.
func acctLabel(a WBAccount) string {
	name := strings.TrimSpace(a.Nickname)
	if name == "" {
		name = strings.TrimSpace(a.UID)
	}
	if name == "" {
		name = strings.TrimSpace(a.ID)
	}
	if name == "" {
		return "未命名账号"
	}
	return wbPhoneRe.ReplaceAllStringFunc(name, func(m string) string {
		if len(m) < 8 {
			return m
		}
		return "***" + m[len(m)-4:]
	})
}

// logf routes pool diagnostics through the caller's logger; a nil hook (tests,
// headless embeds) stays silent rather than panicking.
func (l *Lane) logf(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

// ---- pool membership -------------------------------------------------------

// SetWorkBuddyAccounts replaces the whole pool (boot, config import).
func (l *Lane) SetWorkBuddyAccounts(accts []WBAccount) {
	l.wbMu.Lock()
	prev := map[string]*wbSlot{}
	for _, s := range l.wbSlots {
		prev[s.acct.ID] = s
	}
	next := make([]*wbSlot, 0, len(accts))
	for _, a := range accts {
		if strings.TrimSpace(a.ID) == "" {
			a.ID = wbStableID(a.UID)
		}
		if s, ok := prev[a.ID]; ok {
			// Keep the runtime state: a config reload must not clear a
			// cooldown the upstream just handed us.
			s.mu.Lock()
			s.acct = a
			s.mu.Unlock()
			next = append(next, s)
			continue
		}
		next = append(next, &wbSlot{acct: a, cooled: map[string]int64{}})
	}
	l.wbSlots = next
	if l.wbSlotByIDLocked(l.wbActiveID) == nil {
		l.wbActiveID = ""
	}
	l.wbMu.Unlock()
	l.fireChange()
}

// wbSlotByIDLocked finds a slot; wbMu must be held (read or write).
func (l *Lane) wbSlotByIDLocked(id string) *wbSlot {
	if id == "" {
		return nil
	}
	for _, s := range l.wbSlots {
		if s.acct.ID == id {
			return s
		}
	}
	return nil
}

// wbSlotByID finds a slot by id.
func (l *Lane) wbSlotByID(id string) *wbSlot {
	l.wbMu.RLock()
	defer l.wbMu.RUnlock()
	return l.wbSlotByIDLocked(id)
}

// wbSlotsSnapshot copies the slot pointers. The slice is what wbMu guards; the
// slots themselves are guarded by their own mutexes.
func (l *Lane) wbSlotsSnapshot() []*wbSlot {
	l.wbMu.RLock()
	defer l.wbMu.RUnlock()
	out := make([]*wbSlot, len(l.wbSlots))
	copy(out, l.wbSlots)
	return out
}

// wbOrderedSlots returns the pool starting at the active account and wrapping.
// This is what makes "sequential exhaustion" work: the account now serving is
// tried first, and a recovered earlier account sits behind it in the ring.
func (l *Lane) wbOrderedSlots() []*wbSlot {
	l.wbMu.RLock()
	slots := make([]*wbSlot, len(l.wbSlots))
	copy(slots, l.wbSlots)
	active := l.wbActiveID
	l.wbMu.RUnlock()
	if len(slots) < 2 || active == "" {
		return slots
	}
	for i, s := range slots {
		if s.acct.ID == active {
			out := make([]*wbSlot, 0, len(slots))
			out = append(out, slots[i:]...)
			out = append(out, slots[:i]...)
			return out
		}
	}
	return slots
}

// wbUpsertSlot folds a freshly minted account into the pool. The same UID
// logging in twice updates the tokens in place — a second row for one account
// would double its apparent quota without adding any.
func (l *Lane) wbUpsertSlot(acct WBAccount) *wbSlot {
	if strings.TrimSpace(acct.ID) == "" {
		acct.ID = wbStableID(acct.UID)
	}
	l.wbMu.Lock()
	defer l.wbMu.Unlock()
	if s := l.wbSlotByIDLocked(acct.ID); s != nil {
		s.mu.Lock()
		s.acct = acct
		s.authBad = false
		s.authFails = 0
		s.fails = 0
		s.breakerUntil = 0
		s.cooled = map[string]int64{}
		s.lastErr = ""
		s.noCheckin = false
		s.mu.Unlock()
		return s
	}
	s := &wbSlot{acct: acct, cooled: map[string]int64{}}
	l.wbSlots = append(l.wbSlots, s)
	if l.wbActiveID == "" {
		l.wbActiveID = acct.ID
	}
	return s
}

// wbRemoveSlot drops one account, or every account when id is empty.
func (l *Lane) wbRemoveSlot(id string) {
	l.wbMu.Lock()
	if id == "" {
		l.wbSlots = nil
		l.wbActiveID = ""
	} else {
		keep := l.wbSlots[:0]
		for _, s := range l.wbSlots {
			if s.acct.ID != id {
				keep = append(keep, s)
			}
		}
		l.wbSlots = keep
		if l.wbActiveID == id {
			l.wbActiveID = ""
			if len(l.wbSlots) > 0 {
				l.wbActiveID = l.wbSlots[0].acct.ID
			}
		}
	}
	l.wbMu.Unlock()
	l.fireChange()
}

// wbSetActive records the account that served the last turn.
func (l *Lane) wbSetActive(id string) {
	if id == "" {
		return
	}
	l.wbMu.Lock()
	changed := l.wbActiveID != id
	l.wbActiveID = id
	l.wbMu.Unlock()
	if changed {
		l.fireChange()
	}
}

// ---- readiness -------------------------------------------------------------

// wbServeable reports whether any account could serve a turn, ignoring
// cooldowns: the catalog gate. A model must not vanish from the pickers
// because every account happens to be cooling for a few minutes.
func (l *Lane) wbServeable() bool {
	for _, s := range l.wbSlotsSnapshot() {
		s.mu.Lock()
		ok := s.acct.Enabled && strings.TrimSpace(s.acct.AccessToken) != ""
		s.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

// wbHasToken reports whether any account carries a token at all — the
// distinction between "logged out" and "everyone is busy right now".
func (l *Lane) wbHasToken() bool {
	for _, s := range l.wbSlotsSnapshot() {
		if strings.TrimSpace(s.snapshot().AccessToken) != "" {
			return true
		}
	}
	return false
}

// wbUsableNow reports whether one slot may serve the given model at this
// instant. Callers must hold s.mu.
func (s *wbSlot) usableNow(model string, at int64) bool {
	if !s.acct.Enabled || strings.TrimSpace(s.acct.AccessToken) == "" {
		return false
	}
	if s.authBad || s.breakerUntil > at {
		return false
	}
	if until := s.cooled[wbAllCool]; until > at {
		return false
	}
	if model != "" {
		if until := s.cooled[model]; until > at {
			return false
		}
	}
	return s.inFlight < wbLeaseLimit
}

// wbAcquire picks the next account that can serve this model and takes one
// lease on it, atomically — checking readiness and taking the lease in two
// steps would let two turns both pass the in-flight test and pile onto one
// account.
func (l *Lane) wbAcquire(model string, tried map[string]bool) *wbSlot {
	at := nowMS()
	for _, s := range l.wbOrderedSlots() {
		s.mu.Lock()
		id := s.acct.ID
		if tried[id] || !s.usableNow(model, at) {
			s.mu.Unlock()
			continue
		}
		s.inFlight++
		s.mu.Unlock()
		return s
	}
	return nil
}

// wbRelease returns the lease.
func (l *Lane) wbRelease(s *wbSlot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.mu.Unlock()
}

// wbEarliestRecovery is the shortest wait any account needs, in seconds. It
// answers "when may this turn succeed" when every account is benched.
func (l *Lane) wbEarliestRecovery(model string) int {
	at := nowMS()
	best := int64(0)
	for _, s := range l.wbSlotsSnapshot() {
		s.mu.Lock()
		if s.acct.Enabled && strings.TrimSpace(s.acct.AccessToken) != "" && !s.authBad {
			cands := []int64{s.cooled[wbAllCool], s.breakerUntil}
			if model != "" {
				cands = append(cands, s.cooled[model])
			}
			for _, until := range cands {
				if until > at && (best == 0 || until < best) {
					best = until
				}
			}
		}
		s.mu.Unlock()
	}
	if best == 0 {
		return 0
	}
	sec := int((best - at + 999) / 1000)
	if sec < 1 {
		sec = 1
	}
	return sec
}

// wbAllBusyError is the "everyone is benched" refusal: a quota-class failure
// carrying the earliest recovery, so the client (and the model-level failover
// above us) knows whether waiting is worth it.
func (l *Lane) wbAllBusyError(model string) *UpstreamError {
	wait := l.wbEarliestRecovery(model)
	msg := "所有 WorkBuddy 账号都在限流窗口内"
	if d := l.wbAccountDigest(); d != "" {
		msg += "：" + d
	}
	return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: wait}
}

// wbNoAccountError is the "nothing to log into" refusal.
func (l *Lane) wbNoAccountError() *UpstreamError {
	return &UpstreamError{Code: CodeCredential, Message: "WorkBuddy 账号未登录或已停用"}
}

// ---- cooldown bookkeeping --------------------------------------------------

// wbCoolModel benches one account for one model. waitSec <= 0 means "no clock
// supplied": the exponential soft cooldown applies, and only that one is
// capped. A clock the upstream actually stated (6004's reset wall) is honoured
// as given — clamping it would wake the account up mid-window and spend a
// refusal to learn what we already knew.
func (l *Lane) wbCoolModel(s *wbSlot, model string, waitSec int, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooled == nil {
		s.cooled = map[string]int64{}
	}
	if waitSec <= 0 {
		s.softStreak++
		waitSec = wbPoolSoftCooldown << min(s.softStreak-1, 6)
		if waitSec > wbSoftMaxCooldown {
			waitSec = wbSoftMaxCooldown
		}
	}
	key := model
	if key == "" {
		key = wbAllCool
	}
	until := nowMS() + int64(waitSec)*1000
	if until > s.cooled[key] {
		s.cooled[key] = until
	}
	s.lastErr = truncate(msg, 200)
}

// wbCoolAccount benches the account across every model. Used for paid-balance
// exhaustion, which really is account-wide.
func (l *Lane) wbCoolAccount(s *wbSlot, waitSec int, msg string) {
	if s == nil {
		return
	}
	if waitSec <= 0 {
		waitSec = wbPoolSoftCooldown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooled == nil {
		s.cooled = map[string]int64{}
	}
	until := nowMS() + int64(waitSec)*1000
	if until > s.cooled[wbAllCool] {
		s.cooled[wbAllCool] = until
	}
	s.lastErr = truncate(msg, 200)
}

// wbMarkAuth books a credential refusal. A soft one (12153) only benches the
// account briefly and escalates after wbAuthFailsMax consecutive hits; a hard
// one (401, or a refresh that was refused) takes it out until the user logs in.
func (l *Lane) wbMarkAuth(s *wbSlot, soft bool, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = truncate(msg, 200)
	if soft {
		s.authFails++
		if s.authFails < wbAuthFailsMax {
			if s.cooled == nil {
				s.cooled = map[string]int64{}
			}
			until := nowMS() + int64(wbAuthCooldown)*1000
			if until > s.cooled[wbAllCool] {
				s.cooled[wbAllCool] = until
			}
			return
		}
	}
	s.authBad = true
}

// wbMarkServer books one hard failure. Nothing is parked before the breaker
// trips: the probe path deliberately re-asks the same model right after a
// refusal (a second exit may answer), and a cooldown here would turn that
// second ask into "no account available" — reading a single flaky answer as a
// spent account.
func (l *Lane) wbMarkServer(s *wbSlot, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails++
	s.lastErr = truncate(msg, 200)
	if s.fails < wbBreakerFail {
		return
	}
	shift := min(s.fails-wbBreakerFail, 4)
	wait := wbBreakerBase << shift
	if wait > wbBreakerMax {
		wait = wbBreakerMax
	}
	s.breakerUntil = nowMS() + int64(wait)*1000
}

// wbMarkOK clears the failure bookkeeping after a real success. The model
// cooldowns are left alone: one model answering says nothing about another
// that the upstream has separately windowed.
func (l *Lane) wbMarkOK(s *wbSlot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authBad = false
	s.authFails = 0
	s.fails = 0
	s.softStreak = 0
	s.lastErr = ""
}

// wbThaw lifts the account-wide cooldown. The daily checkin calls it: a
// balance that came back is the one upstream fact that can legitimately end a
// 402 park early.
func (l *Lane) wbThaw(s *wbSlot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cooled != nil {
		delete(s.cooled, wbAllCool)
	}
	s.mu.Unlock()
}

// wbAccountDigest is the one-line summary used in a final refusal.
func (l *Lane) wbAccountDigest() string {
	var parts []string
	for _, s := range l.wbSlotsSnapshot() {
		s.mu.Lock()
		label := acctLabel(s.acct)
		var note string
		switch {
		case s.authBad:
			note = "需重新登录"
		case s.breakerUntil > nowMS():
			note = "熔断中"
		case s.cooled[wbAllCool] > nowMS():
			note = "限流到 " + time.UnixMilli(s.cooled[wbAllCool]).Format("15:04")
		case !s.acct.Enabled:
			note = "已停用"
		default:
			note = "可用"
		}
		s.mu.Unlock()
		parts = append(parts, label+"("+note+")")
	}
	return strings.Join(parts, " / ")
}

// ---- management API --------------------------------------------------------

// WorkBuddyAccounts returns the durable pool in order.
func (l *Lane) WorkBuddyAccounts() []WBAccount {
	slots := l.wbSlotsSnapshot()
	out := make([]WBAccount, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.snapshot())
	}
	return out
}

// WorkBuddyViews returns the pool with runtime state for the settings card.
func (l *Lane) WorkBuddyViews() []WBAccountView {
	at := nowMS()
	slots := l.wbSlotsSnapshot()
	out := make([]WBAccountView, 0, len(slots))
	for _, s := range slots {
		s.mu.Lock()
		v := WBAccountView{
			WBAccount: s.acct,
			InFlight:  s.inFlight,
			LastError: s.lastErr,
			NoCheckin: s.noCheckin,
		}
		switch {
		case strings.TrimSpace(s.acct.AccessToken) == "":
			v.State = "unready"
		case !s.acct.Enabled:
			v.State = "disabled"
		case s.authBad:
			v.State = "auth"
		default:
			// Report the cooldown that actually gates this account, naming the
			// model when it is a per-model one: "cooldown on dfmodel" and
			// "cooldown on everything" are very different facts to show.
			until, key := int64(0), ""
			if u := s.cooled[wbAllCool]; u > at {
				until, key = u, ""
			} else {
				models := make([]string, 0, len(s.cooled))
				for m := range s.cooled {
					if m != wbAllCool {
						models = append(models, m)
					}
				}
				sort.Strings(models)
				for _, m := range models {
					if u := s.cooled[m]; u > at && u > until {
						until, key = u, m
					}
				}
			}
			if until > at {
				v.State = "cooldown"
				v.CooldownUntil = until
				v.CooldownModel = key
			} else if s.breakerUntil > at {
				v.State = "cooldown"
				v.CooldownUntil = s.breakerUntil
			} else {
				v.State = "ok"
			}
		}
		s.mu.Unlock()
		out = append(out, v)
	}
	return out
}

// WorkBuddyActiveID names the account currently serving.
func (l *Lane) WorkBuddyActiveID() string {
	l.wbMu.RLock()
	defer l.wbMu.RUnlock()
	return l.wbActiveID
}

// WorkBuddyUpsert merges a logged-in account into the pool.
func (l *Lane) WorkBuddyUpsert(acct WBAccount) {
	l.wbUpsertSlot(acct)
	l.fireChange()
}

// WorkBuddyRemove drops one account ("" clears the pool).
func (l *Lane) WorkBuddyRemove(id string) { l.wbRemoveSlot(id) }

// WorkBuddySetEnabled flips one account's participation.
func (l *Lane) WorkBuddySetEnabled(id string, enabled bool) {
	s := l.wbSlotByID(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.acct.Enabled = enabled
	acct := s.acct
	s.mu.Unlock()
	l.wbPersistAcct(acct)
	l.fireChange()
}

// WorkBuddySetEnabledAll flips every account at once — the channel switch.
func (l *Lane) WorkBuddySetEnabledAll(enabled bool) {
	for _, s := range l.wbSlotsSnapshot() {
		s.mu.Lock()
		s.acct.Enabled = enabled
		acct := s.acct
		s.mu.Unlock()
		l.wbPersistAcct(acct)
	}
	l.fireChange()
}

// WorkBuddySetPrimary moves one account to the front of the pool and makes it
// the active one: "use this account first" without waiting for a cooldown.
func (l *Lane) WorkBuddySetPrimary(id string) {
	l.wbMu.Lock()
	idx := -1
	for i, s := range l.wbSlots {
		if s.acct.ID == id {
			idx = i
			break
		}
	}
	if idx > 0 {
		moved := l.wbSlots[idx]
		rest := make([]*wbSlot, 0, len(l.wbSlots))
		rest = append(rest, moved)
		rest = append(rest, l.wbSlots[:idx]...)
		rest = append(rest, l.wbSlots[idx+1:]...)
		l.wbSlots = rest
	}
	if idx >= 0 {
		l.wbActiveID = id
	}
	l.wbMu.Unlock()
	l.fireChange()
}

// WorkBuddyMarkNoCheckin records that the meter refuses this account (an
// enterprise seat has no daily reward at all).
func (l *Lane) WorkBuddyMarkNoCheckin(id string, noCheckin bool) {
	s := l.wbSlotByID(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.noCheckin = noCheckin
	s.mu.Unlock()
}

// wbPersistAcct hands a rotated snapshot to the store callback.
func (l *Lane) wbPersistAcct(acct WBAccount) {
	l.wbMu.RLock()
	fn := l.wbPersist
	l.wbMu.RUnlock()
	if fn != nil {
		fn(acct)
	}
}

// wbPersistSlot persists one slot's current durable half.
func (l *Lane) wbPersistSlot(s *wbSlot) {
	if s == nil {
		return
	}
	l.wbPersistAcct(s.snapshot())
}
