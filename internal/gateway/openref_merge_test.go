package gateway

import (
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func newBareServer(t *testing.T) (*Server, *lane.Lane, *store.Store) {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	ln := lane.NewLane()
	return New(ln, st), ln, st
}

// A CapsOnly row (the public capacity reference) states windows and nothing
// else. modalityCapsOf must NOT turn it into a modality verdict: Known stays
// false so routing keeps its soft-pass and the name heuristics still answer
// the vision/audio/file questions — only the capacities and the source badge
// come through.
func TestModalityCapsCapsOnly(t *testing.T) {
	s, _, st := newBareServer(t)
	// An id no name rule matches, so the only signal is the CapsOnly row.
	st.SetModelTag("zeta-new-free", store.ModelTag{
		CapsOnly: true, ContextWindow: 1000000, MaxOutput: 64000,
		Source: store.TagSourceListing,
	})
	c := s.modalityCapsOf("zeta-new-free")
	if c.Known {
		t.Errorf("CapsOnly must not claim a modality verdict (Known=true)")
	}
	if c.Vision || c.Audio || c.File {
		t.Errorf("CapsOnly must not set modalities: %+v", c)
	}
	if c.ContextWindow != 1000000 || c.MaxOutput != 64000 {
		t.Errorf("CapsOnly capacities must merge: %+v", c)
	}
	if c.Source != store.TagSourceListing {
		t.Errorf("Source = %q, want 上游声明 so the badge shows where the numbers came from", c.Source)
	}
}

// A CapsOnly row must not, on replay, overwrite a modality some stronger
// source already recorded. applyStoredTags omits the bool pointers for CapsOnly
// rows precisely because ApplyCapabilityTags writes Audio/File unconditionally
// — without that guard a capacity-only fill would silence a probed model's
// audio/file support back to false on the very next catalog rebuild.
func TestApplyStoredTagsCapsOnlyKeepsModalities(t *testing.T) {
	s, ln, st := newBareServer(t)
	cat, _, _ := ln.Snapshot()
	if len(cat) == 0 {
		t.Fatal("fallback catalog empty")
	}
	id := cat[0].ID

	// First a probe verdict that raises audio/file to true on this model.
	st.SetModelTag(id, store.ModelTag{
		Vision: true, Audio: true, File: true, Source: store.TagSourceProbe,
	})
	s.applyStoredTags()
	if m := findModel(ln, id); m == nil || !m.AudioInput || !m.FileInput {
		t.Fatalf("probe replay should raise audio/file: %+v", m)
	}

	// A later CapsOnly fill for the same id must carry capacities without
	// dragging audio/file back to its unset false bools.
	st.SetModelTag(id, store.ModelTag{
		CapsOnly: true, ContextWindow: 524288, MaxOutput: 131072,
		Source: store.TagSourceListing,
	})
	s.applyStoredTags()
	m := findModel(ln, id)
	if m == nil {
		t.Fatal("model vanished")
	}
	if !m.AudioInput || !m.FileInput {
		t.Errorf("CapsOnly replay silenced probed modalities: %+v", m)
	}
	if m.ContextWindow != 524288 || m.MaxOutput != 131072 {
		t.Errorf("CapsOnly replay should still apply the capacities: %+v", m)
	}
}

func findModel(ln *lane.Lane, id string) *lane.ModelInfo {
	cat, _, _ := ln.Snapshot()
	for i := range cat {
		if cat[i].ID == id {
			return &cat[i]
		}
	}
	return nil
}
