package openref

import (
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func newFlowStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var flowCatalog = []lane.ModelInfo{{ID: "space-bunny-free"}}
var flowDev = map[string]ModelMeta{
	"space-bunny-free": {ContextWindow: 1048576, MaxOutput: 524288},
}

// The whole point of the veto: a locked model keeps the user's number no
// matter what the catalog publishes on the next 6-hourly pull.
func TestApplyExactHonorsLocks(t *testing.T) {
	st := newFlowStore(t)
	st.SetModelTag("space-bunny-free", store.ModelTag{
		ContextWindow: 1048576, MaxOutput: 65536, Source: store.TagSourceManual,
	})
	locked := func(string) bool { return true }
	if got := ApplyExact(st, flowCatalog, flowDev, locked); len(got) != 0 {
		t.Errorf("locked model was rewritten: %+v", got)
	}
	tag, _ := st.ModelTagOf("space-bunny-free")
	if tag.MaxOutput != 65536 {
		t.Errorf("MaxOutput = %d, want the user's 65536", tag.MaxOutput)
	}
	if len(st.CapabilityAudit()) != 0 {
		t.Error("a vetoed pull must not produce audit noise")
	}
}

// A hand-entered verdict shares the top trust rung with a live probe: the
// catalog's declaration may not silently outrank a human decision.
func TestApplyExactDoesNotOverruleManualCalibration(t *testing.T) {
	st := newFlowStore(t)
	st.SetModelTag("space-bunny-free", store.ModelTag{
		ContextWindow: 300000, MaxOutput: 65536, Source: store.TagSourceManual,
	})
	if got := ApplyExact(st, flowCatalog, flowDev); len(got) != 0 {
		t.Errorf("manual verdict was overwritten: %+v", got)
	}
	tag, _ := st.ModelTagOf("space-bunny-free")
	if tag.ContextWindow != 300000 || tag.Source != store.TagSourceManual {
		t.Errorf("manual verdict lost: %+v", tag)
	}
}

// An unlocked correction must be both applied and recorded, with the source
// named so the UI can say who changed it.
func TestApplyExactWritesAuditTrail(t *testing.T) {
	st := newFlowStore(t)
	got := ApplyExact(st, flowCatalog, flowDev)
	if len(got) != 1 {
		t.Fatalf("applied = %+v, want one model", got)
	}
	entries := st.CapabilityAudit()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].Model != "space-bunny-free" || entries[0].Origin != store.CapOriginModelsDev {
		t.Errorf("entry = %+v, want space-bunny-free / models.dev", entries[0])
	}
	if entries[0].Before != nil {
		t.Errorf("the curated table was answering, Before must be nil: %+v", entries[0].Before)
	}
	if entries[0].After.MaxOutput != 524288 {
		t.Errorf("After.MaxOutput = %d, want 524288", entries[0].After.MaxOutput)
	}
}

// The normalized OpenRouter pass must respect the same veto — otherwise a
// user who locked one model would see it changed by the second source.
func TestApplyHonorsLocks(t *testing.T) {
	st := newFlowStore(t)
	meta := map[string]ModelMeta{"space-bunny": {ContextWindow: 1048576, MaxOutput: 524288}}
	locked := func(string) bool { return true }
	if got := Apply(st, []lane.ModelInfo{{ID: "some-new-model-free"}}, meta, locked); len(got) != 0 {
		t.Errorf("locked pass rewrote rows: %+v", got)
	}
	if len(st.CapabilityAudit()) != 0 {
		t.Error("vetoed pull must not produce audit noise")
	}
}

// No predicate keeps the historic behaviour: existing callers and tests are
// unaffected by the audit feature.
func TestApplyWithoutPredicateStillWorks(t *testing.T) {
	st := newFlowStore(t)
	meta := map[string]ModelMeta{"some-new-model": {ContextWindow: 262144, MaxOutput: 8192}}
	if got := Apply(st, []lane.ModelInfo{{ID: "some-new-model-free"}}, meta); len(got) != 1 {
		t.Fatalf("applied = %+v, want one model", got)
	}
	entries := st.CapabilityAudit()
	if len(entries) != 1 || entries[0].Origin != store.CapOriginOpenRouter {
		t.Errorf("OpenRouter write was not audited: %+v", entries)
	}
}
