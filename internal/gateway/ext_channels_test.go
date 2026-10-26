package gateway

import (
	"os"
	"strings"
	"testing"
)

// mustReadFile reads a source file from this package's own directory. Tests
// assert on shipped files (admin.go, channel_wall.go, web/index.html) rather
// than on a copy, so the assertion cannot drift away from what the gateway
// actually serves.
func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()
	blob, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return blob
}

// Removing or disabling an ext account must push the change into everything
// that cached the model set, exactly like adding one does.
//
// Observed: a ModelScope key was added (37 models appeared in /v1/models), then
// removed — and all 37 stayed listed. An agent picker kept offering them and
// every request behind one of them failed. The add path refreshed the catalog;
// the remove path only deleted the account.
func TestExtAccountChangeRefreshesTheCatalog(t *testing.T) {
	src := string(mustReadFile(t, "ext_channels.go"))

	// Every mutating handler must reach the shared refresh. Split on the
	// function boundaries so one handler cannot satisfy another's assertion.
	for _, fn := range []string{
		"func (s *Server) adminExtAdd(",
		"func (s *Server) adminExtRemove(",
		"func (s *Server) adminExtToggle(",
	} {
		body := funcBody(t, src, fn)
		if !strings.Contains(body, "RefreshCatalog") {
			t.Errorf("%s does not rebuild the catalog — its models would linger in the pickers", fn)
		}
		if !strings.Contains(body, "refreshAfterAccountChange") {
			t.Errorf("%s does not re-inject the agent configs — pickers keep the old roster", fn)
		}
	}
}

// funcBody returns one function's source, from its signature to the closing
// brace at column 0 (Go prints top-level braces unindented).
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("no %s in ext_channels.go", sig)
	}
	rest := src[i:]
	if end := strings.Index(rest, "\n}\n"); end > 0 {
		return rest[:end]
	}
	return rest
}

// The wall must report a channel's model count from the same place its roster
// is written. Qoder is the exception: its models arrive as a synced custom
// provider entry, never through channels/qoder.json, so reading the file would
// show "2 accounts · 0 models" next to a working pool.
func TestChannelWallCoversEveryChannel(t *testing.T) {
	src := string(mustReadFile(t, "channel_wall.go"))
	for _, fn := range []string{
		"wallKeylessRows", "wallWorkBuddyRow", "wallCodeArtsRow",
		"wallQoderRow", "wallZCodeRow", "wallTraeRow", "wallExtRows",
	} {
		if !strings.Contains(src, "func (s *Server) "+fn+"(") {
			t.Errorf("channel_wall.go lost %s — that channel would never reach the wall", fn)
		}
	}
}
