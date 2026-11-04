package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/store"
)

// One registry for every daily credit claim. Before this, three independent
// daily jobs lived as two goroutines in main.go and one bespoke admin button,
// so "did everything sign in today?" could only be answered by reading three
// different cards. Adding a fourth platform meant editing main again.
//
// Two kinds of rows live here:
//   - wired tasks, whose Run actually claims the day's reward;
//   - planned tasks, from a node list entry declaring checkin:daily. They are
//     listed as 待接入, never silently reported as done. A declaration is a
//     promise, not an implementation.
//
// What ends the retries is NOT "the API said ok" or a date stamp. It is the
// durable evidence that the account's credits really went up — a reward amount
// from the claim response, a balance delta, or (when the platform exposes no
// balance reader) a successful idempotent claim plus the account's own date
// stamp. Once confirmed for a period the scheduler issues ZERO write requests
// and only re-reads the balance every 6 hours. That is the pattern the public
// tools converged on (workbuddy-auto-signin 先查状态未签才领 + 4h 只读轮询,
// Trae-AutoCheckin 独立的 credit_monitor + 当日状态文件, metapi 解析奖励金额
// 且签到与余额刷新分开排程).
//
// Reset windows are a guess, not a fact: domestic platforms usually roll at
// local 00:00, some overseas ones at 08:00. A wrong guess cannot cause a missed
// day, because a confirmed task is re-verified (read-only) every 6 hours and
// the period is recomputed each pass.

const (
	checkinVerifyInterval = 6 * time.Hour
	checkinRetryInterval  = time.Hour
	checkinBootDelay      = 90 * time.Second
)

const (
	checkinMethodReward  = "领取响应"
	checkinMethodDelta   = "余额增量"
	checkinMethodBalance = "接口成功（余额不可读）"
	checkinMethodStamp   = "账号日期戳"
)

type CheckinTask struct {
	ID      string
	Name    string
	Channel string
	Detail  string
	// ResetHour is the local hour the platform rolls its daily window. 0 =
	// midnight (domestic), 8 = 08:00 (some overseas publishments).
	ResetHour int
	// Available says the task has a usable account. It is a display/skip hint,
	// not a second authorisation layer: Run keeps its own guards.
	Available func() bool
	// LastDate is the local calendar day the claim last landed; "" = never.
	LastDate func() string
	// Probe is READ-ONLY: it reads the account's credit balance (or a monotonic
	// claimed total) and must never sign in. nil = the platform exposes no
	// balance reader. A read failure is recorded, never treated as a failed
	// claim — upstream meters are often one request behind.
	Probe func(context.Context) (float64, string, error)
	// Reward reads the amount the claim response itself declared, if the lane
	// persisted it. This is the strongest evidence and is kept separate from a
	// balance delta so the two never get added together.
	Reward func() (float64, bool)
	// Run claims one day. ran=false means "nothing to do", never an error.
	// nil marks a declared-but-unwired task.
	Run func(context.Context) (bool, error)
}

type checkinResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Ran     bool   `json:"ran"`
	Skipped bool   `json:"skipped,omitempty"`
	// Verified marks a read-only balance check on an already confirmed day.
	Verified bool `json:"verified,omitempty"`
	// Confirmed is the state that stops further write requests this period.
	Confirmed   bool    `json:"confirmed,omitempty"`
	Method      string  `json:"method,omitempty"`
	Reward      float64 `json:"reward,omitempty"`
	Delta       float64 `json:"delta,omitempty"`
	Before      float64 `json:"before,omitempty"`
	After       float64 `json:"after,omitempty"`
	Balance     float64 `json:"balance,omitempty"`
	BalanceKind string  `json:"balanceKind,omitempty"`
	At          int64   `json:"at,omitempty"`
	VerifiedAt  int64   `json:"verifiedAt,omitempty"`
	// Detail explains a skip/verify; Error is a real claim failure.
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"`
}

// checkinTasks is built on demand rather than cached: accounts and node lists
// change while the process runs, and a stale registry would claim a deleted
// account is still eligible.
func (s *Server) checkinTasks() []CheckinTask {
	tasks := []CheckinTask{
		{
			ID: "workbuddy-cn", Name: "WorkBuddy 国内版每日签到", Channel: lane.ChannelWorkBuddy,
			Detail:    "CN 发布版 daily-checkin 端点，一次覆盖全部 CN 账号（含停用账号）；积分余额取首个可读账号",
			ResetHour: 0,
			Available: func() bool {
				for _, v := range s.Lane.WorkBuddyViews() {
					if v.Realm == lane.WBRealmCN && strings.TrimSpace(v.AccessToken) != "" && !v.NoCheckin {
						return true
					}
				}
				return false
			},
			LastDate: func() string {
				latest := ""
				for _, v := range s.Lane.WorkBuddyViews() {
					if v.Realm == lane.WBRealmCN && v.LastCheckinDate > latest {
						latest = v.LastCheckinDate
					}
				}
				return latest
			},
			Probe: s.checkinWBProbe(lane.WBRealmCN),
			Run:   s.Lane.WorkBuddyDailyCheckin,
		},
		{
			ID: "workbuddy-intl", Name: "WorkBuddy 国际版每日活跃", Channel: lane.ChannelWorkBuddy,
			Detail:    "国际版没有签到端点，用一条极小对话领当日活跃积分；重置点按 08:00 估计，猜错由 6 小时只读复核兜底",
			ResetHour: 8,
			Available: func() bool {
				for _, v := range s.Lane.WorkBuddyViews() {
					if v.Realm == lane.WBRealmIntl && strings.TrimSpace(v.AccessToken) != "" {
						return true
					}
				}
				return false
			},
			LastDate: func() string {
				latest := ""
				for _, v := range s.Lane.WorkBuddyViews() {
					if v.Realm == lane.WBRealmIntl && v.LastActivityDate > latest {
						latest = v.LastActivityDate
					}
				}
				return latest
			},
			Probe: s.checkinWBProbe(lane.WBRealmIntl),
			Run:   s.Lane.WorkBuddyDailyActivity,
		},
		{
			ID: "codearts", Name: "CodeArts 每日签到", Channel: lane.ChannelCodeArts,
			Detail:    "华为云每日积分活动（领取幂等）；领取响应只报本次奖励，累计领取是单调可核对的证据",
			ResetHour: 0,
			Available: func() bool {
				acct := s.Lane.CodeArtsSnapshot()
				return strings.TrimSpace(acct.SecurityToken) != "" && acct.Enabled
			},
			LastDate: func() string { return s.Lane.CodeArtsSnapshot().LastCheckinDate },
			Probe: func(context.Context) (float64, string, error) {
				return s.Lane.CodeArtsSnapshot().ClaimedCredits, "累计领取", nil
			},
			Reward: func() (float64, bool) {
				c := s.Lane.CodeArtsSnapshot().LastCheckinCredits
				return c, c > 0
			},
			Run: s.Lane.CodeArtsDailyCheckin,
		},
	}

	// Node-list declarations are the extension point for 更多网站 / 更多算力:
	// a supplier that says checkin:daily shows up here the moment it is imported,
	// with no code change, as an honest 待接入 row.
	for _, p := range s.Store.Config().Providers {
		if p.Checkin != "daily" {
			continue
		}
		p := p
		detail := "节点清单声明了 daily，签到执行器尚未接入"
		if p.SignupURL != "" {
			detail += "；注册地址：" + p.SignupURL
		}
		task := CheckinTask{
			ID: "node:" + p.ID, Name: p.Name + " 每日签到", Channel: p.ID,
			Detail:    detail,
			ResetHour: p.CheckinResetHour,
			Available: func() bool { return false },
			LastDate:  func() string { return "" },
			Run:       nil,
		}
		// A declared balance endpoint makes the row watchable (GET only) even
		// though its claim executor is not wired. The reset hour is data too:
		// no hard-coded 00:00/08:00 guess for a platform we have never seen.
		if p.BalanceURL != "" && p.BalancePath != "" {
			task.Probe = nodeBalanceProbe(p)
			kind := strings.TrimSpace(p.BalanceKind)
			if kind == "" {
				kind = "余额"
			}
			task.Detail += "；已声明只读" + kind + "端点，可查余额但不会签到"
		} else {
			task.Detail += "；未声明只读余额端点"
		}
		tasks = append(tasks, task)
	}
	return tasks
}

// checkinWBProbe reads one account's spendable credit for a publishment. It
// never signs in. WBCredits.Total may legitimately go DOWN when credits are
// spent, which is exactly why a non-positive delta is never used to declare a
// failed check-in.
func (s *Server) checkinWBProbe(realm string) func(context.Context) (float64, string, error) {
	return func(ctx context.Context) (float64, string, error) {
		var lastErr error
		for _, v := range s.Lane.WorkBuddyViews() {
			if v.Realm != realm || strings.TrimSpace(v.AccessToken) == "" {
				continue
			}
			c, err := s.Lane.WorkBuddyCreditsFor(ctx, v.ID)
			if err == nil {
				return c.Total, "积分余额", nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return 0, "积分余额", lastErr
		}
		return 0, "积分余额", errors.New("该发布版没有已登录账号")
	}
}

func readCheckinBalance(ctx context.Context, t CheckinTask) (float64, string, error) {
	if t.Probe == nil {
		return 0, "", errors.New("该平台未接入余额读取")
	}
	return t.Probe(ctx)
}

func fillCheckinResult(r *checkinResult, e store.CheckinEvidence) {
	r.Confirmed = e.Confirmed
	r.Method = e.Method
	r.Reward = e.Reward
	r.Delta = e.Delta
	r.Before = e.Before
	r.After = e.After
	r.Balance = e.Balance
	r.BalanceKind = e.BalanceKind
	r.At = e.At
	r.VerifiedAt = e.VerifiedAt
	r.Note = e.Note
}

// runCheckinTasks is the state machine itself. It takes the task list as an
// argument so tests can drive it with fake tasks instead of real upstreams.
// Writes are serialised with the scheduler by LockCheckin.
func (s *Server) runCheckinTasks(ctx context.Context, tasks []CheckinTask, only string, now time.Time) ([]checkinResult, int, int) {
	results := make([]checkinResult, 0)
	claimed, failed := 0, 0
	for _, t := range tasks {
		if only != "" && t.ID != only {
			continue
		}
		r := checkinResult{ID: t.ID, Name: t.Name}
		if t.Run == nil {
			r.Skipped = true
			r.Detail = "签到执行器尚未接入"
			results = append(results, r)
			continue
		}

		period := store.CheckinPeriod(now, t.ResetHour)
		e, _ := s.Store.CheckinEvidenceOf(t.ID)

		// Already proved for this period: zero write requests. Only a 6-hourly
		// read-only balance check, so a wrong reset-hour guess still self-heals.
		if e.Period == period && e.Confirmed {
			due := e.VerifiedAt == 0 || now.Sub(time.Unix(e.VerifiedAt, 0)) >= checkinVerifyInterval
			if due && t.Probe != nil {
				bal, kind, err := readCheckinBalance(ctx, t)
				e.VerifiedAt = now.Unix()
				if err == nil {
					e.Balance, e.BalanceKind = bal, kind
					e.Note = "已确认后只读复核；本周期不再写签到"
					r.Verified = true
				} else {
					// A lagging / unavailable meter is not a failed claim.
					// Do not re-claim; just try the read again in 6 hours.
					e.Note = "只读复核未读到余额：" + err.Error()
					r.Detail = e.Note
				}
				r.Balance, r.BalanceKind = e.Balance, e.BalanceKind
				s.Store.SetCheckinEvidence(e)
			} else {
				r.Skipped = true
				next := time.Unix(e.VerifiedAt, 0).Add(checkinVerifyInterval)
				if e.VerifiedAt == 0 {
					next = now.Add(checkinVerifyInterval)
				}
				r.Detail = "本周期已确认，无写请求；下次只读复核 " + next.Format("01-02 15:04")
			}
			fillCheckinResult(&r, e)
			results = append(results, r)
			continue
		}

		// New reset window: start a fresh evidence row, keeping only the last
		// known balance as a display baseline (never as proof for the new day).
		if e.Period != period {
			e = store.CheckinEvidence{
				TaskID: t.ID, Period: period, ResetHour: t.ResetHour,
				Balance: e.Balance, BalanceKind: e.BalanceKind,
			}
		}
		if t.Available != nil && !t.Available() {
			r.Skipped = true
			r.Detail = "未登录 / 无可用账号"
			results = append(results, r)
			continue
		}

		before, kind, beforeErr := readCheckinBalance(ctx, t)
		ran, runErr := t.Run(ctx)
		r.Ran = ran
		if runErr != nil {
			r.Error = runErr.Error()
			e.At = now.Unix()
			e.Confirmed = false
			e.Note = "领取失败，下个小时重试：" + runErr.Error()
			s.Store.SetCheckinEvidence(e)
			failed++
			fillCheckinResult(&r, e)
			results = append(results, r)
			continue
		}

		after, afterKind, afterErr := readCheckinBalance(ctx, t)
		if kind == "" {
			kind = afterKind
		}
		e.Before, e.After, e.BalanceKind = before, after, kind
		if afterErr == nil {
			e.Balance = after
		}
		e.Reward = 0
		if t.Reward != nil {
			if v, ok := t.Reward(); ok {
				e.Reward = v
			}
		}
		// Only a delta from two successful reads counts. A balance meter is
		// often one request behind, and a spendable balance can drop while the
		// claim landed — so "not up" is never "not claimed".
		if beforeErr == nil && afterErr == nil {
			e.Delta = after - before
		}
		switch {
		case e.Reward > 0:
			e.Confirmed, e.Method = true, checkinMethodReward
		case e.Delta > 0:
			e.Confirmed, e.Method = true, checkinMethodDelta
		case ran:
			e.Confirmed, e.Method = true, checkinMethodBalance
			if beforeErr != nil || afterErr != nil {
				e.Note = "接口成功但余额读不到，未核对积分增量；6 小时后只读复核"
			} else {
				e.Note = "接口成功，余额未上涨（读数可能滞后或期间有消费）；6 小时后只读复核"
			}
		default:
			// ran=false with no error = the account's own date stamp says the
			// day is already claimed. Trust it rather than writing again.
			e.Confirmed, e.Method = true, checkinMethodStamp
			e.Note = "账号已记录本周期签到，无需重复请求"
		}
		e.At = now.Unix()
		// Record the verify time even when the read failed: otherwise every
		// hourly pass would re-probe a meter that is simply unavailable.
		e.VerifiedAt = now.Unix()
		s.Store.SetCheckinEvidence(e)
		if ran && e.Confirmed {
			claimed++
		}
		fillCheckinResult(&r, e)
		results = append(results, r)
	}
	return results, claimed, failed
}

func (s *Server) checkinPass(ctx context.Context, only string, now time.Time) ([]checkinResult, int, int) {
	unlock := store.LockCheckin()
	defer unlock()
	return s.runCheckinTasks(ctx, s.checkinTasks(), only, now)
}

// checkinStatusRows is the panel's view: the registry plus the durable proof,
// so "今日已完成" means credits really moved, not "we sent a request".
func (s *Server) checkinStatusRows(now time.Time) []map[string]any {
	rows := make([]map[string]any, 0)
	for _, t := range s.checkinTasks() {
		last := ""
		if t.LastDate != nil {
			last = t.LastDate()
		}
		period := store.CheckinPeriod(now, t.ResetHour)
		e, _ := s.Store.CheckinEvidenceOf(t.ID)
		available := t.Run != nil && (t.Available == nil || t.Available())
		done := e.Confirmed && e.Period == period
		next := int64(0)
		if t.Run != nil {
			if done {
				base := e.VerifiedAt
				if base == 0 {
					base = now.Unix()
				}
				next = time.Unix(base, 0).Add(checkinVerifyInterval).Unix()
			} else {
				next = now.Add(checkinRetryInterval).Unix()
			}
		} else if t.Probe != nil {
			// A declared-but-unwired node still has a read-only watch schedule.
			base := e.VerifiedAt
			if base == 0 {
				base = now.Unix()
			}
			next = time.Unix(base, 0).Add(checkinVerifyInterval).Unix()
		}
		rows = append(rows, map[string]any{
			"id": t.ID, "name": t.Name, "channel": t.Channel, "detail": t.Detail,
			"lastDate": last, "doneToday": done,
			"available": available, "wired": t.Run != nil,
			"hasProbe": t.Probe != nil,
			"period":   period, "resetHour": t.ResetHour,
			"confirmed": e.Confirmed, "method": e.Method,
			"reward": e.Reward, "delta": e.Delta,
			"before": e.Before, "after": e.After,
			"balance": e.Balance, "balanceKind": e.BalanceKind,
			"at": e.At, "verifiedAt": e.VerifiedAt, "nextCheckAt": next,
			"note": e.Note,
		})
	}
	return rows
}

func (s *Server) adminCheckinStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	writeJSON(w, 200, map[string]any{
		"ok": true, "today": now.Format("2006-01-02"),
		"verifyHours": int(checkinVerifyInterval / time.Hour),
		"tasks":       s.checkinStatusRows(now),
	})
}

func (s *Server) adminCheckinAll(w http.ResponseWriter, r *http.Request) {
	// A detached daily claim must not ride the handler's context: it is
	// cancelled the instant the response is written.
	results, claimed, failed := s.checkinPass(context.Background(), "", time.Now())
	s.logCat(logx.CatAdmin, "info", "自动签到（手动全部）: 完成 %d / 失败 %d / 跳过 %d",
		claimed, failed, len(results)-claimed-failed)
	writeJSON(w, 200, map[string]any{
		"ok": true, "claimed": claimed, "failed": failed, "results": results,
	})
}

func (s *Server) adminCheckinOne(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "缺少任务 id"})
		return
	}
	results, claimed, failed := s.checkinPass(context.Background(), in.ID, time.Now())
	if len(results) == 0 {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "没有这个签到任务"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "claimed": claimed, "failed": failed, "results": results,
	})
}

// adminCheckinRefresh answers "我给你个密钥，能知道那个账号里面有多少钱".
// It is strictly read-only: no Run, no claim, no date stamp update.
func (s *Server) adminCheckinRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if r.ContentLength > 0 {
		if err := decodeBody(r, &in); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	unlock := store.LockCheckin()
	defer unlock()
	rows, bad := s.refreshCheckinBalances(context.Background(), s.checkinTasks(), in.ID, time.Now())
	if in.ID != "" && len(rows) == 0 {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "没有这个签到任务"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "failed": bad, "rows": rows})
}

// refreshCheckinBalances is strictly read-only: no Run, no claim, no date
// stamp update. Taking the task list as an argument keeps it testable without
// a live upstream account.
func (s *Server) refreshCheckinBalances(ctx context.Context, tasks []CheckinTask, only string, now time.Time) ([]map[string]any, int) {
	rows := make([]map[string]any, 0)
	bad := 0
	for _, t := range tasks {
		if only != "" && t.ID != only {
			continue
		}
		if t.Probe == nil {
			rows = append(rows, map[string]any{"id": t.ID, "name": t.Name, "error": "该任务没有余额读取器"})
			continue
		}
		bal, kind, err := readCheckinBalance(ctx, t)
		row := map[string]any{"id": t.ID, "name": t.Name, "balanceKind": kind}
		e, _ := s.Store.CheckinEvidenceOf(t.ID)
		if e.TaskID == "" {
			e.TaskID, e.Period, e.ResetHour = t.ID, store.CheckinPeriod(now, t.ResetHour), t.ResetHour
		}
		e.VerifiedAt = now.Unix()
		if err == nil {
			e.Balance, e.BalanceKind = bal, kind
			e.Note = "只读查余额（未执行签到）"
			row["balance"] = bal
			row["ok"] = true
		} else {
			row["error"] = err.Error()
			row["ok"] = false
			bad++
		}
		s.Store.SetCheckinEvidence(e)
		rows = append(rows, row)
	}
	return rows, bad
}

// StartCheckinLoop is the single scheduler for all daily claims. One hourly
// pass: unconfirmed tasks retry the claim, confirmed tasks are skipped until
// their 6-hour read-only verification comes due. A boot pass covers a machine
// that was off at midnight.
func (s *Server) StartCheckinLoop(ctx context.Context) {
	go func() {
		select {
		case <-time.After(checkinBootDelay):
		case <-ctx.Done():
			return
		}
		s.runScheduledCheckins(ctx)
		t := time.NewTicker(checkinRetryInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runScheduledCheckins(ctx)
			}
		}
	}()
}

func (s *Server) runScheduledCheckins(ctx context.Context) {
	results, claimed, failed := s.checkinPass(ctx, "", time.Now())
	verified := 0
	for _, r := range results {
		if r.Error != "" {
			s.logCat(logx.CatAdmin, "warn", "自动签到 %s 未完成（下个小时重试）: %s", r.ID, r.Error)
		}
		if r.Verified {
			verified++
		}
	}
	if claimed > 0 || failed > 0 || verified > 0 {
		s.logCat(logx.CatAdmin, "info", "自动签到: 领取 %d / 失败 %d / 只读复核 %d", claimed, failed, verified)
	}
}
