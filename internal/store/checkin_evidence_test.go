package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reset point is a guess, so the window function must be exact: domestic
// platforms roll at local midnight, overseas ones may roll at 08:00. An 08:00
// window keeps 07:59 and 08:00 the next day in the SAME period.
func TestCheckinPeriodWindows(t *testing.T) {
	cases := []struct {
		name  string
		when  time.Time
		reset int
		want  string
	}{
		{"midnight", time.Date(2026, 11, 4, 23, 59, 0, 0, time.Local), 0, "2026-11-04"},
		{"just past midnight", time.Date(2026, 11, 5, 0, 1, 0, 0, time.Local), 0, "2026-11-05"},
		{"08:00 window just before", time.Date(2026, 11, 4, 7, 59, 0, 0, time.Local), 8, "2026-11-03"},
		{"08:00 window at boundary", time.Date(2026, 11, 4, 8, 0, 0, 0, time.Local), 8, "2026-11-04"},
		{"08:00 window next morning", time.Date(2026, 11, 5, 7, 59, 0, 0, time.Local), 8, "2026-11-04"},
		{"invalid reset falls back to midnight", time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local), 25, "2026-11-04"},
		{"negative reset falls back to midnight", time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local), -1, "2026-11-04"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CheckinPeriod(c.when, c.reset); got != c.want {
				t.Fatalf("CheckinPeriod(%s, %d) = %q, want %q", c.when, c.reset, got, c.want)
			}
		})
	}
}

// A crash right after a successful claim must not lose the proof: the public
// tools all keep a same-day state file, and re-claiming without it is the bug
// this whole layer exists to avoid.
func TestCheckinEvidenceRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	s.SetCheckinEvidence(CheckinEvidence{
		TaskID: "codearts", Period: "2026-11-04", ResetHour: 0,
		BalanceKind: "累计领取", Before: 1200, After: 1300, Reward: 100, Delta: 100,
		Confirmed: true, Method: "领取响应", At: 1730000000, VerifiedAt: 1730000000,
		Balance: 1300, Note: "积分真的涨了",
	})

	raw, err := os.ReadFile(filepath.Join(home, "checkin_evidence.json"))
	if err != nil {
		t.Fatalf("evidence was not persisted: %v", err)
	}
	for _, want := range []string{`"taskId": "codearts"`, `"reward": 100`, `"confirmed": true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("evidence file missing %s: %s", want, raw)
		}
	}

	s2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := s2.CheckinEvidenceOf("codearts")
	if !ok || !e.Confirmed || e.Reward != 100 || e.Method != "领取响应" || e.Period != "2026-11-04" {
		t.Fatalf("reopened evidence = %+v (ok=%v)", e, ok)
	}

	// AllCheckinEvidence must be a copy, not the store's live map.
	all := s2.AllCheckinEvidence()
	delete(all, "codearts")
	if _, still := s2.CheckinEvidenceOf("codearts"); !still {
		t.Fatal("mutating the returned map changed the store")
	}
}

// The manual button and the hourly scheduler must never claim at the same
// instant. A single process-wide lock is enough for a once-a-day action.
func TestLockCheckinSerialises(t *testing.T) {
	unlock := LockCheckin()
	entered := make(chan struct{})
	go func() {
		defer close(entered)
		u := LockCheckin()
		defer u()
	}()
	select {
	case <-entered:
		unlock()
		t.Fatal("second claim entered while the first one held the lock")
	case <-time.After(30 * time.Millisecond):
	}
	unlock()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second claim never acquired the released lock")
	}
}
