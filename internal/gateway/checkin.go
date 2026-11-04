package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
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
// Nothing here deletes or replaces the lane's own guards: each Run is still
// once-per-local-day, idempotent, and free to return ran=false. The registry
// only makes them visible and callable from one place.

type CheckinTask struct {
	ID      string
	Name    string
	Channel string
	Detail  string
	// Available says the task has a usable account. It is a display/skip hint,
	// not a second authorisation layer: Run keeps its own guards.
	Available func() bool
	// LastDate is the local calendar day the claim last landed; "" = never.
	LastDate func() string
	// Run claims one day. ran=false means "nothing to do", never an error.
	// nil marks a declared-but-unwired task.
	Run func(context.Context) (bool, error)
}

type checkinResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Ran     bool   `json:"ran"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// checkinTasks is built on demand rather than cached: accounts and node lists
// change while the process runs, and a stale registry would claim a deleted
// account is still eligible.
func (s *Server) checkinTasks() []CheckinTask {
	tasks := []CheckinTask{
		{
			ID: "workbuddy-cn", Name: "WorkBuddy 国内版每日签到", Channel: lane.ChannelWorkBuddy,
			Detail: "CN 发布版 daily-checkin 端点，一次覆盖全部 CN 账号（含停用账号）",
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
			Run: s.Lane.WorkBuddyDailyCheckin,
		},
		{
			ID: "workbuddy-intl", Name: "WorkBuddy 国际版每日活跃", Channel: lane.ChannelWorkBuddy,
			Detail: "国际版没有签到端点，用一条极小对话领当日活跃积分",
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
			Run: s.Lane.WorkBuddyDailyActivity,
		},
		{
			ID: "codearts", Name: "CodeArts 每日签到", Channel: lane.ChannelCodeArts,
			Detail: "华为云每日积分活动（领取幂等，已领取只补确认）",
			Available: func() bool {
				acct := s.Lane.CodeArtsSnapshot()
				return strings.TrimSpace(acct.SecurityToken) != "" && acct.Enabled
			},
			LastDate: func() string { return s.Lane.CodeArtsSnapshot().LastCheckinDate },
			Run:      s.Lane.CodeArtsDailyCheckin,
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
		tasks = append(tasks, CheckinTask{
			ID: "node:" + p.ID, Name: p.Name + " 每日签到", Channel: p.ID,
			Detail:    detail,
			Available: func() bool { return false },
			LastDate:  func() string { return "" },
			Run:       nil,
		})
	}
	return tasks
}

// runCheckins executes every eligible task (or one by id). A partial failure
// is reported per row: one platform being down must not hide the other two.
func (s *Server) runCheckins(ctx context.Context, only string) ([]checkinResult, int, int) {
	results := make([]checkinResult, 0)
	claimed, failed := 0, 0
	for _, t := range s.checkinTasks() {
		if only != "" && t.ID != only {
			continue
		}
		if t.Run == nil || (t.Available != nil && !t.Available()) {
			results = append(results, checkinResult{ID: t.ID, Name: t.Name, Skipped: true})
			continue
		}
		ran, err := t.Run(ctx)
		res := checkinResult{ID: t.ID, Name: t.Name, Ran: ran}
		if err != nil {
			res.Error = err.Error()
			failed++
		} else if ran {
			claimed++
		}
		results = append(results, res)
	}
	return results, claimed, failed
}

func (s *Server) adminCheckinStatus(w http.ResponseWriter, r *http.Request) {
	today := time.Now().Format("2006-01-02")
	rows := make([]map[string]any, 0)
	for _, t := range s.checkinTasks() {
		last := ""
		if t.LastDate != nil {
			last = t.LastDate()
		}
		available := t.Run != nil && (t.Available == nil || t.Available())
		rows = append(rows, map[string]any{
			"id": t.ID, "name": t.Name, "channel": t.Channel, "detail": t.Detail,
			"lastDate": last, "doneToday": last == today,
			"available": available, "wired": t.Run != nil,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "today": today, "tasks": rows})
}

func (s *Server) adminCheckinAll(w http.ResponseWriter, r *http.Request) {
	// A detached daily claim must not ride the handler's context: it is
	// cancelled the instant the response is written.
	results, claimed, failed := s.runCheckins(context.Background(), "")
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
	results, claimed, failed := s.runCheckins(context.Background(), in.ID)
	if len(results) == 0 {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "没有这个签到任务"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "claimed": claimed, "failed": failed, "results": results,
	})
}

// StartCheckinLoop is the single scheduler for all daily claims. It replaces
// the per-platform tickers in main: one pass shortly after boot (covering the
// "already logged in today" case), then an hourly retry. Every claim is
// once-per-day and idempotent, so a pass costs nothing when the day is done.
func (s *Server) StartCheckinLoop(ctx context.Context) {
	go func() {
		select {
		case <-time.After(90 * time.Second):
		case <-ctx.Done():
			return
		}
		s.runScheduledCheckins(ctx)
		t := time.NewTicker(time.Hour)
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
	results, claimed, failed := s.runCheckins(ctx, "")
	for _, r := range results {
		if r.Error != "" {
			s.logCat(logx.CatAdmin, "warn", "自动签到 %s 未完成（下个小时重试）: %s", r.ID, r.Error)
		}
	}
	if claimed > 0 || failed > 0 {
		s.logCat(logx.CatAdmin, "info", "自动签到: 完成 %d / 失败 %d", claimed, failed)
	}
}
