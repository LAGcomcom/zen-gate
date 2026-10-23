package store

import (
	"os"
	"path/filepath"
	"testing"
)

// One channel, one file: opening a fresh home creates an enabled file per
// known channel, roster writes land in the right file, and the enable flag
// survives both a flip and a roster rewrite.
func TestChannelFilesRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ChannelIDs {
		if !st.ChannelEnabled(id) {
			t.Fatalf("channel %s should default to enabled", id)
		}
		if _, err := os.Stat(filepath.Join(home, "channels", id+".json")); err != nil {
			t.Fatalf("channel file missing for %s: %v", id, err)
		}
	}

	st.WriteChannelRoster("zen", []ChannelModel{
		{ID: "deepseek-v4.1-flash-free", Name: "DeepSeek V4.1 Flash", ContextWindow: 1048576, Reasoning: true},
		{ID: "glm-5.3-flash-free", Name: "GLM 5.3 Flash"},
	})
	if err := st.SetChannelEnabled("kilo", false); err != nil {
		t.Fatal(err)
	}

	// A new store over the same home must read the files back exactly.
	st2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if st2.ChannelEnabled("kilo") {
		t.Fatal("the disabled kilo channel did not survive a reload")
	}
	if !st2.ChannelEnabled("zen") {
		t.Fatal("zen must stay enabled")
	}
	views := st2.ChannelViews()
	for _, v := range views {
		if v["id"] == "zen" && v["models"] != 2 {
			t.Fatalf("zen roster count = %v, want 2", v["models"])
		}
		if v["id"] == "kilo" && v["enabled"] != false {
			t.Fatalf("kilo enabled = %v, want false", v["enabled"])
		}
	}

	// Rewriting a roster must never resurrect a disabled channel.
	st2.WriteChannelRoster("kilo", []ChannelModel{{ID: "kilo-auto/free"}})
	if st2.ChannelEnabled("kilo") {
		t.Fatal("a roster write turned a disabled channel back on")
	}

	// A hand-broken channel file must not keep the gateway from starting.
	if err := os.WriteFile(filepath.Join(home, "channels", "zen.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(); err != nil {
		t.Fatalf("a corrupt channel file broke Open: %v", err)
	}
}
