package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// A gateway that multiplexes many suppliers has to answer "how many tokens did
// THIS API burn". Day totals alone cannot: the same model can be served by the
// Zen lane, a mirrored free pool, or a custom provider.
func TestRecordSplitsUsageByChannel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	st.Record(lane.CallRecord{Model: "deepseek-v4.1-flash", Channel: "zen", Ok: true,
		Input: 100, Output: 20, CacheRead: 80, At: at})
	st.Record(lane.CallRecord{Model: "deepseek-v4.1-flash", Channel: "kilo", Ok: true,
		Input: 50, Output: 10, At: at})
	st.Record(lane.CallRecord{Model: "seed/gpt-x", Channel: "seed", Ok: false,
		Input: 7, Output: 0, At: at})
	st.Record(lane.CallRecord{Model: "seed/gpt-x", Channel: "seed", Ok: true,
		Input: 30, Output: 5, At: at})

	days, _ := st.SnapshotStats()
	day := days[time.UnixMilli(at).Format("2006-01-02")]
	if day == nil || len(day.Channels) != 3 {
		t.Fatalf("channels = %+v, want zen/kilo/seed", day)
	}
	zen := day.Channels["zen"]
	if zen.Requests != 1 || zen.Input != 100 || zen.Output != 20 || zen.CacheRead != 80 {
		t.Errorf("zen = %+v", zen)
	}
	seed := day.Channels["seed"]
	if seed.Requests != 2 || seed.Failed != 1 || seed.Input != 37 || seed.Output != 5 {
		t.Errorf("seed = %+v, want 2 requests / 1 failed / 37 in / 5 out", seed)
	}
	// The day total must still be the sum of all channels.
	if day.Input != 187 || day.Output != 35 || day.Failed != 1 {
		t.Errorf("day total = %d/%d failed=%d", day.Input, day.Output, day.Failed)
	}
}

// Records written by older builds have no Channel. A configured custom
// provider prefixes its models with its id, so that one is recoverable; the
// rest must stay unattributed instead of being silently assigned to Zen.
func TestRecordAttributesLegacyRecordsWithoutGuessing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *Config) {
		c.Providers = []Provider{{ID: "seed", Name: "Seed", BaseURL: "https://seed.test/v1"}}
	})
	at := time.Now().UnixMilli()
	st.Record(lane.CallRecord{Model: "seed/gpt-x", Ok: true, Input: 10, Output: 2, At: at})
	st.Record(lane.CallRecord{Model: "kilo-auto/free", Ok: true, Input: 9, Output: 1, At: at})

	days, _ := st.SnapshotStats()
	ch := days[time.UnixMilli(at).Format("2006-01-02")].Channels
	if ch["seed"] == nil || ch["seed"].Input != 10 {
		t.Errorf("namespaced legacy record not attributed: %+v", ch)
	}
	if ch[ChannelUnattributed] == nil || ch[ChannelUnattributed].Input != 9 {
		t.Errorf("free-lane legacy record must stay unattributed: %+v", ch)
	}
	if ch["zen"] != nil || ch["kilo-auto"] != nil {
		t.Errorf("guessed a supplier for a legacy record: %+v", ch)
	}
}

// SnapshotStats hands the UI a deep copy: Record keeps writing into the live
// maps while the admin page renders the snapshot.
func TestSnapshotStatsDeepCopiesChannelMaps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	st.Record(lane.CallRecord{Model: "m", Channel: "zen", Ok: true, Input: 4, Output: 1, At: at})

	days, _ := st.SnapshotStats()
	day := days[time.UnixMilli(at).Format("2006-01-02")]
	day.Channels["zen"].Input = 9999
	day.Models["m"] = 9999
	again, _ := st.SnapshotStats()
	live := again[time.UnixMilli(at).Format("2006-01-02")]
	if live.Channels["zen"].Input != 4 || live.Models["m"] != 1 {
		t.Errorf("snapshot aliased live stats: %+v / %+v", live.Channels, live.Models)
	}
}

// The per-channel ledger has to survive a restart: a field that only lives in
// memory makes the CSV export and the usage panel disagree after a reboot.
func TestChannelStatsPersistToDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Record(lane.CallRecord{Model: "m", Channel: "workbuddy", Ok: true,
		Input: 1234, Output: 56, CacheRead: 1000, At: time.Now().UnixMilli()})
	if err := st.FlushStats(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`"channels"`, `"workbuddy"`, `"cacheRead": 1000`, `"output": 56`} {
		if !strings.Contains(string(raw), p) {
			t.Errorf("channel ledger missing %s from stats.json: %s", p, raw)
		}
	}
}
