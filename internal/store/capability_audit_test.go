package store

import "testing"

func newAuditStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Every automatic catalog correction must leave a before→after pair. Without
// it the user can see the new number but never learn what was replaced.
func TestSetModelTagTrackedRecordsBeforeAfter(t *testing.T) {
	st := newAuditStore(t)
	first := ModelTag{CapsOnly: true, ContextWindow: 1048576, MaxOutput: 65536, Source: TagSourceListing}
	if !st.SetModelTagTracked("space-bunny-free", first, CapOriginModelsDev) {
		t.Fatal("first write must be recorded")
	}
	second := ModelTag{CapsOnly: true, ContextWindow: 1048576, MaxOutput: 524288, Source: TagSourceListing}
	if !st.SetModelTagTracked("space-bunny-free", second, CapOriginModelsDev) {
		t.Fatal("changed write must be recorded")
	}
	entries := st.CapabilityAudit()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// CapabilityAudit is newest-first.
	if entries[0].Before == nil || entries[0].Before.MaxOutput != 65536 {
		t.Errorf("second change must carry the previous verdict, got %+v", entries[0].Before)
	}
	if entries[0].After.MaxOutput != 524288 {
		t.Errorf("After.MaxOutput = %d, want 524288", entries[0].After.MaxOutput)
	}
	if entries[0].Origin != CapOriginModelsDev {
		t.Errorf("Origin = %q, want %q", entries[0].Origin, CapOriginModelsDev)
	}
	if len(entries[0].Fields) != 1 || entries[0].Fields[0] != "maxOutput" {
		t.Errorf("Fields = %v, want only maxOutput", entries[0].Fields)
	}
	if entries[1].Before != nil {
		t.Errorf("first change had no previous row, got %+v", entries[1].Before)
	}
}

// A 6-hourly pull re-reading an unchanged catalog must not churn the log.
func TestSetModelTagTrackedSkipsIdenticalWrite(t *testing.T) {
	st := newAuditStore(t)
	row := ModelTag{CapsOnly: true, ContextWindow: 200000, MaxOutput: 32000, Source: TagSourceListing}
	if !st.SetModelTagTracked("m1", row, CapOriginModelsDev) {
		t.Fatal("first write must be recorded")
	}
	if st.SetModelTagTracked("m1", row, CapOriginModelsDev) {
		t.Error("identical write must not produce a second entry")
	}
	if got := len(st.CapabilityAudit()); got != 1 {
		t.Errorf("entries = %d, want 1", got)
	}
}

// A zero capacity means "this source says nothing", which SetModelTag
// inherits — but a rollback restoring a genuinely zeroed verdict must not.
func TestSetModelTagExactDoesNotInherit(t *testing.T) {
	st := newAuditStore(t)
	st.SetModelTag("m1", ModelTag{ContextWindow: 1000000, MaxOutput: 64000, Source: TagSourceProbe})
	st.SetModelTagExact("m1", ModelTag{MaxOutput: 0, Source: TagSourceManual})
	got, _ := st.ModelTagOf("m1")
	if got.ContextWindow != 0 || got.MaxOutput != 0 {
		t.Errorf("exact write inherited old values: %+v", got)
	}
}

func TestDeleteModelTagReturnsToCuratedTable(t *testing.T) {
	st := newAuditStore(t)
	st.SetModelTag("m1", ModelTag{ContextWindow: 200000, Source: TagSourceListing})
	st.DeleteModelTag("m1")
	if _, ok := st.ModelTagOf("m1"); ok {
		t.Error("row must be gone so the curated table answers again")
	}
}

// Locks live in Config, which is deep-copied on every Mutate. Forgetting the
// new field in clone() would silently drop every veto on the next settings
// save — this test pins that down.
func TestCapabilityLocksSurviveConfigClone(t *testing.T) {
	st := newAuditStore(t)
	st.SetCapabilityLock("space-bunny-free", true)
	st.Mutate(func(c *Config) { c.Notifications = false })
	if !st.CapabilityLocks()["space-bunny-free"] {
		t.Fatal("lock lost after a Config clone")
	}
	st.SetCapabilityLock("space-bunny-free", false)
	if st.CapabilityLocks()["space-bunny-free"] {
		t.Fatal("unlock did not clear the veto")
	}
}

// The audit trail and the catalog snapshot are decision records, not caches:
// they must survive a restart.
func TestCapabilityAuditAndRefsPersist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.SetModelTagTracked("m1", ModelTag{ContextWindow: 1048576, MaxOutput: 65536,
		Source: TagSourceListing}, CapOriginModelsDev)
	if err := st.SetCapabilityRefs(CapOriginModelsDev, map[string]RefCap{
		"m1": {ContextWindow: 1048576, MaxOutput: 65536},
	}); err != nil {
		t.Fatal(err)
	}
	again, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(again.CapabilityAudit()); got != 1 {
		t.Errorf("audit entries after restart = %d, want 1", got)
	}
	if got := again.CapabilityRefs().ModelsDev["m1"]; got.MaxOutput != 65536 {
		t.Errorf("refs after restart = %+v, want MaxOutput 65536", got)
	}
}

func TestSetCapabilityRefsRejectsUnknownSource(t *testing.T) {
	st := newAuditStore(t)
	if err := st.SetCapabilityRefs("somewhere", nil); err == nil {
		t.Error("unknown catalog source must be refused")
	}
}
