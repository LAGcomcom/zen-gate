package store

import (
	"path/filepath"
	"sync"
	"time"
)

// CheckinEvidence is the durable proof that a daily claim actually landed.
// A date stamp alone only says "we tried"; the upstream fact that ends the
// retries is the credit balance going up (or the claim response naming the
// reward). Keeping it on disk means a restart does not re-claim the day.
//
// Learned from the public check-in tools: workbuddy-auto-signin's "先查状态、
// 未签才领" + 4-hourly read-only poll, Trae-AutoCheckin's read-only credit
// monitor with a same-day state file, and metapi's parsed reward amount and
// hourly balance refresh. They all treat "already confirmed today" as zero
// write requests, and they all keep the evidence outside the process.
type CheckinEvidence struct {
	TaskID string `json:"taskId"`
	// Period is the local reset window, e.g. "2026-11-04". ResetHour shifts
	// the boundary: 0 means midnight (domestic), 8 means 08:00 (some overseas
	// publishments). A wrong guess is harmless because a confirmed task is
	// re-verified every 6 hours instead of being trusted until midnight.
	Period    string `json:"period"`
	ResetHour int    `json:"resetHour,omitempty"`
	// BalanceKind names what the number means: 积分余额 (may go down when
	// spent) or 累计领取 (monotonic). The panel prints it verbatim.
	BalanceKind string  `json:"balanceKind,omitempty"`
	Before      float64 `json:"before,omitempty"`
	After       float64 `json:"after,omitempty"`
	// Reward is the amount the claim response itself declared. metapi parses
	// this from the response; it is stronger evidence than a lagging balance
	// read, and is kept separately so the two never get added together.
	Reward float64 `json:"reward,omitempty"`
	Delta  float64 `json:"delta,omitempty"`
	// Confirmed is the state that stops the writes. Method says what proved it:
	// 余额增量 / 领取响应 / 接口成功（余额不可读）.
	Confirmed bool   `json:"confirmed"`
	Method    string `json:"method,omitempty"`
	At        int64  `json:"at"`
	// VerifiedAt is the last read-only balance check. Confirmed tasks are only
	// re-checked 6 hours later; unconfirmed ones retry every hour.
	VerifiedAt int64   `json:"verifiedAt,omitempty"`
	Balance    float64 `json:"balance,omitempty"`
	Note       string  `json:"note,omitempty"`
}

type checkinEvidenceFile struct {
	Version int                        `json:"version"`
	Items   map[string]CheckinEvidence `json:"items"`
}

func (s *Store) checkinEvidencePath() string {
	return filepath.Join(s.Home, "checkin_evidence.json")
}

func (s *Store) loadCheckinEvidence() {
	f, err := loadJSON[checkinEvidenceFile](s.checkinEvidencePath())
	if err != nil || f == nil || f.Items == nil {
		s.checkinEvidence = map[string]CheckinEvidence{}
		return
	}
	s.checkinEvidence = f.Items
}

func (s *Store) saveCheckinEvidenceLocked() {
	_ = writeJSON(s.checkinEvidencePath(), checkinEvidenceFile{
		Version: 1, Items: s.checkinEvidence,
	})
}

// CheckinEvidenceOf reads one task's last proof.
func (s *Store) CheckinEvidenceOf(taskID string) (CheckinEvidence, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.checkinEvidence[taskID]
	return e, ok
}

// AllCheckinEvidence returns a copy; callers never hold the store's map.
func (s *Store) AllCheckinEvidence() map[string]CheckinEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]CheckinEvidence, len(s.checkinEvidence))
	for k, v := range s.checkinEvidence {
		out[k] = v
	}
	return out
}

// SetCheckinEvidence persists immediately: a crash right after a successful
// claim must not lose the proof and re-claim the same day.
func (s *Store) SetCheckinEvidence(e CheckinEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkinEvidence == nil {
		s.checkinEvidence = map[string]CheckinEvidence{}
	}
	s.checkinEvidence[e.TaskID] = e
	s.saveCheckinEvidenceLocked()
}

// CheckinPeriod returns the local reset window key for a task. Subtracting
// ResetHour makes an 08:00 reset read as the same "day" from 08:00 until the
// next 08:00, without hard-coding a timezone or a cron expression.
func CheckinPeriod(now time.Time, resetHour int) string {
	if resetHour < 0 || resetHour > 23 {
		resetHour = 0
	}
	return now.Add(-time.Duration(resetHour) * time.Hour).Format("2006-01-02")
}

// checkinEvidenceMu serialises claim attempts across the scheduler and the
// manual button: two triggers must never sign in the same account at once.
var checkinEvidenceMu sync.Mutex

// LockCheckin serialises one task's claim/verify pass. It is a plain mutex
// (not per-task) on purpose: a daily pass is rare, and the extra map of locks
// is another piece of state to get wrong.
func LockCheckin() func() {
	checkinEvidenceMu.Lock()
	return checkinEvidenceMu.Unlock
}
