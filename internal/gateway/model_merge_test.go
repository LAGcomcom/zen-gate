package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// A rotation can mirror one id across the Zen lane and the Kilo pool. The
// 模型 page must show ONE card listing both channels (two cards would fight
// over the same hidden checkbox and double-count the same quota row), and
// the primary channel — the one the card groups under — is the copy a
// request would actually ride: the probe-healthy one.
func TestAdminStateMergesSameIDAcrossChannels(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	ln := lane.NewLane()
	ln.SetCatalog([]lane.ModelInfo{
		{ID: "deepseek-v4.1-flash-free", Wire: "chat", ContextWindow: 1024, MaxOutput: 16},
		{ID: "deepseek-v4.1-flash-free", Wire: "chat", Channel: lane.ChannelKilo, ContextWindow: 1024, MaxOutput: 16},
		{ID: "glm-5.3-flash-free", Wire: "chat", ContextWindow: 1024, MaxOutput: 16},
	})
	s := New(ln, st)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state struct {
		Models []struct {
			ID       string   `json:"id"`
			Channel  string   `json:"channel"`
			Channels []string `json:"channels"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, m := range state.Models {
		counts[m.ID]++
		if m.ID == "deepseek-v4.1-flash-free" {
			if len(m.Channels) != 2 {
				t.Fatalf("merged card channels = %v, want [zen kilo]", m.Channels)
			}
			if m.Channel != "zen" && m.Channel != "kilo" {
				t.Fatalf("merged card primary = %q", m.Channel)
			}
		}
	}
	if counts["deepseek-v4.1-flash-free"] != 1 {
		t.Fatalf("mirrored id rendered %d cards, want 1", counts["deepseek-v4.1-flash-free"])
	}
	if counts["glm-5.3-flash-free"] != 1 {
		t.Fatalf("single-channel id rendered %d cards", counts["glm-5.3-flash-free"])
	}
}
