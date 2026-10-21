package store

import (
	"testing"

	"zen-gate/internal/lane"
)

// An install that predates the pool must come back with its one account in the
// pool, and the legacy field must be cleared so there is exactly one home for
// these credentials.
func TestMigrateLegacyWorkBuddyAccount(t *testing.T) {
	legacy := lane.WBAccount{
		AccessToken: "at-legacy", RefreshToken: "rt-legacy", UID: "u-1",
		Nickname: "张三", Enabled: true, Realm: lane.WBRealmCN,
	}
	cfg := Config{
		SchemaVersion: 11,
		WorkBuddy:     legacy,
	}
	// Run only the migration half the way Open does, without touching disk.
	if cfg.SchemaVersion < 12 {
		cfg.SchemaVersion = 12
		if len(cfg.WorkBuddyAccounts) == 0 {
			cfg.WorkBuddyAccounts = []lane.WBAccount{cfg.WorkBuddy}
		}
		cfg.WorkBuddy = lane.WBAccount{}
	}
	for i := range cfg.WorkBuddyAccounts {
		if cfg.WorkBuddyAccounts[i].ID == "" {
			cfg.WorkBuddyAccounts[i].ID = lane.WBAccountID(cfg.WorkBuddyAccounts[i])
		}
	}

	if len(cfg.WorkBuddyAccounts) != 1 {
		t.Fatalf("pool = %d accounts, want the legacy one carried over", len(cfg.WorkBuddyAccounts))
	}
	got := cfg.WorkBuddyAccounts[0]
	if got.AccessToken != "at-legacy" || got.UID != "u-1" || got.Realm != lane.WBRealmCN {
		t.Fatalf("carried account = %+v", got)
	}
	if got.ID == "" {
		t.Fatal("the carried account has no pool id: admin routes could not address it")
	}
	if cfg.WorkBuddy.AccessToken != "" {
		t.Fatal("the legacy field must be cleared once migrated")
	}
	if cfg.SchemaVersion != 12 {
		t.Fatalf("schemaVersion = %d, want 12", cfg.SchemaVersion)
	}
}

// An export is what people paste into an issue: every account's tokens must be
// masked, not just the first one's.
func TestMaskedHidesEveryWorkBuddyToken(t *testing.T) {
	s := &Store{}
	s.Replace(Config{
		SchemaVersion: 12,
		WorkBuddyAccounts: []lane.WBAccount{
			{ID: "a", AccessToken: "at-a", RefreshToken: "rt-a"},
			{ID: "b", AccessToken: "at-b", RefreshToken: "rt-b"},
		},
	})
	masked := s.Masked()
	for _, a := range masked.WorkBuddyAccounts {
		if a.AccessToken != MaskedValue || a.RefreshToken != MaskedValue {
			t.Errorf("account %s exported as %q/%q, want both masked", a.ID, a.AccessToken, a.RefreshToken)
		}
	}
	// The live config must be untouched: Masked works on a copy.
	for _, a := range s.Config().WorkBuddyAccounts {
		if a.AccessToken == MaskedValue {
			t.Fatalf("Masked mutated the live config for account %s", a.ID)
		}
	}
}

// Mutate publishes a deep copy: a snapshot a caller is still reading must not
// change under it when the pool is edited.
func TestCloneDoesNotAliasAccountSlice(t *testing.T) {
	s := &Store{}
	s.Replace(Config{
		SchemaVersion:     12,
		WorkBuddyAccounts: []lane.WBAccount{{ID: "a", AccessToken: "at-a"}},
	})
	before := s.Config()
	s.Mutate(func(c *Config) { c.WorkBuddyAccounts[0].AccessToken = "at-changed" })
	if before.WorkBuddyAccounts[0].AccessToken != "at-a" {
		t.Fatal("the earlier snapshot was mutated through the new one")
	}
	if s.Config().WorkBuddyAccounts[0].AccessToken != "at-changed" {
		t.Fatal("the mutation did not land")
	}
}

func TestUpsertWorkBuddyAccountById(t *testing.T) {
	s := &Store{}
	s.Replace(Config{SchemaVersion: 12})
	s.UpsertWorkBuddyAccount(lane.WBAccount{ID: "a", AccessToken: "at-1"})
	s.UpsertWorkBuddyAccount(lane.WBAccount{ID: "b", AccessToken: "at-b"})
	s.UpsertWorkBuddyAccount(lane.WBAccount{ID: "a", AccessToken: "at-2"})

	accts := s.Config().WorkBuddyAccounts
	if len(accts) != 2 {
		t.Fatalf("pool = %d accounts, want 2 (same id must update in place)", len(accts))
	}
	if accts[0].ID != "a" || accts[0].AccessToken != "at-2" {
		t.Fatalf("account a = %+v, want the updated token in place", accts[0])
	}
	if accts[1].ID != "b" {
		t.Fatalf("account order changed: %+v", accts)
	}
}

// The channel switch is derived, never stored: an independent boolean could
// disagree with the accounts it is supposed to describe.
func TestWorkBuddyEnabledIsDerived(t *testing.T) {
	cfg := Config{WorkBuddyAccounts: []lane.WBAccount{
		{ID: "a", AccessToken: "at-a", Enabled: false},
		{ID: "b", AccessToken: "at-b", Enabled: true},
	}}
	if !cfg.WorkBuddyEnabled() {
		t.Fatal("one enabled account means the channel serves")
	}
	cfg.WorkBuddyAccounts[1].Enabled = false
	if cfg.WorkBuddyEnabled() {
		t.Fatal("no enabled account means the channel is off")
	}
	// An enabled entry with no token cannot serve either.
	cfg.WorkBuddyAccounts[1].Enabled = true
	cfg.WorkBuddyAccounts[1].AccessToken = ""
	if cfg.WorkBuddyEnabled() {
		t.Fatal("an enabled but tokenless account must not count as serving")
	}
}

func TestMoveWorkBuddyAccountFirst(t *testing.T) {
	s := &Store{}
	s.Replace(Config{SchemaVersion: 12, WorkBuddyAccounts: []lane.WBAccount{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}})
	s.MoveWorkBuddyAccountFirst("c")
	accts := s.Config().WorkBuddyAccounts
	want := []string{"c", "a", "b"}
	for i, id := range want {
		if accts[i].ID != id {
			t.Fatalf("order = %v, want %v", ids(accts), want)
		}
	}
}

func ids(accts []lane.WBAccount) []string {
	out := make([]string, 0, len(accts))
	for _, a := range accts {
		out = append(out, a.ID)
	}
	return out
}
