package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// fakeCheckin stands in for an upstream platform. The tests care about how
// many WRITE requests (Run) and READ requests (Probe) the state machine makes,
// not about any particular HTTP endpoint.
type fakeCheckin struct {
	runs, probes int
	balance      float64
	balanceAfter float64
	reward       float64
	probeErr     error
	runErr       error
	ran          bool
	id           string
	reset        int
}

func (f *fakeCheckin) task() CheckinTask {
	id := f.id
	if id == "" {
		id = "codearts"
	}
	return CheckinTask{
		ID: id, Name: "假平台每日签到", Channel: "fake", ResetHour: f.reset,
		Available: func() bool { return true },
		LastDate:  func() string { return "" },
		Probe: func(context.Context) (float64, string, error) {
			f.probes++
			if f.probeErr != nil {
				return 0, "积分余额", f.probeErr
			}
			return f.balance, "积分余额", nil
		},
		Reward: func() (float64, bool) { return f.reward, f.reward > 0 },
		Run: func(context.Context) (bool, error) {
			f.runs++
			if f.runErr != nil {
				return true, f.runErr
			}
			if f.balanceAfter != 0 {
				f.balance = f.balanceAfter
			}
			return f.ran, nil
		},
	}
}

func checkinTestTime(day, hour, min int) time.Time {
	return time.Date(2026, 11, day, hour, min, 0, 0, time.Local)
}

// The whole point of evidence-based check-in: once the credits are proven to
// have moved, there are ZERO further claim writes this period. Only a 6-hourly
// read-only balance check is allowed, which also heals a wrong reset-hour guess.
func TestCheckinConfirmedStopsWriteRequestsForSixHours(t *testing.T) {
	s, _, st := newBareServer(t)
	f := &fakeCheckin{balance: 100, balanceAfter: 130, reward: 5, ran: true}
	tasks := []CheckinTask{f.task()}
	t0 := checkinTestTime(4, 9, 0)

	results, claimed, failed := s.runCheckinTasks(context.Background(), tasks, "", t0)
	if claimed != 1 || failed != 0 || len(results) != 1 {
		t.Fatalf("first pass = %+v (%d/%d)", results, claimed, failed)
	}
	if r := results[0]; !r.Confirmed || r.Method != checkinMethodReward || r.Reward != 5 || r.Delta != 30 {
		t.Fatalf("reward evidence lost: %+v", r)
	}
	if f.runs != 1 || f.probes != 2 {
		t.Fatalf("first pass should be 1 claim + 2 balance reads, got %d/%d", f.runs, f.probes)
	}
	e, ok := st.CheckinEvidenceOf("codearts")
	if !ok || !e.Confirmed || e.Period != "2026-11-04" || e.VerifiedAt != t0.Unix() {
		t.Fatalf("durable evidence = %+v (ok=%v)", e, ok)
	}

	// One hour later: skip, no claim, no balance read.
	_, claimed, _ = s.runCheckinTasks(context.Background(), tasks, "", t0.Add(time.Hour))
	if claimed != 0 || f.runs != 1 || f.probes != 2 {
		t.Fatalf("a confirmed day issued another request: runs=%d probes=%d", f.runs, f.probes)
	}

	// Six hours later: read-only verification, still no claim.
	f.balance = 150
	results, _, _ = s.runCheckinTasks(context.Background(), tasks, "", t0.Add(6*time.Hour))
	if !results[0].Verified || results[0].Ran || f.runs != 1 {
		t.Fatalf("6h pass must be read-only: %+v (runs=%d)", results[0], f.runs)
	}
	e, _ = st.CheckinEvidenceOf("codearts")
	if e.Balance != 150 || !e.Confirmed {
		t.Fatalf("verification did not refresh the balance: %+v", e)
	}
}

// When the claim response names no reward, a real before/after balance delta
// is the proof. metapi parses the reward; this is the other half of the same
// idea for platforms that only expose a meter.
func TestCheckinBalanceDeltaIsProof(t *testing.T) {
	s, _, _ := newBareServer(t)
	f := &fakeCheckin{balance: 100, balanceAfter: 130, ran: true}
	results, _, _ := s.runCheckinTasks(context.Background(), []CheckinTask{f.task()}, "", checkinTestTime(4, 9, 0))
	if r := results[0]; !r.Confirmed || r.Method != checkinMethodDelta || r.Delta != 30 || r.Reward != 0 {
		t.Fatalf("delta evidence = %+v", r)
	}
}

// A transient claim error must leave the period OPEN so the next hourly pass
// retries — and must never be recorded as a confirmed day.
func TestCheckinClaimErrorRetriesNextHour(t *testing.T) {
	s, _, st := newBareServer(t)
	f := &fakeCheckin{balance: 100, runErr: errors.New("upstream 500"), ran: true}
	tasks := []CheckinTask{f.task()}
	t0 := checkinTestTime(4, 9, 0)

	results, claimed, failed := s.runCheckinTasks(context.Background(), tasks, "", t0)
	if failed != 1 || claimed != 0 || results[0].Error == "" {
		t.Fatalf("error pass = %+v (%d/%d)", results, claimed, failed)
	}
	e, _ := st.CheckinEvidenceOf("codearts")
	if e.Confirmed {
		t.Fatal("a failed claim was recorded as confirmed")
	}

	f.runErr, f.balanceAfter = nil, 130
	results, claimed, failed = s.runCheckinTasks(context.Background(), tasks, "", t0.Add(time.Hour))
	if failed != 0 || claimed != 1 || !results[0].Confirmed || f.runs != 2 {
		t.Fatalf("retry pass = %+v (%d/%d), runs=%d", results, claimed, failed, f.runs)
	}
}

// A lagging / broken balance meter is NOT a failed check-in. The community
// hit exactly this: "余额看着没变，接口返回的是领取前的值".
func TestCheckinUnreadableBalanceDoesNotFailClaim(t *testing.T) {
	s, _, _ := newBareServer(t)
	f := &fakeCheckin{probeErr: errors.New("meter 404"), ran: true}
	results, claimed, failed := s.runCheckinTasks(context.Background(), []CheckinTask{f.task()}, "", checkinTestTime(4, 9, 0))
	if failed != 0 || claimed != 1 {
		t.Fatalf("a read error must not fail the claim: %+v (%d/%d)", results, claimed, failed)
	}
	if r := results[0]; !r.Confirmed || r.Method != checkinMethodBalance || r.Delta != 0 {
		t.Fatalf("weak evidence must be labelled honestly: %+v", r)
	}
}

// ran=false means the account's own date stamp already has the day. Trust it
// instead of issuing another write when our evidence file was lost.
func TestCheckinAccountStampIsProofWithoutWrite(t *testing.T) {
	s, _, _ := newBareServer(t)
	f := &fakeCheckin{balance: 100, ran: false}
	results, claimed, _ := s.runCheckinTasks(context.Background(), []CheckinTask{f.task()}, "", checkinTestTime(4, 9, 0))
	if !results[0].Confirmed || results[0].Method != checkinMethodStamp || claimed != 0 {
		t.Fatalf("stamp evidence = %+v (claimed=%d)", results[0], claimed)
	}
}

// Crossing the reset boundary starts a new period. The 6-hour verify gate
// must never suppress a genuinely new day.
func TestCheckinNewPeriodResetsEvidence(t *testing.T) {
	s, _, st := newBareServer(t)
	f := &fakeCheckin{balance: 100, balanceAfter: 130, reward: 5, ran: true}
	tasks := []CheckinTask{f.task()}
	s.runCheckinTasks(context.Background(), tasks, "", checkinTestTime(4, 23, 30))
	results, claimed, _ := s.runCheckinTasks(context.Background(), tasks, "", checkinTestTime(5, 0, 30))
	if claimed != 1 || f.runs != 2 || !results[0].Confirmed {
		t.Fatalf("new day must claim again: %+v (claimed=%d runs=%d)", results, claimed, f.runs)
	}
	e, _ := st.CheckinEvidenceOf("codearts")
	if e.Period != "2026-11-05" {
		t.Fatalf("period = %q, want the new day", e.Period)
	}
}

// "我给你个密钥能知道那个账号里面有多少钱" — the refresh path must answer
// the balance question without ever claiming or marking the day done.
func TestCheckinRefreshIsReadOnly(t *testing.T) {
	s, _, st := newBareServer(t)
	f := &fakeCheckin{balance: 888, ran: true}
	tasks := []CheckinTask{f.task()}
	t0 := checkinTestTime(4, 9, 0)
	rows, bad := s.refreshCheckinBalances(context.Background(), tasks, "", t0)
	if bad != 0 || len(rows) != 1 || rows[0]["balance"] != float64(888) {
		t.Fatalf("refresh rows = %+v (bad=%d)", rows, bad)
	}
	if f.runs != 0 {
		t.Fatal("read-only refresh issued a claim")
	}
	e, _ := st.CheckinEvidenceOf("codearts")
	if e.Confirmed || e.Balance != 888 || e.VerifiedAt != t0.Unix() {
		t.Fatalf("refresh must not confirm the day: %+v", e)
	}
}

func TestCheckinRefreshEndpointNeverSignsIn(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/checkin/refresh", `{}`)
	if out["ok"] != true {
		t.Fatalf("refresh endpoint = %+v", out)
	}
	if rows, ok := out["rows"].([]any); !ok || len(rows) < 3 {
		t.Fatalf("expected a row per wired task, got %+v", out["rows"])
	}
	code, j := getAdminJSON(t, ts.URL+"/admin/api/checkin/status")
	if code != http.StatusOK || j["ok"] != true {
		t.Fatalf("status = %d %+v", code, j)
	}
	for _, raw := range j["tasks"].([]any) {
		row := raw.(map[string]any)
		if row["doneToday"] == true {
			t.Errorf("a read-only balance check marked %v as checked in", row["id"])
		}
		if row["hasProbe"] != true && row["wired"] == true {
			t.Errorf("wired task %v lost its balance reader", row["id"])
		}
	}
}

// The status panel reads the durable proof, not the date stamp: "今日已完成"
// has to mean credits really moved.
func TestCheckinStatusExposesEvidence(t *testing.T) {
	s, _, _ := newBareServer(t)
	f := &fakeCheckin{balance: 100, balanceAfter: 130, reward: 5, ran: true}
	t0 := checkinTestTime(4, 9, 0)
	rows := s.checkinStatusRows(t0)
	var before map[string]any
	for _, row := range rows {
		if row["id"] == "codearts" {
			before = row
		}
	}
	if before == nil || before["doneToday"] != false {
		t.Fatalf("unclaimed day reported done: %+v", before)
	}
	s.runCheckinTasks(context.Background(), []CheckinTask{f.task()}, "", t0)
	rows = s.checkinStatusRows(t0)
	for _, row := range rows {
		if row["id"] != "codearts" {
			continue
		}
		if row["doneToday"] != true || row["method"] != checkinMethodReward || row["balance"] != float64(130) {
			t.Fatalf("status lost the evidence: %+v", row)
		}
		if row["nextCheckAt"].(int64) != t0.Add(6*time.Hour).Unix() {
			t.Fatalf("next read-only verify = %v", row["nextCheckAt"])
		}
		return
	}
	t.Fatal("codearts row missing from status")
}

// Manual "立即全部签到" and the scheduler share one lock; drive both paths
// through the HTTP surface to prove the handler wiring compiles and stays
// read-only-safe when no account is logged in.
func TestCheckinHandlersRespectEvidenceEndToEnd(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/checkin/all", `{}`)
	if out["ok"] != true || out["claimed"].(float64) != 0 {
		t.Fatalf("all with no accounts = %+v", out)
	}
	one := postAdmin(t, ts.URL+"/admin/api/checkin/one", `{"id":"codearts"}`)
	if one["ok"] != true {
		t.Fatalf("one = %+v", one)
	}
}
