package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func nodeAt(id string, models ...string) *store.Provider {
	if len(models) == 0 {
		models = []string{"m"}
	}
	return &store.Provider{
		ID: id, Name: id, BaseURL: "https://" + id + ".test/v1",
		APIKey: "k", Enabled: true, Models: models,
	}
}

func nodeErr(code string) *lane.UpstreamError {
	return &lane.UpstreamError{Code: code, Message: code}
}

func nodeOK(e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
	if e != nil {
		e(lane.Chunk{Delta: "ok"})
	}
	return lane.Usage{Output: 1}, "stop", nil
}

// The whole point of health-aware ordering: after one node fails, the NEXT
// request must not pay that node's timeout again. It goes straight to the
// healthy twin, with no error surfaced to the caller.
func TestNodeSwitchSkipsDeadNodeOnFollowingRequest(t *testing.T) {
	s, _, _ := newBareServer(t)
	a, b := nodeAt("a"), nodeAt("b")
	cands := []nodeCandidate{{a, "m"}, {b, "m"}}
	var calls []string
	run := func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
		calls = append(calls, c.Provider.ID)
		if c.Provider.ID == "a" {
			return lane.Usage{}, "", nodeErr(lane.CodeTransport)
		}
		return nodeOK(e)
	}

	res := s.runNodeSwitch(context.Background(), cands, nil, run)
	if len(res.Attempts) != 2 || res.Switches != 1 || res.Served.Provider.ID != "b" {
		t.Fatalf("first request = %+v (served %s, switches %d)", res.Attempts, res.Served.Provider.ID, res.Switches)
	}
	if res.Attempts[1].Err != nil {
		t.Fatalf("the caller should see one clean answer, got %v", res.Attempts[1].Err)
	}

	calls = nil
	res2 := s.runNodeSwitch(context.Background(), cands, nil, run)
	if len(calls) != 1 || calls[0] != "b" {
		t.Fatalf("next request still tried the dead node: %v", calls)
	}
	if len(res2.Attempts) != 1 || res2.Switches != 0 {
		t.Fatalf("healthy request = %+v", res2)
	}
}

// Once content reached the client, switching would duplicate the output.
// The error is then the honest answer (same rule lane.Complete uses).
func TestNodeSwitchStopsAfterContentReachedCaller(t *testing.T) {
	s, _, _ := newBareServer(t)
	cands := []nodeCandidate{{nodeAt("a"), "m"}, {nodeAt("b"), "m"}}
	calls := 0
	run := func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
		calls++
		e(lane.Chunk{Delta: "half an answer"})
		return lane.Usage{}, "", nodeErr(lane.CodeServer)
	}
	res := s.runNodeSwitch(context.Background(), cands, func(lane.Chunk) {}, run)
	if calls != 1 || res.Switches != 0 || len(res.Attempts) != 1 {
		t.Fatalf("a half-stream must not switch: calls=%d res=%+v", calls, res)
	}
}

func TestNodeSwitchRespectsAttemptBudget(t *testing.T) {
	s, _, _ := newBareServer(t)
	var cands []nodeCandidate
	for i := 0; i < 6; i++ {
		cands = append(cands, nodeCandidate{nodeAt(fmt.Sprintf("n%d", i)), "m"})
	}
	run := func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
		return lane.Usage{}, "", nodeErr(lane.CodeServer)
	}
	res := s.runNodeSwitch(context.Background(), cands, nil, run)
	if len(res.Attempts) != maxNodeSwitches+1 || res.Switches != maxNodeSwitches {
		t.Fatalf("attempts=%d switches=%d, want %d/%d", len(res.Attempts), res.Switches, maxNodeSwitches+1, maxNodeSwitches)
	}
}

// A 400 is our own request body: every node would refuse it, and cooling a
// healthy node over a client defect would be a false outage.
func TestNodeSwitchDoesNotRetryRequestDefects(t *testing.T) {
	s, _, _ := newBareServer(t)
	cands := []nodeCandidate{{nodeAt("a"), "m"}, {nodeAt("b"), "m"}}
	calls := 0
	run := func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
		calls++
		return lane.Usage{}, "", nodeErr(lane.CodeClient)
	}
	res := s.runNodeSwitch(context.Background(), cands, nil, run)
	if calls != 1 || res.Switches != 0 {
		t.Fatalf("a 400 must not switch: calls=%d", calls)
	}
	if h := s.book().snapshot(time.Now())["a"]; h.Fails != 0 {
		t.Fatalf("a client error cooled the node: %+v", h)
	}
}

func TestNodeSwitchRecoveryClearsCooldown(t *testing.T) {
	s, _, _ := newBareServer(t)
	a, b := nodeAt("a"), nodeAt("b")
	run := func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
		if c.Provider.ID == "a" {
			return lane.Usage{}, "", nodeErr(lane.CodeTimeout)
		}
		return nodeOK(e)
	}
	s.runNodeSwitch(context.Background(), []nodeCandidate{{a, "m"}, {b, "m"}}, nil, run)
	if h := s.book().snapshot(time.Now())["a"]; h.Fails != 1 || h.CooldownUntil == 0 {
		t.Fatalf("failed node was not cooled: %+v", h)
	}
	// Only the cooled node remains: it is still tried (never removed), and one
	// success lifts the cooldown instead of permanently disabling it.
	res := s.runNodeSwitch(context.Background(), []nodeCandidate{{a, "m"}}, nil,
		func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
			return nodeOK(e)
		})
	if len(res.Attempts) != 1 || res.Attempts[0].Err != nil {
		t.Fatalf("cooled node was skipped entirely: %+v", res.Attempts)
	}
	h := s.book().snapshot(time.Now())["a"]
	if h.Fails != 0 || h.CooldownUntil != 0 || h.Recoveries != 1 {
		t.Fatalf("recovery did not clear the cooldown: %+v", h)
	}
}

// A switch may change the NODE but never the model the caller asked for.
func TestNodeCandidatesForKeepsModelIdentity(t *testing.T) {
	s, _, st := newBareServer(t)
	st.Mutate(func(c *store.Config) {
		c.Providers = []store.Provider{
			*nodeAt("primary"), *nodeAt("twin"), *nodeAt("other", "different-model"),
			{ID: "off", Name: "off", Enabled: false, Models: []string{"m"}},
		}
	})
	cands := s.nodeCandidatesFor(nodeAt("primary"), "m")
	if len(cands) != 2 || cands[0].Provider.ID != "primary" || cands[1].Provider.ID != "twin" {
		t.Fatalf("candidates = %+v, want primary + enabled same-model twin", cands)
	}
	for _, c := range cands {
		if c.Model != "m" {
			t.Fatalf("candidate changed the model: %+v", c)
		}
	}
}

func TestNodeSwitchableCodes(t *testing.T) {
	for _, c := range []string{
		lane.CodeQuota, lane.CodeFreeTier, lane.CodeRegion, lane.CodeServer,
		lane.CodeTimeout, lane.CodeTransport, lane.CodeReset, lane.CodeEmpty,
		lane.CodeCredential,
	} {
		if !nodeSwitchable(nodeErr(c)) {
			t.Errorf("%s is a node-shaped failure and should be switchable", c)
		}
	}
	for _, c := range []string{lane.CodeClient, lane.CodeAborted, ""} {
		if nodeSwitchable(nodeErr(c)) {
			t.Errorf("%s is not a node-shaped failure and must not switch", c)
		}
	}
}

func TestNodesHealthEndpointReportsCooling(t *testing.T) {
	s, ts := newNodesServer(t)
	defer ts.Close()
	s.Store.Mutate(func(c *store.Config) {
		c.Providers = []store.Provider{*nodeAt("a"), *nodeAt("b")}
	})
	cfg := s.Store.Config()
	a, b := &cfg.Providers[0], &cfg.Providers[1]
	s.runNodeSwitch(context.Background(), []nodeCandidate{{a, "m"}, {b, "m"}}, nil,
		func(c nodeCandidate, e func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError) {
			if c.Provider.ID == "a" {
				return lane.Usage{}, "", nodeErr(lane.CodeTransport)
			}
			return nodeOK(e)
		})

	code, j := getAdminJSON(t, ts.URL+"/admin/api/nodes/health")
	if code != http.StatusOK || j["ok"] != true {
		t.Fatalf("health = %d %+v", code, j)
	}
	if j["maxSwitches"] != float64(maxNodeSwitches) {
		t.Fatalf("maxSwitches = %v", j["maxSwitches"])
	}
	rows := j["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	var found bool
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["id"] != "a" {
			continue
		}
		found = true
		if row["healthy"] != false || row["coolingMs"].(float64) <= 0 || row["switchesAway"] != float64(1) {
			t.Fatalf("cooling node row = %+v", row)
		}
	}
	if !found {
		t.Fatal("the failed node is missing from the health report")
	}
}
