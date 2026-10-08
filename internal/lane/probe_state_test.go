package lane

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamFrames serves a scripted sequence: refusal first, then a normal
// stream, so a test can see whether a negative verdict got rechecked.
func refusalThenOK(t *testing.T, refusals int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if int(n) <= refusals {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"The model is unavailable for this account"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	old := UpstreamBase
	UpstreamBase = srv.URL
	t.Cleanup(func() { UpstreamBase = old; srv.Close() })
	return srv, &hits
}

// A verdict is one exit sampled once. Free quota is per egress IP and moves on
// a seconds scale, so a verdict must stop speaking for the model — otherwise a
// single 429-shaped answer keeps a working model hidden for the whole
// probeIntervalMinutes cycle.
func TestFreshStateExpiresAStaleVerdict(t *testing.T) {
	fresh := ProbeResult{State: StateUnavailable, At: time.Now().Add(-time.Minute).UnixMilli()}
	if got := FreshState(fresh); got != StateUnavailable {
		t.Errorf("a 1-minute-old verdict = %q, want it to still count", got)
	}
	stale := ProbeResult{State: StateUnavailable, At: time.Now().Add(-31 * time.Minute).UnixMilli()}
	if got := FreshState(stale); got != StateUnknown {
		t.Errorf("a 31-minute-old verdict = %q, want unknown once the TTL passed", got)
	}
	if got := FreshState(ProbeResult{}); got != StateUnknown {
		t.Errorf("no verdict at all = %q, want unknown", got)
	}
}

// The client-facing list used to delete any model one probe had refused, so
// 面板标 unavailable 的模型从 GET /v1/models 直接消失、客户端选不到它。
func TestServableModelsKeepsProbeRefusedModels(t *testing.T) {
	l := NewLane()
	l.catalog = []ModelInfo{
		{ID: "refused-free", Name: "refused", Wire: "chat"},
		{ID: "blocked-free", Name: "blocked", Wire: "chat"},
		{ID: "judge", Name: "judge", Wire: "systemone", SystemOne: true},
	}
	l.availability["refused-free"] = ProbeResult{Model: "refused-free", State: StateUnavailable,
		At: time.Now().UnixMilli()}
	l.availability["blocked-free"] = ProbeResult{Model: "blocked-free", State: StateRegionBlock,
		At: time.Now().UnixMilli()}

	got := map[string]bool{}
	for _, m := range l.ServableModels() {
		got[m.ID] = true
	}
	if !got["refused-free"] {
		t.Errorf("a probe-unavailable model was deleted from the client list — one exit's answer is not a global verdict")
	}
	if !got["blocked-free"] {
		t.Errorf("region-blocked model vanished even though exposeRegion is on")
	}
	if got["judge"] {
		t.Errorf("the decision model must stay out of a chat picker")
	}

	// The exposure switch is a user choice, not a probe verdict, so it still wins.
	l.SetExposeRegion(false)
	for _, m := range l.ServableModels() {
		if m.ID == "blocked-free" {
			t.Errorf("exposeRegion=false was ignored")
		}
	}
}

// Routing still honours a verdict — that is what keeps a request off a dead
// model — but only while it is fresh.
func TestNextCandidateGivesAStaleVerdictNoVeto(t *testing.T) {
	l := NewLane()
	l.catalog = []ModelInfo{{ID: "a-free", Name: "a", Wire: "chat"}, {ID: "b-free", Name: "b", Wire: "chat"}}
	l.strategy = StrategyCatalog
	l.smartRouting = false

	l.availability["a-free"] = ProbeResult{Model: "a-free", State: StateUnavailable, At: time.Now().UnixMilli()}
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "b-free" {
		t.Errorf("fresh verdict: candidate = %q, want the refused model skipped", got)
	}

	l.availability["a-free"] = ProbeResult{Model: "a-free", State: StateUnavailable,
		At: time.Now().Add(-31 * time.Minute).UnixMilli()}
	if got := l.nextCandidate(Needs{}, map[string]bool{}); got != "a-free" {
		t.Errorf("stale verdict: candidate = %q, want the expired refusal to stop vetoing", got)
	}
}

// Real traffic is the best evidence there is: a call that returned a completion
// proves the model works, so it flips the panel immediately and holds the
// verdict down for a window instead of the next probe overwriting it again.
func TestNoteRealSuccessFlipsOnceAndBlocksTheNextProbe(t *testing.T) {
	_, hits := refusalThenOK(t, 1<<30) // never succeeds: a probe can only refuse
	l := NewLane()
	l.catalog = []ModelInfo{{ID: "muse-free", Name: "muse", Wire: "chat"}}
	l.availability["muse-free"] = ProbeResult{Model: "muse-free", State: StateThrottled,
		At: time.Now().UnixMilli()}

	var changes int32
	l.OnChange = func() { atomic.AddInt32(&changes, 1) }

	l.NoteRealSuccess("muse-free")
	_, av, _ := l.Snapshot()
	if av["muse-free"].State != StateAvailable {
		t.Fatalf("after a real success the panel still says %q", av["muse-free"].State)
	}

	// Nothing to flip the second time: every successful request would otherwise
	// trigger a dashboard rebuild.
	before := atomic.LoadInt32(&changes)
	l.NoteRealSuccess("muse-free")
	l.NoteRealSuccess("muse-free")
	if after := atomic.LoadInt32(&changes); after != before {
		t.Errorf("a no-op success fired %d change events, want 0", after-before)
	}

	// The protection window is what makes 面板和事实不符 stop recurring: a probe
	// may not put the negative verdict back while real traffic says otherwise.
	l.ProbeOne("muse-free")
	_, av, _ = l.Snapshot()
	if av["muse-free"].State != StateAvailable {
		t.Errorf("a probe downgraded a model real traffic had just succeeded with: %q", av["muse-free"].State)
	}
	if atomic.LoadInt32(hits) == 0 {
		t.Errorf("no probe request was even attempted")
	}
}

// Outside the window the probe speaks again — the protection must not become a
// permanent immunity.
func TestProbeCanDowngradeOnceTheWindowCloses(t *testing.T) {
	refusalThenOK(t, 1<<30)
	l := NewLane()
	l.catalog = []ModelInfo{{ID: "muse-free", Name: "muse", Wire: "chat"}}
	l.mu.Lock()
	l.availability["muse-free"] = ProbeResult{Model: "muse-free", State: StateAvailable,
		At: time.Now().UnixMilli()}
	l.realOK["muse-free"] = time.Now().Add(-2 * realSuccessProtect).UnixMilli()
	l.mu.Unlock()

	r := l.ProbeOne("muse-free")
	if r.State != StateUnavailable {
		t.Fatalf("probe verdict = %q, want the refusal to be read as unavailable", r.State)
	}
	_, av, _ := l.Snapshot()
	if av["muse-free"].State != r.State {
		t.Errorf("after the window the probe verdict is %q, want %q", av["muse-free"].State, r.State)
	}
}

// One refusal is not a verdict when several exits exist: re-ask, and let any
// success win. This is what turns the reporter's 6 funded exits out of 20 into
// a model that reads available instead of throttled.
func TestNegativeVerdictIsRecheckedOnOtherExits(t *testing.T) {
	_, hits := refusalThenOK(t, 1) // first request refuses, the retry succeeds
	l := NewLane()
	l.catalog = []ModelInfo{{ID: "space-bunny-free", Name: "bunny", Wire: "chat"}}

	r := l.ProbeOne("space-bunny-free")
	if atomic.LoadInt32(hits) < 2 {
		t.Fatalf("upstream hits = %d, want a refusal rechecked on another exit", atomic.LoadInt32(hits))
	}
	if r.State != StateAvailable {
		t.Errorf("verdict after a successful recheck = %q, want available", r.State)
	}
}
