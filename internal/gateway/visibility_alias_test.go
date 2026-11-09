package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// A visibility toggle used to compact the published snapshot's HiddenAuto
// backing array in place (cur.HiddenAuto[:0] + append). A reader that captured
// the previous snapshot saw rewritten elements, and both snapshots shared one
// array. The toggle must publish a fresh slice instead.
func TestVisibilityToggleLeavesPublishedSnapshotIntact(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	s := newTestServer(t, up)

	s.Store.Mutate(func(cfg *store.Config) {
		cfg.HiddenModels = []string{"drop"}
		cfg.HiddenAuto = []string{"keep", "drop"}
	})
	previous := s.Store.Config()
	wantPrevious := append([]string(nil), previous.HiddenAuto...)

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/admin/api/models/visibility", "application/json",
		strings.NewReader(`{"ids":["drop"],"hidden":false}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("visibility = %d, 期望 200", resp.StatusCode)
	}

	if got := previous.HiddenAuto; !reflect.DeepEqual(got, wantPrevious) {
		t.Fatalf("已发布的旧快照被原地改写: got %v, want %v", got, wantPrevious)
	}
	if got, want := s.Store.Config().HiddenAuto, []string{"keep"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("新快照 HiddenAuto = %v, 期望 %v", got, want)
	}
}
