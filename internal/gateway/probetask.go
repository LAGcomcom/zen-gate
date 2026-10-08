package gateway

// The probe task registry: what the 模型 page's 测试 and 能力实测 buttons report
// while they run. Progress has to live server-side because the dashboard
// rebuilds the whole card grid every 10s — anything written only into the DOM
// disappears mid-run, which is why a click used to look like it did nothing.
// Runs stay in memory only (5 per model, newest first): they are a live view of
// one action, not history worth a file.

import (
	"fmt"
	"sync"
	"time"

	"zen-gate/internal/lane"
)

const maxTaskRuns = 5

// staleTaskAfter bounds how long a run may sit "running" without reporting a
// phase. A wedged goroutine would otherwise lock its card against ever being
// tested again.
var staleTaskAfter = 10 * time.Minute

const (
	taskRunning = "running"
	taskDone    = "done"
	taskFailed  = "failed"
)

// ProbeTask is one run of one action against one model.
type ProbeTask struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // probe = 可用性测试, caps = 能力实测
	Model   string `json:"model"`
	Status  string `json:"status"`
	Started int64  `json:"started"`
	Updated int64  `json:"updated"`
	// Steps are the phases in the order they were reported.
	Steps []lane.ProbeStep `json:"steps"`
	// Result and Verdicts are the outcome once the run has one.
	Result   *lane.ProbeResult `json:"result,omitempty"`
	Verdicts map[string]string `json:"verdicts,omitempty"`
}

type taskRegistry struct {
	mu      sync.Mutex
	byModel map[string][]*ProbeTask
	seq     int
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{byModel: map[string][]*ProbeTask{}}
}

// begin starts a run, or hands back the run already going for this kind so a
// second click cannot fire a second upstream call and burn quota twice.
func (r *taskRegistry) begin(kind, model string) (*ProbeTask, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UnixMilli()
	runs := r.byModel[model]
	for _, t := range runs {
		if t.Kind != kind || t.Status != taskRunning {
			continue
		}
		if now-t.Started > staleTaskAfter.Milliseconds() {
			t.Status = taskFailed
			t.Updated = now
			t.Steps = append(t.Steps, lane.ProbeStep{
				At: now, Name: "已中止", Status: lane.StepUnknown,
				Detail: fmt.Sprintf("这次运行超过 %d 分钟没有回报，按超时处理", int(staleTaskAfter.Minutes())),
			})
			continue
		}
		return t, false
	}
	r.seq++
	t := &ProbeTask{
		ID: fmt.Sprintf("ptask-%d", r.seq), Kind: kind, Model: model,
		Status: taskRunning, Started: now, Updated: now,
	}
	r.byModel[model] = append([]*ProbeTask{t}, runs...)
	if len(r.byModel[model]) > maxTaskRuns {
		r.byModel[model] = r.byModel[model][:maxTaskRuns]
	}
	return t, true
}

// step appends one phase, stamped here so the drawer can always say when.
func (r *taskRegistry) step(t *ProbeTask, st lane.ProbeStep) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.At == 0 {
		st.At = time.Now().UnixMilli()
	}
	t.Steps = append(t.Steps, st)
	t.Updated = st.At
}

// sink is the lane-side hook: phases reported through the context land on the
// run without every probe call site needing a parameter.
func (r *taskRegistry) sink(t *ProbeTask) lane.ProbeEventSink {
	return func(st lane.ProbeStep) { r.step(t, st) }
}

// finish ends a run.
func (r *taskRegistry) finish(t *ProbeTask, status string) {
	r.conclude(t, status, nil, nil)
}

// conclude ends a run with whatever it learned on the way.
func (r *taskRegistry) conclude(t *ProbeTask, status string, res *lane.ProbeResult, verdicts map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Result = res
	t.Verdicts = verdicts
	t.Status = status
	t.Updated = time.Now().UnixMilli()
}

// history returns copies, newest first: the JSON encoder reads them while a
// probe goroutine may still be appending.
func (r *taskRegistry) history(model string) []*ProbeTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	src := r.byModel[model]
	out := make([]*ProbeTask, 0, len(src))
	for _, t := range src {
		c := *t
		c.Steps = append([]lane.ProbeStep(nil), t.Steps...)
		out = append(out, &c)
	}
	return out
}

// latest is the run the card should show: the newest of either kind.
func (r *taskRegistry) latest(model string) *ProbeTask {
	h := r.history(model)
	if len(h) == 0 {
		return nil
	}
	return h[0]
}
