package subs

import (
	"testing"
	"time"
)

// The ledger's whole job is to skip a combination already known to be bad.
func TestBanLedgerSkipsKnownBadPair(t *testing.T) {
	m, pool := poolForPick()
	m.Report("n3", "m", "other")
	for i := 0; i < 80; i++ {
		if id := m.Pick("", "m"); id == "n3" {
			t.Fatal("an exit banned for this model was picked")
		}
	}
	// A ban is per (exit, model): another model may still use that exit.
	used := false
	for i := 0; i < 80; i++ {
		if m.Pick("", "other-model") == "n3" {
			used = true
			break
		}
	}
	if !used {
		t.Error("the ban leaked across models")
	}
	_ = pool
}

// A first failure is usually transient, so it parks the pair for seconds, not
// minutes; only repeated failures grow the delay.
func TestBanDelayGrowsWithRepeatedFailures(t *testing.T) {
	m, _ := poolForPick()
	m.Report("n3", "m", "other")
	first := time.Until(m.bans[banKey("n3", "m")])
	if first > banBaseOther+2*time.Second {
		t.Errorf("first transport ban = %v, want about %v (a transient failure)", first, banBaseOther)
	}
	for i := 0; i < 3; i++ {
		m.Report("n3", "m", "other")
	}
	grown := time.Until(m.bans[banKey("n3", "m")])
	if grown <= first {
		t.Errorf("ban did not grow: first %v, after four reports %v", first, grown)
	}
	if grown > banCeilOther+2*time.Second {
		t.Errorf("ban %v exceeded its ceiling %v", grown, banCeilOther)
	}
	// A deterministic verdict is not part of the growth ladder.
	m.Report("n3", "m", "region")
	if got := time.Until(m.bans[banKey("n3", "m")]); got < banRegion-time.Second {
		t.Errorf("region ban = %v, want about %v", got, banRegion)
	}
}

// The count restarts after a long quiet spell, so a node that misbehaves once
// an hour is never treated as persistently broken.
func TestFailCountForgetsAfterTheMemoryWindow(t *testing.T) {
	m, _ := poolForPick()
	m.Report("n3", "m", "other")
	k := banKey("n3", "m")
	m.failAt[k] = time.Now().Add(-2 * failMemory)
	m.Report("n3", "m", "other")
	if got := m.fails[k]; got != 1 {
		t.Errorf("fail count = %d after a quiet spell, want it restarted at 1", got)
	}
}

// Real traffic is proof: a served request clears the cooldown and the pair's
// ban.
func TestReviveClearsCooldownAndBans(t *testing.T) {
	m, health := poolForPick()
	m.park("n3")
	m.Report("n3", "m", "other")
	if _, banned := m.bans[banKey("n3", "m")]; !banned {
		t.Fatal("setup: the pair should be banned")
	}
	m.Revive("n3", "m")
	if health["n3"].CoolUntil != 0 {
		t.Error("Revive left a cooldown in place")
	}
	if _, banned := m.bans[banKey("n3", "m")]; banned {
		t.Error("Revive left the (exit, model) ban in place")
	}
	if health["n3"].LastOKMs == 0 {
		t.Error("Revive did not record the real-traffic success")
	}
}

// A success for one model must not lift another model's ban on the same exit:
// model B working through an exit says nothing about model A, which the exit
// may region-gate or quota separately. The old prefix sweep re-armed A's wall
// on every B success.
func TestReviveDoesNotLiftSiblingModelBans(t *testing.T) {
	m, health := poolForPick()
	m.Report("n3", "model-a", "region")
	m.Report("n3", "model-b", "other")
	// A real request for model B succeeds through the same exit.
	m.Revive("n3", "model-b")
	if _, banned := m.bans[banKey("n3", "model-b")]; banned {
		t.Error("the succeeding pair's ban must be lifted")
	}
	if _, banned := m.bans[banKey("n3", "model-a")]; !banned {
		t.Error("a sibling model's ban was lifted by another model's success")
	}
	// The node-level cooldown is model-independent and does clear.
	if health["n3"].LastOKMs == 0 {
		t.Error("Revive did not record the real-traffic success")
	}
	// And the banned sibling stays skipped by the picker.
	if id := m.Pick("", "model-a"); id == "n3" {
		t.Error("Pick returned an exit still banned for this model")
	}
}

// An expired ban must not keep the exit out of rotation.
func TestExpiredBanReleasesTheExit(t *testing.T) {
	m, _ := poolForPick()
	k := banKey("n3", "m")
	m.bans[k] = time.Now().Add(-time.Second)
	used := false
	for i := 0; i < 80; i++ {
		if m.Pick("", "m") == "n3" {
			used = true
			break
		}
	}
	if !used {
		t.Error("an expired ban still kept the exit out of rotation")
	}
	if _, still := m.bans[k]; still {
		t.Error("the expired entry was not cleared")
	}
}
