package lane

import "testing"

// nextCandidate returns the best failover target. With the route wall set, it
// must skip channels the winning rule denies and restrict to the allowed set.
func TestRouteWallDenyChannelSkips(t *testing.T) {
	l := NewLane()
	l.smartRouting = false // isolate wall filtering from capability ordering
	l.catalog = []ModelInfo{
		{ID: "a-kilo", Channel: "kilo"},
		{ID: "b-workbuddy", Channel: "workbuddy"},
		{ID: "c-zcode", Channel: "zcode"},
	}
	l.availability = map[string]ProbeResult{"a-kilo": {}, "b-workbuddy": {}, "c-zcode": {}}
	l.SetRouteWall([]RouteRule{{Enabled: true, Name: "no-wb", MatchType: "any", DenyChannels: []string{"workbuddy"}}})

	allow, deny := l.routeAllowDeny("any")
	got := l.nextCandidate(Needs{}, map[string]bool{}, allow, deny)
	if got == "b-workbuddy" {
		t.Fatalf("route wall deny must skip workbuddy, got %q", got)
	}
	if got == "" {
		t.Fatalf("expected a non-denied model, got empty")
	}
}

func TestRouteWallAllowChannelRestricts(t *testing.T) {
	l := NewLane()
	l.smartRouting = false
	l.catalog = []ModelInfo{
		{ID: "a-kilo", Channel: "kilo"},
		{ID: "b-workbuddy", Channel: "workbuddy"},
	}
	l.availability = map[string]ProbeResult{"a-kilo": {}, "b-workbuddy": {}}
	l.SetRouteWall([]RouteRule{{Enabled: true, Name: "only-wb", MatchType: "any", AllowChannels: []string{"workbuddy"}}})

	allow, deny := l.routeAllowDeny("any")
	got := l.nextCandidate(Needs{}, map[string]bool{}, allow, deny)
	if got != "b-workbuddy" {
		t.Fatalf("route wall allow must restrict to workbuddy, want b-workbuddy, got %q", got)
	}
}

func TestRouteWallDisabledRuleIgnored(t *testing.T) {
	l := NewLane()
	l.smartRouting = false
	l.catalog = []ModelInfo{
		{ID: "a-kilo", Channel: "kilo"},
		{ID: "b-workbuddy", Channel: "workbuddy"},
	}
	l.availability = map[string]ProbeResult{"a-kilo": {}, "b-workbuddy": {}}
	l.SetRouteWall([]RouteRule{{Enabled: false, Name: "off", MatchType: "any", DenyChannels: []string{"workbuddy"}}})

	allow, deny := l.routeAllowDeny("any") // disabled rule -> nil,nil
	got := l.nextCandidate(Needs{}, map[string]bool{}, allow, deny)
	if got == "" {
		t.Fatalf("disabled rule must not filter, got empty")
	}
}

// routeAllowDeny is the unit under test for the short-circuit + priority logic.
func TestRouteWallMatchModelScopes(t *testing.T) {
	l := NewLane()
	l.smartRouting = false
	l.catalog = []ModelInfo{
		{ID: "claude-sonnet", Channel: "kilo"},
		{ID: "gpt-x", Channel: "workbuddy"},
	}
	l.availability = map[string]ProbeResult{"claude-sonnet": {}, "gpt-x": {}}
	l.SetRouteWall([]RouteRule{{Enabled: true, Name: "claude-only", MatchType: "model", Match: "claude", DenyChannels: []string{"workbuddy"}}})

	_, denyClaude := l.routeAllowDeny("claude-sonnet")
	if !denyClaude["workbuddy"] {
		t.Fatalf("claude request should deny workbuddy")
	}
	_, denyGpt := l.routeAllowDeny("gpt-x")
	if denyGpt["workbuddy"] {
		t.Fatalf("gpt request should NOT match the claude rule, so workbuddy stays allowed")
	}
}

// Highest-priority matching rule wins; a lower-priority allow cannot rescue a
// channel a higher-priority rule denied.
func TestRouteWallPriorityShortCircuit(t *testing.T) {
	l := NewLane()
	l.smartRouting = false
	l.catalog = []ModelInfo{
		{ID: "a-kilo", Channel: "kilo"},
		{ID: "b-workbuddy", Channel: "workbuddy"},
	}
	l.availability = map[string]ProbeResult{"a-kilo": {}, "b-workbuddy": {}}
	l.SetRouteWall([]RouteRule{
		{Enabled: true, Name: "allow-wb-low", MatchType: "any", AllowChannels: []string{"workbuddy"}, Priority: 1},
		{Enabled: true, Name: "deny-wb-high", MatchType: "any", DenyChannels: []string{"workbuddy"}, Priority: 10},
	})
	// High-priority deny wins even though low-priority allows it.
	_, deny := l.routeAllowDeny("anything")
	if !deny["workbuddy"] {
		t.Fatalf("higher-priority deny must win over lower-priority allow")
	}
	got := l.nextCandidate(Needs{}, map[string]bool{}, nil, deny)
	if got == "b-workbuddy" {
		t.Fatalf("priority: workbuddy should be denied, got %q", got)
	}
}

func TestRouteWallEmptyFallsThrough(t *testing.T) {
	l := NewLane()
	l.smartRouting = false
	l.catalog = []ModelInfo{{ID: "a-kilo", Channel: "kilo"}, {ID: "b-workbuddy", Channel: "workbuddy"}}
	l.availability = map[string]ProbeResult{"a-kilo": {}, "b-workbuddy": {}}
	l.SetRouteWall(nil)
	got := l.nextCandidate(Needs{}, map[string]bool{}, nil, nil)
	if got == "" {
		t.Fatalf("no wall must behave like plain nextCandidate, got empty")
	}
	allow, deny := l.routeAllowDeny("x")
	if allow != nil || deny != nil {
		t.Fatalf("empty wall must return nil,nil; got %v,%v", allow, deny)
	}
}
