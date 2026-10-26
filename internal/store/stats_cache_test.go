package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// The day buckets are the only durable place usage lives: Recent is capped at
// 40 rows, so a cache hit that lands only there is gone within the hour. Before
// DayStat carried CacheRead, a measured 165k-token cache hit (99.3% of the
// prompt) vanished from stats.json entirely — the panel had no input to render
// a hit rate from, which is why "we have no cache" looked true.
func TestRecordPersistsCacheFieldsInDayBuckets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	st.Record(lane.CallRecord{
		Model: "deepseek-v4.1-flash", Ok: true, Input: 166741, Output: 929,
		CacheRead: 165632, CacheWrite: 512, At: at,
	})
	if err := st.FlushStats(); err != nil {
		t.Fatal(err)
	}

	days, _ := st.SnapshotStats()
	day := days[time.UnixMilli(at).Format("2006-01-02")]
	if day == nil {
		t.Fatal("the record did not create a day bucket")
	}
	if day.Input != 166741 || day.Output != 929 {
		t.Errorf("input/output = %d/%d, want 166741/929", day.Input, day.Output)
	}
	if day.CacheRead != 165632 {
		t.Errorf("CacheRead = %d, want 165632", day.CacheRead)
	}
	if day.CacheWrite != 512 {
		t.Errorf("CacheWrite = %d, want 512", day.CacheWrite)
	}

	// And the FIELDS must survive serialization: the in-memory snapshot above
	// would pass even if the JSON tag were misspelled, and a field the panel
	// cannot read back from disk is the same as no field at all.
	raw, err := os.ReadFile(filepath.Join(home, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"cacheRead": 165632`, `"cacheWrite": 512`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("stats.json is missing %s\n%s", key, raw)
		}
	}
}

// A call that reports no cache must not grow the file: both fields are
// omitempty, so a kilo/zen day bucket stays the shape older builds wrote and an
// older binary reading the file keeps working.
func TestRecordOmitsCacheFieldsWhenZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZEN_GATE_HOME", home)
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Record(lane.CallRecord{Model: "kilo-auto/free", Ok: true, Input: 47, Output: 36,
		At: time.Now().UnixMilli()})
	if err := st.FlushStats(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cacheRead") || strings.Contains(string(raw), "cacheWrite") {
		t.Errorf("a no-cache call wrote cache fields into stats.json:\n%s", raw)
	}
}

// A day bucket written before these fields existed unmarshals with both at
// zero, and Record must keep folding into it without losing the older numbers.
func TestRecordAccumulatesCacheFieldsOnAnExistingBucket(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	st.Record(lane.CallRecord{Model: "m", Ok: true, Input: 100, Output: 10, At: at})
	st.Record(lane.CallRecord{Model: "m", Ok: true, Input: 200, Output: 20, CacheRead: 150, At: at})

	days, _ := st.SnapshotStats()
	day := days[time.UnixMilli(at).Format("2006-01-02")]
	if day == nil {
		t.Fatal("no day bucket")
	}
	if day.Requests != 2 || day.Input != 300 || day.Output != 30 {
		t.Errorf("totals = %d req / %d in / %d out, want 2/300/30", day.Requests, day.Input, day.Output)
	}
	if day.CacheRead != 150 {
		t.Errorf("CacheRead = %d, want 150 accumulated across the two calls", day.CacheRead)
	}
}
