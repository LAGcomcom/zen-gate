package relay

import (
	"testing"
	"time"
)

func resetBal() {
	bal = &keyBalancer{rr: map[string]int{}, cool: map[string]map[string]time.Time{}}
}

func TestPickKeyRoundRobin(t *testing.T) {
	resetBal()
	keys := []string{"k1", "k2", "k3"}
	pid := "rr"
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		k, _ := pickKey(pid, keys)
		seen[k]++
	}
	if seen["k1"] != 3 || seen["k2"] != 3 || seen["k3"] != 3 {
		t.Fatalf("round-robin uneven: %v", seen)
	}
}

func TestPickKeySkipsCooled(t *testing.T) {
	resetBal()
	keys := []string{"good", "bad"}
	pid := "skip"
	coolKey(pid, "bad", 30*time.Second)
	if k, _ := pickKey(pid, keys); k != "good" {
		t.Fatalf("got %q, want good", k)
	}
}

func TestPickKeyAllCooledLeastBad(t *testing.T) {
	resetBal()
	keys := []string{"a", "b"}
	pid := "all"
	// a parks for 5s, b for 1s — b lifts sooner, so pick b.
	coolKey(pid, "a", 5*time.Second)
	coolKey(pid, "b", time.Second)
	if k, _ := pickKey(pid, keys); k != "b" {
		t.Fatalf("got %q, want b", k)
	}
}

func TestCooldownFor(t *testing.T) {
	if got := cooldownFor("INVALID_CREDENTIAL"); got != 10*time.Minute {
		t.Fatalf("credential cooldown = %v", got)
	}
	if got := cooldownFor("RATE_LIMIT"); got != 10*time.Second {
		t.Fatalf("quota cooldown = %v", got)
	}
	if got := cooldownFor("SERVER"); got != 15*time.Second {
		t.Fatalf("server cooldown = %v", got)
	}
	if got := cooldownFor("TRANSPORT"); got != 0 {
		t.Fatalf("transport should not cool, got %v", got)
	}
}
