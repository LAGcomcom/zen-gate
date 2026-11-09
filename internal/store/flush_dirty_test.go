package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A failed flush must not clear the dirty flag, otherwise the batch is lost
// until some later record happens to re-arm it.
func TestFlushFlagsSurviveWriteFailure(t *testing.T) {
	st := openPerfStore(t)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.Home = blocker // a regular file, so both writes fail

	st.mu.Lock()
	st.stats.dirty = true
	st.mu.Unlock()
	if err := st.FlushStats(); err == nil {
		t.Fatal("FlushStats succeeded against a non-directory home")
	}
	st.mu.Lock()
	statsDirty := st.stats.dirty
	st.mu.Unlock()
	if !statsDirty {
		t.Fatal("stats dirty flag cleared after a failed write")
	}

	st.AddTTFTSample("m", 100)
	st.FlushPerf()
	st.mu.Lock()
	perfDirty := st.perfDirty
	st.mu.Unlock()
	if !perfDirty {
		t.Fatal("perf dirty flag cleared after a failed write")
	}
}
