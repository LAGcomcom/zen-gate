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
	"zen-gate/internal/store"
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
	// A Kilo row is verified by its roster entry, not by a request upstream, so
	// the run carries the two phases a click reports and never a first byte.
	want := []string{lane.PhaseRequest, lane.PhaseVerdict}
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
// the user-added providers too. It used to run the free lane alone and leave
// every custom model on the placeholder "custom" state until each card was
// clicked by hand.
func TestReprobeCoversCustomProviderModels(t *testing.T) {
	up := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"pong"}}]}`,
		`[DONE]`,
	})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	s.Store.Mutate(func(cfg *store.Config) {
		cfg.Providers = []store.Provider{{
			ID: "myprov", Name: "我的供应商", BaseURL: up.URL,
			Protocol: "openai", Enabled: true,
			Models: []string{"my-model"},
		}}
	})

	resp, err := http.Post(ts.URL+"/admin/api/reprobe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("reprobe → %d", resp.StatusCode)
	}

	const id = "myprov/my-model"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.customProbeOf(id); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pr, ok := s.customProbeOf(id)
	if !ok {
		t.Fatalf("reprobe never probed %s: no verdict was filed", id)
	}
	if pr.State == "" || pr.State == "custom" {
		t.Fatalf("state = %q, want a real verdict", pr.State)
	}

	// The card reads its state from that verdict, so it must no longer show
	// the placeholder.
	out := getJSON(t, ts.URL+"/admin/api/state")
	for _, mv := range out["models"].([]any) {
		m := mv.(map[string]any)
		if m["id"] != id {
			continue
		}
		if m["state"] == "custom" {
			t.Fatalf("card still parked on the placeholder state: %v", m)
		}
		return
	}
	t.Fatalf("%s is missing from the state rows", id)
}

// A second press while the first round is still running must not buy a second
// upstream call per model: the task registry de-duplicates, and these
// upstreams bill per request.
func TestReprobeTwiceDoesNotDoubleProbe(t *testing.T) {
	release := make(chan struct{})
	var hits int32
	// The provider gets its own upstream so the free-lane round running in
	// parallel cannot be counted as a custom-provider call.
	provSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer provSrv.Close()

	laneUp := fakeUpstream(t, []string{
		`{"choices":[{"delta":{"content":"pong"}}]}`,
		`[DONE]`,
	})
	defer laneUp.Close()
	s := newTestServer(t, laneUp)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	s.Store.Mutate(func(cfg *store.Config) {
		cfg.Providers = []store.Provider{{
			ID: "myprov", Name: "我的供应商", BaseURL: provSrv.URL,
			Protocol: "openai", Enabled: true,
			Models: []string{"my-model"},
		}}
	})

	postReprobe(t, ts)
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("the first round never reached the upstream")
	}
	postReprobe(t, ts)

	close(release)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := getJSON(t, ts.URL+"/admin/api/tasks?model=myprov/my-model")
		runs, _ := out["runs"].([]any)
		if len(runs) > 0 && runs[0].(map[string]any)["status"] == taskDone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want the in-flight round reused", got)
	}
}

func postReprobe(t *testing.T, ts *httptest.Server) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/admin/api/reprobe", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("reprobe → %d", resp.StatusCode)
	}
}
