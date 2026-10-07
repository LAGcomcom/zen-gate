package gateway

import (
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// The task registry is what makes the 测试 / 能力实测 buttons observable: the
// dashboard rebuilds its whole grid every 10s, so any progress that lives only
// in the DOM is erased. These tests pin the storage rules that the UI depends on.

func TestTaskRegistryKeepsFiveRunsPerModel(t *testing.T) {
	r := newTaskRegistry()
	ids := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		task, created := r.begin("probe", "m1")
		if !created {
			t.Fatalf("run %d: begin refused to start a finished task", i)
		}
		r.finish(task, "done")
		ids = append(ids, task.ID)
	}
	h := r.history("m1")
	if len(h) != 5 {
		t.Fatalf("history length = %d, want the 5 newest runs", len(h))
	}
	if h[0].ID != ids[6] || h[4].ID != ids[2] {
		t.Fatalf("history order = %s..%s, want newest first %s..%s",
			h[0].ID, h[4].ID, ids[6], ids[2])
	}
	if got := len(r.history("never-probed")); got != 0 {
		t.Fatalf("unknown model history = %d, want 0", got)
	}
}

func TestTaskRegistryReusesARunningTask(t *testing.T) {
	r := newTaskRegistry()
	first, created := r.begin("probe", "m2")
	if !created {
		t.Fatal("the first begin must create the task")
	}
	second, created := r.begin("probe", "m2")
	if created {
		t.Fatal("a second 测试 while one is running must not start a second upstream call")
	}
	if second != first {
		t.Fatal("the reused task must be the in-flight one, so the click sees its progress")
	}
	// A different kind is a different action, not a duplicate click.
	caps, created := r.begin("caps", "m2")
	if !created {
		t.Fatal("能力实测 must run alongside 测试 on the same model")
	}
	if caps.Kind != "caps" {
		t.Fatalf("caps task kind = %q", caps.Kind)
	}
	r.finish(first, "done")
	if _, created := r.begin("probe", "m2"); !created {
		t.Fatal("a finished task must not block the next run")
	}
}

func TestTaskRegistryRecordsStepsAndFinishStatus(t *testing.T) {
	r := newTaskRegistry()
	task, _ := r.begin("probe", "m3")
	r.step(task, lane.ProbeStep{Name: "请求已发出", Status: lane.StepRunning, Detail: "https://example/v1"})
	r.step(task, lane.ProbeStep{Name: "首字节到达", Status: lane.StepOK, Ms: 1234})
	r.step(task, lane.ProbeStep{Name: "结论", Status: lane.StepFail, Detail: "429 upstream saturated"})
	r.finish(task, "failed")

	got := r.latest("m3")
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(got.Steps) != 3 {
		t.Fatalf("steps = %d, want the three phases in order", len(got.Steps))
	}
	if got.Steps[1].Ms != 1234 {
		t.Fatalf("first-token step lost its ms: %+v", got.Steps[1])
	}
	if got.Steps[2].Detail != "429 upstream saturated" {
		t.Fatalf("the upstream error text must reach the drawer, got %q", got.Steps[2].Detail)
	}
	if got.Updated < got.Started {
		t.Fatalf("updated %d is before started %d", got.Updated, got.Started)
	}
	// The caller never sets At: the registry stamps it so the drawer can show
	// when each phase happened without trusting the emitter.
	if got.Steps[0].At <= 0 {
		t.Fatalf("step 0 has no timestamp: %+v", got.Steps[0])
	}
}

func TestTaskRegistryReclaimsAnAbandonedRun(t *testing.T) {
	r := newTaskRegistry()
	stuck, _ := r.begin("caps", "m4")
	stuck.Started = time.Now().Add(-staleTaskAfter - time.Minute).UnixMilli()
	r.step(stuck, lane.ProbeStep{Name: "vision", Status: lane.StepRunning})

	task, created := r.begin("caps", "m4")
	if !created {
		t.Fatal("a run that stopped reporting must not lock the card forever")
	}
	if stuck.Status != "failed" {
		t.Fatalf("abandoned run status = %q, want failed", stuck.Status)
	}
	if task == stuck {
		t.Fatal("the reclaimed card must start a fresh run")
	}
}
