package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// The 模型 page's progress band is driven by server-side task state, because
// the grid is rebuilt every 10s and would otherwise wipe a running click. What
// these tests pin down is the contract the dashboard reads.

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s → %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func postProbe(t *testing.T, ts *httptest.Server, model string) map[string]any {
	t.Helper()
	resp, err := http.Post(ts.URL+"/admin/api/probe/"+model, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("probe %s → %d", model, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProbeOneRecordsItsRun(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"pong"}}]}`,
		`[DONE]`,
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	postProbe(t, ts, "mimo-v2.6-flash-free")

	out := getJSON(t, ts.URL+"/admin/api/tasks?model=mimo-v2.6-flash-free")
	runs, _ := out["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want the one probe just run", out["runs"])
	}
	run := runs[0].(map[string]any)
	if run["status"] != taskDone {
		t.Fatalf("status = %v, want the run closed", run["status"])
	}
	if run["kind"] != "probe" {
		t.Fatalf("kind = %v", run["kind"])
	}
	steps, _ := run["steps"].([]any)
	var names []string
	for _, sv := range steps {
		names = append(names, sv.(map[string]any)["name"].(string))
	}
	want := []string{lane.PhaseRequest, lane.PhaseFirstByte, lane.PhaseVerdict}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("phases = %v, want %v", names, want)
	}
}

func TestStateRowsCarryTheLatestRun(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"pong"}}]}`,
		`[DONE]`,
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	postProbe(t, ts, "mimo-v2.6-flash-free")

	out := getJSON(t, ts.URL+"/admin/api/state")
	for _, mv := range out["models"].([]any) {
		m := mv.(map[string]any)
		if m["id"] != "mimo-v2.6-flash-free" {
			continue
		}
		task, ok := m["task"].(map[string]any)
		if !ok {
			t.Fatalf("model row has no task: %v", m)
		}
		if task["status"] != taskDone {
			t.Fatalf("task = %v", task)
		}
		return
	}
	t.Fatal("the probed model is missing from the state rows")
}

func TestSecondClickReusesTheRunInsteadOfCallingUpstreamAgain(t *testing.T) {
	release := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()
	s := newTestServer(t, srv)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	firstDone := make(chan map[string]any, 1)
	go func() { firstDone <- postProbe(t, ts, "mimo-v2.6-flash-free") }()

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("the first probe never reached the upstream")
	}

	second := postProbe(t, ts, "mimo-v2.6-flash-free")
	if second["running"] != true {
		t.Fatalf("second click = %v, want it to report the run already going", second)
	}
	out := getJSON(t, ts.URL+"/admin/api/tasks?model=mimo-v2.6-flash-free")
	runs := out["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the in-flight run reused", len(runs))
	}
	if runs[0].(map[string]any)["status"] != taskRunning {
		t.Fatalf("run 0 = %v, want still running", runs[0])
	}

	close(release)
	<-firstDone
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want the queued probe not fired twice", got)
	}
}
