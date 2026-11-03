package relay

import (
	"testing"
	"time"

	"zen-gate/internal/store"
)

// Two keys rotate: consecutive picks walk the list instead of hammering one
// account until it 429s (issue #104).
func TestKeyBalancerRotates(t *testing.T) {
	b := &keyBalancer{}
	seen := map[int]int{}
	for i := 0; i < 6; i++ {
		seen[b.pick(3)]++
	}
	for i := 0; i < 3; i++ {
		if seen[i] != 2 {
			t.Fatalf("pick distribution = %v, want each of 3 keys exactly twice", seen)
		}
	}
}

// A penalized key steps out until its cooldown ends; the others absorb the
// traffic.
func TestKeyBalancerSkipsCoolingKeys(t *testing.T) {
	b := &keyBalancer{}
	b.penalize(0, 2, time.Minute)
	for i := 0; i < 5; i++ {
		if got := b.pick(2); got == 0 {
			t.Fatal("a cooling key was picked")
		}
	}
	// After the cooldown passes the key rejoins the rotation.
	b.cool[0] = time.Now().Add(-time.Second)
	if got := b.pick(2); got != 0 {
		t.Fatalf("pick after cooldown = %d, want the freed key 0", got)
	}
}

// When every key is cooling, the earliest to unfreeze wins — queueing behind
// the best key beats spinning on a dead one.
func TestKeyBalancerAllCoolingReturnsEarliest(t *testing.T) {
	b := &keyBalancer{}
	now := time.Now()
	b.cool = []time.Time{now.Add(5 * time.Minute), now.Add(time.Second), now.Add(3 * time.Minute)}
	if got := b.pick(3); got != 1 {
		t.Fatalf("all cooling: pick = %d, want the earliest-unfreezing key 1", got)
	}
}

// Failure classification drives the park duration: a credential rejection
// parks far longer than a rate limit, and a model-level failure (other 4xx)
// parks nothing because the key is not implicated. Each class runs on its own
// provider: penalize only ever extends a cooldown, never shortens one.
func TestPenalizeKeyByStatusClass(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ra     time.Duration
		want   time.Duration // approx; 0 = no cooldown at all
	}{
		{"2xx parks nothing", 200, 0, 0},
		{"401 parks long", 401, 0, keyCoolCredential},
		{"403 parks long", 403, 0, keyCoolCredential},
		{"429 honours retry-after", 429, 30 * time.Second, 30 * time.Second},
		{"429 default", 429, 0, keyCoolRate},
		{"5xx parks a bit", 502, 0, keyCoolServer},
		{"model-level 4xx parks nothing", 404, 0, 0},
	}
	for _, tc := range cases {
		p := &store.Provider{ID: "prov-" + tc.name, APIKeys: []string{"k1", "k2"}}
		penalizeKey(p, "k1", tc.status, tc.ra)
		b := balancerFor(p.ID, 2)
		if tc.want == 0 {
			if len(b.cool) > 0 && !b.cool[0].IsZero() {
				t.Fatalf("%s: cooled for %v, want none", tc.name, time.Until(b.cool[0]))
			}
			continue
		}
		d := time.Until(b.cool[0])
		if d <= tc.want-time.Second || d > tc.want+time.Second {
			t.Fatalf("%s: cool = %v, want about %v", tc.name, d, tc.want)
		}
	}
}

// A shorter new cooldown never shortens an existing one.
func TestPenalizeKeyNeverShortens(t *testing.T) {
	p := &store.Provider{ID: "prov-monotonic", APIKeys: []string{"k1", "k2"}}
	penalizeKey(p, "k1", 429, time.Minute)
	penalizeKey(p, "k1", 429, time.Second)
	b := balancerFor(p.ID, 2)
	if d := time.Until(b.cool[0]); d < 50*time.Second {
		t.Fatalf("a short retry-after shortened an existing cooldown: %v", d)
	}
}
